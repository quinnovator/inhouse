package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/quinnovator/inhouse/internal/authz"
	"github.com/quinnovator/inhouse/internal/console"
	"github.com/quinnovator/inhouse/internal/engine"
	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
)

// WhoIs identifies the caller behind a remote address.
type WhoIs func(ctx context.Context, remoteAddr string) (authz.Principal, error)

type Server struct {
	Engine *engine.Engine
	WhoIs  WhoIs
}

// Handler serves the console, /v1 and /mcp. Every /v1 and /mcp request is
// identified first; callers without a valid grant get 403 and are recorded as
// denied. Browser requests from other origins may only read: the caller's
// tailnet identity is ambient, so another site could otherwise act with it.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	e := s.Engine
	mux.Handle("/mcp", MCPHandler(e))

	mux.HandleFunc("GET /v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, e)(e.Whoami(r.Context()))
	})
	mux.HandleFunc("GET /v1/services", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, e)(e.List(r.Context()))
	})
	mux.HandleFunc("GET /v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, e)(e.Get(r.Context(), r.PathValue("name")))
	})
	mux.HandleFunc("POST /v1/plan", func(w http.ResponseWriter, r *http.Request) {
		raw, err := readSpec(w, r)
		if err != nil {
			reply(w, r, e)(nil, err)
			return
		}
		reply(w, r, e)(e.Plan(r.Context(), raw))
	})
	mux.HandleFunc("POST /v1/deploy", func(w http.ResponseWriter, r *http.Request) {
		raw, err := readSpec(w, r)
		if err != nil {
			reply(w, r, e)(nil, err)
			return
		}
		reply(w, r, e)(e.Deploy(r.Context(), raw, r.Header.Get("Idempotency-Key")))
	})
	mux.HandleFunc("POST /v1/services/{name}/rollback", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			To      int  `json:"to_rev"`
			Restore bool `json:"restore_volumes"`
		}
		if err := decode(w, r, &q); err != nil {
			reply(w, r, e)(nil, err)
			return
		}
		reply(w, r, e)(e.Rollback(r.Context(), r.PathValue("name"), q.To, q.Restore, r.Header.Get("Idempotency-Key")))
	})
	mux.HandleFunc("DELETE /v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, e)(e.Delete(r.Context(), r.PathValue("name"), r.Header.Get("Idempotency-Key")))
	})
	mux.HandleFunc("GET /v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		wait, err := intParam(r, "wait")
		if err != nil {
			reply(w, r, e)(nil, err)
			return
		}
		reply(w, r, e)(e.Wait(r.Context(), r.PathValue("id"), time.Duration(wait)*time.Second))
	})
	mux.HandleFunc("GET /v1/services/{name}/logs", func(w http.ResponseWriter, r *http.Request) {
		rev, err1 := intParam(r, "rev")
		tail, err2 := intParam(r, "tail")
		if err := errors.Join(err1, err2); err != nil {
			reply(w, r, e)(nil, err)
			return
		}
		q := r.URL.Query()
		reply(w, r, e)(e.Logs(r.Context(), engine.LogQuery{Service: r.PathValue("name"), Rev: rev, Container: q.Get("container"), Tail: tail, Cursor: q.Get("cursor")}))
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		since, err1 := intParam(r, "since_id")
		limit, err2 := intParam(r, "limit")
		if err := errors.Join(err1, err2); err != nil {
			reply(w, r, e)(nil, err)
			return
		}
		reply(w, r, e)(e.Events(r.Context(), store.EventQuery{Service: r.URL.Query().Get("service"), Since: int64(since), Limit: limit}))
	})
	mux.HandleFunc("GET /v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, e)(e.ListSecrets(r.Context()))
	})
	mux.HandleFunc("PUT /v1/secrets/{name...}", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Value string `json:"value"`
		}
		if err := decode(w, r, &q); err != nil {
			reply(w, r, e)(nil, err)
			return
		}
		name := r.PathValue("name")
		err := e.SetSecret(r.Context(), name, q.Value)
		reply(w, r, e)(map[string]any{"name": name, "saved": true}, err)
	})
	mux.HandleFunc("DELETE /v1/secrets/{name...}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		err := e.DeleteSecret(r.Context(), name)
		reply(w, r, e)(map[string]any{"name": name, "deleted": true}, err)
	})

	sameOrigin := http.NewCrossOriginProtection()
	identified := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, err := s.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil || !p.Authenticated() {
			if p.ID != "" {
				e.Audit(authz.With(r.Context(), p), "", "no grant for "+r.Method+" "+r.URL.Path)
			}
			pr := Problem{Code: "unauthenticated", Message: "cannot identify the tailnet caller", status: http.StatusUnauthorized}
			if errors.Is(err, authz.ErrForbidden) || err == nil {
				pr = Problem{Code: "permission_denied", Message: "caller has no inhouse capability grant", Hint: "Add a grant for the instance's capability to the tailnet policy file.", status: http.StatusForbidden}
			}
			write(w, pr.status, map[string]Problem{"error": pr})
			return
		}
		ctx := authz.With(r.Context(), p)
		if sameOrigin.Check(r) != nil {
			e.Audit(ctx, "", "cross-origin browser request refused: "+r.Method+" "+r.URL.Path)
			pr := Problem{Code: "cross_origin", Message: "browser requests from other sites cannot change anything", Hint: "Use the console on this host, the CLI or MCP.", status: http.StatusForbidden}
			write(w, pr.status, map[string]Problem{"error": pr})
			return
		}
		mux.ServeHTTP(w, r.WithContext(ctx))
	})

	// Everything that isn't the API or MCP is the console's: its files, or
	// its shell for any page so the app can route it.
	site := console.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/v1/")
		if !api && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			site.ServeHTTP(w, r)
			return
		}
		identified.ServeHTTP(w, r)
	})
}

// reply writes v, or the problem err describes. Refused calls are audited.
func reply(w http.ResponseWriter, r *http.Request, e *engine.Engine) func(v any, err error) {
	return func(v any, err error) {
		if err == nil {
			write(w, http.StatusOK, v)
			return
		}
		if errors.Is(err, authz.ErrForbidden) {
			service := r.PathValue("name")
			if !spec.ValidName(service) {
				service = ""
			}
			e.Audit(r.Context(), service, r.Method+" "+r.URL.Path+": "+err.Error())
		}
		pr := problem(err)
		write(w, pr.status, map[string]Problem{"error": pr})
	}
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readSpec(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, spec.MaxBytes))
	if err != nil {
		return nil, &engine.Invalid{Err: errors.New("spec exceeds 64 KiB")}
	}
	return raw, nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return &engine.Invalid{Err: errors.New("request body must be a JSON object with known fields")}
	}
	if d.Decode(new(any)) != io.EOF {
		return &engine.Invalid{Err: errors.New("request body must be one JSON object")}
	}
	return nil
}

func intParam(r *http.Request, name string) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, &engine.Invalid{Err: errors.New(name + " must be a non-negative integer")}
	}
	return n, nil
}
