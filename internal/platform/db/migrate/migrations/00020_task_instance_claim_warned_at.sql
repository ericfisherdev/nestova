-- +goose NO TRANSACTION
-- Claim-expiry warning tracking (NES-118): claim_warned_at records when the
-- "claim expiring soon" notification was emitted for the CURRENT claim
-- window, mirroring reminded_at's role for due-soon reminders (00006). NULL
-- means no warning has been sent yet for the active claim.
--
-- NO TRANSACTION because the partial index is built CONCURRENTLY, matching
-- 00006's rationale: CONCURRENTLY cannot run inside a transaction, so this
-- keeps the migration from blocking writes on task_instance during deploy.
--
-- The index is DROPped before being (re)created rather than guarded with
-- CREATE INDEX CONCURRENTLY IF NOT EXISTS, for the same reason 00006 does:
-- an interrupted CONCURRENTLY build leaves behind an INVALID index that IF
-- NOT EXISTS would see and skip, permanently keeping the unusable index.

-- +goose Up
ALTER TABLE task_instance ADD COLUMN IF NOT EXISTS claim_warned_at timestamptz;

-- Partial index backing TaskInstanceRepository.ClaimWarnings: only
-- currently-claimed, not-yet-warned rows with a real expiry are ever
-- indexed, mirroring task_instance_claim_expires_idx's shape.
DROP INDEX IF EXISTS task_instance_claim_warn_idx;
CREATE INDEX CONCURRENTLY task_instance_claim_warn_idx
    ON task_instance (claim_expires_at)
    WHERE claim_expires_at IS NOT NULL AND claim_warned_at IS NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS task_instance_claim_warn_idx;
ALTER TABLE task_instance DROP COLUMN IF EXISTS claim_warned_at;
