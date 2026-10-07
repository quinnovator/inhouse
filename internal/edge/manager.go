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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/tailnet"
	"tailscale.com/ipn"
)

// Manager runs one tailnet node and HTTPS listener per service.
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
	port    int
	stopped error // why the listener stopped serving; nil while it serves
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
		if expose == spec.ExposeFunnel {
			return n.Server.ListenFunnel("tcp", ":443")
		}
		return n.Server.ListenTLS("tcp", ":443")
	}
	return m
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
	v := &managed{expose: expose}
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
	v.server = &http.Server{Handler: v.edge, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, ConnContext: connContext}
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
			// The upstream sends the old name as Host; make the next Switch
			// build it again.
			m.mu.Lock()
			v.edge.Switch(nil)
			v.port = 0
			m.mu.Unlock()
		}
		n = replacement
		v.node.Store(n)
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

func (m *Manager) Switch(service string, port int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.nodes[service]
	if v == nil {
		return errors.New("edge node is not running")
	}
	if v.port == port {
		return nil
	}
	target, err := NewTarget(fmt.Sprintf("http://127.0.0.1:%d", port), v.node.Load().DNS)
	if err != nil {
		return err
	}
	v.edge.Switch(target)
	v.port = port
	return nil
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
		out = errors.Join(out, v.server.Close(), v.node.Load().Close())
	}
	return out
}
