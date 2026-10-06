package tailnet

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
)

// TestOfflineNodesIdentifyEachOther runs two nodes against Tailscale's
// in-process test control server: no real tailnet is touched.
func TestOfflineNodesIdentifyEachOther(t *testing.T) {
	netns.SetEnabled(false)
	t.Cleanup(func() { netns.SetEnabled(true) })
	control := &testcontrol.Server{DERPMap: integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1"), DNSConfig: &tailcfg.DNSConfig{Proxied: true}, MagicDNSDomain: "test-inhouse.ts.net", Logf: t.Logf}
	control.HTTPTestServer = httptest.NewServer(control)
	defer control.HTTPTestServer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	dir := t.TempDir()
	open := func(name string) *Node {
		n, err := Open(ctx, NodeConfig{Dir: filepath.Join(dir, name), Hostname: name, Tag: TagService, ControlURL: control.HTTPTestServer.URL, AuthKey: "test-auth-key"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = n.Close() })
		return n
	}
	server, caller := open("deploy"), open("agent")
	if server.DNS != "deploy.test-inhouse.ts.net" {
		t.Fatal(server.DNS)
	}
	listener, err := server.Server.Listen("tcp", ":8080")
	if err != nil {
		t.Fatal(err)
	}
	h := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, err := server.Client.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil || who.Node == nil {
			http.Error(w, "identity missing", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, who.Node.ComputedName)
	})}
	defer func() { _ = h.Close() }()
	go func() { _ = h.Serve(listener) }()
	ip, _ := server.Server.TailscaleIPs()
	client := &http.Client{Transport: &http.Transport{DialContext: caller.Server.Dial}, Timeout: 20 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+ip.String()+":8080/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "agent" {
		t.Fatalf("HTTP %d %q", resp.StatusCode, body)
	}
}
