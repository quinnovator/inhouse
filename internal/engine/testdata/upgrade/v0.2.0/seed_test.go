package engine

// scripts/upgrade-fixture.sh copies this file into a checkout of v0.2.0 and
// runs it there, so the fixture database is exactly what that release's own
// engine, store and vault write. It uses that release's test fakes, and its
// stacks are the YAML files beside it.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/vault"
)

func TestWriteUpgradeFixture(t *testing.T) {
	dir := os.Getenv("INHOUSE_UPGRADE_FIXTURE")
	if dir == "" {
		t.Skip("INHOUSE_UPGRADE_FIXTURE names the fixture directory to write")
	}
	db, err := store.Open(filepath.Join(dir, "inhouse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	v, err := vault.Open(db, filepath.Join(dir, "age.key"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		rt:    &fakeRuntime{running: map[string]bool{}, starts: map[string]int{}, stops: map[string]int{}, unhealthy: map[int]bool{}},
		vols:  &fakeVolumes{snapshots: map[string]bool{}},
		edges: &fakeEdges{ports: map[string]int{}, nodes: map[string]bool{}, stopped: map[string]error{}, ensures: map[string]int{}},
		db:    db,
	}
	cfg := DefaultConfig()
	cfg.Drain, cfg.HealthInterval = time.Millisecond, time.Millisecond
	h.Engine = New(cfg, db, v, h.rt, h.vols, h.edges)
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join(dir, "stacks", name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	want := func(o store.Operation, state store.OpState, rev int) {
		t.Helper()
		if o.State != state || o.Rev != rev {
			t.Fatalf("%+v", o)
		}
	}
	// The real volume manager allocates each service's subordinate-ID slot
	// on its first deploy; the fake doesn't.
	slot := func(service string) {
		t.Helper()
		if _, err := db.Namespace(admin(), service, 64); err != nil {
			t.Fatal(err)
		}
	}

	// Two versions of a secret: deploys pin the newest.
	for _, value := range []string{"first-password", "second-password"} {
		if err = h.SetSecret(admin(), "notes-db-password", value); err != nil {
			t.Fatal(err)
		}
	}

	// notes: two containers, a volume and a secret. r2 replaces r1, r3 fails
	// its health checks, and a rollback to r2 with its volumes is live as r4.
	slot("notes")
	want(h.deploy(t, read("notes-1.yaml"), "notes-1"), store.Succeeded, 1)
	want(h.deploy(t, read("notes-2.live.yaml"), "notes-2"), store.Succeeded, 2)
	h.rt.unhealthy[3] = true
	want(h.deploy(t, read("notes-3.yaml"), "notes-3"), store.OpFailed, 3)
	delete(h.rt.unhealthy, 3)
	o, err := h.Rollback(admin(), "notes", 2, true, "notes-rollback")
	want(h.run(t, o, err), store.Succeeded, 4)

	// gone: deployed and deleted, leaving its events and namespace slot.
	slot("gone")
	want(h.deploy(t, read("gone.yaml"), ""), store.Succeeded, 1)
	o, err = h.Delete(admin(), "gone", "")
	want(h.run(t, o, err), store.Succeeded, 0)

	// preview-docs: an agent's ephemeral service, expired long before any
	// upgrade test reads it.
	slot("preview-docs")
	o, err = h.Deploy(agent("ci@example.com"), read("preview.yaml"), "")
	want(h.run(t, o, err), store.Succeeded, 1)

	// hello: live as r1, with r2 recorded but not yet started, as if the
	// daemon was upgraded in the middle of a deploy.
	slot("hello")
	want(h.deploy(t, read("hello-1.yaml"), "hello-1"), store.Succeeded, 1)
	if o, err = h.Deploy(admin(), read("hello-2.live.yaml"), "hello-2"); err != nil || o.State != store.Running {
		t.Fatal(o, err)
	}
}
