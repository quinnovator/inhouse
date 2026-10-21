package edge

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/tailnet"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

// Manager runs one tailnet node and HTTPS listener per service, plus the
// raw TCP and UDP ports its live revision publishes.
type Manager struct {
	StateDir  string // one subdirectory per node
	Enroll    tailnet.Credential
	Lifecycle tailnet.Credential

	// open starts a service's node and listen opens its HTTPS listener;
	// tests replace them.
	open   func(ctx context.Context, service string, kind store.Kind) (*tailnet.Node, error)
	listen func(n *tailnet.Node, expose string) (net.Listener, error)

	mu    sync.Mutex
	nodes map[string]*managed
}

type managed struct {
	node   atomic.Pointer[tailnet.Node] // replaced if it stops
	edge   *Edge
	server *http.Server
	expose string

	// Guarded by Manager.mu.
	port    int                    // the live revision's ingress host port; 0 when cleared
	raw     map[string]rawListener // by spec.Listener key
	stopped error                  // why the listener stopped serving; nil while it serves
}

func NewManager(stateDir string, enroll, lifecycle tailnet.Credential) *Manager {
	m := &Manager{StateDir: stateDir, Enroll: enroll, Lifecycle: lifecycle, nodes: map[string]*managed{}}
	m.open = func(ctx context.Context, service string, kind store.Kind) (*tailnet.Node, error) {
		return tailnet.Open(ctx, tailnet.NodeConfig{
			Dir: filepath.Join(m.StateDir, service), Hostname: service, Tag: tagFor(kind),
			Ephemeral: kind == store.Ephemeral, Enroll: m.Enroll, Certificate: true,
		})
	}
	m.listen = func(n *tailnet.Node, expose string) (net.Listener, error) {
		conf := tlsConfig(n.Client.GetCertificate)
		if expose == spec.ExposeFunnel {
			return n.Server.ListenFunnel("tcp", ":443", tsnet.FunnelTLSConfig(conf))
		}
		ln, err := n.Server.Listen("tcp", ":443")
		if err != nil {
			return nil, err
		}
		return tls.NewListener(ln, conf), nil
	}
	return m
}

// tlsConfig offers HTTP/2 beside HTTP/1.1. tsnet's ListenTLS offers neither,
// so every client would fall back to HTTP/1.1.
func tlsConfig(getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{GetCertificate: getCert, NextProtos: []string{"h2", "http/1.1"}}
}

// newServer serves an edge over HTTP/1.1 and, where TLS negotiates it,
// HTTP/2. Go doesn't offer WebSockets over HTTP/2, so browsers open them
// over HTTP/1.1, which the proxy upgrades.
func newServer(h http.Handler) *http.Server {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, ConnContext: connContext, Protocols: &protocols}
}

func tagFor(kind store.Kind) string {
	if kind == store.Ephemeral {
		return tailnet.TagEphemeral
	}
	return tailnet.TagService
}

// Ensure starts the service's node and listener if they aren't running. A
// listener that stopped is started again, on a new node if its node stopped
// too.
func (m *Manager) Ensure(ctx context.Context, service string, kind store.Kind, expose string) (string, error) {
	if expose != spec.ExposeTailnet && expose != spec.ExposeFunnel {
		return "", errors.New("invalid exposure mode")
	}
	if kind == store.Ephemeral && expose == spec.ExposeFunnel {
		return "", errors.New("ephemeral services cannot use Funnel")
	}
	m.mu.Lock()
	existing := m.nodes[service]
	var stopped error
	if existing != nil {
		stopped = existing.stopped
	}
	m.mu.Unlock()
	if existing != nil {
		if existing.expose != expose {
			return "", errors.New("a node's exposure mode cannot change; delete the service first")
		}
		if stopped != nil {
			return m.restart(ctx, service, kind, existing)
		}
		return existing.node.Load().DNS, nil
	}
	n, err := m.open(ctx, service, kind)
	if err != nil {
		return "", err
	}
	v := &managed{expose: expose, raw: map[string]rawListener{}}
	v.node.Store(n)
	v.edge = New(func(ctx context.Context, remote string) (Identity, error) {
		who, err := v.node.Load().Client.WhoIs(ctx, remote)
		if err != nil {
			return Identity{}, err
		}
		if who.UserProfile == nil || (who.Node != nil && len(who.Node.Tags) > 0) {
			return Identity{}, nil
		}
		return Identity{Login: who.UserProfile.LoginName, Name: who.UserProfile.DisplayName}, nil
	})
	listener, err := m.listen(n, expose)
	if err != nil {
		_ = n.Close()
		return "", fmt.Errorf("listen on %s: %w", n.DNS, err)
	}
	v.server = newServer(v.edge)
	m.mu.Lock()
	m.nodes[service] = v
	m.mu.Unlock()
	go m.serve(service, v, listener)
	return n.DNS, nil
}

// serve runs until the listener stops. Unless the service was deleted or the
// manager closed, it records why, so Stopped reports it and the next Ensure
// starts the listener again.
func (m *Manager) serve(service string, v *managed, listener net.Listener) {
	err := v.server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nodes[service] == v {
		log.Printf("edge %s: HTTPS listener stopped: %v", service, err)
		v.stopped = err
	}
}

// restart starts a stopped listener again. A stopped tsnet node still lets
// a listener open but never accepts on it, so a node that no longer runs is
// first replaced from its state directory. The proxy keeps its upstream, so
// the live revision is served again as soon as the listener is back.
func (m *Manager) restart(ctx context.Context, service string, kind store.Kind, v *managed) (string, error) {
	down := func(err error) (string, error) {
		m.mu.Lock()
		v.stopped = err
		m.mu.Unlock()
		return "", err
	}
	n := v.node.Load()
	if !running(ctx, n) {
		_ = n.Close()
		replacement, err := m.open(ctx, service, kind)
		if err != nil {
			return down(err)
		}
		if replacement.DNS != n.DNS {
			// The service's address is its node's name, and callers know
			// the old one. Tailscale suffixes a name while a stale device
			// holds it, so a later restart can get it back.
			_ = replacement.Close()
			return down(fmt.Errorf("node rejoined as %s instead of %s; refusing implicit rename", replacement.DNS, n.DNS))
		}
		n = replacement
		v.node.Store(n)
		// Raw listeners belonged to the stopped node; open them again on the
		// new one, pointed where they were.
		m.mu.Lock()
		err = m.reopenRaw(n, v)
		m.mu.Unlock()
		if err != nil {
			_ = n.Close()
			return down(err)
		}
	}
	listener, err := m.listen(n, v.expose)
	if err != nil {
		return down(fmt.Errorf("listen on %s: %w", n.DNS, err))
	}
	m.mu.Lock()
	v.stopped = nil
	m.mu.Unlock()
	go m.serve(service, v, listener)
	return n.DNS, nil
}

// running reports whether the node still answers and is connected.
func running(ctx context.Context, n *tailnet.Node) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := n.Client.StatusWithoutPeers(ctx)
	return err == nil && st.BackendState == ipn.Running.String()
}

// Stopped reports why the service's HTTPS listener stopped serving, or nil
// while it serves or if it was never started.
func (m *Manager) Stopped(service string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := m.nodes[service]; v != nil {
		return v.stopped
	}
	return nil
}

// connContext marks Funnel connections by their transport, never by a header.
func connContext(ctx context.Context, conn net.Conn) context.Context {
	if tc, ok := conn.(*tls.Conn); ok {
		if _, ok := tc.NetConn().(*ipn.FunnelConn); ok {
			return Anonymous(ctx)
		}
	}
	return ctx
}

// Switch points the service's HTTPS address, its routes and its raw ports at
// revision r. Requests, connections and flows already under way stay on the
// revision they started on.
func (m *Manager) Switch(r store.Revision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.nodes[r.Service]
	if v == nil {
		return errors.New("edge node is not running")
	}
	if v.port == r.Port {
		return nil
	}
	n := v.node.Load()
	_, ingress := r.Spec.Ingress()
	target, err := NewTarget(origin(r.Port), n.DNS, ingress.Protocol)
	if err != nil {
		return err
	}
	routed := map[int]*Target{}
	for _, route := range r.Spec.Routes {
		to := routed[route.Port]
		if to == nil {
			port := r.Ports[spec.RouteKey(route.Port)]
			if port == 0 {
				return fmt.Errorf("r%d has no host port for %s", r.Rev, spec.RouteKey(route.Port))
			}
			if to, err = NewTarget(origin(port), n.DNS, route.Protocol); err != nil {
				return err
			}
			routed[route.Port] = to
		}
		target.Route(route.Path, to)
	}
	want := map[string]*upstream{}
	for _, l := range r.Spec.Listeners() {
		port := r.Ports[l.Key()]
		if port == 0 {
			return fmt.Errorf("r%d has no host port for %s", r.Rev, l.Key())
		}
		want[l.Key()] = &upstream{rev: r.Port, port: port}
		if v.raw[l.Key()] == nil {
			raw, err := listenRaw(n, l)
			if err != nil {
				return fmt.Errorf("listen on %s %s: %w", n.DNS, l.Key(), err)
			}
			v.raw[l.Key()] = raw
		}
	}
	for key, raw := range v.raw {
		if want[key] == nil {
			raw.close()
			delete(v.raw, key)
		} else {
			raw.point(want[key])
		}
	}
	v.edge.Switch(target)
	v.port = r.Port
	return nil
}

func origin(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

// reopenRaw opens v's raw listeners on n, replacing those of a stopped node,
// all or none: after a failure v keeps every key, so the next restart tries
// them all again. Guarded by m.mu.
func (m *Manager) reopenRaw(n *tailnet.Node, v *managed) error {
	fresh := map[string]rawListener{}
	for key, old := range v.raw {
		old.close()
		protocol, port, _ := strings.Cut(key, "/")
		number, _ := strconv.Atoi(port)
		raw, err := listenRaw(n, spec.Listener{Protocol: protocol, Port: number})
		if err != nil {
			for _, opened := range fresh {
				opened.close()
			}
			return fmt.Errorf("listen on %s %s: %w", n.DNS, key, err)
		}
		raw.point(old.upstream())
		fresh[key] = raw
	}
	v.raw = fresh
	return nil
}

// Retire closes the raw connections and flows still open to the revision
// whose ingress host port is port, before its ports are released for reuse.
// HTTP requests need nothing: they end with the revision's pod.
func (m *Manager) Retire(service string, port int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := m.nodes[service]; v != nil && port != 0 {
		for _, raw := range v.raw {
			raw.retire(port)
		}
	}
}

// Addrs returns the service node's tailnet IPv4 and IPv6 addresses, or
// empty strings while it has no node.
func (m *Manager) Addrs(service string) (ipv4, ipv6 string) {
	m.mu.Lock()
	v := m.nodes[service]
	m.mu.Unlock()
	if v == nil {
		return "", ""
	}
	ip4, ip6 := v.node.Load().Server.TailscaleIPs()
	if ip4.IsValid() {
		ipv4 = ip4.String()
	}
	if ip6.IsValid() {
		ipv6 = ip6.String()
	}
	return ipv4, ipv6
}

func (m *Manager) Port(service string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := m.nodes[service]; v != nil {
		return v.port
	}
	return 0
}

func (m *Manager) Clear(service string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := m.nodes[service]; v != nil {
		v.edge.Switch(nil)
		v.port = 0
		for _, raw := range v.raw {
			raw.point(nil)
		}
	}
}

// Delete removes the service's device from the tailnet and its local state.
func (m *Manager) Delete(ctx context.Context, service string, kind store.Kind) error {
	if !spec.ValidName(service) {
		return errors.New("invalid node name")
	}
	dir := filepath.Join(m.StateDir, service)
	stop := func() {
		m.mu.Lock()
		v := m.nodes[service]
		delete(m.nodes, service)
		m.mu.Unlock()
		if v != nil {
			n := v.node.Load()
			_ = v.server.Close()
			m.mu.Lock()
			for _, raw := range v.raw {
				raw.close()
			}
			m.mu.Unlock()
			_ = n.Client.Logout(ctx)
			_ = n.Close()
		}
	}
	id, err := tailnet.ReadIdentity(dir)
	if errors.Is(err, os.ErrNotExist) {
		if _, err = os.Stat(filepath.Join(dir, "tailscaled.state")); errors.Is(err, os.ErrNotExist) {
			stop()
			return os.RemoveAll(dir) // the node never joined
		}
		return errors.New("node joined but its identity was never saved; delete the device in the admin console, then remove " + dir)
	}
	if err != nil {
		return err
	}
	if id.Tag != tagFor(kind) || !strings.HasPrefix(id.DNS, service+".") {
		return errors.New("saved node identity does not match the service; refusing device deletion")
	}
	if kind == store.Ephemeral && m.Lifecycle.ClientID == "" {
		// Tailscale removes an ephemeral device itself once it logs out.
		stop()
		return os.RemoveAll(dir)
	}
	if m.Lifecycle.ClientID == "" {
		return errors.New("deleting a persistent service's device needs the lifecycle OAuth client (-lifecycle-client-id)")
	}
	err = tailnet.DeleteDevice(ctx, m.Lifecycle, id, stop)
	if err != nil {
		return fmt.Errorf("delete tailnet device: %w", err)
	}
	return os.RemoveAll(dir)
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out error
	for _, v := range m.nodes {
		for _, raw := range v.raw {
			raw.close()
		}
		out = errors.Join(out, v.server.Close(), v.node.Load().Close())
	}
	return out
}
