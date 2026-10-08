package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/quinnovator/inhouse/internal/spec"
	"github.com/quinnovator/inhouse/internal/store"
)

// Run reconciles on start, whenever nudged, and every Interval until ctx ends.
// Between passes it probes every live revision each ProbeInterval. Services
// reconcile in parallel; each service has at most one pass or probe running.
func (e *Engine) Run(ctx context.Context) error {
	ticker := time.NewTicker(e.cfg.Interval)
	defer ticker.Stop()
	probes := time.NewTicker(e.cfg.ProbeInterval)
	defer probes.Stop()
	defer e.wg.Wait()
	e.Nudge()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-probes.C:
			e.probeAll(ctx)
			continue
		case <-ticker.C:
		case <-e.nudge:
		}
		e.pass(ctx)
	}
}

func (e *Engine) pass(ctx context.Context) {
	if err := e.removeOrphans(ctx); err != nil && ctx.Err() == nil {
		log.Printf("orphan sweep: %v", err)
	}
	services, err := e.store.Services(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("reconcile: %v", err)
		}
		return
	}
	for _, s := range services {
		if !e.claim(s.Name, true) {
			continue
		}
		e.wg.Add(1)
		go func(name string) {
			defer e.wg.Done()
			defer e.release(name)
			err := e.reconcile(ctx, name)
			if ctx.Err() == nil {
				e.report(ctx, name, 0, "reconcile_error", err)
			}
		}(s.Name)
	}
}

// probeAll checks every live revision that no pass is busy with.
func (e *Engine) probeAll(ctx context.Context) {
	services, err := e.store.Services(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("probe: %v", err)
		}
		return
	}
	for _, s := range services {
		if s.Current == 0 || s.Deleted != 0 || s.Stopped != 0 || !e.claim(s.Name, false) {
			continue
		}
		e.wg.Add(1)
		go func(name string) {
			defer e.wg.Done()
			defer e.release(name)
			rev, err := e.probe(ctx, name)
			if ctx.Err() == nil {
				e.report(ctx, name, rev, "live_unavailable", err)
			}
		}(s.Name)
	}
}

// claim reserves a service for one pass or probe. A reconcile that finds the
// service busy runs again as soon as it is released, so a deploy recorded
// during a restart doesn't wait for the next Interval.
func (e *Engine) claim(name string, reconcile bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.busy[name] {
		e.missed[name] = e.missed[name] || reconcile
		return false
	}
	e.busy[name] = true
	return true
}

func (e *Engine) release(name string) {
	e.mu.Lock()
	missed := e.missed[name]
	delete(e.busy, name)
	delete(e.missed, name)
	e.mu.Unlock()
	if missed {
		e.Nudge()
	}
}

// report logs an error and records it as an event, once per distinct error,
// so a persistent problem doesn't flood the timeline every pass.
func (e *Engine) report(ctx context.Context, service string, rev int, kind string, err error) {
	key := service + "\x00" + kind
	e.mu.Lock()
	if err == nil {
		delete(e.lastError, key)
		e.mu.Unlock()
		return
	}
	repeated := e.lastError[key] == err.Error()
	e.lastError[key] = err.Error()
	e.mu.Unlock()
	if repeated {
		return
	}
	log.Printf("%s %s: %v", kind, service, err)
	_ = e.store.Event(ctx, service, rev, "reconciler", kind, err.Error())
}

// reconcile makes one service's actual state match its desired state.
func (e *Engine) reconcile(ctx context.Context, name string) error {
	svc, err := e.store.Service(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if svc.Deleted != 0 {
		return e.teardown(ctx, svc)
	}
	if svc.Expires > 0 && svc.Expires <= time.Now().Unix() {
		if _, err = e.store.Tombstone(ctx, name, "reconciler", "", "expire", true); err != nil {
			return err
		}
		svc.Deleted = time.Now().Unix()
		return e.teardown(ctx, svc)
	}
	op, err := e.store.RunningOperation(ctx, name)
	hasOp := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	switch {
	case svc.Stopped != 0:
		if err = e.halt(ctx, svc, op, hasOp); err != nil {
			return err
		}
		return e.cleanup(ctx, name)
	case hasOp && op.Kind == store.Start:
		if err = e.resume(ctx, svc, op); err != nil {
			return err
		}
		return e.cleanup(ctx, name)
	}
	// A recreate update stops the live revision on purpose; don't restart it.
	replacing := hasOp && e.replacing(ctx, op)
	if svc.Current > 0 && !replacing {
		live, err := e.store.Revision(ctx, name, svc.Current)
		if err != nil {
			return err
		}
		e.report(ctx, name, live.Rev, "live_unavailable", e.keepLive(ctx, svc, live, false))
	}
	if hasOp {
		if err = e.advance(ctx, op); err != nil {
			return err
		}
	}
	return e.cleanup(ctx, name)
}

func (e *Engine) replacing(ctx context.Context, o store.Operation) bool {
	if o.Kind == store.Delete {
		return false
	}
	r, err := e.store.Revision(ctx, o.Service, o.Rev)
	return err == nil && r.State == store.Starting && r.Spec.UpdateStrategy == spec.Recreate
}

// probe checks a service's live revision once, unless the service is being
// deleted, has expired, is stopped or being started, or a recreate update
// stopped the live revision on purpose: the next pass handles those.
func (e *Engine) probe(ctx context.Context, name string) (rev int, err error) {
	svc, err := e.store.Service(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil || svc.Current == 0 || svc.Deleted != 0 || svc.Stopped != 0 || (svc.Expires > 0 && svc.Expires <= time.Now().Unix()) {
		return 0, err
	}
	op, err := e.store.RunningOperation(ctx, name)
	if err == nil && (op.Kind == store.Start || e.replacing(ctx, op)) {
		return 0, nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	live, err := e.store.Revision(ctx, name, svc.Current)
	if err != nil {
		return 0, err
	}
	return live.Rev, e.keepLive(ctx, svc, live, true)
}

// keepLive ensures the live revision runs, passes its health checks, and has
// the edge pointed at it and listening. In the steady state a pass makes
// three cheap checks and a probe also runs every container's health check
// once. A revision that stopped, or failed ProbeFailures probes in a row, is
// marked degraded and restarted with backoff; an edge listener that stopped
// is restarted with backoff too, and the revision is still probed while it
// is down. After a daemon restart, a revision that is still running gets
// traffic again once it passes health.
func (e *Engine) keepLive(ctx context.Context, svc store.Service, r store.Revision, probe bool) error {
	running, err := e.runtime.Running(ctx, r)
	if err != nil {
		return err
	}
	var listening error
	switch {
	case !running:
		return e.restart(ctx, svc, r, "not every container is running")
	case e.edges.Port(r.Service) != r.Port:
		if svc.Health == store.Degraded {
			return e.restart(ctx, svc, r, svc.HealthReason)
		}
		if err = e.health(ctx, r, time.Now()); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return e.restart(ctx, svc, r, err.Error())
		}
		return e.serve(ctx, r)
	case e.edges.Stopped(r.Service) != nil:
		listening = e.relisten(ctx, r)
	}
	if !probe {
		return listening
	}
	err = e.check(ctx, r)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if n := e.failed(r, err); n >= e.cfg.ProbeFailures {
		return e.restart(ctx, svc, r, fmt.Sprintf("%d health checks failed in a row, last: %v", n, err))
	}
	if err == nil && svc.Restarts > 0 && time.Since(time.Unix(svc.RestartedAt, 0)) >= e.cfg.RestartReset {
		if err = e.store.ClearRestarts(ctx, r); err != nil {
			return err
		}
	}
	return listening
}

// failed counts r's failed probes in a row, starting over after a pass.
func (e *Engine) failed(r store.Revision, err error) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.failures[r.Service]
	if err == nil || f.rev != r.Rev {
		f = failures{rev: r.Rev}
	}
	if err != nil {
		f.n++
	}
	e.failures[r.Service] = f
	return f.n
}

// restart marks the live revision degraded, takes it out of service, and
// stops and restarts its pod once the backoff since its previous restart
// has passed. Until then later probes and passes try again. The reason
// shown in get_service is the first failure, or the last failed restart.
func (e *Engine) restart(ctx context.Context, svc store.Service, r store.Revision, reason string) error {
	e.edges.Clear(r.Service)
	if svc.Health != store.Degraded {
		if err := e.store.Degrade(ctx, r, reason); err != nil {
			return err
		}
	}
	if svc.Restarts > 0 && time.Since(time.Unix(svc.RestartedAt, 0)) < e.backoff(svc.Restarts) {
		return nil
	}
	if err := e.store.Restarting(ctx, r, svc.Restarts+1); err != nil {
		return err
	}
	e.failed(r, nil)
	err := e.runtime.Stop(ctx, r)
	if err == nil {
		err = e.revive(ctx, r)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return e.store.Degrade(ctx, r, "restart failed: "+err.Error())
	}
	return e.store.Recover(ctx, r)
}

// relisten starts the service's stopped HTTPS listener again, on a new node
// if its node stopped too. The revision is unaffected and keeps its health.
// A listener that keeps stopping waits as a restarted revision does, and
// once one stays up for RestartReset its next restart counts as the first.
func (e *Engine) relisten(ctx context.Context, r store.Revision) error {
	stopped := e.edges.Stopped(r.Service)
	e.mu.Lock()
	l := e.relistens[r.Service]
	if time.Since(l.at) >= e.cfg.RestartReset {
		l.n = 0
	}
	wait := l.n > 0 && time.Since(l.at) < e.backoff(l.n)
	if !wait {
		l = relistens{n: l.n + 1, at: time.Now()}
		e.relistens[r.Service] = l
	}
	e.mu.Unlock()
	if wait {
		return fmt.Errorf("HTTPS listener is down: %w", stopped)
	}
	msg := fmt.Sprintf("HTTPS listener stopped (%v); restarting it (restart %d in a row)", stopped, l.n)
	if err := e.store.Event(ctx, r.Service, r.Rev, "reconciler", "listener_stopped", msg); err != nil {
		return err
	}
	if err := e.serve(ctx, r); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("HTTPS listener is down: %w", err)
	}
	return e.store.Event(ctx, r.Service, r.Rev, "reconciler", "listener_restarted", "HTTPS listener is serving again")
}

// backoff is the least time between restart n and the next one.
func (e *Engine) backoff(n int) time.Duration {
	d := e.cfg.RestartBackoff
	for i := 1; i < n && d < e.cfg.MaxRestartBackoff; i++ {
		d *= 2
	}
	return min(d, e.cfg.MaxRestartBackoff)
}

// revive starts a live revision's stopped pod from its pinned images, waits
// for health, and only then points the edge at it.
func (e *Engine) revive(ctx context.Context, r store.Revision) error {
	e.edges.Clear(r.Service)
	for _, name := range r.Spec.Order {
		image := r.Spec.Containers[name].Image
		resolved, err := e.runtime.Resolve(ctx, image)
		if err != nil {
			return err
		}
		if resolved != image {
			return fmt.Errorf("registry no longer serves pinned image %s", image)
		}
	}
	if err := e.runtime.Up(ctx, r); err != nil {
		return err
	}
	if err := e.health(ctx, r, time.Now()); err != nil {
		return err
	}
	return e.serve(ctx, r)
}

// halt keeps a stopped service's live revision stopped and completes the
// stop operation once it is. The node stays up and answers 503, so the
// address still says the service exists.
func (e *Engine) halt(ctx context.Context, svc store.Service, op store.Operation, hasOp bool) error {
	e.edges.Clear(svc.Name)
	e.mu.Lock()
	delete(e.failures, svc.Name)
	e.mu.Unlock()
	live, err := e.store.Revision(ctx, svc.Name, svc.Current)
	if err != nil {
		return err
	}
	running, err := e.runtime.Running(ctx, live)
	if err == nil && running {
		err = e.runtime.Stop(ctx, live)
	}
	if err != nil {
		return err
	}
	if hasOp && op.Kind == store.Stop {
		if err = e.store.Complete(ctx, op, ""); err != nil {
			return err
		}
	}
	if _, err = e.edges.Ensure(ctx, svc.Name, svc.Kind, live.Spec.Expose); err != nil {
		return fmt.Errorf("stopped; its address does not answer: %w", err)
	}
	e.edges.Clear(svc.Name)
	return nil
}

// resume starts a stopped service's live revision again, as after a
// reboot. If it doesn't come up healthy the start fails and the service is
// degraded, so it is restarted with backoff like any live revision that
// went down.
func (e *Engine) resume(ctx context.Context, svc store.Service, op store.Operation) error {
	live, err := e.store.Revision(ctx, svc.Name, svc.Current)
	if err != nil {
		return err
	}
	if err = e.revive(ctx, live); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := e.store.Degrade(ctx, live, "start failed: "+err.Error()); err != nil {
			return err
		}
		return e.store.Complete(ctx, op, err.Error())
	}
	if err = e.store.Started(ctx, live); err != nil {
		return err
	}
	return e.store.Complete(ctx, op, "")
}

// serve points the service's edge at r.
func (e *Engine) serve(ctx context.Context, r store.Revision) error {
	dns, err := e.edges.Ensure(ctx, r.Service, store.KindOf(r.Spec), r.Spec.Expose)
	if err != nil {
		return err
	}
	if err = e.store.SetDNS(ctx, r.Service, dns); err != nil {
		return err
	}
	return e.edges.Switch(r.Service, r.Port)
}

// candidateFailure fails the operation; any other error is retried next pass.
type candidateFailure struct{ err error }

func (f candidateFailure) Error() string { return f.err.Error() }

func failed(err error) error { return candidateFailure{err} }

// advance moves a deploy or rollback forward from wherever it stopped:
// pending (resolve images, pin secrets) → starting (pod up, health) → live
// (cutover) → drain old revisions → succeeded.
func (e *Engine) advance(ctx context.Context, o store.Operation) error {
	if o.Kind == store.Delete {
		return nil
	}
	r, err := e.store.Revision(ctx, o.Service, o.Rev)
	if err != nil {
		return err
	}
	done, err := e.step(ctx, o, r)
	var f candidateFailure
	if errors.As(err, &f) {
		return e.fail(ctx, o, r, f.err)
	}
	if err != nil || done {
		return err
	}
	if err = e.cleanup(ctx, o.Service); err != nil {
		return err
	}
	return e.store.Complete(ctx, o, "")
}

// step returns done when the operation already finished as a no-op.
func (e *Engine) step(ctx context.Context, o store.Operation, r store.Revision) (done bool, err error) {
	if r.State == store.Pending {
		pinned, err := e.pin(ctx, o, r)
		if err != nil {
			return false, failed(err)
		}
		if o.Kind == store.Deploy {
			svc, err := e.store.Service(ctx, r.Service)
			if err != nil {
				return false, err
			}
			if svc.Current > 0 {
				live, err := e.store.Revision(ctx, r.Service, svc.Current)
				if err != nil {
					return false, err
				}
				if live.Hash == pinned.Hash() {
					return true, e.store.Noop(ctx, o, live)
				}
			}
		}
		if err = e.store.Normalize(ctx, r, pinned); err != nil {
			return false, err
		}
		if r, err = e.store.Revision(ctx, r.Service, r.Rev); err != nil {
			return false, err
		}
	}
	switch r.State {
	case store.Starting:
		return false, e.start(ctx, o, r)
	case store.Live:
		return false, nil // cutover finished before a restart; only cleanup remains
	default:
		return false, failed(fmt.Errorf("unexpected revision state %s", r.State))
	}
}

// pin resolves every image to a digest and, for deploys, pins secret versions.
// Rollbacks keep the versions their target was deployed with.
func (e *Engine) pin(ctx context.Context, o store.Operation, r store.Revision) (spec.Stack, error) {
	s := r.Spec
	pull, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for _, name := range s.Order {
		c := s.Containers[name]
		image, err := e.runtime.Resolve(pull, c.Image)
		if err != nil {
			return s, fmt.Errorf("%s: %w", name, err)
		}
		c.Image = image
		s.Containers[name] = c
	}
	if o.Kind == store.Deploy {
		if err := e.vault.Pin(ctx, &s); err != nil {
			return s, err
		}
	}
	return s, nil
}

// start brings up the candidate and cuts over to it once healthy.
func (e *Engine) start(ctx context.Context, o store.Operation, r store.Revision) error {
	dns, err := e.edges.Ensure(ctx, r.Service, store.KindOf(r.Spec), r.Spec.Expose)
	if err != nil {
		return failed(err)
	}
	if err = e.store.SetDNS(ctx, r.Service, dns); err != nil {
		return failed(err)
	}
	if r.Spec.UpdateStrategy == spec.Recreate {
		// Stop the old writer before its volumes are snapshotted or reused.
		svc, err := e.store.Service(ctx, r.Service)
		if err != nil {
			return err
		}
		if svc.Current > 0 && svc.Current != r.Rev {
			old, err := e.store.Revision(ctx, r.Service, svc.Current)
			if err != nil {
				return err
			}
			e.edges.Clear(r.Service)
			if err = e.runtime.Stop(ctx, old); err != nil {
				return failed(err)
			}
		}
	}
	if o.RestoreFrom > 0 {
		if err = e.volumes.Restore(ctx, r, o.RestoreFrom); err != nil {
			return failed(err)
		}
	}
	if err = e.volumes.Snapshot(ctx, r); err != nil {
		return failed(err)
	}
	if err = e.runtime.Up(ctx, r); err != nil {
		return failed(err)
	}
	if err = e.store.StartHealth(ctx, r); err != nil {
		return err
	}
	if r, err = e.store.Revision(ctx, r.Service, r.Rev); err != nil {
		return err
	}
	if err = e.health(ctx, r, time.Unix(r.HealthStarted, 0)); err != nil {
		return failed(err)
	}
	if err = e.store.Cutover(ctx, r, e.cfg.Drain); err != nil {
		return err
	}
	return e.edges.Switch(r.Service, r.Port)
}

// fail stops the candidate, keeps its last log lines as an event and fails
// the operation. The live revision keeps serving; after a recreate update it
// is started again.
func (e *Engine) fail(ctx context.Context, o store.Operation, r store.Revision, reason error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	cleanup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := e.runtime.Stop(cleanup, r); err != nil {
		_ = e.store.Event(ctx, r.Service, r.Rev, "reconciler", "stop_error", err.Error())
	}
	if ingress, _ := r.Spec.Ingress(); ingress != "" {
		if logs, err := e.runtime.Logs(cleanup, r, ingress, 20); err == nil && logs != "" {
			logs = e.vault.Redact(ctx, logs)
			if len(logs) > 8<<10 {
				logs = logs[len(logs)-8<<10:]
			}
			_ = e.store.Event(ctx, r.Service, r.Rev, "reconciler", "failure_logs", logs)
		}
	}
	if err := e.store.Complete(ctx, o, reason.Error()); err != nil {
		return err
	}
	if r.Spec.UpdateStrategy == spec.Recreate {
		svc, err := e.store.Service(ctx, r.Service)
		if err == nil && svc.Current > 0 && svc.Current != r.Rev {
			if live, err := e.store.Revision(ctx, r.Service, svc.Current); err == nil {
				e.report(ctx, r.Service, live.Rev, "live_unavailable", e.revive(ctx, live))
			}
		}
	}
	return nil
}

// health requires every container's check to pass three times in a row
// before the deadline set by the longest health timeout in the stack.
// Sidecars without a check must stay running.
func (e *Engine) health(ctx context.Context, r store.Revision, started time.Time) error {
	timeout := time.Duration(0)
	for _, c := range r.Spec.Containers {
		if t, _ := time.ParseDuration(c.Health.Timeout); t > timeout {
			timeout = t
		}
	}
	ctx, cancel := context.WithDeadline(ctx, started.Add(timeout))
	defer cancel()
	passes, last := 0, "deadline passed before the first check"
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("health check timed out: %s", last)
		}
		if err := e.check(ctx, r); err != nil {
			passes, last = 0, err.Error()
		} else if passes++; passes == 3 {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(e.cfg.HealthInterval):
		}
	}
}

// check runs every container's health check once, sidecars first.
func (e *Engine) check(ctx context.Context, r store.Revision) error {
	for _, name := range r.Spec.StartOrder() {
		if err := e.runtime.Check(ctx, r, name); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// cleanup stops revisions whose drain window ended, removes stopped pods and
// releases their ports, and removes failed revisions after FailedRetention.
func (e *Engine) cleanup(ctx context.Context, name string) error {
	svc, err := e.store.Service(ctx, name)
	if err != nil {
		return err
	}
	revs, err := e.store.Revisions(ctx, name)
	if err != nil {
		return err
	}
	for _, r := range revs {
		if r.Rev == svc.Current || r.Port == 0 {
			continue
		}
		switch r.State {
		case store.Draining:
			if wait := time.Until(time.Unix(r.DrainUntil, 0)); wait > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(wait):
				}
			}
			if err = e.runtime.Stop(ctx, r); err != nil {
				return err
			}
			if err = e.store.MarkStopped(ctx, r); err != nil {
				return err
			}
			fallthrough
		case store.Stopped:
			if err = e.runtime.Remove(ctx, r); err != nil {
				return err
			}
			if err = e.store.ReleasePort(ctx, r); err != nil {
				return err
			}
		case store.Failed:
			if err = e.runtime.Stop(ctx, r); err != nil {
				return err
			}
			if time.Since(time.Unix(r.Finished, 0)) >= e.cfg.FailedRetention {
				if err = e.runtime.Remove(ctx, r); err != nil {
					return err
				}
				if err = e.store.ReleasePort(ctx, r); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// removeOrphans deletes labelled pods with no revision in the database, once
// they are older than OrphanGrace (a revision may be mid-creation).
func (e *Engine) removeOrphans(ctx context.Context) error {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pods, err := e.runtime.Pods(bounded)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if pod.Created == 0 || time.Since(time.Unix(pod.Created, 0)) < e.cfg.OrphanGrace {
			continue
		}
		_, err := e.store.Revision(ctx, pod.Service, pod.Rev)
		if err == nil {
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err = e.runtime.RemoveOrphan(bounded, pod); err != nil {
			return err
		}
		_ = e.store.Event(ctx, pod.Service, pod.Rev, "reconciler", "orphan_removed", "removed a labelled pod with no matching revision")
	}
	return nil
}

// teardown removes everything a tombstoned service owns, then the service.
func (e *Engine) teardown(ctx context.Context, svc store.Service) error {
	e.edges.Clear(svc.Name)
	e.mu.Lock()
	delete(e.failures, svc.Name)
	delete(e.relistens, svc.Name)
	e.mu.Unlock()
	revs, err := e.store.Revisions(ctx, svc.Name)
	if err != nil {
		return err
	}
	for _, r := range revs {
		if err = e.runtime.Stop(ctx, r); err != nil {
			return err
		}
		if err = e.runtime.Remove(ctx, r); err != nil {
			return err
		}
		if err = e.store.ReleasePort(ctx, r); err != nil {
			return err
		}
	}
	// Also remove labelled pods and secrets with no revision row: once an
	// ephemeral service is gone its ID slot and name may be reused, and
	// nothing of it may outlive it.
	pods, err := e.runtime.Pods(ctx)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if pod.Service != svc.Name {
			continue
		}
		if err = e.runtime.RemoveOrphan(ctx, pod); err != nil {
			return err
		}
	}
	if err = e.runtime.RemoveServiceSecrets(ctx, svc.Name); err != nil {
		return err
	}
	if err = e.volumes.Delete(ctx, svc); err != nil {
		return err
	}
	if err = e.edges.Delete(ctx, svc.Name, svc.Kind); err != nil {
		return err
	}
	return e.store.FinishDelete(ctx, svc.Name)
}
