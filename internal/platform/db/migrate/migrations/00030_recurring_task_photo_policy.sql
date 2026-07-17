-- +goose Up
-- Chore photo policy (NES-120): a recurring task may require proof photos
-- before an instance can be completed. The policy lives on recurring_task,
-- NOT on task_instance — every generated instance reads its parent task's
-- CURRENT policy at completion time (a join, enforced in
-- TaskService.CompleteInstance, not a per-instance column) rather than
-- having the policy copied onto each instance at generation time. This is a
-- deliberate design choice, not an oversight:
--   - No backfill is needed for already-materialised instances (there is no
--     per-instance column to backfill).
--   - Editing a task's policy takes effect immediately for every OPEN
--     instance (pending/overdue), matching how editing points or category
--     already would if this codebase supported task edits — there is no
--     stale, instance-pinned copy of the policy to fall out of sync.
--   - The generator (internal/tasks/app/generator.go) needs NO changes: it
--     never reads or writes photo_policy, since task_instance carries none.
--
-- 'none' is the DEFAULT (and every recurring task created before NES-120
-- implicitly has it — see RecurringTask.PhotoPolicy's Go doc for how the
-- application layer treats the zero Go value the same way NES-116 already
-- treats TaskInstance.Kind's zero value): completion behaves exactly as it
-- did before this migration for every existing task.
ALTER TABLE recurring_task
    ADD COLUMN photo_policy text NOT NULL DEFAULT 'none'
        CHECK (photo_policy IN ('none', 'after_only', 'before_after'));

-- +goose Down
ALTER TABLE recurring_task DROP COLUMN IF EXISTS photo_policy;
