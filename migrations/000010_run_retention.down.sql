BEGIN;

-- Only run retention uses leases so far; a later job that uses the table
-- must move it to a migration of its own.
DROP TABLE IF EXISTS leases;
DROP INDEX IF EXISTS idx_runs_retention;
ALTER TABLE runs DROP COLUMN IF EXISTS data_deleted_at;

COMMIT;
