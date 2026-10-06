package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func target(t *testing.T, raw string) *Target {
	t.Helper()
	u, err := NewTarget(raw, "hello.example.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func owner(context.Context, string) (Identity, error) {
	return Identity{Login: "owner@example.com", Name: "Owner"}, nil
}

func TestIdentityAndForwardedHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(r.Header) }))
	defer upstream.Close()
	e := New(owner)
	e.Switch(target(t, upstream.URL))
	for _, anonymous := range []bool{false, true} {
		t.Run(fmt.Sprint("anonymous=", anonymous), func(t *testing.T) {
			r := httptest.NewRequest("GET", "https://forged.example/", nil)
			r.RemoteAddr = "100.64.0.1:12345"
			r.Header["tailscale-user-custom"] = []string{"forged"}
			r.Header.Add("Tailscale-User-Login", "forged@example.com")
			r.Header.Add("Tailscale-User-Login", "another@example.com")
			r.Header.Set("Tailscale-User-Name", "Forged")
			r.Header.Set("Connection", "Tailscale-User-Login, Tailscale-User-Name")
			r.Header.Set("X-Forwarded-For", "1.2.3.4")
			r.Header.Set("X-Forwarded-Host", "forged.example")
			r.Header.Set("X-Forwarded-Proto", "http")
			if anonymous {
				r = r.WithContext(Anonymous(r.Context()))
			}
			w := httptest.NewRecorder()
			e.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
			var headers http.Header
			if err := json.Unmarshal(w.Body.Bytes(), &headers); err != nil {
				t.Fatal(err)
			}
			if headers.Get("X-Forwarded-For") != "100.64.0.1" || headers.Get("X-Forwarded-Host") != "hello.example.ts.net" || headers.Get("X-Forwarded-Proto") != "https" {
				t.Fatal(headers)
			}
			if anonymous {
				for key := range headers {
					if strings.HasPrefix(strings.ToLower(key), "tailscale-user-") {
						t.Fatal(headers)
					}
				}
			} else if headers.Get("Tailscale-User-Login") != "owner@example.com" || len(headers.Values("Tailscale-User-Login")) != 1 || headers.Get("Tailscale-User-Name") != "Owner" || headers.Get("Tailscale-User-Custom") != "" {
				t.Fatal(headers)
			}
		})
	}
}

func TestIdentityFailureAndNoRevision(t *testing.T) {
	e := New(func(context.Context, string) (Identity, error) { return Identity{}, errors.New("no identity") })
	r := httptest.NewRequest("GET", "https://edge.example/", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	e.Switch(target(t, "http://127.0.0.1:1"))
	w = httptest.NewRecorder()
	e.ServeHTTP(w, r)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "cannot identify tailnet caller") {
		t.Fatal(w.Code, w.Body)
	}
}

func TestSwapPreservesInflightRequest(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { close(started); <-release; _, _ = io.WriteString(w, "a") }))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "b") }))
	defer b.Close()
	e := New(owner)
	e.Switch(target(t, a.URL))
	done := make(chan *httptest.ResponseRecorder)
	go func() { w := httptest.NewRecorder(); e.ServeHTTP(w, httptest.NewRequest("GET", "/", nil)); done <- w }()
	<-started
	e.Switch(target(t, b.URL))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Body.String() != "b" || w.Code != 200 {
		t.Fatal(w)
	}
	close(release)
	w = <-done
	if w.Body.String() != "a" || w.Code != 200 {
		t.Fatal(w)
	}
}

func TestConcurrentSwaps(t *testing.T) {
	servers := make([]*httptest.Server, 2)
	targets := make([]*Target, 2)
	for i := range servers {
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
		defer servers[i].Close()
		targets[i] = target(t, servers[i].URL)
	}
	e := New(owner)
	e.Switch(targets[0])
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				w := httptest.NewRecorder()
				e.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
				if w.Code != 200 || w.Body.String() != "ok" {
					t.Errorf("response: %d %s", w.Code, w.Body)
				}
			}
		})
	}
	for i := range 1000 {
		e.Switch(targets[i%2])
	}
	wg.Wait()
}

func TestStreamingAndWebSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			kind, data, err := conn.Read(ctx)
			if err == nil {
				_ = conn.Write(ctx, kind, data)
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "last\n")
	}))
	defer upstream.Close()
	e := New(owner)
	e.Switch(target(t, upstream.URL))
	front := httptest.NewServer(e)
	defer front.Close()
	req, _ := http.NewRequestWithContext(ctx, "GET", front.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	first := make([]byte, 6)
	_, err = io.ReadFull(resp.Body, first)
	close(release)
	if err != nil || string(first) != "first\n" {
		t.Fatal(err, string(first))
	}
	last, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(last) != "last\n" {
		t.Fatal(err, string(last))
	}
	conn, _, err := websocket.Dial(ctx, front.URL+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	if err := conn.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil || string(data) != "hello" {
		t.Fatal(err, string(data))
	}
}

func TestRejectNonLoopbackOrigins(t *testing.T) {
	for _, raw := range []string{"http://example.com:80", "http://192.0.2.1:80", "https://127.0.0.1:80", "http://127.0.0.1:80/path", "http://user@127.0.0.1:80", "http://127.0.0.1"} {
		if _, err := NewTarget(raw, "edge.example"); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
