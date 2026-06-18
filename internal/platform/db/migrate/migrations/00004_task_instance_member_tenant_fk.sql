-- +goose Up
-- Tenant-scope task_instance's member references. The 00003 schema referenced
-- member(id) alone, which would allow assigning (or marking complete-by) a
-- member from a different household. Replace those with composite FKs to
-- member(household_id, id) so a cross-household member id cannot be stored,
-- matching the rotation_member pattern. ON DELETE SET NULL (column) (Postgres
-- 15+) nulls only the member column on member deletion, leaving the NOT NULL
-- household_id intact. With MATCH SIMPLE, a NULL member column (claimable or
-- not-yet-completed instance) skips the FK check, so unassigned rows are fine.
ALTER TABLE task_instance
    DROP CONSTRAINT task_instance_assignee_id_fkey,
    DROP CONSTRAINT task_instance_completed_by_fkey,
    ADD CONSTRAINT task_instance_assignee_fk
        FOREIGN KEY (household_id, assignee_id)
        REFERENCES member (household_id, id) ON DELETE SET NULL (assignee_id),
    ADD CONSTRAINT task_instance_completed_by_fk
        FOREIGN KEY (household_id, completed_by)
        REFERENCES member (household_id, id) ON DELETE SET NULL (completed_by);

-- +goose Down
ALTER TABLE task_instance
    DROP CONSTRAINT task_instance_assignee_fk,
    DROP CONSTRAINT task_instance_completed_by_fk,
    ADD CONSTRAINT task_instance_assignee_id_fkey
        FOREIGN KEY (assignee_id) REFERENCES member (id) ON DELETE SET NULL,
    ADD CONSTRAINT task_instance_completed_by_fkey
        FOREIGN KEY (completed_by) REFERENCES member (id) ON DELETE SET NULL;
