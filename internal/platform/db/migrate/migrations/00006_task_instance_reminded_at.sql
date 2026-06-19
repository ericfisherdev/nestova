-- +goose NO TRANSACTION
-- reminded_at records when a due-soon reminder was emitted for a pending
-- instance. NULL means no due-soon reminder has been sent yet. It is used by
-- ClaimDueSoonReminders (NES-34) to emit the reminder exactly once: the claim
-- CTE atomically marks reminded_at = now() and returns the row, so a second
-- call never sees reminded_at IS NULL for an already-reminded instance.
--
-- NO TRANSACTION because the partial index is built CONCURRENTLY (Postgres
-- forbids CONCURRENTLY inside a transaction) so it does not block writes on
-- task_instance during deploy; the IF [NOT] EXISTS clauses keep the migration
-- re-runnable if a CONCURRENTLY build is interrupted.

-- +goose Up
ALTER TABLE task_instance ADD COLUMN IF NOT EXISTS reminded_at timestamptz;

-- Partial index backing ClaimDueSoonReminders: only pending, un-reminded rows
-- are eligible for a due-soon sweep, so the index stays small as instances
-- complete or are reminded.
CREATE INDEX CONCURRENTLY IF NOT EXISTS task_instance_due_soon_idx
    ON task_instance (due_on) WHERE status = 'pending' AND reminded_at IS NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS task_instance_due_soon_idx;
ALTER TABLE task_instance DROP COLUMN IF EXISTS reminded_at;
