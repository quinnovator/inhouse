package console

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// The router's serialized state holds a NUL, which browsers hash as U+FFFD.
const boot = "self.$_TSR={i:\"__root__\x00\"};\r\n"

var build = fstest.MapFS{
	"_shell.html":         {Data: []byte(`<!DOCTYPE html><html><body><p>Loading…</p><script data-tsr-stream-part="">` + boot + `</script><script type="module" async="" src="/assets/index-abc.js"></script></body></html>`)},
	"assets/index-abc.js": {Data: []byte(`console.log(1)`)},
	"icon.svg":            {Data: []byte(`<svg/>`)},
	".vite/manifest.json": {Data: []byte(`{}`)},
}

func get(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Result()
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func TestPagesGetTheShellAndFilesAreServedByPath(t *testing.T) {
	h := New(build)
	for _, path := range []string{"/", "/services/blog", "/events"} {
		r := get(t, h, path)
		if r.StatusCode != http.StatusOK || !strings.Contains(body(t, r), "Loading…") || r.Header.Get("Cache-Control") != "no-cache" {
			t.Fatal(path, r.StatusCode, r.Header)
		}
	}
	r := get(t, h, "/assets/index-abc.js")
	if r.StatusCode != http.StatusOK || !strings.Contains(r.Header.Get("Cache-Control"), "immutable") || !strings.Contains(r.Header.Get("Content-Type"), "javascript") {
		t.Fatal(r.StatusCode, r.Header)
	}
	if r = get(t, h, "/favicon.ico"); r.StatusCode != http.StatusOK || body(t, r) != "<svg/>" {
		t.Fatal("favicon", r.StatusCode)
	}
	for _, path := range []string{"/assets/missing.js", "/.vite/manifest.json", "/robots.txt"} {
		if r = get(t, h, path); r.StatusCode != http.StatusNotFound {
			t.Fatal(path, r.StatusCode)
		}
	}
}

func TestPolicyAllowsOnlyTheShellsInlineScripts(t *testing.T) {
	csp := get(t, New(build), "/").Header.Get("Content-Security-Policy")
	sum := sha256.Sum256([]byte("self.$_TSR={i:\"__root__\uFFFD\"};\n"))
	want := "script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "';"
	if !strings.Contains(csp, want) || strings.Count(csp, "sha256-") != 1 || strings.Contains(csp, "unsafe") {
		t.Fatal(csp)
	}
}

func TestUnbuiltConsoleSaysSo(t *testing.T) {
	r := get(t, New(fstest.MapFS{".gitkeep": {}}), "/services/blog")
	if r.StatusCode != http.StatusOK || !strings.Contains(body(t, r), "isn't built") {
		t.Fatal(r.StatusCode)
	}
}
