-- +goose Up
-- Bound recurring_task.title (NES-172). Every user-entered text column in this
-- schema was previously unbounded: 90 text columns and no length CHECK on any
-- of them, so a 10,000-character title was accepted and stored in full.
--
-- The constraint is the backstop, not the primary guard: tasks/domain's
-- ValidateTitle rejects an over-length title first and returns a readable
-- message. This exists so a caller that bypasses the service layer still
-- cannot write one. The 200 here must match domain.MaxTitleLength.
--
-- char_length counts characters, not bytes, matching the domain's rune count —
-- octet_length would reject a shorter multi-byte title.

-- Existing rows come first: the very bug this migration closes means a
-- database can already hold an over-length title, and ADD CONSTRAINT validates
-- immediately, so the migration would fail on exactly the installations that
-- need it. Truncating to the new bound is the only forward-compatible choice —
-- left() counts characters, so a multi-byte title keeps whole runes.
UPDATE recurring_task
   SET title = left(title, 200)
 WHERE char_length(title) > 200;

ALTER TABLE recurring_task
    ADD CONSTRAINT recurring_task_title_length
    CHECK (char_length(title) BETWEEN 1 AND 200);

-- +goose Down
-- Dropping the constraint restores the old unbounded column; the truncation
-- above is not reversible, and this deliberately does not try to fake it.
ALTER TABLE recurring_task
    DROP CONSTRAINT IF EXISTS recurring_task_title_length;
