package store

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/quinnovator/inhouse/internal/spec"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func stack(t *testing.T, name string) spec.Stack {
	t.Helper()
	s, err := spec.Parse([]byte("name: " + name + "\ncontainers:\n  web: {image: docker.io/a/b:1, port: 80}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReopenKeepsVersion(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s, err = Open(file); err != nil {
		t.Fatal(err)
	}
	var v int
	_ = s.db.QueryRow("PRAGMA user_version").Scan(&v)
	_ = s.Close()
	if v != len(migrations) {
		t.Fatal(v)
	}
}

func TestRecordReservesPortsAndSerializesOperations(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	req := Request{Stack: stack(t, "a"), Kind: Deploy, Key: "k", RequestHash: "h", Actor: "me", PortLow: 20000, PortHigh: 20001}
	o, err := s.Record(ctx, req)
	if err != nil || o.Rev != 1 {
		t.Fatal(o, err)
	}
	if again, err := s.Record(ctx, req); err != nil || again.ID != o.ID {
		t.Fatal("key replay", err)
	}
	req.RequestHash = "other"
	var c *Conflict
	if _, err = s.Record(ctx, req); !errors.As(err, &c) {
		t.Fatal("key reuse", err)
	}
	req.Key, req.RequestHash = "", "h"
	if _, err = s.Record(ctx, req); !errors.As(err, &c) || c.OperationID != o.ID {
		t.Fatal("busy", err)
	}
	b, err := s.Record(ctx, Request{Stack: stack(t, "b"), Kind: Deploy, RequestHash: "x", Actor: "me", PortLow: 20000, PortHigh: 20001})
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := s.Revision(ctx, "b", b.Rev)
	ra, _ := s.Revision(ctx, "a", o.Rev)
	if ra.Port == rb.Port {
		t.Fatal("port reused")
	}
	if _, err = s.Record(ctx, Request{Stack: stack(t, "c"), Kind: Deploy, RequestHash: "y", Actor: "me", PortLow: 20000, PortHigh: 20001}); err == nil {
		t.Fatal("port range exhaustion not detected")
	}
}

func TestTombstoneAndExpiry(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	o, _ := s.Record(ctx, Request{Stack: stack(t, "a"), Kind: Deploy, RequestHash: "h", Actor: "me", PortLow: 1, PortHigh: 9})
	if _, err := s.Tombstone(ctx, "a", "me", "", "d", false); err == nil {
		t.Fatal("delete while busy")
	}
	d, err := s.Tombstone(ctx, "a", "reconciler", "", "expire", true)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled, _ := s.Operation(ctx, o.ID); cancelled.State != OpFailed {
		t.Fatal("expiry did not cancel the running deploy")
	}
	if again, _ := s.Tombstone(ctx, "a", "me", "", "d", false); again.ID != d.ID {
		t.Fatal("second delete did not join the first")
	}
	if err = s.FinishDelete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Service(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestEventsWindow(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for i := range 30 {
		_ = s.Event(ctx, "a", i, "me", "test", "event")
	}
	newest, _ := s.Events(ctx, EventQuery{Service: "a", Limit: 5})
	if len(newest) != 5 || newest[4].Rev != 29 || newest[0].Rev != 25 {
		t.Fatal(newest)
	}
	after, _ := s.Events(ctx, EventQuery{Service: "a", Since: newest[0].ID, Limit: 2})
	if len(after) != 2 || after[0].Rev != 26 {
		t.Fatal(after)
	}
}

func TestNamespaceSlotsAreStableAndBounded(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	a, _ := s.Namespace(ctx, "a", 2)
	b, _ := s.Namespace(ctx, "b", 2)
	again, _ := s.Namespace(ctx, "a", 2)
	if a != 0 || b != 1 || again != 0 {
		t.Fatal(a, b, again)
	}
	if _, err := s.Namespace(ctx, "c", 2); err == nil {
		t.Fatal("pool exhaustion not detected")
	}
}

func TestEphemeralSlotsAreReused(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	ttl := "1h"
	preview := stack(t, "preview-a")
	preview.TTL = &ttl
	for i, st := range []spec.Stack{stack(t, "blog"), preview, stack(t, "wiki")} {
		if _, err := s.Record(ctx, Request{Stack: st, Kind: Deploy, RequestHash: st.Name, Actor: "me", PortLow: 20000, PortHigh: 20009}); err != nil {
			t.Fatal(err)
		}
		if slot, err := s.Namespace(ctx, st.Name, 4); err != nil || slot != i {
			t.Fatal(st.Name, slot, err)
		}
	}
	for _, name := range []string{"blog", "preview-a"} {
		if _, err := s.Tombstone(ctx, name, "me", "", "delete "+name, true); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishDelete(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	// The deleted preview's slot 1 is reused; the deleted persistent
	// service's slot 0 is not, since its trashed data keeps that ownership.
	for _, want := range []int{1, 3} {
		if slot, err := s.Namespace(ctx, "next-"+strconv.Itoa(want), 4); err != nil || slot != want {
			t.Fatal(want, slot, err)
		}
	}
	if _, err := s.Namespace(ctx, "full", 4); err == nil {
		t.Fatal("allocated past the pool")
	}
}
