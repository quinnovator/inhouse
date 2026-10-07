package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quinnovator/inhouse/internal/authz"
	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/vault"
)

type fakeRuntime struct {
	mu        sync.Mutex
	running   map[string]bool
	starts    map[string]int
	stops     map[string]int
	unhealthy map[int]bool // revisions whose checks fail
	logs      string
	upHook    func(store.Revision)
	strays    []store.Revision // labelled pods with no revision row
	orphans   []string         // pods RemoveOrphan removed
	swept     []string         // services whose secrets were swept
}

func key(r store.Revision) string { return fmt.Sprintf("%s/%d", r.Service, r.Rev) }

func (f *fakeRuntime) Resolve(_ context.Context, image string) (string, error) {
	if strings.Contains(image, "missing") {
		return "", errors.New("image pull failed: manifest unknown")
	}
	if spec.Pinned(image) {
		return image, nil
	}
	// Tags of the same length resolve to the same digest, standing in for
	// two tags of one image.
	tag := image[strings.LastIndex(image, ":")+1:]
	return image[:strings.LastIndex(image, ":")] + "@sha256:" + strings.Repeat(string("abcdef"[len(tag)%6]), 64), nil
}
func (f *fakeRuntime) Up(_ context.Context, r store.Revision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upHook != nil {
		f.upHook(r)
	}
	f.running[key(r)] = true
	f.starts[key(r)]++
	return nil
}
func (f *fakeRuntime) Running(_ context.Context, r store.Revision) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[key(r)], nil
}
func (f *fakeRuntime) Check(_ context.Context, r store.Revision, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unhealthy[r.Rev] {
		return errors.New("HTTP 500 from /health")
	}
	if !f.running[key(r)] {
		return errors.New("container is not running")
	}
	return nil
}
func (f *fakeRuntime) Stop(_ context.Context, r store.Revision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[key(r)] = false
	f.stops[key(r)]++
	return nil
}
func (f *fakeRuntime) Remove(_ context.Context, r store.Revision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, key(r))
	return nil
}
func (f *fakeRuntime) Logs(context.Context, store.Revision, string, int) (string, error) {
	return f.logs, nil
}
func (f *fakeRuntime) Pods(context.Context) ([]store.Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Revision(nil), f.strays...), nil
}
func (f *fakeRuntime) RemoveOrphan(_ context.Context, r store.Revision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orphans = append(f.orphans, key(r))
	return nil
}
func (f *fakeRuntime) RemoveServiceSecrets(_ context.Context, service string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.swept = append(f.swept, service)
	return nil
}

type fakeVolumes struct {
	snapshots map[string]bool
	restored  []string
	deleted   []string
	onRestore func(store.Revision)
}

func (v *fakeVolumes) Snapshot(_ context.Context, r store.Revision) error {
	if len(r.Spec.VolumeNames()) > 0 {
		v.snapshots[key(r)] = true
	}
	return nil
}
func (v *fakeVolumes) CheckRestore(_ context.Context, r store.Revision) error {
	if !v.snapshots[key(r)] {
		return errors.New("no snapshot")
	}
	return nil
}
func (v *fakeVolumes) Restore(_ context.Context, r store.Revision, from int) error {
	if v.onRestore != nil {
		v.onRestore(r)
	}
	v.restored = append(v.restored, fmt.Sprintf("%s<-r%d", key(r), from))
	return nil
}
func (v *fakeVolumes) Delete(_ context.Context, s store.Service) error {
	v.deleted = append(v.deleted, s.Name)
	return nil
}

type fakeEdges struct {
	mu        sync.Mutex
	ports     map[string]int
	nodes     map[string]bool
	failNext  bool
	deletions []string
}

func (f *fakeEdges) Ensure(_ context.Context, name string, _ store.Kind, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[name] = true
	return name + ".example.ts.net", nil
}
func (f *fakeEdges) Switch(name string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return errors.New("injected crash after cutover")
	}
	f.ports[name] = port
	return nil
}
func (f *fakeEdges) Port(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ports[name]
}
func (f *fakeEdges) Clear(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ports, name)
}
func (f *fakeEdges) Delete(_ context.Context, name string, _ store.Kind) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.nodes, name)
	delete(f.ports, name)
	f.deletions = append(f.deletions, name)
	return nil
}

type harness struct {
	*Engine
	rt    *fakeRuntime
	vols  *fakeVolumes
	edges *fakeEdges
	db    *store.Store
}

func setup(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	v, err := vault.Open(db, filepath.Join(dir, "age.key"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		rt:    &fakeRuntime{running: map[string]bool{}, starts: map[string]int{}, stops: map[string]int{}, unhealthy: map[int]bool{}},
		vols:  &fakeVolumes{snapshots: map[string]bool{}},
		edges: &fakeEdges{ports: map[string]int{}, nodes: map[string]bool{}},
		db:    db,
	}
	cfg := DefaultConfig()
	cfg.Drain, cfg.HealthInterval = time.Millisecond, time.Millisecond
	h.Engine = New(cfg, db, v, h.rt, h.vols, h.edges)
	return h
}

func admin() context.Context {
	return authz.With(context.Background(), authz.Principal{ID: "owner@example.com", Grants: []authz.Grant{{Role: authz.Admin}}})
}

func agent(id string) context.Context {
	return authz.With(context.Background(), authz.Principal{ID: id, Grants: []authz.Grant{{Role: authz.Deployer, Services: []string{"preview-*"}, Expose: []string{"tailnet"}, MaxTTL: "24h"}}})
}

func stack(name, image, extra string) []byte {
	return []byte("name: " + name + "\n" + extra + "containers:\n  web:\n    image: " + image + "\n    port: 80\n    health: {path: /health, timeout: 5s}\n")
}

func withVolume(raw []byte) []byte {
	return []byte(strings.Replace(string(raw), "port: 80\n", "port: 80\n    volumes: {data: /data}\n", 1))
}

// run records a request and reconciles until it settles.
func (h *harness) run(t *testing.T, o store.Operation, err error) store.Operation {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if err = h.reconcile(context.Background(), o.Service); err != nil {
		t.Fatal(err)
	}
	o, err = h.db.Operation(context.Background(), o.ID)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (h *harness) deploy(t *testing.T, raw []byte, key string) store.Operation {
	t.Helper()
	o, err := h.Deploy(admin(), raw, key)
	return h.run(t, o, err)
}

func (h *harness) service(t *testing.T, name string) store.Service {
	t.Helper()
	s, err := h.db.Service(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDeployCutsOverAfterHealth(t *testing.T) {
	h := setup(t)
	o := h.deploy(t, stack("hello", "docker.io/traefik/whoami:1", ""), "k1")
	if o.State != store.Succeeded || o.Rev != 1 {
		t.Fatalf("%+v", o)
	}
	s := h.service(t, "hello")
	r, _ := h.db.Revision(context.Background(), "hello", 1)
	if s.Current != 1 || s.DNS != "hello.example.ts.net" || h.edges.ports["hello"] != r.Port || r.State != store.Live {
		t.Fatalf("%+v %+v", s, r)
	}
	if !spec.Pinned(r.Spec.Containers["web"].Image) {
		t.Fatal("image not pinned to a digest")
	}
}

func TestFailedHealthKeepsLiveAndRollbackIsNewRevision(t *testing.T) {
	h := setup(t)
	h.rt.logs = "boom: listening failed\n"
	h.deploy(t, stack("hello", "docker.io/traefik/whoami:1", ""), "one")
	port := h.edges.ports["hello"]
	h.rt.unhealthy[2] = true
	failed := h.deploy(t, stack("hello", "docker.io/traefik/whoami:two", ""), "two")
	if failed.State != store.OpFailed || !strings.Contains(failed.Reason, "health") {
		t.Fatalf("%+v", failed)
	}
	if s := h.service(t, "hello"); s.Current != 1 || h.edges.ports["hello"] != port {
		t.Fatal("a failed candidate moved traffic")
	}
	if h.rt.running["hello/2"] {
		t.Fatal("failed candidate still running")
	}
	events, _ := h.db.Events(context.Background(), store.EventQuery{Service: "hello", Limit: 100})
	found := false
	for _, ev := range events {
		found = found || (ev.Kind == "failure_logs" && strings.Contains(ev.Message, "boom"))
	}
	if !found {
		t.Fatal("failure logs not recorded")
	}
	o, err := h.Rollback(admin(), "hello", 1, false, "rb")
	o = h.run(t, o, err)
	if o.State != store.Succeeded || o.Rev != 3 || h.service(t, "hello").Current != 3 {
		t.Fatalf("%+v", o)
	}
	r1, _ := h.db.Revision(context.Background(), "hello", 1)
	r3, _ := h.db.Revision(context.Background(), "hello", 3)
	if r1.Hash != r3.Hash {
		t.Fatal("rollback did not copy the exact revision")
	}
	again, err := h.Rollback(admin(), "hello", 1, false, "rb")
	if err != nil || again.ID != o.ID {
		t.Fatal("rollback retry with the same key was not idempotent")
	}
}

func TestIdempotencyAndNoop(t *testing.T) {
	h := setup(t)
	first := h.deploy(t, stack("hello", "docker.io/traefik/whoami:1", ""), "same")
	retry, err := h.Deploy(admin(), stack("hello", "docker.io/traefik/whoami:1", ""), "same")
	if err != nil || retry.ID != first.ID {
		t.Fatal("retry created a new operation")
	}
	if _, err = h.Deploy(admin(), stack("hello", "docker.io/traefik/whoami:2", ""), "same"); err == nil {
		t.Fatal("reused key accepted for a different request")
	}
	// A different tag resolving to the same digest is a no-op on the live revision.
	noop := h.deploy(t, stack("hello", "docker.io/traefik/whoami:7", ""), "other")
	if noop.State != store.Succeeded || noop.Rev != first.Rev {
		t.Fatalf("%+v", noop)
	}
	revs, _ := h.db.Revisions(context.Background(), "hello")
	if len(revs) != 1 {
		t.Fatalf("no-op left %d revisions", len(revs))
	}
}

func TestPullFailureLeavesLiveRevision(t *testing.T) {
	h := setup(t)
	h.deploy(t, stack("hello", "docker.io/traefik/whoami:1", ""), "")
	o := h.deploy(t, stack("hello", "docker.io/library/missing:1", ""), "")
	if o.State != store.OpFailed || !strings.Contains(o.Reason, "pull failed") || h.service(t, "hello").Current != 1 {
		t.Fatalf("%+v", o)
	}
}

func TestResumesAfterCrashAndRestartsAfterReboot(t *testing.T) {
	h := setup(t)
	o, err := h.Deploy(admin(), stack("hello", "docker.io/traefik/whoami:1", ""), "")
	if err != nil {
		t.Fatal(err)
	}
	h.edges.failNext = true
	if err = h.reconcile(context.Background(), "hello"); err == nil {
		t.Fatal("expected the injected crash")
	}
	saved, _ := h.db.Operation(context.Background(), o.ID)
	if saved.State != store.Running || h.service(t, "hello").Current != 1 {
		t.Fatal("cutover was not durable before the crash")
	}
	if o = h.run(t, o, nil); o.State != store.Succeeded || h.edges.ports["hello"] == 0 {
		t.Fatal("did not resume after the crash")
	}
	// Reboot: pods and edges are gone, the database remains.
	h.rt.running["hello/1"] = false
	h.edges.Clear("hello")
	if err = h.reconcile(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if h.rt.starts["hello/1"] != 2 || h.edges.ports["hello"] == 0 {
		t.Fatal("live revision not restored")
	}
	// Steady state does nothing.
	if err = h.reconcile(context.Background(), "hello"); err != nil || h.rt.starts["hello/1"] != 2 {
		t.Fatal("steady state restarted the pod")
	}
}

func TestRecreateStopsWriterAndRestartsItOnFailure(t *testing.T) {
	h := setup(t)
	first := h.deploy(t, withVolume(stack("db", "docker.io/library/postgres:17", "")), "")
	r1, _ := h.db.Revision(context.Background(), "db", first.Rev)
	if r1.Spec.UpdateStrategy != spec.Recreate {
		t.Fatal("volumes did not imply recreate")
	}
	h.rt.upHook = func(r store.Revision) {
		if r.Rev == 2 && h.rt.running["db/1"] {
			t.Error("candidate started while the old writer was running")
		}
	}
	h.rt.unhealthy[2] = true
	o := h.deploy(t, withVolume(stack("db", "docker.io/library/postgres:18.1", "")), "")
	if o.State != store.OpFailed {
		t.Fatalf("%+v", o)
	}
	if !h.rt.running["db/1"] || h.rt.stops["db/1"] == 0 || h.edges.ports["db"] != r1.Port {
		t.Fatal("old revision was not stopped and then restored")
	}
	if !h.vols.snapshots["db/2"] {
		t.Fatal("no pre-deploy snapshot")
	}
}

func TestVolumeRestoreIsAdminOnlyAndStopsWriterFirst(t *testing.T) {
	h := setup(t)
	h.deploy(t, withVolume(stack("blog", "docker.io/library/app:1", "")), "")
	h.deploy(t, withVolume(stack("blog", "docker.io/library/app:22", "")), "")
	deployer := authz.With(context.Background(), authz.Principal{ID: "ci", Grants: []authz.Grant{{Role: authz.Deployer, Services: []string{"blog"}, Expose: []string{"tailnet"}}}})
	if _, err := h.Rollback(deployer, "blog", 1, true, ""); !errors.Is(err, authz.ErrForbidden) {
		t.Fatal(err)
	}
	h.vols.onRestore = func(store.Revision) {
		if h.rt.running["blog/2"] {
			t.Error("restored volumes while the writer was running")
		}
	}
	o, err := h.Rollback(admin(), "blog", 1, true, "")
	if o = h.run(t, o, err); o.State != store.Succeeded || len(h.vols.restored) != 1 || h.vols.restored[0] != "blog/3<-r1" {
		t.Fatalf("%+v %v", o, h.vols.restored)
	}
}

func TestDeleteAndExpiry(t *testing.T) {
	h := setup(t)
	h.deploy(t, stack("hello", "docker.io/traefik/whoami:1", ""), "")
	o, err := h.Delete(admin(), "hello", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Deploy(admin(), stack("hello", "docker.io/traefik/whoami:2", ""), ""); err == nil {
		t.Fatal("deploy accepted while deleting")
	}
	h.rt.strays = []store.Revision{{Service: "hello", Rev: 7}, {Service: "other", Rev: 1}}
	if o = h.run(t, o, nil); o.State != store.Succeeded || len(h.rt.running) != 0 || len(h.edges.deletions) != 1 {
		t.Fatalf("%+v %v", o, h.rt.running)
	}
	if fmt.Sprint(h.rt.orphans) != "[hello/7]" || fmt.Sprint(h.rt.swept) != "[hello]" {
		t.Fatal("teardown left or overreached on stray pods or secrets:", h.rt.orphans, h.rt.swept)
	}
	h.rt.strays = nil
	if _, err = h.db.Service(context.Background(), "hello"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("service row survived delete")
	}

	// An agent may deploy and delete only its own short-lived previews.
	if _, err = h.Deploy(agent("node:a"), stack("preview-x", "docker.io/traefik/whoami:1", "ttl: 48h\n"), ""); !errors.Is(err, authz.ErrForbidden) {
		t.Fatal("ttl above max_ttl accepted")
	}
	if _, err = h.Deploy(agent("node:a"), stack("blog", "docker.io/traefik/whoami:1", "ttl: 1h\n"), ""); !errors.Is(err, authz.ErrForbidden) {
		t.Fatal("service outside grant accepted")
	}
	o, err = h.Deploy(agent("node:a"), stack("preview-x", "docker.io/traefik/whoami:1", "ttl: 1s\n"), "")
	h.run(t, o, err)
	if _, err = h.Delete(agent("node:b"), "preview-x", ""); !errors.Is(err, authz.ErrForbidden) {
		t.Fatal("another agent deleted the preview")
	}
	time.Sleep(1100 * time.Millisecond)
	if err = h.reconcile(context.Background(), "preview-x"); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Service(context.Background(), "preview-x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("expired preview survived")
	}
}

func TestImmutableKindExposureAndReservedName(t *testing.T) {
	h := setup(t)
	h.deploy(t, stack("hello", "docker.io/traefik/whoami:1", ""), "")
	for _, raw := range [][]byte{
		stack("hello", "docker.io/traefik/whoami:1", "ttl: 1h\n"),
		stack("hello", "docker.io/traefik/whoami:1", "expose: funnel\n"),
		stack("deploy", "docker.io/traefik/whoami:1", ""),
	} {
		if _, err := h.Deploy(admin(), raw, ""); !IsInvalid(err) {
			t.Fatalf("accepted %s: %v", raw, err)
		}
	}
}

func TestBusyServiceNamesRunningOperation(t *testing.T) {
	h := setup(t)
	o, err := h.Deploy(admin(), stack("hello", "docker.io/traefik/whoami:1", ""), "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Deploy(admin(), stack("hello", "docker.io/traefik/whoami:2", ""), "")
	var conflict *store.Conflict
	if !errors.As(err, &conflict) || conflict.OperationID != o.ID {
		t.Fatal(err)
	}
	if o.Rev != 0 {
		t.Fatal("pending deploy exposed its reserved revision")
	}
}

func TestSecretsArePinnedAndRedacted(t *testing.T) {
	h := setup(t)
	if err := h.SetSecret(agent("node:a"), "db-password", "x"); !errors.Is(err, authz.ErrForbidden) {
		t.Fatal("non-admin set a secret")
	}
	if err := h.SetSecret(admin(), "db-password", "hunter2-v1"); err != nil {
		t.Fatal(err)
	}
	raw := []byte(strings.Replace(string(stack("app", "docker.io/library/app:1", "")), "port: 80\n", "port: 80\n    secrets: {DB_PASSWORD: db-password}\n", 1))
	h.deploy(t, raw, "")
	r1, _ := h.db.Revision(context.Background(), "app", 1)
	pinned := r1.Spec.Containers["web"].SecretVersions["db-password"]
	if len(pinned) != 64 || strings.Contains(fmt.Sprint(r1), "hunter2") {
		t.Fatal("secret not pinned by hash, or value leaked into the revision")
	}
	_ = h.SetSecret(admin(), "db-password", "hunter2-v2")
	h.rt.logs = "connecting with hunter2-v1\n"
	page, err := h.Logs(admin(), LogQuery{Service: "app"})
	if err != nil || len(page.Lines) != 1 || strings.Contains(page.Lines[0], "hunter2") {
		t.Fatalf("%+v %v", page, err)
	}
	if err = h.DeleteSecret(admin(), "db-password"); err == nil {
		t.Fatal("deleted a secret a running revision uses")
	}
	list, _ := h.ListSecrets(admin())
	if len(list) != 1 || list[0].Name != "db-password" {
		t.Fatal(list)
	}
}

func TestLogPaging(t *testing.T) {
	h := setup(t)
	h.deploy(t, stack("hello", "docker.io/traefik/whoami:1", ""), "")
	lines := []string{}
	for i := range 120 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	h.rt.logs = strings.Join(lines, "\n") + "\n"
	page, err := h.Logs(admin(), LogQuery{Service: "hello", Tail: 50})
	if err != nil || len(page.Lines) != 50 || page.Lines[49] != "line 119" || page.Next == "" {
		t.Fatalf("%+v %v", page, err)
	}
	older, err := h.Logs(admin(), LogQuery{Service: "hello", Tail: 50, Cursor: page.Next})
	if err != nil || older.Lines[49] != "line 69" {
		t.Fatalf("%+v %v", older, err)
	}
	h.rt.logs += "new line\n"
	if _, err = h.Logs(admin(), LogQuery{Service: "hello", Tail: 50, Cursor: page.Next}); !IsInvalid(err) {
		t.Fatal("stale cursor accepted")
	}
}

func TestEventsAreFilteredByGrant(t *testing.T) {
	h := setup(t)
	h.deploy(t, stack("blog", "docker.io/traefik/whoami:1", ""), "")
	_ = h.SetSecret(admin(), "token", "value")
	o, err := h.Deploy(agent("node:a"), stack("preview-a", "docker.io/traefik/whoami:1", "ttl: 1h\n"), "")
	h.run(t, o, err)
	events, err := h.Events(agent("node:a"), store.EventQuery{})
	if err != nil || len(events) == 0 {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Service != "preview-a" {
			t.Fatalf("agent saw %+v", ev)
		}
	}
	if _, err = h.Events(agent("node:a"), store.EventQuery{Service: "blog"}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatal("agent read another service's events")
	}
}

func TestPlanShowsKeysNotValues(t *testing.T) {
	h := setup(t)
	raw := []byte(strings.Replace(string(stack("app", "docker.io/library/app:1", "")), "port: 80\n", "port: 80\n    env: {API_KEY: very-secret-env}\n", 1))
	p, err := h.Plan(agent("node:a"), raw)
	if !errors.Is(err, authz.ErrForbidden) {
		t.Fatal("plan visible without read access")
	}
	p, err = h.Plan(admin(), raw)
	if err != nil || !p.MayApply || p.Exists || strings.Contains(fmt.Sprint(p), "very-secret-env") || p.After["web"].EnvKeys[0] != "API_KEY" {
		t.Fatalf("%+v %v", p, err)
	}
}
