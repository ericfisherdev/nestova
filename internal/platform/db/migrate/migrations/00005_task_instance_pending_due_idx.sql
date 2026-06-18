-- +goose NO TRANSACTION
-- The global overdue sweep (TaskInstanceRepository.MarkPendingOverdueAll) filters
-- on (status='pending', due_on) with no household predicate, which the
-- household-leading task_instance_due_idx cannot serve efficiently. This partial
-- index covers only pending rows (so it stays small as instances complete) and
-- lets the sweep avoid a full-table scan. Created CONCURRENTLY (hence
-- NO TRANSACTION) so the migration does not block writes on task_instance.

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS task_instance_pending_due_idx
    ON task_instance (due_on) WHERE status = 'pending';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS task_instance_pending_due_idx;
