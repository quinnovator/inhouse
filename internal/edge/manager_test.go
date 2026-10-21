package edge

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/tailnet"
	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
)

// revision is web's revision whose ingress publishes on port, with ports
// mapping each raw listener it declares to its loopback port.
func revision(t *testing.T, port int, ports map[string]int) store.Revision {
	t.Helper()
	doc := "name: web\ncontainers:\n  web:\n    image: docker.io/a/b:1\n    port: 80\n"
	if len(ports) > 0 {
		doc += "    ports:\n"
		for key := range ports {
			protocol, number, _ := strings.Cut(key, "/")
			doc += "      - {port: " + number + ", protocol: " + protocol + "}\n"
		}
	}
	s, err := spec.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return store.Revision{Service: "web", Rev: port, Spec: s, Hash: s.Hash(), Port: port, Ports: ports}
}

// TestStoppedListenerComesBack runs real nodes against Tailscale's
// in-process test control server, serving plain HTTP since it issues no
// certificates. No real tailnet is touched.
func TestStoppedListenerComesBack(t *testing.T) {
	netns.SetEnabled(false)
	t.Cleanup(func() { netns.SetEnabled(true) })
	control := &testcontrol.Server{DERPMap: integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1"), DNSConfig: &tailcfg.DNSConfig{Proxied: true}, MagicDNSDomain: "test-inhouse.ts.net", Logf: t.Logf}
	control.HTTPTestServer = httptest.NewServer(control)
	defer control.HTTPTestServer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	dir := t.TempDir()
	open := func(ctx context.Context, name string, _ store.Kind) (*tailnet.Node, error) {
		return tailnet.Open(ctx, tailnet.NodeConfig{Dir: filepath.Join(dir, name), Hostname: name, Tag: tailnet.TagService, ControlURL: control.HTTPTestServer.URL, AuthKey: "test-auth-key"})
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "live revision")
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	port, _ := strconv.Atoi(u.Port())

	m := NewManager(dir, tailnet.Credential{}, tailnet.Credential{})
	m.open = open
	var mu sync.Mutex
	var listener net.Listener
	m.listen = func(n *tailnet.Node, _ string) (net.Listener, error) {
		l, err := n.Server.Listen("tcp", ":80")
		mu.Lock()
		listener = l
		mu.Unlock()
		return l, err
	}
	defer func() { _ = m.Close() }()
	if _, err := m.Ensure(ctx, "web", store.Persistent, spec.ExposeTailnet); err != nil {
		t.Fatal(err)
	}
	echo := newUDPEcho(t, "live revision")
	live := revision(t, port, map[string]int{"udp/5000": echo.port()})
	if err := m.Switch(live); err != nil {
		t.Fatal(err)
	}

	caller, err := open(ctx, "agent", store.Persistent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = caller.Close() }()
	client := &http.Client{Transport: &http.Transport{DialContext: caller.Server.Dial, DisableKeepAlives: true}, Timeout: 20 * time.Second}
	node := func() *tailnet.Node {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.nodes["web"].node.Load()
	}
	get := func() {
		t.Helper()
		ip, _ := node().Server.TailscaleIPs()
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+ip.String()+"/", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != "live revision" {
			t.Fatalf("HTTP %d %q", resp.StatusCode, body)
		}
		// Raw ports follow the node too.
		callerIP, _ := caller.Server.TailscaleIPs()
		pc, err := caller.Server.ListenPacket("udp", netip.AddrPortFrom(callerIP, 0).String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = pc.Close() }()
		to := net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, 5000))
		if got, err := askUDP(ctx, pc, to, "ping", 40); err != nil || got != "live revision: ping" {
			t.Fatal(got, err)
		}
	}
	waitStopped := func() {
		t.Helper()
		for m.Stopped("web") == nil {
			if ctx.Err() != nil {
				t.Fatal("stopped listener was not noticed")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	restart := func() {
		t.Helper()
		if dns, err := m.Ensure(ctx, "web", store.Persistent, spec.ExposeTailnet); err != nil || dns != "web.test-inhouse.ts.net" {
			t.Fatal(dns, err)
		}
		if err := m.Stopped("web"); err != nil {
			t.Fatal(err)
		}
		if err := m.Switch(live); err != nil {
			t.Fatal(err)
		}
	}
	get()

	// The listener alone stops: it comes back on the same node.
	first := node()
	mu.Lock()
	_ = listener.Close()
	mu.Unlock()
	waitStopped()
	restart()
	if node() != first {
		t.Fatal("replaced a node that was still running")
	}
	get()

	// The node stops too: a new node takes over from its state directory.
	_ = first.Close()
	waitStopped()
	restart()
	if node() == first {
		t.Fatal("listened on a stopped node")
	}
	get()

	// A replacement that rejoins under another name is refused: callers know
	// the service by its old name. A later restart that gets it back works.
	second := node()
	m.open = func(ctx context.Context, _ string, kind store.Kind) (*tailnet.Node, error) {
		return open(ctx, "web-renamed", kind)
	}
	_ = second.Close()
	waitStopped()
	if _, err := m.Ensure(ctx, "web", store.Persistent, spec.ExposeTailnet); err == nil || m.Stopped("web") == nil || node() != second {
		t.Fatal("accepted a renamed node", err)
	}
	m.open = open
	restart()
	get()

	// Closing the manager is not a stopped listener.
	_ = m.Close()
	time.Sleep(100 * time.Millisecond)
	if err := m.Stopped("web"); err != nil {
		t.Fatal(err)
	}
}
