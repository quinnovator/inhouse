package edge

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/tailnet"
	"tailscale.com/ipn"
)

// Manager runs one tailnet node and HTTPS listener per service.
type Manager struct {
	StateDir   string // one subdirectory per node
	Enroll     tailnet.Credential
	Lifecycle  tailnet.Credential
	ControlURL string // tests only

	mu    sync.Mutex
	nodes map[string]*managed
}

type managed struct {
	node   *tailnet.Node
	edge   *Edge
	server *http.Server
	port   int
	expose string
}

func NewManager(stateDir string, enroll, lifecycle tailnet.Credential) *Manager {
	return &Manager{StateDir: stateDir, Enroll: enroll, Lifecycle: lifecycle, nodes: map[string]*managed{}}
}

func tagFor(kind store.Kind) string {
	if kind == store.Ephemeral {
		return tailnet.TagEphemeral
	}
	return tailnet.TagService
}

// Ensure starts the service's node and listener if they aren't running.
func (m *Manager) Ensure(ctx context.Context, service string, kind store.Kind, expose string) (string, error) {
	if expose != spec.ExposeTailnet && expose != spec.ExposeFunnel {
		return "", errors.New("invalid exposure mode")
	}
	if kind == store.Ephemeral && expose == spec.ExposeFunnel {
		return "", errors.New("ephemeral services cannot use Funnel")
	}
	m.mu.Lock()
	existing := m.nodes[service]
	m.mu.Unlock()
	if existing != nil {
		if existing.expose != expose {
			return "", errors.New("a node's exposure mode cannot change; delete the service first")
		}
		return existing.node.DNS, nil
	}
	n, err := tailnet.Open(ctx, tailnet.NodeConfig{
		Dir: filepath.Join(m.StateDir, service), Hostname: service, Tag: tagFor(kind),
		Ephemeral: kind == store.Ephemeral, Enroll: m.Enroll, Certificate: true, ControlURL: m.ControlURL,
	})
	if err != nil {
		return "", err
	}
	e := New(func(ctx context.Context, remote string) (Identity, error) {
		who, err := n.Client.WhoIs(ctx, remote)
		if err != nil {
			return Identity{}, err
		}
		if who.UserProfile == nil || (who.Node != nil && len(who.Node.Tags) > 0) {
			return Identity{}, nil
		}
		return Identity{Login: who.UserProfile.LoginName, Name: who.UserProfile.DisplayName}, nil
	})
	var listener net.Listener
	if expose == spec.ExposeFunnel {
		listener, err = n.Server.ListenFunnel("tcp", ":443")
	} else {
		listener, err = n.Server.ListenTLS("tcp", ":443")
	}
	if err != nil {
		_ = n.Close()
		return "", fmt.Errorf("listen on %s: %w", n.DNS, err)
	}
	server := &http.Server{Handler: e, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, ConnContext: connContext}
	m.mu.Lock()
	m.nodes[service] = &managed{node: n, edge: e, server: server, expose: expose}
	m.mu.Unlock()
	go func() { _ = server.Serve(listener) }()
	return n.DNS, nil
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
	target, err := NewTarget(fmt.Sprintf("http://127.0.0.1:%d", port), v.node.DNS)
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
			_ = v.server.Close()
			_ = v.node.Client.Logout(ctx)
			_ = v.node.Close()
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
		out = errors.Join(out, v.server.Close(), v.node.Close())
	}
	return out
}
