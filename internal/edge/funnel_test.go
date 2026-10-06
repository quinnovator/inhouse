package edge

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"tailscale.com/ipn"
)

func TestFunnelTransportNeverInjectsTailnetIdentity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Tailscale-User-Login"))
	}))
	defer upstream.Close()
	target, e := NewTarget(upstream.URL, "service.example.ts.net")
	if e != nil {
		t.Fatal(e)
	}
	proxy := New(func(context.Context, string) (Identity, error) {
		return Identity{Login: "owner", Name: "Owner"}, nil
	})
	proxy.Switch(target)
	for _, funnel := range []bool{false, true} {
		a, b := net.Pipe()
		raw := a
		if funnel {
			raw = &ipn.FunnelConn{Conn: a}
		}
		conn := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS12})
		ctx := connContext(context.Background(), conn)
		req := httptest.NewRequest("GET", "https://service.example.ts.net/", nil).WithContext(ctx)
		req.Header.Set("Tailscale-User-Login", "forged-owner")
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)
		want := "owner"
		if funnel {
			want = ""
		}
		if w.Code != 200 || w.Body.String() != want {
			t.Fatalf("funnel=%v: %d %q", funnel, w.Code, w.Body.String())
		}
		_ = a.Close()
		_ = b.Close()
	}
}
