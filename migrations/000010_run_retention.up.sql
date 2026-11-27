BEGIN;

-- Run retention (CONQUEST_RUN_RETENTION): a terminal run's data (its objects,
-- or its own bucket) is deleted once the run has been terminal for longer
-- than the retention period. The run itself stays; data_deleted_at records
-- when its data went.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS data_deleted_at TIMESTAMPTZ;

-- The retention sweep's query: terminal runs with data, oldest first.
CREATE INDEX IF NOT EXISTS idx_runs_retention ON runs (finished_at, run_id)
    WHERE data_deleted_at IS NULL AND finished_at IS NOT NULL;

-- Leases of cluster-wide background jobs, such as the retention sweep: one
-- server replica holds a job's lease until expires_at, and the others skip
-- the job meanwhile.
CREATE TABLE IF NOT EXISTS leases (
    name       TEXT PRIMARY KEY,
    holder     TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

-- The identity of this Conquest: one random ID per database. Conquest
-- labels the run buckets it makes with it, so that it looks for buckets
-- without a run only among its own, never among those of another Conquest
-- (another database) in the same namespace.
CREATE TABLE IF NOT EXISTS instance (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    id        TEXT NOT NULL
);
INSERT INTO instance (id) VALUES (gen_random_uuid()::text)
    ON CONFLICT (singleton) DO NOTHING;

COMMIT;
