BEGIN;

-- Run ownership: the caller that created a run owns it and its tasks.
--
-- owner is the stable identifier of the creator (for OpenID Connect callers
-- "<issuer>|<subject>"); NULL for runs created without authentication, which
-- no authenticated caller can see. auth_context records how the owner
-- authenticated (identifiers such as client_id and scope, never tokens).
-- Tasks have no owner column: they belong to their run.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS owner TEXT;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS auth_context JSONB;

-- Owner-scoped listing uses the same keyset as unscoped listing.
CREATE INDEX IF NOT EXISTS idx_runs_owner_created
    ON runs (owner, created_at DESC, run_id DESC)
    WHERE owner IS NOT NULL;

COMMIT;
