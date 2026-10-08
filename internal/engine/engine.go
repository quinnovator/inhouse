// Package engine is inhouse's core: the requests callers make and the
// reconciler that carries them out.
//
// SQLite holds desired state. Podman and the tailnet edges are actual state.
// Requests only record intent (an operation and a pending revision) and nudge
// the reconciler, which does the slow work: pulling images, starting pods,
// health checks and cutover. The HTTP API and the MCP server are thin
// adapters over this package, so both behave and authorize identically.
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
	"github.com/quinnovator/inhouse/internal/vault"
)

// Runtime runs revisions as pods. Every method must be idempotent and must
// only touch objects it labelled itself.
type Runtime interface {
	// Resolve pulls an image and returns its pinned digest reference.
	Resolve(ctx context.Context, image string) (string, error)
	Up(ctx context.Context, r store.Revision) error
	Running(ctx context.Context, r store.Revision) (bool, error)
	// Check runs one container's health check once.
	Check(ctx context.Context, r store.Revision, container string) error
	Stop(ctx context.Context, r store.Revision) error
	Remove(ctx context.Context, r store.Revision) error
	Logs(ctx context.Context, r store.Revision, container string, tail int) (string, error)
	// Pods lists every labelled pod as the revision it claims to be
	// (Service, Rev, Hash and Created are set).
	Pods(ctx context.Context) ([]store.Revision, error)
	// RemoveOrphan removes a labelled pod with no revision, and its secrets.
	RemoveOrphan(ctx context.Context, r store.Revision) error
	// RemoveServiceSecrets removes every secret labelled for the service.
	RemoveServiceSecrets(ctx context.Context, service string) error
}

// Volumes manages service volumes and their snapshots.
type Volumes interface {
	// Snapshot takes a read-only snapshot of each volume, named for r.
	Snapshot(ctx context.Context, r store.Revision) error
	// CheckRestore verifies r's snapshots exist and are read-only.
	CheckRestore(ctx context.Context, r store.Revision) error
	// Restore replaces candidate's volume data with the snapshots taken for
	// revision from. The writer must already be stopped.
	Restore(ctx context.Context, candidate store.Revision, from int) error
	Delete(ctx context.Context, svc store.Service) error
}

// Edges are the per-service tailnet nodes that proxy HTTPS to a revision.
type Edges interface {
	// Ensure starts the service's node if needed and returns its DNS name.
	Ensure(ctx context.Context, service string, kind store.Kind, expose string) (string, error)
	// Switch atomically points the edge at a local port.
	Switch(service string, port int) error
	// Port reports where the edge points, or 0.
	Port(service string) int
	// Clear makes the edge answer 503 until the next Switch.
	Clear(service string)
	// Stopped reports why the service's HTTPS listener stopped serving, or
	// nil. Ensure starts it again.
	Stopped(service string) error
	// Delete removes the node from the tailnet and forgets its state.
	Delete(ctx context.Context, service string, kind store.Kind) error
}

type Config struct {
	// ControlName is the control node's hostname; no service may use it.
	ControlName string
	// PortLow..PortHigh is the loopback range pods publish their ingress on.
	PortLow, PortHigh int
	// Drain is how long a replaced revision keeps running for in-flight requests.
	Drain time.Duration
	// HealthInterval separates health check attempts.
	HealthInterval time.Duration
	// ProbeInterval separates checks of each live revision while it serves.
	ProbeInterval time.Duration
	// ProbeFailures in a row restart a live revision.
	ProbeFailures int
	// RestartBackoff separates the first two restarts in a row; each further
	// restart waits twice as long as the last, up to MaxRestartBackoff.
	RestartBackoff, MaxRestartBackoff time.Duration
	// RestartReset is how long a restarted revision must stay healthy before
	// its next restart counts as the first again.
	RestartReset time.Duration
	// Interval is the reconciler's periodic pass.
	Interval time.Duration
	// FailedRetention keeps failed revisions' containers for their logs.
	FailedRetention time.Duration
	// OrphanGrace is how old an unknown labelled pod must be before removal.
	OrphanGrace time.Duration
	// PullTimeout bounds pulling each image.
	PullTimeout time.Duration
}

func DefaultConfig() Config {
	return Config{
		ControlName:       "deploy",
		PortLow:           20000,
		PortHigh:          29999,
		Drain:             10 * time.Second,
		HealthInterval:    2 * time.Second,
		ProbeInterval:     10 * time.Second,
		ProbeFailures:     3,
		RestartBackoff:    10 * time.Second,
		MaxRestartBackoff: 5 * time.Minute,
		RestartReset:      10 * time.Minute,
		Interval:          30 * time.Second,
		FailedRetention:   24 * time.Hour,
		OrphanGrace:       10 * time.Minute,
		PullTimeout:       30 * time.Minute,
	}
}

type Engine struct {
	cfg     Config
	store   *store.Store
	vault   *vault.Vault
	runtime Runtime
	volumes Volumes
	edges   Edges

	nudge chan struct{}
	wg    sync.WaitGroup

	mu        sync.Mutex
	busy      map[string]bool
	missed    map[string]bool // a reconcile found the service busy
	lastError map[string]string
	failures  map[string]failures
	relistens map[string]relistens
}

// failures counts a live revision's failed probes in a row.
type failures struct{ rev, n int }

// relistens counts a service's HTTPS listener restarts in a row.
type relistens struct {
	n  int
	at time.Time
}

func New(cfg Config, s *store.Store, v *vault.Vault, r Runtime, vol Volumes, e Edges) *Engine {
	return &Engine{
		cfg: cfg, store: s, vault: v, runtime: r, volumes: vol, edges: e,
		nudge:     make(chan struct{}, 1),
		busy:      map[string]bool{},
		missed:    map[string]bool{},
		lastError: map[string]string{},
		failures:  map[string]failures{},
		relistens: map[string]relistens{},
	}
}

// Nudge asks the reconciler for a pass soon.
func (e *Engine) Nudge() {
	select {
	case e.nudge <- struct{}{}:
	default:
	}
}

// Invalid is a request the caller must change before retrying.
type Invalid struct{ Err error }

func (e *Invalid) Error() string { return e.Err.Error() }
func (e *Invalid) Unwrap() error { return e.Err }

func invalidf(format string, args ...any) error {
	return &Invalid{fmt.Errorf(format, args...)}
}

// IsInvalid reports whether err is the caller's to fix.
func IsInvalid(err error) bool {
	var i *Invalid
	return errors.As(err, &i)
}

// URL is a service's HTTPS address once its node has joined.
func URL(s store.Service) string {
	if s.DNS == "" {
		return ""
	}
	return "https://" + s.DNS
}

func validKey(key string) error {
	if len(key) > 128 {
		return invalidf("idempotency key must be at most 128 characters")
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return invalidf("idempotency key cannot contain control characters")
		}
	}
	return nil
}

func validService(name string) error {
	if !spec.ValidName(name) {
		return invalidf("invalid service name %q", name)
	}
	return nil
}
