-- +goose Up
-- As-needed cadence support (NES-116): a chore with cadence "as needed" is
-- never scheduled by the recurrence engine. Instead it has exactly one open
-- "standing" instance at all times, with no due date, that reappears
-- immediately on completion. due_on becomes nullable to represent that, and
-- kind distinguishes a standing instance from a normal ahead-of-time
-- materialised occurrence (kind = 'scheduled', the default so every
-- pre-existing row is classified without a backfill).
--
-- The task_instance_task_due_uniq unique constraint on (recurring_task_id,
-- due_on) is unaffected: Postgres treats each NULL due_on as distinct, so a
-- task's completed standing instances accumulate as separate history rows,
-- exactly like scheduled instances do.

ALTER TABLE task_instance ALTER COLUMN due_on DROP NOT NULL;

ALTER TABLE task_instance ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'scheduled'
    CHECK (kind IN ('scheduled', 'standing'));

-- A standing instance carries no due date and a scheduled instance always
-- does; tying due_on's nullability to kind (rather than allowing an
-- unclassified NULL) keeps the two columns from drifting out of sync.
ALTER TABLE task_instance ADD CONSTRAINT task_instance_standing_no_due_on
    CHECK ((kind = 'standing') = (due_on IS NULL));

-- At most one open standing instance per as-needed task. The application
-- maintains this invariant transactionally (seed on create, respawn on
-- completion); the index is the schema-level safety net against a future
-- call site or retry double-inserting one.
CREATE UNIQUE INDEX task_instance_standing_open_uniq
    ON task_instance (recurring_task_id)
    WHERE kind = 'standing' AND status = 'pending';

-- +goose Down
DROP INDEX IF EXISTS task_instance_standing_open_uniq;
-- Standing instances cannot exist in the old schema (their due_on is NULL,
-- which the restored NOT NULL forbids), so rolling back removes them.
DELETE FROM task_instance WHERE kind = 'standing';
ALTER TABLE task_instance DROP CONSTRAINT IF EXISTS task_instance_standing_no_due_on;
ALTER TABLE task_instance DROP COLUMN IF EXISTS kind;
ALTER TABLE task_instance ALTER COLUMN due_on SET NOT NULL;
