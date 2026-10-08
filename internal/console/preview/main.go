// preview serves the console on localhost against a real engine with fake
// pods, volumes and tailnet nodes, seeded with a few services. It is a
// development aid: nothing here touches Podman or Tailscale.
//
//	go run ./internal/console/preview -addr 127.0.0.1:8484
//	pnpm -C console dev   # http://localhost:3000, /v1 proxied here
//
// The caller defaults to an admin; set a cookie "as" to "agent" (a preview-*
// deployer), "viewer" or "nobody" to see the console with other grants.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/quinnovator/inhouse/internal/api"
	"github.com/quinnovator/inhouse/internal/authz"
	"github.com/quinnovator/inhouse/internal/engine"
	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/vault"
)

var callers = map[string]authz.Principal{
	"admin":  {ID: "alice@example.com", Login: "alice@example.com", Grants: []authz.Grant{{Role: authz.Admin}}},
	"agent":  {ID: "node:agent-ci", Tags: []string{"tag:inhouse-agent"}, Grants: []authz.Grant{{Role: authz.Deployer, Services: []string{"preview-*"}, Expose: []string{"tailnet"}, MaxTTL: "24h"}}},
	"viewer": {ID: "bob@example.com", Login: "bob@example.com", Grants: []authz.Grant{{Role: authz.Viewer, Services: []string{"notes", "blog"}}}},
	"nobody": {ID: "carol@example.com", Grants: []authz.Grant{}},
}

type callerKey struct{}

func main() {
	addr := flag.String("addr", "127.0.0.1:8484", "listen address")
	flag.Parse()
	dir, err := os.MkdirTemp("", "inhouse-preview-")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	db, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		log.Fatal(err)
	}
	v, err := vault.Open(db, filepath.Join(dir, "age.key"))
	if err != nil {
		log.Fatal(err)
	}
	rt := &runtime{running: map[string]bool{}, up: map[string]time.Time{}, sick: map[string]bool{}, slow: map[string]time.Duration{}}
	cfg := engine.DefaultConfig()
	cfg.Drain = 2 * time.Second
	eng := engine.New(cfg, db, v, rt, volumes{}, &edges{ports: map[string]int{}})
	ctx := context.Background()
	go func() { _ = eng.Run(ctx) }()
	go seed(eng, v, rt)

	s := &api.Server{Engine: eng, WhoIs: func(ctx context.Context, _ string) (authz.Principal, error) {
		p := callers[ctx.Value(callerKey{}).(string)]
		if !p.Authenticated() {
			return p, authz.ErrForbidden
		}
		return p, nil
	}}
	h := s.Handler()
	log.Printf("console preview at http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as := "admin"
		if c, err := r.Cookie("as"); err == nil {
			as = c.Value
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, as)))
	})))
}

func as(name string) context.Context {
	return authz.With(context.Background(), callers[name])
}

func deploy(eng *engine.Engine, who, raw string) {
	o, err := eng.Deploy(as(who), []byte(raw), "")
	if err != nil {
		log.Printf("seed deploy: %v", err)
		return
	}
	if _, err = eng.Wait(as("admin"), o.ID, engine.MaxWait); err != nil {
		log.Printf("seed wait: %v", err)
	}
}

func seed(eng *engine.Engine, v *vault.Vault, rt *runtime) {
	ctx := as("admin")
	_ = v.Set(ctx, "notes-db-password", "hunter2-correct-horse", "alice@example.com")
	_ = v.Set(ctx, "registry-auth/ghcr.io", `{"username":"alice","password":"ghp_example"}`, "alice@example.com")

	notes := func(tag string) string {
		return `name: notes
containers:
  db:
    image: docker.io/library/postgres:17
    env: {POSTGRES_USER: notes}
    secrets: {POSTGRES_PASSWORD: notes-db-password}
    volumes: {data: /var/lib/postgresql/data}
    health: {command: [pg_isready, -U, notes]}
  web:
    image: ghcr.io/example/notes:` + tag + `
    port: 8080
    args: [--listen, ":8080"]
    health: {path: /healthz, timeout: 30s}
    resources: {memory: 256Mi, cpus: 0.5}
`
	}
	var wg sync.WaitGroup
	run := func(fn func()) { wg.Add(1); go func() { defer wg.Done(); fn() }() }
	run(func() {
		deploy(eng, "admin", notes("1.0.0"))
		deploy(eng, "admin", notes("1.0.1"))
		deploy(eng, "admin", notes("1.1.0"))
		if o, err := eng.Rollback(ctx, "notes", 2, false, ""); err == nil {
			_, _ = eng.Wait(ctx, o.ID, engine.MaxWait)
		}
	})
	run(func() {
		deploy(eng, "admin", "name: blog\nexpose: funnel\ncontainers:\n  web:\n    image: ghcr.io/example/blog:31\n    port: 3000\n    health: {path: /}\n")
	})
	run(func() {
		deploy(eng, "admin", "name: hello\ncontainers:\n  web:\n    image: docker.io/traefik/whoami:v1.10\n    port: 80\n    health: {path: /health}\n")
	})
	run(func() {
		deploy(eng, "admin", "name: wiki\nupdate_strategy: recreate\ncontainers:\n  web:\n    image: ghcr.io/example/wiki:2.3.1\n    port: 3000\n    volumes: {data: /data}\n    health: {path: /healthz}\n")
		time.Sleep(5 * time.Second)
		rt.setSick("wiki", true) // hangs: degraded, restarted with backoff
	})
	run(func() {
		deploy(eng, "agent", "name: preview-pr-39\nttl: 18h\ncontainers:\n  web:\n    image: ghcr.io/example/app:pr-39-a\n    port: 8080\n    health: {path: /healthz, timeout: 10s}\n")
		rt.setSick("preview-pr-39", true)
		deploy(eng, "agent", "name: preview-pr-39\nttl: 18h\ncontainers:\n  web:\n    image: ghcr.io/example/app:pr-39-b\n    port: 8080\n    health: {path: /healthz, timeout: 10s}\n")
		rt.setSick("preview-pr-39", false)
	})
	wg.Wait()
	// A deploy that stays in health checks for a while.
	rt.mu.Lock()
	rt.slow["preview-pr-42"] = 45 * time.Second
	rt.mu.Unlock()
	go deploy(eng, "agent", "name: preview-pr-42\nttl: 6h\ncontainers:\n  web:\n    image: ghcr.io/example/app:pr-42\n    port: 8080\n    health: {path: /healthz, timeout: 90s}\n")
	// A refused call, for the timeline.
	_, _ = eng.Deploy(as("viewer"), []byte(notes("2.0.0")), "")
	eng.Audit(as("viewer"), "notes", "POST /v1/deploy: permission denied by tailnet capability grant: no single deployer grant covers service notes with expose tailnet and this ttl")
	log.Print("seeded")
}

// ---- fakes ----

type runtime struct {
	mu      sync.Mutex
	running map[string]bool
	up      map[string]time.Time
	sick    map[string]bool
	slow    map[string]time.Duration
}

func key(r store.Revision) string { return fmt.Sprintf("%s/%d", r.Service, r.Rev) }

func (f *runtime) setSick(service string, sick bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sick[service] = sick
}

func (f *runtime) Resolve(_ context.Context, image string) (string, error) {
	time.Sleep(time.Second)
	if spec.Pinned(image) {
		return image, nil
	}
	sum := sha256.Sum256([]byte(image))
	return image[:strings.LastIndex(image, ":")] + "@sha256:" + hex.EncodeToString(sum[:]), nil
}

func (f *runtime) Up(_ context.Context, r store.Revision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[key(r)] = true
	f.up[key(r)] = time.Now()
	return nil
}

func (f *runtime) Running(_ context.Context, r store.Revision) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[key(r)], nil
}

func (f *runtime) Check(_ context.Context, r store.Revision, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running[key(r)] {
		return errors.New("container is not running")
	}
	if time.Since(f.up[key(r)]) < f.slow[r.Service] {
		return errors.New(container + ": connection refused")
	}
	if f.sick[r.Service] {
		return errors.New(container + ": HTTP request failed")
	}
	return nil
}

func (f *runtime) Stop(_ context.Context, r store.Revision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[key(r)] = false
	return nil
}

func (f *runtime) Remove(_ context.Context, r store.Revision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, key(r))
	return nil
}

func (f *runtime) Logs(_ context.Context, r store.Revision, container string, tail int) (string, error) {
	start := time.Now().Add(-time.Duration(tail) * 7 * time.Second)
	var b strings.Builder
	for i := range tail {
		t := start.Add(time.Duration(i) * 7 * time.Second).UTC().Format(time.RFC3339)
		switch {
		case i == 3:
			fmt.Fprintf(&b, "%s %s starting r%d\n", t, container, r.Rev)
		case i%41 == 7 && r.Service == "notes":
			fmt.Fprintf(&b, "%s connecting with password=hunter2-correct-horse\n", t)
		case i%9 == 0:
			fmt.Fprintf(&b, "%s POST /api/items 201 %dms alice@example.com\n", t, 10+i%30)
		default:
			fmt.Fprintf(&b, "%s GET /healthz 200 %dms\n", t, 1+i%4)
		}
	}
	return b.String(), nil
}

func (f *runtime) Pods(context.Context) ([]store.Revision, error)     { return nil, nil }
func (f *runtime) RemoveOrphan(context.Context, store.Revision) error { return nil }
func (f *runtime) RemoveServiceSecrets(context.Context, string) error { return nil }

type volumes struct{}

func (volumes) Snapshot(context.Context, store.Revision) error     { return nil }
func (volumes) CheckRestore(context.Context, store.Revision) error { return nil }
func (volumes) Restore(context.Context, store.Revision, int) error { return nil }
func (volumes) Delete(context.Context, store.Service) error        { return nil }

type edges struct {
	mu    sync.Mutex
	ports map[string]int
}

func (e *edges) Ensure(_ context.Context, name string, _ store.Kind, _ string) (string, error) {
	return name + ".tail4f2c.ts.net", nil
}
func (e *edges) Switch(name string, port int) error {
	e.mu.Lock()
	e.ports[name] = port
	e.mu.Unlock()
	return nil
}
func (e *edges) Port(name string) int                             { e.mu.Lock(); defer e.mu.Unlock(); return e.ports[name] }
func (e *edges) Clear(name string)                                { e.mu.Lock(); delete(e.ports, name); e.mu.Unlock() }
func (e *edges) Stopped(string) error                             { return nil }
func (e *edges) Delete(context.Context, string, store.Kind) error { return nil }
