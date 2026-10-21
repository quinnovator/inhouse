package edge

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/quinnovator/inhouse/internal/spec"
)

// tlsFront serves h as a node does: over TLS offering h2 and http/1.1.
func tlsFront(t *testing.T, h http.Handler) string {
	t.Helper()
	signer := httptest.NewTLSServer(nil)
	cert := signer.TLS.Certificates[0]
	signer.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(h)
	go func() {
		_ = srv.Serve(tls.NewListener(ln, tlsConfig(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil })))
	}()
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + ln.Addr().String()
}

// h2cUpstream serves h over HTTP/2 without TLS only, as a gRPC server does.
func h2cUpstream(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var only http.Protocols
	only.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: h, Protocols: &only, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

func insecure(h2 bool) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: h2}}
}

// A gRPC call is a bidirectional HTTP/2 stream ending in trailers. The
// caller speaks HTTP/2 to the edge, the edge h2c to the pod, and each
// message is answered before the next is sent.
func TestHTTP2FrontAndH2CUpstreamCarryBidirectionalStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	upstream := h2cUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			http.Error(w, "HTTP/2 only", http.StatusHTTPVersionNotSupported)
			return
		}
		w.Header().Set("Trailer", "Grpc-Status")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_ = rc.Flush()
		lines := bufio.NewScanner(r.Body)
		for lines.Scan() {
			_, _ = io.WriteString(w, "echo: "+lines.Text()+"\n")
			_ = rc.Flush()
		}
		w.Header().Set("Grpc-Status", "0")
	}))
	to, err := NewTarget(upstream, "grpc.example.ts.net", spec.ProtocolH2C)
	if err != nil {
		t.Fatal(err)
	}
	e := New(owner)
	e.Switch(to)
	front := tlsFront(t, e)

	body, send := io.Pipe()
	req, _ := http.NewRequestWithContext(ctx, "POST", front+"/pkg.Echo/Chat", body)
	resp, err := insecure(true).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 2 || resp.StatusCode != 200 {
		t.Fatalf("%s %d", resp.Proto, resp.StatusCode)
	}
	replies := bufio.NewReader(resp.Body)
	for _, msg := range []string{"one", "two"} {
		if _, err = io.WriteString(send, msg+"\n"); err != nil {
			t.Fatal(err)
		}
		line, err := replies.ReadString('\n')
		if err != nil || line != "echo: "+msg+"\n" {
			t.Fatalf("%q %v", line, err)
		}
	}
	_ = send.Close()
	if rest, err := io.ReadAll(replies); err != nil || len(rest) != 0 {
		t.Fatal(string(rest), err)
	}
	if got := resp.Trailer.Get("Grpc-Status"); got != "0" {
		t.Fatalf("trailer %q", got)
	}
}

// Turning on HTTP/2 must not break WebSockets: Go doesn't advertise them
// over HTTP/2, so clients upgrade an HTTP/1.1 connection.
func TestWebSocketOverTLSBesideHTTP2(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if kind, data, err := conn.Read(ctx); err == nil {
			_ = conn.Write(ctx, kind, data)
		}
	}))
	defer upstream.Close()
	e := New(owner)
	e.Switch(target(t, upstream.URL))
	front := tlsFront(t, e)

	resp, err := insecure(true).Get(front + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("edge negotiated %s, want HTTP/2", resp.Proto)
	}
	conn, _, err := websocket.Dial(ctx, "wss"+front[len("https"):]+"/ws", &websocket.DialOptions{HTTPClient: insecure(false)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	if err = conn.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := conn.Read(ctx); err != nil || string(data) != "hello" {
		t.Fatal(err, string(data))
	}
}

func TestRoutesPickTheLongestMatchingPrefix(t *testing.T) {
	named := func(name string) *Target {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, name+" "+r.URL.Path+" "+r.Header.Get("Tailscale-User-Login"))
		}))
		t.Cleanup(s.Close)
		return target(t, s.URL)
	}
	ingress := named("ingress")
	ingress.Route("/rtc", named("rtc"))
	ingress.Route("/rtc/admin", named("admin"))
	e := New(owner)
	e.Switch(ingress)
	for path, want := range map[string]string{
		"/":              "ingress / owner@example.com",
		"/rtcx":          "ingress /rtcx owner@example.com",
		"/rtc":           "rtc /rtc owner@example.com",
		"/rtc/v1?a=b":    "rtc /rtc/v1 owner@example.com",
		"/rtc/admin/x":   "admin /rtc/admin/x owner@example.com",
		"/rtc/adminx":    "rtc /rtc/adminx owner@example.com",
		"/other/rtc/abc": "ingress /other/rtc/abc owner@example.com",
	} {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Body.String() != want {
			t.Errorf("%s: %q, want %q", path, w.Body.String(), want)
		}
	}
}

func TestUnknownUpstreamProtocol(t *testing.T) {
	if _, err := NewTarget("http://127.0.0.1:80", "edge.example", "h3"); err == nil {
		t.Fatal("accepted an unknown protocol")
	}
}
