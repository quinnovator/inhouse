// Package store keeps desired state, history and encrypted secrets in SQLite.
//
// Every write is one transaction that also records an event, so the event
// table is a complete, attributed timeline. State is written before the
// reconciler acts on it; a crash mid-action is re-evaluated on the next pass.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/quinnovator/inhouse/internal/spec"
	_ "modernc.org/sqlite"
)

var (
	//go:embed schema.sql
	schemaV1 string
	//go:embed v2_health.sql
	healthV2 string
)

// migrations are forward-only; entry i upgrades user_version i to i+1.
var migrations = []string{schemaV1, healthV2}

var ErrNotFound = errors.New("not found")

// Conflict reports a request that clashes with current state. OperationID
// names the running operation to wait on, when there is one.
type Conflict struct {
	Message     string
	OperationID string
}

func (e *Conflict) Error() string { return e.Message }

type Kind string

const (
	Persistent Kind = "persistent"
	Ephemeral  Kind = "ephemeral"
)

// KindOf reports which kind of service a stack creates.
func KindOf(s spec.Stack) Kind {
	if s.TTL != nil {
		return Ephemeral
	}
	return Persistent
}

type RevState string

const (
	Pending  RevState = "pending"
	Starting RevState = "starting"
	Live     RevState = "live"
	Draining RevState = "draining"
	Stopped  RevState = "stopped"
	Failed   RevState = "failed"
)

// Health is how a service's live revision is doing.
type Health string

const (
	Healthy  Health = "healthy"
	Degraded Health = "degraded"
)

type OpKind string

const (
	Deploy   OpKind = "deploy"
	Rollback OpKind = "rollback"
	Delete   OpKind = "delete"
)

type OpState string

const (
	Running   OpState = "running"
	Succeeded OpState = "succeeded"
	OpFailed  OpState = "failed"
)

type Service struct {
	Name      string `json:"name"`
	Kind      Kind   `json:"kind"`
	DNS       string `json:"node_dns,omitempty"`
	Current   int    `json:"current_rev"`
	Expires   int64  `json:"expires_at,omitempty"`
	CreatedBy string `json:"created_by"`
	Created   int64  `json:"created_at"`
	Deleted   int64  `json:"deleted_at,omitempty"`
	// Health is empty until the service has a live revision.
	Health       Health `json:"health,omitempty"`
	HealthReason string `json:"health_reason,omitempty"`
	Restarts     int    `json:"restarts,omitempty"`
	RestartedAt  int64  `json:"restarted_at,omitempty"`
}

type Revision struct {
	Service       string     `json:"service"`
	Rev           int        `json:"rev"`
	Spec          spec.Stack `json:"spec"`
	Hash          string     `json:"spec_hash"`
	Port          int        `json:"host_port,omitempty"`
	State         RevState   `json:"state"`
	Reason        string     `json:"reason,omitempty"`
	CreatedBy     string     `json:"created_by"`
	Created       int64      `json:"created_at"`
	HealthStarted int64      `json:"health_started_at,omitempty"`
	DrainUntil    int64      `json:"drain_until,omitempty"`
	Finished      int64      `json:"finished_at,omitempty"`
}

type Operation struct {
	ID          string  `json:"operation_id"`
	Kind        OpKind  `json:"kind"`
	Service     string  `json:"service"`
	Rev         int     `json:"rev,omitempty"`
	RestoreFrom int     `json:"restore_from,omitempty"`
	State       OpState `json:"state"`
	Key         string  `json:"idempotency_key,omitempty"`
	RequestHash string  `json:"-"`
	Reason      string  `json:"reason,omitempty"`
	CreatedBy   string  `json:"created_by"`
	Created     int64   `json:"created_at"`
	Finished    int64   `json:"finished_at,omitempty"`
}

type Event struct {
	ID      int64  `json:"id"`
	TS      int64  `json:"ts"`
	Service string `json:"service,omitempty"`
	Rev     int    `json:"rev,omitempty"`
	Actor   string `json:"actor"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type Store struct{ db *sql.DB }

// Open creates or upgrades the database at file. One connection serializes
// all access, which keeps transactions simple and SQLite happy.
func Open(file string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", file)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*Store, error) { _ = db.Close(); return nil, err }
	if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000"); err != nil {
		return fail(err)
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if version > len(migrations) {
		return fail(fmt.Errorf("database version %d is newer than this binary supports", version))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return fail(err)
		}
		if _, err = tx.Exec(migrations[i]); err == nil {
			_, err = tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", i+1))
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			return fail(fmt.Errorf("migrate to version %d: %w", i+1, err))
		}
	}
	if err = os.Chmod(file, 0o600); err != nil {
		return fail(err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func now() int64 { return time.Now().Unix() }

func event(tx *sql.Tx, service string, rev int, actor, kind, message string) error {
	_, err := tx.Exec("INSERT INTO events(ts,service,rev,actor,kind,message) VALUES(?,?,?,?,?,?)", now(), service, rev, actor, kind, message)
	return err
}

// Event records one attributed line in the timeline.
func (s *Store) Event(ctx context.Context, service string, rev int, actor, kind, message string) error {
	return s.write(ctx, func(tx *sql.Tx) error { return event(tx, service, rev, actor, kind, message) })
}

type scanner interface{ Scan(...any) error }

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ---- services ----

const serviceCols = "name,kind,node_dns,current_rev,COALESCE(expires_at,0),created_by,created_at,COALESCE(deleted_at,0),health,health_reason,restarts,COALESCE(restarted_at,0)"

func scanService(row scanner) (Service, error) {
	var v Service
	err := row.Scan(&v.Name, &v.Kind, &v.DNS, &v.Current, &v.Expires, &v.CreatedBy, &v.Created, &v.Deleted, &v.Health, &v.HealthReason, &v.Restarts, &v.RestartedAt)
	return v, notFound(err)
}

func (s *Store) Service(ctx context.Context, name string) (Service, error) {
	return scanService(s.db.QueryRowContext(ctx, "SELECT "+serviceCols+" FROM services WHERE name=?", name))
}

func (s *Store) Services(ctx context.Context) ([]Service, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+serviceCols+" FROM services ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Service{}
	for rows.Next() {
		v, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetDNS records the node's actual MagicDNS name. Tailscale may suffix a
// taken name; a later change would silently move the service, so it is refused.
func (s *Store) SetDNS(ctx context.Context, name, dns string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var old string
		if err := tx.QueryRow("SELECT node_dns FROM services WHERE name=?", name).Scan(&old); err != nil {
			return notFound(err)
		}
		if old == dns {
			return nil
		}
		if old != "" {
			return fmt.Errorf("node DNS changed from %s to %s; refusing implicit rename", old, dns)
		}
		if _, err := tx.Exec("UPDATE services SET node_dns=? WHERE name=?", dns, name); err != nil {
			return err
		}
		return event(tx, name, 0, "reconciler", "node_ready", "HTTPS node ready at "+dns)
	})
}

// ---- revisions ----

const revisionCols = "service,rev,spec_json,spec_hash,COALESCE(host_port,0),state,reason,created_by,created_at,COALESCE(health_started_at,0),COALESCE(drain_until,0),COALESCE(finished_at,0)"

func scanRevision(row scanner) (Revision, error) {
	var r Revision
	var raw string
	err := row.Scan(&r.Service, &r.Rev, &raw, &r.Hash, &r.Port, &r.State, &r.Reason, &r.CreatedBy, &r.Created, &r.HealthStarted, &r.DrainUntil, &r.Finished)
	if err != nil {
		return r, notFound(err)
	}
	return r, json.Unmarshal([]byte(raw), &r.Spec)
}

func (s *Store) Revision(ctx context.Context, service string, rev int) (Revision, error) {
	return scanRevision(s.db.QueryRowContext(ctx, "SELECT "+revisionCols+" FROM revisions WHERE service=? AND rev=?", service, rev))
}

// Revisions lists a service's revisions, newest first.
func (s *Store) Revisions(ctx context.Context, service string) ([]Revision, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+revisionCols+" FROM revisions WHERE service=? ORDER BY rev DESC", service)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Revision{}
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Normalize replaces a pending revision's spec with its pinned form and moves
// it to starting.
func (s *Store) Normalize(ctx context.Context, r Revision, pinned spec.Stack) error {
	raw, err := json.Marshal(pinned)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec("UPDATE revisions SET spec_json=?,spec_hash=?,state='starting' WHERE service=? AND rev=? AND state='pending'", string(raw), pinned.Hash(), r.Service, r.Rev)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return &Conflict{Message: "revision is no longer pending"}
		}
		return event(tx, r.Service, r.Rev, "reconciler", "normalized", "images pinned to digests and secrets to versions")
	})
}

// StartHealth records when health checking began, once, so a restarted
// daemon keeps the original deadline.
func (s *Store) StartHealth(ctx context.Context, r Revision) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE revisions SET health_started_at=COALESCE(health_started_at,?) WHERE service=? AND rev=?", now(), r.Service, r.Rev)
		return err
	})
}

// Cutover makes r the desired live revision and starts draining the old one.
// It refuses if the service was tombstoned meanwhile.
func (s *Store) Cutover(ctx context.Context, r Revision, drain time.Duration) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var old int
		var deleted int64
		if err := tx.QueryRow("SELECT current_rev,COALESCE(deleted_at,0) FROM services WHERE name=?", r.Service).Scan(&old, &deleted); err != nil {
			return notFound(err)
		}
		if deleted != 0 {
			return &Conflict{Message: "service is being deleted"}
		}
		if old != 0 && old != r.Rev {
			if _, err := tx.Exec("UPDATE revisions SET state='draining',drain_until=? WHERE service=? AND rev=?", time.Now().Add(drain).Unix(), r.Service, old); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("UPDATE services SET current_rev=?,expires_at=?,health='healthy',health_reason='',restarts=0,restarted_at=NULL WHERE name=?", r.Rev, expiry(r.Spec), r.Service); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE revisions SET state='live',reason='' WHERE service=? AND rev=?", r.Service, r.Rev); err != nil {
			return err
		}
		return event(tx, r.Service, r.Rev, "reconciler", "cutover", fmt.Sprintf("r%d is healthy and now live", r.Rev))
	})
}

// expiry restarts an ephemeral service's TTL from now.
func expiry(s spec.Stack) any {
	if s.TTL == nil {
		return nil
	}
	return time.Now().Add(s.TTLDuration()).Unix()
}

// MarkStopped records that a drained revision's pod has stopped.
func (s *Store) MarkStopped(ctx context.Context, r Revision) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE revisions SET state='stopped',finished_at=? WHERE service=? AND rev=?", now(), r.Service, r.Rev); err != nil {
			return err
		}
		return event(tx, r.Service, r.Rev, "reconciler", "stopped", "drain window ended; pod stopped")
	})
}

// ReleasePort records that a revision's pod is gone and frees its port.
func (s *Store) ReleasePort(ctx context.Context, r Revision) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE revisions SET host_port=NULL WHERE service=? AND rev=?", r.Service, r.Rev)
		return err
	})
}

// ---- health ----

// setLive updates the service row only while r is its live revision (and
// cond holds), and records the event only if it did.
func (s *Store) setLive(ctx context.Context, r Revision, kind, message, set, cond string, args ...any) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec("UPDATE services SET "+set+" WHERE name=? AND current_rev=? AND deleted_at IS NULL"+cond, append(args, r.Service, r.Rev)...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 || kind == "" {
			return nil
		}
		return event(tx, r.Service, r.Rev, "reconciler", kind, message)
	})
}

// Degrade records that the live revision r is failing, and why.
func (s *Store) Degrade(ctx context.Context, r Revision, reason string) error {
	return s.setLive(ctx, r, "degraded", fmt.Sprintf("r%d is unhealthy: %s", r.Rev, reason),
		"health='degraded',health_reason=?", "", reason)
}

// Restarting records a restart of the live revision r before it happens, so
// the backoff survives a daemon restart.
func (s *Store) Restarting(ctx context.Context, r Revision, restarts int) error {
	return s.setLive(ctx, r, "restarting", fmt.Sprintf("restarting r%d (restart %d in a row)", r.Rev, restarts),
		"restarts=?,restarted_at=?", "", restarts, now())
}

// Recover records that the live revision r passes its health checks again.
// It does nothing unless r is degraded.
func (s *Store) Recover(ctx context.Context, r Revision) error {
	return s.setLive(ctx, r, "recovered", fmt.Sprintf("r%d is healthy and serving again", r.Rev),
		"health='healthy',health_reason=''", " AND health='degraded'")
}

// ClearRestarts ends a run of restarts once the live revision stays healthy.
func (s *Store) ClearRestarts(ctx context.Context, r Revision) error {
	return s.setLive(ctx, r, "", "", "restarts=0", "")
}

// ---- operations ----

const operationCols = "id,kind,service,rev,restore_from,state,COALESCE(idempotency_key,''),request_hash,reason,created_by,created_at,COALESCE(finished_at,0)"

func scanOperation(row scanner) (Operation, error) {
	var o Operation
	err := row.Scan(&o.ID, &o.Kind, &o.Service, &o.Rev, &o.RestoreFrom, &o.State, &o.Key, &o.RequestHash, &o.Reason, &o.CreatedBy, &o.Created, &o.Finished)
	return o, notFound(err)
}

func (s *Store) Operation(ctx context.Context, id string) (Operation, error) {
	return scanOperation(s.db.QueryRowContext(ctx, "SELECT "+operationCols+" FROM operations WHERE id=?", id))
}

// RunningOperation returns the service's one running operation.
func (s *Store) RunningOperation(ctx context.Context, service string) (Operation, error) {
	return scanOperation(s.db.QueryRowContext(ctx, "SELECT "+operationCols+" FROM operations WHERE service=? AND state='running'", service))
}

// OperationByKey finds the operation an idempotency key created.
func (s *Store) OperationByKey(ctx context.Context, key string) (Operation, error) {
	if key == "" {
		return Operation{}, ErrNotFound
	}
	return scanOperation(s.db.QueryRowContext(ctx, "SELECT "+operationCols+" FROM operations WHERE idempotency_key=?", key))
}

// replay returns the operation previously created with key, if any. Reusing
// a key for a different request is a conflict.
func replay(tx *sql.Tx, key, requestHash string) (Operation, bool, error) {
	if key == "" {
		return Operation{}, false, nil
	}
	o, err := scanOperation(tx.QueryRow("SELECT "+operationCols+" FROM operations WHERE idempotency_key=?", key))
	if errors.Is(err, ErrNotFound) {
		return o, false, nil
	}
	if err != nil {
		return o, false, err
	}
	if o.RequestHash != requestHash {
		return o, false, &Conflict{Message: "idempotency key was already used for a different request"}
	}
	return o, true, nil
}

func busy(tx *sql.Tx, service string) error {
	active, err := scanOperation(tx.QueryRow("SELECT "+operationCols+" FROM operations WHERE service=? AND state='running'", service))
	if err == nil {
		return &Conflict{Message: "service has a running operation", OperationID: active.ID}
	}
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func nullable(key string) any {
	if key == "" {
		return nil
	}
	return key
}

// Request describes a deploy or rollback to record.
type Request struct {
	Stack       spec.Stack
	Kind        OpKind
	Key         string
	RequestHash string
	Actor       string
	RestoreFrom int
	PortLow     int
	PortHigh    int
}

// Record durably reserves a pending revision, a host port and an operation
// before anything touches the runtime.
func (s *Store) Record(ctx context.Context, q Request) (Operation, error) {
	var out Operation
	err := s.write(ctx, func(tx *sql.Tx) error {
		if o, ok, err := replay(tx, q.Key, q.RequestHash); ok || err != nil {
			out = o
			return err
		}
		if err := busy(tx, q.Stack.Name); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO services(name,kind,expires_at,created_by,created_at) VALUES(?,?,?,?,?) ON CONFLICT(name) DO NOTHING",
			q.Stack.Name, KindOf(q.Stack), expiry(q.Stack), q.Actor, now()); err != nil {
			return err
		}
		var deleted int64
		if err := tx.QueryRow("SELECT COALESCE(deleted_at,0) FROM services WHERE name=?", q.Stack.Name).Scan(&deleted); err != nil {
			return err
		}
		if deleted != 0 {
			return &Conflict{Message: "service is being deleted"}
		}
		var rev int
		if err := tx.QueryRow("SELECT COALESCE(MAX(rev),0)+1 FROM revisions WHERE service=?", q.Stack.Name).Scan(&rev); err != nil {
			return err
		}
		port, err := freePort(tx, q.PortLow, q.PortHigh)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(q.Stack)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO revisions(service,rev,spec_json,spec_hash,host_port,state,created_by,created_at) VALUES(?,?,?,?,?,'pending',?,?)",
			q.Stack.Name, rev, string(raw), q.Stack.Hash(), port, q.Actor, now()); err != nil {
			return err
		}
		out = Operation{ID: uuid.NewString(), Kind: q.Kind, Service: q.Stack.Name, Rev: rev, RestoreFrom: q.RestoreFrom, State: Running, Key: q.Key, RequestHash: q.RequestHash, CreatedBy: q.Actor, Created: now()}
		if _, err = tx.Exec("INSERT INTO operations(id,kind,service,rev,restore_from,state,idempotency_key,request_hash,created_by,created_at) VALUES(?,?,?,?,?,'running',?,?,?,?)",
			out.ID, out.Kind, out.Service, rev, q.RestoreFrom, nullable(q.Key), q.RequestHash, q.Actor, out.Created); err != nil {
			return err
		}
		message := fmt.Sprintf("%s recorded as r%d", q.Kind, rev)
		if q.RestoreFrom > 0 {
			message += fmt.Sprintf(", restoring volumes from r%d", q.RestoreFrom)
		}
		return event(tx, q.Stack.Name, rev, q.Actor, string(q.Kind), message)
	})
	return out, err
}

func freePort(tx *sql.Tx, low, high int) (int, error) {
	rows, err := tx.Query("SELECT host_port FROM revisions WHERE host_port IS NOT NULL")
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	used := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return 0, err
		}
		used[p] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for p := low; p <= high; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, errors.New("host port range exhausted")
}

// Complete finishes an operation. A non-empty reason fails it and its revision.
func (s *Store) Complete(ctx context.Context, o Operation, reason string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		state := Succeeded
		if reason != "" {
			state = OpFailed
			if _, err := tx.Exec("UPDATE revisions SET state='failed',reason=?,finished_at=? WHERE service=? AND rev=?", reason, now(), o.Service, o.Rev); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("UPDATE operations SET state=?,reason=?,finished_at=? WHERE id=?", state, reason, now(), o.ID); err != nil {
			return err
		}
		message := string(o.Kind) + " succeeded"
		if reason != "" {
			message = string(o.Kind) + " failed: " + reason
		}
		return event(tx, o.Service, o.Rev, "reconciler", string(state), message)
	})
}

// Noop completes a deploy whose pinned spec equals the live revision: the
// reserved revision is discarded and the operation reports the live one.
// An ephemeral service's TTL restarts.
func (s *Store) Noop(ctx context.Context, o Operation, live Revision) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM revisions WHERE service=? AND rev=? AND state='pending'", o.Service, o.Rev); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE operations SET rev=?,state='succeeded',finished_at=? WHERE id=?", live.Rev, now(), o.ID); err != nil {
			return err
		}
		if live.Spec.TTL != nil {
			if _, err := tx.Exec("UPDATE services SET expires_at=? WHERE name=? AND deleted_at IS NULL", expiry(live.Spec), o.Service); err != nil {
				return err
			}
		}
		return event(tx, o.Service, live.Rev, "reconciler", "noop", fmt.Sprintf("spec matches live r%d; nothing to do", live.Rev))
	})
}

// Tombstone marks a service for deletion and records the delete operation.
// An expiry cancels whatever operation is running; a caller's delete waits.
func (s *Store) Tombstone(ctx context.Context, name, actor, key, requestHash string, expired bool) (Operation, error) {
	var out Operation
	err := s.write(ctx, func(tx *sql.Tx) error {
		if o, ok, err := replay(tx, key, requestHash); ok || err != nil {
			out = o
			return err
		}
		var deleted int64
		if err := tx.QueryRow("SELECT COALESCE(deleted_at,0) FROM services WHERE name=?", name).Scan(&deleted); err != nil {
			return notFound(err)
		}
		active, err := scanOperation(tx.QueryRow("SELECT "+operationCols+" FROM operations WHERE service=? AND state='running'", name))
		switch {
		case err == nil && active.Kind == Delete:
			out = active
			return nil
		case err == nil && !expired:
			return &Conflict{Message: "service has a running operation", OperationID: active.ID}
		case err == nil:
			if _, err := tx.Exec("UPDATE operations SET state='failed',reason='service expired',finished_at=? WHERE id=?", now(), active.ID); err != nil {
				return err
			}
		case !errors.Is(err, ErrNotFound):
			return err
		}
		if _, err := tx.Exec("UPDATE services SET deleted_at=COALESCE(deleted_at,?) WHERE name=?", now(), name); err != nil {
			return err
		}
		out = Operation{ID: uuid.NewString(), Kind: Delete, Service: name, State: Running, Key: key, RequestHash: requestHash, CreatedBy: actor, Created: now()}
		if _, err := tx.Exec("INSERT INTO operations(id,kind,service,state,idempotency_key,request_hash,created_by,created_at) VALUES(?,'delete',?,'running',?,?,?,?)",
			out.ID, name, nullable(key), requestHash, actor, out.Created); err != nil {
			return err
		}
		message := "service marked for deletion"
		if expired {
			message = "ttl expired; service marked for deletion"
		}
		return event(tx, name, 0, actor, "delete", message)
	})
	return out, err
}

// FinishDelete removes a torn-down service and its revisions. Events and
// operations remain as history. An ephemeral service's ID slot is released:
// its volumes and snapshots are already gone, so no data keeps its ownership.
func (s *Store) FinishDelete(ctx context.Context, name string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM namespaces WHERE service=? AND EXISTS (SELECT 1 FROM services WHERE name=? AND kind='ephemeral' AND deleted_at IS NOT NULL)", name, name); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM revisions WHERE service=?", name); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM services WHERE name=? AND deleted_at IS NOT NULL", name); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE operations SET state='succeeded',finished_at=? WHERE service=? AND kind='delete' AND state='running'", now(), name); err != nil {
			return err
		}
		return event(tx, name, 0, "reconciler", "deleted", "pods, volumes and node removed")
	})
}

// ---- events ----

// EventQuery selects events. With Since set, it returns the events after that
// id, oldest first; otherwise the newest Limit events, oldest first.
type EventQuery struct {
	Service string
	Since   int64
	Limit   int
}

func (s *Store) Events(ctx context.Context, q EventQuery) ([]Event, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	query := "SELECT id,ts,service,rev,actor,kind,message FROM events WHERE (?='' OR service=?) AND id>? ORDER BY id ASC LIMIT ?"
	if q.Since == 0 {
		query = "SELECT * FROM (SELECT id,ts,service,rev,actor,kind,message FROM events WHERE (?='' OR service=?) AND id>? ORDER BY id DESC LIMIT ?) ORDER BY id ASC"
	}
	rows, err := s.db.QueryContext(ctx, query, q.Service, q.Service, q.Since, q.Limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Event{}
	for rows.Next() {
		var v Event
		if err := rows.Scan(&v.ID, &v.TS, &v.Service, &v.Rev, &v.Actor, &v.Kind, &v.Message); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---- namespaces ----

// Namespace returns the service's subordinate-ID slot, allocating the lowest
// free one on first use. Persistent services keep their slot for the host's
// lifetime; deleted ephemeral services release theirs (see FinishDelete).
func (s *Store) Namespace(ctx context.Context, service string, slots int) (int, error) {
	var slot int
	err := s.write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow("SELECT slot FROM namespaces WHERE service=?", service).Scan(&slot)
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err = tx.QueryRow("SELECT MIN(slot) FROM (SELECT 0 AS slot UNION ALL SELECT slot+1 FROM namespaces) WHERE slot NOT IN (SELECT slot FROM namespaces)").Scan(&slot); err != nil {
			return err
		}
		if slot >= slots {
			return errors.New("subordinate ID pool exhausted; enlarge the inhouse user's subuid/subgid range")
		}
		_, err = tx.Exec("INSERT INTO namespaces(service,slot) VALUES(?,?)", service, slot)
		return err
	})
	return slot, err
}
