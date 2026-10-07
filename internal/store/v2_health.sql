-- How the live revision is doing, as the reconciler last saw it. Cutover
-- resets it: a new live revision has just passed its health checks.
ALTER TABLE services ADD COLUMN health TEXT NOT NULL DEFAULT '' CHECK (health IN ('', 'healthy', 'degraded'));
ALTER TABLE services ADD COLUMN health_reason TEXT NOT NULL DEFAULT '';
-- Restarts of the live revision in a row; cleared once it stays healthy.
ALTER TABLE services ADD COLUMN restarts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE services ADD COLUMN restarted_at INTEGER;
UPDATE services SET health = 'healthy' WHERE current_rev > 0;
