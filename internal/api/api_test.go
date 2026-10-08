package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/quinnovator/inhouse/internal/authz"
	"github.com/quinnovator/inhouse/internal/engine"
	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/vault"
)

// newServer serves the API with callers named by an X-Test-Caller header:
// "admin", "agent", "nobody" (a tailnet device with no grant), or "unknown".
func newServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	v, err := vault.Open(db, filepath.Join(dir, "age.key"))
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(engine.DefaultConfig(), db, v, nil, nil, nil)
	callers := map[string]authz.Principal{
		"admin":  {ID: "owner@example.com", Grants: []authz.Grant{{Role: authz.Admin}}},
		"agent":  {ID: "node:agent", Grants: []authz.Grant{{Role: authz.Deployer, Services: []string{"preview-*"}, Expose: []string{"tailnet"}, MaxTTL: "24h"}}},
		"nobody": {ID: "node:nobody", Grants: []authz.Grant{}},
	}
	s := &Server{Engine: e, WhoIs: func(ctx context.Context, _ string) (authz.Principal, error) {
		name, _ := ctx.Value(callerKey{}).(string)
		p, ok := callers[name]
		if !ok {
			return authz.Principal{}, errors.New("unknown peer")
		}
		if !p.Authenticated() {
			return p, authz.ErrForbidden
		}
		return p, nil
	}}
	h := s.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, r.Header.Get("X-Test-Caller"))))
	}))
	t.Cleanup(srv.Close)
	return srv, db
}

type callerKey struct{}

func call(t *testing.T, srv *httptest.Server, caller, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-Test-Caller", caller)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

const preview = "name: preview-a\nttl: 1h\ncontainers:\n  web: {image: docker.io/traefik/whoami:1, port: 80}\n"

func TestCallersNeedAGrant(t *testing.T) {
	srv, db := newServer(t)
	if code, _ := call(t, srv, "unknown", "GET", "/v1/services", ""); code != http.StatusUnauthorized {
		t.Fatal(code)
	}
	if code, body := call(t, srv, "nobody", "GET", "/v1/services", ""); code != http.StatusForbidden || !strings.Contains(body, "permission_denied") {
		t.Fatal(code, body)
	}
	events, _ := db.Events(context.Background(), store.EventQuery{})
	if len(events) != 1 || events[0].Actor != "node:nobody" || events[0].Kind != "denied" {
		t.Fatalf("denied call not audited: %+v", events)
	}
}

func TestDeployThroughHTTP(t *testing.T) {
	srv, _ := newServer(t)
	code, body := call(t, srv, "agent", "POST", "/v1/deploy", preview)
	if code != http.StatusOK || !strings.Contains(body, `"state":"running"`) {
		t.Fatal(code, body)
	}
	var op store.Operation
	_ = json.Unmarshal([]byte(body), &op)
	if code, body = call(t, srv, "agent", "GET", "/v1/operations/"+op.ID, ""); code != http.StatusOK {
		t.Fatal(code, body)
	}
	code, body = call(t, srv, "agent", "POST", "/v1/deploy", strings.Replace(preview, "1h", "2h", 1))
	if code != http.StatusConflict || !strings.Contains(body, op.ID) {
		t.Fatal("busy service did not name the running operation", code, body)
	}
	if code, _ = call(t, srv, "agent", "POST", "/v1/deploy", strings.Replace(preview, "preview-a", "blog", 1)); code != http.StatusForbidden {
		t.Fatal(code)
	}
	if code, _ = call(t, srv, "agent", "POST", "/v1/deploy", "name: x\n"); code != http.StatusBadRequest {
		t.Fatal(code)
	}
	if code, _ = call(t, srv, "agent", "GET", "/v1/secrets", ""); code != http.StatusForbidden {
		t.Fatal("agent listed secrets")
	}
	if code, body = call(t, srv, "admin", "PUT", "/v1/secrets/registry-auth/ghcr.io", `{"value":"{\"username\":\"u\",\"password\":\"p\"}"}`); code != http.StatusOK {
		t.Fatal(code, body)
	}
	if code, body = call(t, srv, "admin", "GET", "/v1/secrets", ""); code != http.StatusOK || strings.Contains(body, `"p"`) || !strings.Contains(body, "registry-auth/ghcr.io") {
		t.Fatal(code, body)
	}
}

func TestMCPBindsEachRequestToItsCaller(t *testing.T) {
	srv, _ := newServer(t)
	for _, caller := range []string{"admin", "agent", "admin"} {
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		transport := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: callerTransport(caller)}}
		session, err := client.Connect(context.Background(), transport, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(result.StructuredContent)
		var got struct{ Result authz.Principal }
		if err = json.Unmarshal(raw, &got); err != nil || (caller == "admin") != got.Result.IsAdmin() {
			t.Fatalf("%s saw %s", caller, raw)
		}
		tools, err := session.ListTools(context.Background(), nil)
		if err != nil || len(tools.Tools) != 18 {
			t.Fatal(len(tools.Tools), err)
		}
		for _, tool := range tools.Tools {
			if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || tool.Annotations.OpenWorldHint == nil {
				t.Fatalf("%s lacks annotations", tool.Name)
			}
		}
		if caller == "agent" {
			result, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "deploy", Arguments: map[string]any{"spec": strings.Replace(preview, "preview-a", "blog", 1)}})
			if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "permission_denied") {
				t.Fatal("agent deployed outside its grant", err)
			}
		}
		_ = session.Close()
	}
}

type callerTransport string

func (c callerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Test-Caller", string(c))
	return http.DefaultTransport.RoundTrip(r)
}

func TestConsoleIsServedWithoutData(t *testing.T) {
	srv, db := newServer(t)
	for _, path := range []string{"/", "/services/blog", "/events"} {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") || resp.Header.Get("ETag") == "" {
			t.Fatal(path, resp.StatusCode, resp.Header)
		}
	}
	if code, _ := call(t, srv, "unknown", "GET", "/assets/missing.js", ""); code != http.StatusNotFound {
		t.Fatal("missing asset served", code)
	}
	if code, _ := call(t, srv, "unknown", "GET", "/v1/services", ""); code != http.StatusUnauthorized {
		t.Fatal("API reachable without identity", code)
	}
	if events, _ := db.Events(context.Background(), store.EventQuery{}); len(events) != 0 {
		t.Fatalf("loading the console was audited: %+v", events)
	}
}

func TestCrossOriginBrowserRequestsCannotWrite(t *testing.T) {
	srv, db := newServer(t)
	send := func(method, path, site, body string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("X-Test-Caller", "agent")
		req.Header.Set("Sec-Fetch-Site", site)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	if code, body := send("POST", "/v1/deploy", "cross-site", preview); code != http.StatusForbidden || !strings.Contains(body, "cross_origin") {
		t.Fatal(code, body)
	}
	if code, _ := send("GET", "/v1/services", "cross-site", ""); code != http.StatusOK {
		t.Fatal("cross-origin read refused", code)
	}
	if code, body := send("POST", "/v1/deploy", "same-origin", preview); code != http.StatusOK {
		t.Fatal("console deploy refused", code, body)
	}
	events, _ := db.Events(context.Background(), store.EventQuery{Limit: 100})
	if len(events) == 0 || events[0].Kind != "denied" || events[0].Actor != "node:agent" || !strings.Contains(events[0].Message, "cross-origin") {
		t.Fatalf("refusal not audited: %+v", events)
	}
}

func TestServiceActionsThroughHTTP(t *testing.T) {
	srv, _ := newServer(t)
	if code, body := call(t, srv, "agent", "POST", "/v1/deploy", preview); code != http.StatusOK {
		t.Fatal(code, body)
	}
	for _, action := range []string{"restart", "redeploy", "stop", "start", "extend"} {
		// No reconciler runs here, so preview-a never gets a live revision.
		if code, body := call(t, srv, "agent", "POST", "/v1/services/preview-a/"+action, ""); code != http.StatusConflict || !strings.Contains(body, "live revision") {
			t.Fatal(action, code, body)
		}
		if code, _ := call(t, srv, "agent", "POST", "/v1/services/blog/"+action, ""); code != http.StatusForbidden {
			t.Fatal(action, "outside the grant:", code)
		}
		if code, _ := call(t, srv, "admin", "POST", "/v1/services/missing/"+action, ""); code != http.StatusNotFound {
			t.Fatal(action, "of a missing service:", code)
		}
	}
}
