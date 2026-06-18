-- +goose Up
-- Tasks schema: recurring task templates, round-robin rotation pools, and
-- materialized task instances. The cadence column is stored as jsonb so the
-- application layer can marshal/unmarshal household.Cadence without a custom
-- pgx codec (see NES-29 for the adapter; NES-30 for the use-cases).

CREATE TABLE recurring_task (
    id              uuid        PRIMARY KEY,
    household_id    uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    title           text        NOT NULL,
    category        text        NOT NULL CHECK (category IN ('chore', 'maintenance')),
    cadence         jsonb       NOT NULL,
    rotation_policy text        NOT NULL CHECK (rotation_policy IN ('fixed', 'round_robin', 'claimable')),
    points          int         NOT NULL DEFAULT 0  CHECK (points >= 0),
    lead_time_days  int         NOT NULL DEFAULT 0  CHECK (lead_time_days >= 0),
    active          boolean     NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- Partial index backing RecurringTaskRepository.ListActive (active tasks for a
-- household). Indexing only active rows keeps it small as tasks are retired.
CREATE INDEX recurring_task_household_active_idx ON recurring_task (household_id) WHERE active = true;

-- Round-robin pool for a recurring_task. Position is the zero-based slot in the
-- rotation order; the application layer enforces that members belong to the
-- task's household (no DB-level composite FK here — the member table's
-- household_id is accessible via the recurring_task join if needed).
CREATE TABLE rotation_member (
    recurring_task_id uuid NOT NULL REFERENCES recurring_task (id) ON DELETE CASCADE,
    member_id         uuid NOT NULL REFERENCES member (id) ON DELETE CASCADE,
    position          int  NOT NULL CHECK (position >= 0),
    PRIMARY KEY (recurring_task_id, member_id),
    -- One member per slot keeps the round-robin order well-defined. The unique
    -- constraint's implicit index also serves the ordered "members by position"
    -- query, so no separate index is needed.
    CONSTRAINT rotation_member_task_position_uniq UNIQUE (recurring_task_id, position)
);

-- Materialized instance of a recurring_task. Each row represents one scheduled
-- occurrence. The unique constraint (recurring_task_id, due_on) backs the
-- idempotent-insert sentinel ErrDuplicateInstance in the domain (NES-29).
CREATE TABLE task_instance (
    id                uuid        PRIMARY KEY,
    recurring_task_id uuid        NOT NULL REFERENCES recurring_task (id) ON DELETE CASCADE,
    household_id      uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    -- assignee_id is NULL for claimable/unassigned instances.
    assignee_id       uuid                 REFERENCES member (id) ON DELETE SET NULL,
    due_on            date        NOT NULL,
    status            text        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'done', 'skipped', 'overdue')),
    -- completed_at / completed_by are populated when status transitions to 'done'.
    completed_at      timestamptz,
    completed_by      uuid                 REFERENCES member (id) ON DELETE SET NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    -- updated_at is refreshed on every status transition for auditability; the
    -- NES-29 adapter maintains it on Claim/Complete/Skip/MarkPendingOverdue.
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT task_instance_task_due_uniq UNIQUE (recurring_task_id, due_on),
    -- A 'done' instance has a completion time and vice versa. Only completed_at
    -- is constrained (not completed_by) because completed_by is SET NULL when the
    -- completing member is deleted, which must not retroactively violate this.
    CONSTRAINT task_instance_done_completed_at CHECK ((status = 'done') = (completed_at IS NOT NULL))
);

-- Supports the scheduler's "list pending/overdue instances for a household
-- in a date window" query and the overdue-sweep (MarkPendingOverdue).
CREATE INDEX task_instance_due_idx ON task_instance (household_id, status, due_on);

-- +goose Down
DROP TABLE IF EXISTS task_instance;
DROP TABLE IF EXISTS rotation_member;
DROP TABLE IF EXISTS recurring_task;
