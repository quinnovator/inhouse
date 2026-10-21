package edge

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
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

// udpEcho answers every datagram with name, ": " and the datagram, and
// records which addresses it heard from.
type udpEcho struct {
	pc   net.PacketConn
	mu   sync.Mutex
	from map[string]bool
}

func newUDPEcho(t *testing.T, name string) *udpEcho {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	u := &udpEcho{pc: pc, from: map[string]bool{}}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			u.mu.Lock()
			u.from[from.String()] = true
			u.mu.Unlock()
			_, _ = pc.WriteTo(append([]byte(name+": "), buf[:n]...), from)
		}
	}()
	return u
}

func (u *udpEcho) port() int { return u.pc.LocalAddr().(*net.UDPAddr).Port }

func (u *udpEcho) peers() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.from)
}

// askUDP sends msg up to attempts times, half a second apart, until an
// answer arrives: the first datagrams may race the tailnet path coming up.
func askUDP(ctx context.Context, pc net.PacketConn, to net.Addr, msg string, attempts int) (string, error) {
	buf := make([]byte, 2048)
	for range attempts {
		if _, err := pc.WriteTo([]byte(msg), to); err != nil {
			return "", err
		}
		_ = pc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _, err := pc.ReadFrom(buf)
		if err == nil {
			return string(buf[:n]), nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return "", errors.New("no answer")
}

// tcpEcho answers each line with name, ": " and the line.
func tcpEcho(t *testing.T, name string) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				lines := bufio.NewScanner(c)
				for lines.Scan() {
					_, _ = io.WriteString(c, name+": "+lines.Text()+"\n")
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestRawPortsFollowTheLiveRevision runs real nodes against Tailscale's
// in-process test control server. A caller node reaches the service node's
// raw UDP and TCP ports, which follow switches the way HTTPS does: new flows
// and connections go to the live revision, open ones stay where they
// started until that revision is retired.
func TestRawPortsFollowTheLiveRevision(t *testing.T) {
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
	m := NewManager(dir, tailnet.Credential{}, tailnet.Credential{})
	m.open = open
	m.listen = func(n *tailnet.Node, _ string) (net.Listener, error) { return n.Server.Listen("tcp", ":80") }
	defer func() { _ = m.Close() }()
	if _, err := m.Ensure(ctx, "web", store.Persistent, spec.ExposeTailnet); err != nil {
		t.Fatal(err)
	}
	caller, err := open(ctx, "caller", store.Persistent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = caller.Close() }()
	ip4, ip6 := m.Addrs("web")
	if ip4 == "" || ip6 == "" {
		t.Fatal("no tailnet addresses", ip4, ip6)
	}
	service := netip.MustParseAddr(ip4)

	udpA, udpB := newUDPEcho(t, "a"), newUDPEcho(t, "b")
	tcpA, tcpB := tcpEcho(t, "a"), tcpEcho(t, "b")
	// Ingress host ports 1 and 2 stand in for two revisions; nothing serves
	// HTTP in this test.
	revA := revision(t, 1, map[string]int{"udp/5000": udpA.port(), "tcp/5001": tcpA})
	revB := revision(t, 2, map[string]int{"udp/5000": udpB.port(), "tcp/5001": tcpB})
	if err = m.Switch(revA); err != nil {
		t.Fatal(err)
	}

	callerIP, _ := caller.Server.TailscaleIPs()
	flow := func() net.PacketConn {
		t.Helper()
		pc, err := caller.Server.ListenPacket("udp", netip.AddrPortFrom(callerIP, 0).String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pc.Close() })
		return pc
	}
	to := net.UDPAddrFromAddrPort(netip.AddrPortFrom(service, 5000))
	expect := func(pc net.PacketConn, msg, want string) {
		t.Helper()
		if got, err := askUDP(ctx, pc, to, msg, 40); err != nil || got != want {
			t.Fatalf("%q: got %q %v, want %q", msg, got, err, want)
		}
	}

	first, second := flow(), flow()
	expect(first, "x", "a: x")
	expect(second, "y", "a: y")
	if udpA.peers() != 2 {
		t.Fatalf("revision saw %d peers, want one per flow", udpA.peers())
	}

	dial := func() (net.Conn, *bufio.Reader) {
		t.Helper()
		c, err := caller.Server.Dial(ctx, "tcp", netip.AddrPortFrom(service, 5001).String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c, bufio.NewReader(c)
	}
	line := func(c net.Conn, r *bufio.Reader, msg string) (string, error) {
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(c, msg+"\n"); err != nil {
			return "", err
		}
		return r.ReadString('\n')
	}
	oldConn, oldReader := dial()
	if got, err := line(oldConn, oldReader, "p"); err != nil || got != "a: p\n" {
		t.Fatal(got, err)
	}

	// Cutover: open flows and connections stay, new ones go to B.
	if err = m.Switch(revB); err != nil {
		t.Fatal(err)
	}
	expect(first, "x2", "a: x2")
	expect(flow(), "z", "b: z")
	if got, err := line(oldConn, oldReader, "q"); err != nil || got != "a: q\n" {
		t.Fatal(got, err)
	}
	newConn, newReader := dial()
	if got, err := line(newConn, newReader, "r"); err != nil || got != "b: r\n" {
		t.Fatal(got, err)
	}

	// Retiring A ends what is still open to it; the flow starts over on B.
	m.Retire("web", revA.Port)
	expect(first, "x3", "b: x3")
	if got, err := line(oldConn, oldReader, "s"); err == nil {
		t.Fatalf("connection to a retired revision still answered %q", got)
	}

	// Cleared, new flows get nothing; a revision without the port closes it.
	m.Clear("web")
	if got, err := askUDP(ctx, flow(), to, "dropped", 4); err == nil {
		t.Fatalf("cleared edge answered %q", got)
	}
	if err = m.Switch(revision(t, 3, nil)); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	left := len(m.nodes["web"].raw)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d raw listeners outlived the ports", left)
	}
	if _, err = caller.Server.Dial(ctx, "tcp", netip.AddrPortFrom(service, 5001).String()); err == nil {
		t.Fatal("closed TCP port accepted a connection")
	}
}
