-- Desired state. The reconciler makes Podman and the tailnet edges match it.

CREATE TABLE services (
  name        TEXT PRIMARY KEY,
  kind        TEXT NOT NULL CHECK (kind IN ('persistent', 'ephemeral')),
  node_dns    TEXT NOT NULL DEFAULT '',   -- actual MagicDNS name once the node joins
  current_rev INTEGER NOT NULL DEFAULT 0, -- desired live revision; 0 = none yet
  expires_at  INTEGER,                    -- unix seconds; ephemeral only
  created_by  TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  deleted_at  INTEGER                     -- tombstone; the reconciler tears down
);

-- Revisions are immutable once normalized: images are pinned to digests and
-- secrets to exact versions, so rolling back runs exactly the same bytes.
CREATE TABLE revisions (
  service           TEXT NOT NULL REFERENCES services(name),
  rev               INTEGER NOT NULL,
  spec_json         TEXT NOT NULL,
  spec_hash         TEXT NOT NULL,
  host_port         INTEGER UNIQUE,       -- NULL once the pod is removed
  state             TEXT NOT NULL CHECK (state IN ('pending', 'starting', 'live', 'draining', 'stopped', 'failed')),
  reason            TEXT NOT NULL DEFAULT '',
  created_by        TEXT NOT NULL,
  created_at        INTEGER NOT NULL,
  health_started_at INTEGER,
  drain_until       INTEGER,
  finished_at       INTEGER,
  PRIMARY KEY (service, rev)
);

-- Deploys, rollbacks and deletes run as operations callers can wait on.
CREATE TABLE operations (
  id              TEXT PRIMARY KEY,
  kind            TEXT NOT NULL CHECK (kind IN ('deploy', 'rollback', 'delete')),
  service         TEXT NOT NULL,
  rev             INTEGER NOT NULL DEFAULT 0,
  restore_from    INTEGER NOT NULL DEFAULT 0, -- rollback that also restores volume snapshots
  state           TEXT NOT NULL CHECK (state IN ('running', 'succeeded', 'failed')),
  idempotency_key TEXT UNIQUE,
  request_hash    TEXT NOT NULL,
  reason          TEXT NOT NULL DEFAULT '',
  created_by      TEXT NOT NULL,
  created_at      INTEGER NOT NULL,
  finished_at     INTEGER
);
CREATE UNIQUE INDEX one_running_operation ON operations(service) WHERE state = 'running';

-- Attributed timeline of everything that happened; also the audit log.
CREATE TABLE events (
  id      INTEGER PRIMARY KEY,
  ts      INTEGER NOT NULL,
  service TEXT NOT NULL DEFAULT '',
  rev     INTEGER NOT NULL DEFAULT 0,
  actor   TEXT NOT NULL,
  kind    TEXT NOT NULL,
  message TEXT NOT NULL
);
CREATE INDEX events_by_service ON events(service, id);

-- Current secret values, age-encrypted.
CREATE TABLE secrets (
  name       TEXT PRIMARY KEY,
  ciphertext BLOB NOT NULL,
  updated_by TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

-- Every secret version a revision was pinned to, keyed by ciphertext hash.
CREATE TABLE secret_versions (
  hash       TEXT PRIMARY KEY,
  ciphertext BLOB NOT NULL
);

-- Each service's fixed slice of subordinate IDs. Persistent services' slots
-- are never reused: retained volumes and backups keep their numeric
-- ownership. A deleted ephemeral service's slot is freed for reuse.
CREATE TABLE namespaces (
  service TEXT PRIMARY KEY,
  slot    INTEGER NOT NULL UNIQUE
);
