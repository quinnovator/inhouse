package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/quinnovator/inhouse/internal/authz"
	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
)

// Every request reads its caller from ctx (see authz.With).

func requestHash(parts ...any) string {
	out := ""
	for _, p := range parts {
		out += fmt.Sprint(p) + "\x00"
	}
	return out
}

// replay returns the operation an idempotency key already created.
func (e *Engine) replay(ctx context.Context, key, hash string) (store.Operation, bool, error) {
	o, err := e.store.OperationByKey(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return o, false, nil
	}
	if err != nil {
		return o, false, err
	}
	if o.RequestHash != hash {
		return o, false, &store.Conflict{Message: "idempotency key was already used for a different request"}
	}
	return o, true, nil
}

// Deploy records a new revision of the stack in raw (YAML or JSON) and
// returns its operation. The reconciler does the rest.
func (e *Engine) Deploy(ctx context.Context, raw []byte, key string) (store.Operation, error) {
	p := authz.From(ctx)
	stack, err := spec.Parse(raw)
	if err != nil {
		return store.Operation{}, &Invalid{err}
	}
	if stack.Name == e.cfg.ControlName {
		return store.Operation{}, invalidf("%q is reserved for the control node", stack.Name)
	}
	if err = p.RequireDeploy(stack); err != nil {
		return store.Operation{}, err
	}
	if err = validKey(key); err != nil {
		return store.Operation{}, err
	}
	hash := requestHash(p.ID, store.Deploy, stack.Hash())
	if o, ok, err := e.replay(ctx, key, hash); ok || err != nil {
		return e.visible(ctx, o, err)
	}
	svc, err := e.store.Service(ctx, stack.Name)
	switch {
	case err == nil:
		if err := e.immutable(ctx, svc, stack); err != nil {
			return store.Operation{}, err
		}
	case !errors.Is(err, store.ErrNotFound):
		return store.Operation{}, err
	}
	o, err := e.store.Record(ctx, store.Request{Stack: stack, Kind: store.Deploy, Key: key, RequestHash: hash, Actor: p.ID, PortLow: e.cfg.PortLow, PortHigh: e.cfg.PortHigh})
	if err == nil {
		e.Nudge()
	}
	return e.visible(ctx, o, err)
}

// immutable refuses changes that would need a new tailnet node.
func (e *Engine) immutable(ctx context.Context, svc store.Service, stack spec.Stack) error {
	if svc.Deleted != 0 {
		return &store.Conflict{Message: "service is being deleted; wait for its delete operation"}
	}
	if svc.Stopped != 0 {
		return &store.Conflict{Message: "service is stopped; start it first"}
	}
	if svc.Kind != store.KindOf(stack) {
		return invalidf("service %s is %s; delete it before redeploying it as %s", svc.Name, svc.Kind, store.KindOf(stack))
	}
	if svc.Current == 0 {
		return nil
	}
	live, err := e.store.Revision(ctx, svc.Name, svc.Current)
	if err != nil {
		return err
	}
	if live.Spec.Expose != stack.Expose {
		return invalidf("service %s is exposed as %s; delete it before changing exposure", svc.Name, live.Spec.Expose)
	}
	return nil
}

// Rollback deploys an exact copy of an earlier revision as a new revision.
// With restoreVolumes (admin only), volume data is also replaced by the
// snapshots taken when that revision was deployed.
func (e *Engine) Rollback(ctx context.Context, name string, to int, restoreVolumes bool, key string) (store.Operation, error) {
	p := authz.From(ctx)
	if err := validService(name); err != nil {
		return store.Operation{}, err
	}
	if to < 1 {
		return store.Operation{}, invalidf("target revision must be positive")
	}
	if err := p.RequireRead(name); err != nil {
		return store.Operation{}, err
	}
	if err := validKey(key); err != nil {
		return store.Operation{}, err
	}
	target, err := e.store.Revision(ctx, name, to)
	if err != nil {
		return store.Operation{}, err
	}
	if err = p.RequireDeploy(target.Spec); err != nil {
		return store.Operation{}, err
	}
	for _, c := range target.Spec.Containers {
		if !spec.Pinned(c.Image) {
			return store.Operation{}, invalidf("r%d never got past image resolution; there is nothing to roll back to", to)
		}
	}
	restoreFrom := 0
	if restoreVolumes {
		if err = p.RequireAdmin(); err != nil {
			return store.Operation{}, err
		}
		if target.Spec.TTL != nil {
			return store.Operation{}, invalidf("volume restore is only available for persistent services")
		}
		if err = e.volumes.CheckRestore(ctx, target); err != nil {
			return store.Operation{}, &Invalid{err}
		}
		restoreFrom = to
	}
	hash := requestHash(p.ID, store.Rollback, target.Hash, to, restoreVolumes)
	if o, ok, err := e.replay(ctx, key, hash); ok || err != nil {
		return e.visible(ctx, o, err)
	}
	svc, err := e.store.Service(ctx, name)
	if err != nil {
		return store.Operation{}, err
	}
	if err = e.immutable(ctx, svc, target.Spec); err != nil {
		return store.Operation{}, err
	}
	o, err := e.store.Record(ctx, store.Request{Stack: target.Spec, Kind: store.Rollback, Key: key, RequestHash: hash, Actor: p.ID, RestoreFrom: restoreFrom, PortLow: e.cfg.PortLow, PortHigh: e.cfg.PortHigh})
	if err == nil {
		e.Nudge()
	}
	return e.visible(ctx, o, err)
}

// Delete tombstones a service. The reconciler stops its pods, removes its
// node and volumes (persistent volumes go to trash for seven days).
func (e *Engine) Delete(ctx context.Context, name, key string) (store.Operation, error) {
	p := authz.From(ctx)
	if err := validService(name); err != nil {
		return store.Operation{}, err
	}
	if err := p.RequireRead(name); err != nil {
		return store.Operation{}, err
	}
	if err := validKey(key); err != nil {
		return store.Operation{}, err
	}
	hash := requestHash(p.ID, store.Delete, name)
	if o, ok, err := e.replay(ctx, key, hash); ok || err != nil {
		return o, err
	}
	svc, err := e.store.Service(ctx, name)
	if err != nil {
		return store.Operation{}, err
	}
	if !p.CanDelete(name, svc.Kind == store.Ephemeral, svc.CreatedBy) {
		return store.Operation{}, fmt.Errorf("%w: deployers may delete only ephemeral services they created", authz.ErrForbidden)
	}
	o, err := e.store.Tombstone(ctx, name, p.ID, key, hash, false)
	if err == nil {
		e.Nudge()
	}
	return o, err
}

// live returns a service and its live revision, for a request that acts on
// it as a deploy would: the caller needs a deployer grant that covers the
// live revision's spec.
func (e *Engine) live(ctx context.Context, name string) (store.Service, store.Revision, error) {
	p := authz.From(ctx)
	svc, err := e.store.Service(ctx, name)
	if err != nil {
		return svc, store.Revision{}, err
	}
	if svc.Deleted != 0 {
		return svc, store.Revision{}, &store.Conflict{Message: "service is being deleted; wait for its delete operation"}
	}
	if svc.Current == 0 {
		return svc, store.Revision{}, &store.Conflict{Message: "service has no live revision yet"}
	}
	live, err := e.store.Revision(ctx, name, svc.Current)
	if err != nil {
		return svc, live, err
	}
	return svc, live, p.RequireDeploy(live.Spec)
}

// request checks what every request on an existing service needs, and
// returns the operation an idempotency key already created, if any.
func (e *Engine) request(ctx context.Context, name, key, hash string) (store.Operation, bool, error) {
	if err := validService(name); err != nil {
		return store.Operation{}, false, err
	}
	if err := authz.From(ctx).RequireRead(name); err != nil {
		return store.Operation{}, false, err
	}
	if err := validKey(key); err != nil {
		return store.Operation{}, false, err
	}
	return e.replay(ctx, key, hash)
}

// Restart deploys an exact copy of the live revision as a new revision, as
// a rollback to it would: the same images and secret versions. It follows
// the service's update strategy, so a rolling service keeps serving.
func (e *Engine) Restart(ctx context.Context, name, key string) (store.Operation, error) {
	p := authz.From(ctx)
	hash := requestHash(p.ID, store.Restart, name)
	if o, ok, err := e.request(ctx, name, key, hash); ok || err != nil {
		return e.visible(ctx, o, err)
	}
	svc, live, err := e.live(ctx, name)
	if err != nil {
		return store.Operation{}, err
	}
	if err = e.immutable(ctx, svc, live.Spec); err != nil {
		return store.Operation{}, err
	}
	o, err := e.store.Record(ctx, store.Request{Stack: live.Spec, Kind: store.Restart, Key: key, RequestHash: hash, Actor: p.ID, PortLow: e.cfg.PortLow, PortHigh: e.cfg.PortHigh})
	if err == nil {
		e.Nudge()
	}
	return e.visible(ctx, o, err)
}

// Redeploy deploys the live revision's spec again with every secret pinned
// to its current value, so a rotated secret reaches the service. Images
// stay at the same digests. If no secret changed, it is a no-op, as any
// unchanged deploy is.
func (e *Engine) Redeploy(ctx context.Context, name, key string) (store.Operation, error) {
	p := authz.From(ctx)
	hash := requestHash(p.ID, "redeploy", name)
	if o, ok, err := e.request(ctx, name, key, hash); ok || err != nil {
		return e.visible(ctx, o, err)
	}
	svc, live, err := e.live(ctx, name)
	if err != nil {
		return store.Operation{}, err
	}
	if err = e.immutable(ctx, svc, live.Spec); err != nil {
		return store.Operation{}, err
	}
	o, err := e.store.Record(ctx, store.Request{Stack: live.Spec, Kind: store.Deploy, Key: key, RequestHash: hash, Actor: p.ID, PortLow: e.cfg.PortLow, PortHigh: e.cfg.PortHigh})
	if err == nil {
		e.Nudge()
	}
	return e.visible(ctx, o, err)
}

// Stop takes a service offline without deleting it: its live revision stops
// and its address answers 503 until it is started. Revisions, volumes and
// the node are kept, and an ephemeral service still expires on time.
func (e *Engine) Stop(ctx context.Context, name, key string) (store.Operation, error) {
	return e.setStopped(ctx, store.Stop, name, key)
}

// Start runs a stopped service's live revision again.
func (e *Engine) Start(ctx context.Context, name, key string) (store.Operation, error) {
	return e.setStopped(ctx, store.Start, name, key)
}

func (e *Engine) setStopped(ctx context.Context, kind store.OpKind, name, key string) (store.Operation, error) {
	p := authz.From(ctx)
	hash := requestHash(p.ID, kind, name)
	if o, ok, err := e.request(ctx, name, key, hash); ok || err != nil {
		return o, err
	}
	if _, _, err := e.live(ctx, name); err != nil {
		return store.Operation{}, err
	}
	record := e.store.StartService
	if kind == store.Stop {
		record = e.store.StopService
	}
	o, err := record(ctx, name, p.ID, key, hash)
	if err == nil {
		e.Nudge()
	}
	return o, err
}

// Extend restarts an ephemeral service's TTL from now, as deploying it
// unchanged would, and returns the service.
func (e *Engine) Extend(ctx context.Context, name string) (ServiceView, error) {
	if _, _, err := e.request(ctx, name, "", ""); err != nil {
		return ServiceView{}, err
	}
	svc, live, err := e.live(ctx, name)
	if err != nil {
		return ServiceView{}, err
	}
	if svc.Kind != store.Ephemeral {
		return ServiceView{}, invalidf("service %s is persistent; only ephemeral services expire", name)
	}
	if err = e.store.Extend(ctx, live, authz.From(ctx).ID); err != nil {
		return ServiceView{}, err
	}
	svc, err = e.store.Service(ctx, name)
	return view(svc), err
}

// ---- reads ----

type ServiceView struct {
	store.Service
	URL string `json:"url,omitempty"`
}

type ServiceDetail struct {
	Service   ServiceView      `json:"service"`
	Revisions []store.Revision `json:"revisions"`
	Events    []store.Event    `json:"events"`
}

func view(s store.Service) ServiceView { return ServiceView{s, URL(s)} }

func (e *Engine) Whoami(ctx context.Context) (authz.Principal, error) {
	p := authz.From(ctx)
	if !p.Authenticated() {
		return p, authz.ErrForbidden
	}
	return p, nil
}

// List returns the services the caller may read.
func (e *Engine) List(ctx context.Context) ([]ServiceView, error) {
	p := authz.From(ctx)
	if !p.Authenticated() {
		return nil, authz.ErrForbidden
	}
	all, err := e.store.Services(ctx)
	if err != nil {
		return nil, err
	}
	out := []ServiceView{}
	for _, s := range all {
		if p.CanRead(s.Name) {
			out = append(out, view(s))
		}
	}
	return out, nil
}

// Get returns a service with its ten newest revisions and five newest events.
func (e *Engine) Get(ctx context.Context, name string) (ServiceDetail, error) {
	if err := authz.From(ctx).RequireRead(name); err != nil {
		return ServiceDetail{}, err
	}
	svc, err := e.store.Service(ctx, name)
	if err != nil {
		return ServiceDetail{}, err
	}
	revs, err := e.store.Revisions(ctx, name)
	if err != nil {
		return ServiceDetail{}, err
	}
	if len(revs) > 10 {
		revs = revs[:10]
	}
	events, err := e.store.Events(ctx, store.EventQuery{Service: name, Limit: 5})
	return ServiceDetail{view(svc), revs, events}, err
}

// Operation returns an operation the caller may read.
func (e *Engine) Operation(ctx context.Context, id string) (store.Operation, error) {
	o, err := e.store.Operation(ctx, id)
	if err != nil {
		return o, err
	}
	if err = authz.From(ctx).RequireRead(o.Service); err != nil {
		return store.Operation{}, err
	}
	return e.visible(ctx, o, nil)
}

// visible hides a deploy's reserved revision number until its images are
// resolved: a no-op deploy reports the live revision instead.
func (e *Engine) visible(ctx context.Context, o store.Operation, err error) (store.Operation, error) {
	if err != nil || o.Kind != store.Deploy || o.State != store.Running {
		return o, err
	}
	r, err := e.store.Revision(ctx, o.Service, o.Rev)
	if errors.Is(err, store.ErrNotFound) {
		return e.store.Operation(ctx, o.ID)
	}
	if err != nil {
		return o, err
	}
	if r.State == store.Pending {
		o.Rev = 0
	}
	return o, nil
}

// MaxWait bounds Wait so a call never outlives an HTTP or MCP request.
const MaxWait = 120 * time.Second

// Wait blocks until the operation finishes or timeout passes, then returns
// its state either way.
func (e *Engine) Wait(ctx context.Context, id string, timeout time.Duration) (store.Operation, error) {
	if timeout < 0 || timeout > MaxWait {
		return store.Operation{}, invalidf("timeout must be 0–120 seconds")
	}
	deadline := time.Now().Add(timeout)
	for {
		o, err := e.Operation(ctx, id)
		if err != nil || o.State != store.Running || time.Now().After(deadline) {
			return o, err
		}
		select {
		case <-ctx.Done():
			return o, nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Events returns the timeline the caller may read. Events without a service
// (secret changes, denied calls) are visible to admins only.
func (e *Engine) Events(ctx context.Context, q store.EventQuery) ([]store.Event, error) {
	p := authz.From(ctx)
	if !p.Authenticated() {
		return nil, authz.ErrForbidden
	}
	if q.Limit == 0 {
		q.Limit = 20
	}
	if q.Limit < 1 || q.Limit > 100 || q.Since < 0 {
		return nil, invalidf("limit must be 1–100 and since_id non-negative")
	}
	if q.Service != "" {
		if err := p.RequireRead(q.Service); err != nil {
			return nil, err
		}
		return e.store.Events(ctx, q)
	}
	if p.IsAdmin() {
		return e.store.Events(ctx, q)
	}
	want := q.Limit
	q.Limit = 1000
	all, err := e.store.Events(ctx, q)
	if err != nil {
		return nil, err
	}
	out := []store.Event{}
	for _, v := range all {
		if v.Service != "" && p.CanRead(v.Service) {
			out = append(out, v)
		}
	}
	if len(out) > want {
		if q.Since > 0 {
			out = out[:want]
		} else {
			out = out[len(out)-want:]
		}
	}
	return out, nil
}

// Audit records a refused call, attributed to its caller.
func (e *Engine) Audit(ctx context.Context, service, message string) {
	_ = e.store.Event(context.WithoutCancel(ctx), service, 0, authz.From(ctx).ID, "denied", message)
}

// ---- secrets ----

func (e *Engine) SetSecret(ctx context.Context, name, value string) error {
	p := authz.From(ctx)
	if err := p.RequireAdmin(); err != nil {
		return err
	}
	if err := e.vault.Set(ctx, name, value, p.ID); err != nil {
		return &Invalid{err}
	}
	return nil
}

func (e *Engine) ListSecrets(ctx context.Context) ([]store.SecretInfo, error) {
	if err := authz.From(ctx).RequireAdmin(); err != nil {
		return nil, err
	}
	return e.store.Secrets(ctx)
}

func (e *Engine) DeleteSecret(ctx context.Context, name string) error {
	p := authz.From(ctx)
	if err := p.RequireAdmin(); err != nil {
		return err
	}
	return e.store.DeleteSecret(ctx, name, p.ID)
}

// ---- plan ----

type ContainerPlan struct {
	Image     string            `json:"image"`
	Port      int               `json:"port,omitempty"`
	EnvKeys   []string          `json:"env_keys"`
	Secrets   map[string]string `json:"secrets,omitempty"`
	Volumes   map[string]string `json:"volumes,omitempty"`
	Resources spec.Resources    `json:"resources"`
}

type Plan struct {
	Service        string                   `json:"service"`
	Exists         bool                     `json:"exists"`
	LiveRev        int                      `json:"live_rev,omitempty"`
	Before         map[string]ContainerPlan `json:"before"`
	After          map[string]ContainerPlan `json:"after"`
	Expose         string                   `json:"expose"`
	TTL            *string                  `json:"ttl,omitempty"`
	UpdateStrategy string                   `json:"update_strategy"`
	Warnings       []string                 `json:"warnings"`
	MayApply       bool                     `json:"may_apply"`
}

func containers(s *spec.Stack) map[string]ContainerPlan {
	out := map[string]ContainerPlan{}
	if s == nil {
		return out
	}
	for name, c := range s.Containers {
		keys := []string{}
		for k := range c.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out[name] = ContainerPlan{c.Image, c.Port, keys, c.Secrets, c.Volumes, c.Resources}
	}
	return out
}

// Plan shows what deploying raw would change, without changing anything or
// pulling images. Env values and secret values are never shown.
func (e *Engine) Plan(ctx context.Context, raw []byte) (Plan, error) {
	p := authz.From(ctx)
	s, err := spec.Parse(raw)
	if err != nil {
		return Plan{}, &Invalid{err}
	}
	if err = p.RequireRead(s.Name); err != nil {
		return Plan{}, err
	}
	out := Plan{Service: s.Name, After: containers(&s), Expose: s.Expose, TTL: s.TTL, UpdateStrategy: s.UpdateStrategy, Warnings: []string{}, MayApply: p.CanDeploy(s)}
	var live *spec.Stack
	svc, err := e.store.Service(ctx, s.Name)
	switch {
	case err == nil:
		out.Exists = true
		if conflict := e.immutable(ctx, svc, s); conflict != nil {
			out.Warnings = append(out.Warnings, "Deploy will be refused: "+conflict.Error())
			out.MayApply = false
		}
		if svc.Current > 0 {
			r, err := e.store.Revision(ctx, s.Name, svc.Current)
			if err != nil {
				return Plan{}, err
			}
			live, out.LiveRev = &r.Spec, r.Rev
		}
	case !errors.Is(err, store.ErrNotFound):
		return Plan{}, err
	}
	out.Before = containers(live)
	if s.Name == e.cfg.ControlName {
		out.Warnings = append(out.Warnings, "Deploy will be refused: the name is reserved for the control node.")
		out.MayApply = false
	}
	if s.UpdateStrategy == spec.Recreate && live != nil {
		out.Warnings = append(out.Warnings, "Recreate update: the live revision stops before the new one starts, so the service is briefly unavailable.")
	}
	for _, c := range s.Containers {
		if !spec.Pinned(c.Image) {
			out.Warnings = append(out.Warnings, "Mutable image tags are resolved to digests at deploy time.")
			break
		}
	}
	if s.Expose == spec.ExposeFunnel {
		out.Warnings = append(out.Warnings, "Funnel makes this service reachable from the public Internet, without tailnet identity.")
	}
	return out, nil
}
