package podman

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRefusesUnownedPodBeforeAnyMutation(t *testing.T) {
	mutated := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutated = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Id":"other","Labels":{"inhouse":"1","inhouse.service":"other","inhouse.revision":"1"}}`))
	}))
	defer server.Close()
	p := &Podman{client: &http.Client{Transport: rewriteTransport{server.URL, http.DefaultTransport}}}
	p.Offset = func(context.Context, string) (int, error) { return 1, nil }
	r := store.Revision{Service: "hello", Rev: 1, Hash: "abc"}
	for _, action := range []func(context.Context, store.Revision) error{p.Up, p.Stop, p.Remove} {
		if e := action(context.Background(), r); e == nil {
			t.Fatal("accepted mismatched labels")
		}
	}
	if mutated {
		t.Fatal("mutated unowned pod")
	}
}

func TestLeftoverSecretsAreRemovedByLabel(t *testing.T) {
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE":
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, apiPrefix+"/secrets/"))
		case strings.HasSuffix(r.URL.Path, "/secrets/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"ID":"a1","Spec":{"Name":"svc-preview-x-r1-db","Labels":{"inhouse":"1","inhouse.service":"preview-x","inhouse.revision":"1","inhouse.spec":"h1"}}},
				{"ID":"a2","Spec":{"Name":"svc-preview-x-r2-db","Labels":{"inhouse":"1","inhouse.service":"preview-x","inhouse.revision":"2","inhouse.spec":"h2"}}},
				{"ID":"b1","Spec":{"Name":"svc-blog-r1-db","Labels":{"inhouse":"1","inhouse.service":"blog","inhouse.revision":"1","inhouse.spec":"h1"}}},
				{"ID":"c1","Spec":{"Name":"svc-preview-x-r1-db","Labels":{"inhouse.service":"preview-x"}}},
				{"ID":"c2","Spec":{"Name":"someone-elses","Labels":{"inhouse":"1","inhouse.service":"preview-x"}}}
			]`))
		default: // the orphan pod is already gone
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := &Podman{client: &http.Client{Transport: rewriteTransport{server.URL, http.DefaultTransport}}}
	if err := p.RemoveOrphan(context.Background(), store.Revision{Service: "preview-x", Rev: 1, Hash: "h1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(deleted, ",") != "a1" {
		t.Fatal("orphan removal deleted", deleted)
	}
	deleted = nil
	if err := p.RemoveServiceSecrets(context.Background(), "preview-x"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(deleted, ",") != "a1,a2" {
		t.Fatal("service sweep deleted", deleted)
	}
}

type rewriteTransport struct {
	base string
	next http.RoundTripper
}

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	u := *r.URL
	u.Host = strings.TrimPrefix(t.base, "http://")
	copy.URL = &u
	return t.next.RoundTrip(copy)
}

// Opt-in integration against a real rootless Podman socket. The only objects
// created/removed belong to this disposable revision with verified labels.
func TestPodmanIntegration(t *testing.T) {
	socket := os.Getenv("INHOUSE_TEST_PODMAN_SOCKET")
	if socket == "" {
		t.Skip("set INHOUSE_TEST_PODMAN_SOCKET for real rootless API test")
	}
	p := New(socket)
	p.Network = os.Getenv("INHOUSE_TEST_NETWORK")
	// Exercise the fixed private namespace used for new production services.
	// Offset one fits the runner's default 65536 subordinate IDs and excludes
	// its own UID (mapped at offset zero). Older Podman auto pod namespaces
	// cannot reliably be joined by a second container.
	p.Offset = func(context.Context, string) (int, error) { return 1, nil }
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, e := spec.Parse([]byte(`name: podman-integration
containers:
  sleeper:
    image: docker.io/library/alpine:3.22
    command: ["sleep"]
    args: ["300"]
    health: {command: ["true"]}
  web:
    image: docker.io/traefik/whoami@sha256:200689790a0a0ea48ca45992e0450bc26ccab5307375b41c84dfc4f2475937ab
    port: 80
    health: {path: /health}
`))
	if e != nil {
		t.Fatal(e)
	}
	for _, n := range s.Order {
		c := s.Containers[n]
		c.Image, e = p.Resolve(ctx, c.Image)
		if e != nil {
			t.Fatal(e)
		}
		s.Containers[n] = c
	}
	r := store.Revision{Service: s.Name, Rev: 1, Spec: s, Hash: s.Hash(), Port: 29998}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if e = p.Remove(cleanup, r); e != nil {
			t.Errorf("cleanup: %v", e)
		}
	}()
	if e = p.Up(ctx, r); e != nil {
		t.Fatal(e)
	}
	if e = p.Up(ctx, r); e != nil {
		t.Fatalf("idempotent up: %v", e)
	}
	online, e := p.Running(ctx, r)
	if e != nil || !online {
		t.Fatalf("running=%v %v", online, e)
	}
	for _, n := range s.Order {
		if e = p.Check(ctx, r, n); e != nil {
			t.Fatalf("%s health: %v", n, e)
		}
	}
	if _, e = p.Logs(ctx, r, "web", 20); e != nil {
		t.Fatal(e)
	}
	objects, e := p.Pods(ctx)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, o := range objects {
		if o.Service == r.Service && o.Rev == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("inventory did not find labelled pod")
	}
	if e = p.Stop(ctx, r); e != nil {
		t.Fatal(e)
	}
	if e = p.Up(ctx, r); e != nil {
		t.Fatalf("restore stopped pod: %v", e)
	}
}

func TestStopAlreadyStoppedIsIdempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Id":"mine","Labels":{"inhouse":"1","inhouse.service":"hello","inhouse.revision":"1","inhouse.spec":"hash"}}`))
	}))
	defer server.Close()
	p := &Podman{client: &http.Client{Transport: rewriteTransport{server.URL, http.DefaultTransport}}}
	if e := p.Stop(context.Background(), store.Revision{Service: "hello", Rev: 1, Hash: "hash"}); e != nil {
		t.Fatal(e)
	}
}

func frame(stream byte, payload string) []byte {
	h := []byte{stream, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, payload...)
}

func TestLogsRequestsUpToFiveHundredLines(t *testing.T) {
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/logs") {
			asked = r.URL.Query().Get("tail")
			_, _ = w.Write(frame(1, "out\n"))
			_, _ = w.Write(frame(2, "err\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Id":"mine","Pod":"mine","Labels":{"inhouse":"1","inhouse.service":"hello","inhouse.revision":"1","inhouse.spec":"hash"},"Config":{"Labels":{"inhouse":"1","inhouse.service":"hello","inhouse.revision":"1","inhouse.spec":"hash"}}}`))
	}))
	defer server.Close()
	p := &Podman{client: &http.Client{Transport: rewriteTransport{server.URL, http.DefaultTransport}}}
	r := store.Revision{Service: "hello", Rev: 1, Hash: "hash", Spec: spec.Stack{Containers: map[string]spec.Container{"web": {Port: 80}}}}
	for tail, want := range map[int]string{0: "1", 20: "20", 500: "500", 900: "500"} {
		logs, e := p.Logs(context.Background(), r, "web", tail)
		if e != nil {
			t.Fatal(e)
		}
		if asked != want || logs != "out\nerr\n" {
			t.Fatalf("tail %d: asked %q, logs %q", tail, asked, logs)
		}
	}
}

func TestDemuxKeepsNewestOutput(t *testing.T) {
	var stream bytes.Buffer
	for i := 0; i < 2000; i++ {
		stream.Write(frame(1, fmt.Sprintf("line %04d %s\n", i, strings.Repeat("x", 100))))
	}
	got, e := demuxNewest(&stream, 4096)
	if e != nil {
		t.Fatal(e)
	}
	if len(got) > 4096 || !strings.HasSuffix(got, "\n") || !strings.HasPrefix(got, "line ") || !strings.Contains(got, "line 1999 ") || strings.Contains(got, "line 0000 ") {
		t.Fatalf("unexpected window: %d bytes, starts %q", len(got), got[:20])
	}
	plain, e := demuxNewest(strings.NewReader("plain text\nlog\n"), 4096)
	if e != nil || plain != "plain text\nlog\n" {
		t.Fatalf("plain stream: %q %v", plain, e)
	}
	truncated, e := demuxNewest(bytes.NewReader(frame(1, "complete\n")[:12]), 4096)
	if e != nil || truncated != "comp" {
		t.Fatalf("truncated frame: %q %v", truncated, e)
	}
}

// fakeRegistryPodman serves Podman's pull and inspect endpoints. Pulls of
// references in missing return an error line, as Podman does for a 404 manifest.
func fakeRegistryPodman(t *testing.T, repoDigests []string, missing map[string]bool) (*Podman, *[]string) {
	t.Helper()
	pulls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/images/pull") {
			ref := r.URL.Query().Get("reference")
			pulls = append(pulls, ref+" "+r.URL.Query().Get("policy"))
			if missing[ref] {
				_, _ = w.Write([]byte(`{"error":"manifest unknown"}` + "\n"))
				return
			}
			_, _ = w.Write([]byte(`{"id":"image"}` + "\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"RepoDigests": repoDigests})
	}))
	t.Cleanup(server.Close)
	return &Podman{client: &http.Client{Transport: rewriteTransport{server.URL, http.DefaultTransport}}}, &pulls
}

func TestResolvePinsOnlyADigestTheRegistryServes(t *testing.T) {
	upstream := "registry.example/hello@sha256:" + strings.Repeat("a", 64)
	served := "registry.example/hello@sha256:" + strings.Repeat("b", 64)
	other := "docker.io/traefik/whoami@sha256:" + strings.Repeat("c", 64)
	p, pulls := fakeRegistryPodman(t, []string{other, upstream, served}, map[string]bool{upstream: true})
	got, e := p.Resolve(context.Background(), "registry.example/hello:1")
	if e != nil || got != served {
		t.Fatalf("resolved %q, %v; want %q", got, e, served)
	}
	want := []string{"registry.example/hello:1 always", upstream + " always", served + " always"}
	if strings.Join(*pulls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("pulls:\n%s\nwant:\n%s", strings.Join(*pulls, "\n"), strings.Join(want, "\n"))
	}
}

func TestResolveFailsWhenNoCandidateIsServed(t *testing.T) {
	stale := "registry.example/hello@sha256:" + strings.Repeat("a", 64)
	p, _ := fakeRegistryPodman(t, []string{stale}, map[string]bool{stale: true})
	if got, e := p.Resolve(context.Background(), "registry.example/hello:1"); e == nil {
		t.Fatalf("pinned unserved digest %q", got)
	}
}

func TestResolveKeepsRequestedDigestWithoutRecheck(t *testing.T) {
	pinned := "registry.example/hello@sha256:" + strings.Repeat("d", 64)
	p, pulls := fakeRegistryPodman(t, []string{pinned}, nil)
	got, e := p.Resolve(context.Background(), pinned)
	if e != nil || got != pinned || len(*pulls) != 1 || (*pulls)[0] != pinned+" missing" {
		t.Fatalf("resolved %q, %v, pulls %v", got, e, *pulls)
	}
}
