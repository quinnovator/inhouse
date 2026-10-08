-- A stopped service keeps its revisions, volumes and node, but its live
-- revision doesn't run until the service is started again.
ALTER TABLE services ADD COLUMN stopped_at INTEGER;

-- Restarts, stops and starts run as operations too. SQLite can't change a
-- CHECK constraint, so the table is rebuilt.
CREATE TABLE operations_v3 (
  id              TEXT PRIMARY KEY,
  kind            TEXT NOT NULL CHECK (kind IN ('deploy', 'rollback', 'delete', 'restart', 'stop', 'start')),
  service         TEXT NOT NULL,
  rev             INTEGER NOT NULL DEFAULT 0,
  restore_from    INTEGER NOT NULL DEFAULT 0,
  state           TEXT NOT NULL CHECK (state IN ('running', 'succeeded', 'failed')),
  idempotency_key TEXT UNIQUE,
  request_hash    TEXT NOT NULL,
  reason          TEXT NOT NULL DEFAULT '',
  created_by      TEXT NOT NULL,
  created_at      INTEGER NOT NULL,
  finished_at     INTEGER
);
INSERT INTO operations_v3 (id, kind, service, rev, restore_from, state, idempotency_key, request_hash, reason, created_by, created_at, finished_at)
  SELECT id, kind, service, rev, restore_from, state, idempotency_key, request_hash, reason, created_by, created_at, finished_at FROM operations;
DROP TABLE operations;
ALTER TABLE operations_v3 RENAME TO operations;
CREATE UNIQUE INDEX one_running_operation ON operations(service) WHERE state = 'running';
