package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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

func TestHealthMigrationAndTransitions(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schemaV1 + "; PRAGMA user_version=1;" +
		"INSERT INTO services(name,kind,current_rev,created_by,created_at) VALUES('live','persistent',1,'me',0),('new','persistent',0,'me',0)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	live, _ := s.Service(ctx, "live")
	fresh, _ := s.Service(ctx, "new")
	if live.Health != Healthy || fresh.Health != "" {
		t.Fatal("migration did not mark live services healthy", live, fresh)
	}

	r1 := Revision{Service: "live", Rev: 1}
	if err = s.Degrade(ctx, r1, "web: HTTP 500"); err != nil {
		t.Fatal(err)
	}
	if err = s.Restarting(ctx, r1, 1); err != nil {
		t.Fatal(err)
	}
	live, _ = s.Service(ctx, "live")
	if live.Health != Degraded || live.HealthReason != "web: HTTP 500" || live.Restarts != 1 || time.Since(time.Unix(live.RestartedAt, 0)) > time.Minute {
		t.Fatal(live)
	}
	// A revision that is no longer live changes nothing and records nothing.
	if err = s.Recover(ctx, Revision{Service: "live", Rev: 2}); err != nil {
		t.Fatal(err)
	}
	events, _ := s.Events(ctx, EventQuery{Service: "live"})
	if live, _ = s.Service(ctx, "live"); live.Health != Degraded || len(events) != 2 {
		t.Fatal(live, events)
	}
	if err = s.Recover(ctx, r1); err != nil {
		t.Fatal(err)
	}
	if err = s.ClearRestarts(ctx, r1); err != nil {
		t.Fatal(err)
	}
	if live, _ = s.Service(ctx, "live"); live.Health != Healthy || live.HealthReason != "" || live.Restarts != 0 {
		t.Fatal(live)
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

func TestEventsForReadableServicesOnABusyHost(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for i := range 3 {
		_ = s.Event(ctx, "quiet", i, "me", "test", "event")
	}
	_ = s.Event(ctx, "", 0, "me", "secret", "no service")
	for i := range 3000 {
		_ = s.Event(ctx, "busy", i, "me", "test", "event")
	}
	quiet := func(name string) bool { return name == "quiet" }
	newest, err := s.Events(ctx, EventQuery{CanRead: quiet, Limit: 5})
	if err != nil || len(newest) != 3 || newest[0].Rev != 0 || newest[2].Rev != 2 {
		t.Fatal(newest, err)
	}
	after, _ := s.Events(ctx, EventQuery{CanRead: quiet, Since: newest[0].ID, Limit: 1})
	if len(after) != 1 || after[0].Rev != 1 {
		t.Fatal(after)
	}
	all := func(string) bool { return true }
	everything, _ := s.Events(ctx, EventQuery{CanRead: all, Limit: 100})
	for _, ev := range everything {
		if ev.Service == "" {
			t.Fatal("served an event without a service")
		}
	}
	none, _ := s.Events(ctx, EventQuery{CanRead: func(string) bool { return false }, Limit: 100})
	if len(none) != 0 {
		t.Fatal(none)
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

func TestLifecycleMigrationKeepsOperations(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schemaV1 + ";" + healthV2 + "; PRAGMA user_version=2;" +
		"INSERT INTO services(name,kind,current_rev,created_by,created_at) VALUES('live','persistent',1,'me',0);" +
		"INSERT INTO operations(id,kind,service,rev,state,idempotency_key,request_hash,created_by,created_at) VALUES('op1','deploy','live',1,'running','k1','h','me',0)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if o, err := s.OperationByKey(ctx, "k1"); err != nil || o.ID != "op1" || o.State != Running {
		t.Fatal("migration lost an operation", o, err)
	}
	// The running deploy still holds the service, and the new kinds fit.
	var conflict *Conflict
	if _, err = s.StopService(ctx, "live", "me", "", "stop"); !errors.As(err, &conflict) || conflict.OperationID != "op1" {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE operations SET state='succeeded'"); err != nil {
		t.Fatal(err)
	}
	if o, err := s.StopService(ctx, "live", "me", "", "stop"); err != nil || o.Kind != Stop || o.Rev != 1 {
		t.Fatal(o, err)
	}
	if svc, _ := s.Service(ctx, "live"); svc.Stopped == 0 {
		t.Fatal("not marked stopped")
	}
	// A probe that finishes after the stop can't mark it healthy or degraded.
	if err = s.Recover(ctx, Revision{Service: "live", Rev: 1}); err != nil {
		t.Fatal(err)
	}
	if svc, _ := s.Service(ctx, "live"); svc.Health != "" {
		t.Fatal("a stopped service's health changed", svc.Health)
	}
}

func TestExtendRefusesAnExpiredService(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	st := stack(t, "preview")
	ttl := "1h"
	st.TTL = &ttl
	o, err := s.Record(ctx, Request{Stack: st, Kind: Deploy, RequestHash: "h", Actor: "me", PortLow: 20000, PortHigh: 20010})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := s.Revision(ctx, "preview", o.Rev)
	if err = s.Cutover(ctx, r, 0); err != nil {
		t.Fatal(err)
	}
	if err = s.Extend(ctx, r, "me"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE services SET expires_at=1"); err != nil {
		t.Fatal(err)
	}
	var conflict *Conflict
	if err = s.Extend(ctx, r, "me"); !errors.As(err, &conflict) {
		t.Fatal("extended an expired service", err)
	}
}
