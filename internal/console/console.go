// Package console serves the web console: a single-page app built from
// console/ (TanStack Start in SPA mode) and embedded into inhoused.
//
// The files hold no data, so they are served to anyone who reaches the
// control node. Everything the app shows comes from /v1, where each call is
// identified and authorized exactly like a CLI call.
package console

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

// dist holds the build that `pnpm -C console build` copies here. A checkout
// without one still compiles: dist then holds only .gitkeep.
//
//go:embed all:dist
var dist embed.FS

// Handler serves the embedded build.
func Handler() http.Handler {
	build, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embedded tree is fixed at build time
	}
	return New(build)
}

type file struct {
	name string
	body []byte
	etag string
}

func newFile(name string, body []byte) file {
	sum := sha256.Sum256(body)
	return file{name, body, `"` + hex.EncodeToString(sum[:8]) + `"`}
}

type site struct {
	files  map[string]file // by URL path
	shell  file
	policy string
}

// New serves a console build: its files by path, and the prerendered shell
// for every other path so the app can route it. Without a shell it serves a
// page saying the console isn't built.
func New(build fs.FS) http.Handler {
	s := &site{files: map[string]file{}}
	err := fs.WalkDir(build, ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case p != "." && strings.HasPrefix(d.Name(), "."):
			if d.IsDir() {
				return fs.SkipDir // build metadata such as .vite
			}
			return nil
		case d.IsDir():
			return nil
		}
		body, err := fs.ReadFile(build, p)
		if err != nil {
			return err
		}
		s.files["/"+p] = newFile(p, body)
		return nil
	})
	if err != nil {
		panic(err)
	}
	var ok bool
	if s.shell, ok = s.files["/_shell.html"]; ok {
		s.shell.name = "index.html"
		delete(s.files, "/_shell.html")
	} else {
		s.shell = newFile("index.html", []byte(unbuilt))
	}
	s.policy = policy(s.shell.body)
	return s
}

const unbuilt = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>inhouse</title></head>
<body><h1>The console isn't built into this binary</h1>
<p>Build it with <code>pnpm -C console build</code>, then rebuild inhoused. The <code>inhouse</code> CLI and the MCP server work without it.</p>
</body></html>
`

var inlineScript = regexp.MustCompile(`(?s)<script\b([^>]*)>(.*?)</script>`)

// policy confines the app to its own files and the API on this origin. The
// shell's few inline bootstrap scripts are allowed by hash, so no other
// inline script can run.
func policy(shell []byte) string {
	scripts := "'self'"
	for _, m := range inlineScript.FindAllSubmatch(shell, -1) {
		if bytes.Contains(m[1], []byte("src=")) {
			continue
		}
		sum := sha256.Sum256(parsed(m[2]))
		scripts += " 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	}
	return "default-src 'none'; script-src " + scripts + "; style-src 'self'; img-src 'self'; connect-src 'self'; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}

// parsed is script text as a browser's HTML parser hands it to the CSP
// check: newlines normalized and NUL replaced by U+FFFD. (The router's
// serialized state does contain NULs.)
func parsed(text []byte) []byte {
	text = bytes.ReplaceAll(text, []byte("\r\n"), []byte("\n"))
	text = bytes.ReplaceAll(text, []byte("\r"), []byte("\n"))
	return bytes.ReplaceAll(text, []byte("\x00"), []byte("\uFFFD"))
}

func (s *site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/favicon.ico" {
		p = "/icon.svg"
	}
	if f, ok := s.files[p]; ok {
		cache := "no-cache"
		if strings.HasPrefix(p, "/assets/") {
			cache = "public, max-age=31536000, immutable" // names carry a content hash
		}
		s.serve(w, r, f, cache)
		return
	}
	if path.Ext(p) != "" {
		http.NotFound(w, r) // a missing file, not a page of the app
		return
	}
	s.serve(w, r, s.shell, "no-cache")
}

func (s *site) serve(w http.ResponseWriter, r *http.Request, f file, cache string) {
	h := w.Header()
	h.Set("Content-Security-Policy", s.policy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", cache)
	h.Set("ETag", f.etag)
	http.ServeContent(w, r, f.name, time.Time{}, bytes.NewReader(f.body))
}
