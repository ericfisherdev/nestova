-- +goose Up
-- Chore claim expiry and penalty (NES-117): track when a claim was made and
-- when it expires so the background sweep can revert an incomplete claim and
-- penalize the claimant.
--
-- assignee_id's existing semantics are left untouched: TaskInstanceRepository.
-- Claim only ever moves assignee_id away from NULL when an originally-
-- unassigned (claimable, or standing per NES-116) instance is claimed. A
-- fixed/round-robin instance's assignee_id is never reassigned by a claim —
-- only the already-assigned member can "claim" their own instance, which
-- records claimed_by/claimed_at but sets no claim_expires_at (no risk, since
-- the chore was already theirs). That is why claim_expires_at is only ever
-- non-NULL for a claim made on a previously-unassigned instance, and why the
-- sweep's revert (see below) is always "assignee_id back to NULL" — a
-- rotation instance's assignee_id never moves in the first place, so it
-- never needs reverting.
ALTER TABLE task_instance
    ADD COLUMN IF NOT EXISTS claimed_by       uuid,
    ADD COLUMN IF NOT EXISTS claimed_at       timestamptz,
    ADD COLUMN IF NOT EXISTS claim_expires_at timestamptz;

-- Tenant consistency for claimed_by, matching the assignee_id / completed_by
-- composite FK pattern from 00004_task_instance_member_tenant_fk.sql.
ALTER TABLE task_instance
    ADD CONSTRAINT task_instance_claimed_by_fk
        FOREIGN KEY (household_id, claimed_by)
        REFERENCES member (household_id, id) ON DELETE SET NULL (claimed_by);

-- claimed_by may only be set when a claim timestamp is on record — but NOT
-- the reverse. claimed_by is nulled by ON DELETE SET NULL (claimed_by) when
-- the claimant's member row is deleted, while claimed_at deliberately
-- survives that as "claimed by someone since deleted" so the row still
-- carries enough information for the sweep to revert it cleanly (NES-117).
-- This is the same directional pattern task_instance_completed_by_done
-- already uses for completed_by vs completed_at/status (00003_tasks.sql): a
-- symmetric CHECK here would make member deletion fail whenever the deleted
-- member held an active claim.
ALTER TABLE task_instance
    ADD CONSTRAINT task_instance_claim_consistency
        CHECK (claimed_by IS NULL OR claimed_at IS NOT NULL);

-- An expiry timer can only exist alongside a recorded claim timestamp. This
-- is anchored to claimed_at rather than claimed_by deliberately: claimed_at
-- has no ON DELETE action and so is never independently nulled out from under
-- this constraint the way claimed_by can be by a member deletion.
ALTER TABLE task_instance
    ADD CONSTRAINT task_instance_claim_expiry_requires_claim
        CHECK (claim_expires_at IS NULL OR claimed_at IS NOT NULL);

-- Supports the background sweep's "find expired claims" query
-- (TaskInstanceRepository.SweepExpiredClaims). Partial so the index stays
-- small — only currently-claimed-with-a-timer rows are ever indexed.
CREATE INDEX task_instance_claim_expires_idx
    ON task_instance (claim_expires_at)
    WHERE claim_expires_at IS NOT NULL;

-- point_ledger gains a nullable claim_started_at, populated only for
-- source_type = 'claim_expiry' rows. It carries the expired claim's
-- claimed_at so the idempotency index below can distinguish one claim window
-- from the next, independent claim on the same instance (an instance can be
-- claimed, expire, and be claimed again later).
ALTER TABLE point_ledger
    ADD COLUMN IF NOT EXISTS claim_started_at timestamptz;

-- Idempotency guard for the claim-expiry sweep, mirroring
-- point_ledger_task_completion_uniq: at most one penalty per (instance,
-- claim window). Keying on claim_started_at (not source_id alone) lets a
-- later, independent claim on the same instance be penalized again if it
-- also expires.
CREATE UNIQUE INDEX point_ledger_claim_expiry_uniq
    ON point_ledger (source_id, claim_started_at)
    WHERE source_type = 'claim_expiry';

-- +goose Down
DROP INDEX IF EXISTS point_ledger_claim_expiry_uniq;
ALTER TABLE point_ledger DROP COLUMN IF EXISTS claim_started_at;
DROP INDEX IF EXISTS task_instance_claim_expires_idx;
ALTER TABLE task_instance
    DROP CONSTRAINT IF EXISTS task_instance_claim_expiry_requires_claim,
    DROP CONSTRAINT IF EXISTS task_instance_claim_consistency,
    DROP CONSTRAINT IF EXISTS task_instance_claimed_by_fk,
    DROP COLUMN IF EXISTS claim_expires_at,
    DROP COLUMN IF EXISTS claimed_at,
    DROP COLUMN IF EXISTS claimed_by;
