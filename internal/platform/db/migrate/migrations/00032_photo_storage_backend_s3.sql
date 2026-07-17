-- +goose Up
-- S3 PhotoStore adapter (NES-132): widens the storage_backend CHECK on both
-- photo (00028) and task_instance_photo (00029) to admit 's3' alongside
-- 'local' — the PR-97 decision that this constraint widens "alongside the
-- S3 adapter" landing, not ahead of it. Both tables are widened together,
-- in one migration, because they mirror each other's storage_backend column
-- exactly (00029's own comment: "Mirrors photo.storage_backend (00028)") and
-- the NES-132 reaper walks both photo classes under one bucket — leaving
-- one table's CHECK narrower than the other would be an inconsistency with
-- no purpose.
--
-- This migration does NOT change the column's DEFAULT ('local') on either
-- table. The backend is selected once, app-wide, via MEDIA_STORAGE_BACKEND
-- (config.go), and the repositories stamp the active backend onto every
-- row they create (NES-132), so new rows always record where their bytes
-- actually live. The DEFAULT continues to describe every pre-NES-132 row
-- correctly; retroactive backfill of historical rows is left to NES-133's
-- migrate/verify tooling — the deliberate, auditable place for that.
ALTER TABLE photo DROP CONSTRAINT photo_storage_backend_check;
ALTER TABLE photo
    ADD CONSTRAINT photo_storage_backend_check
        CHECK (storage_backend IN ('local', 's3'));

ALTER TABLE task_instance_photo DROP CONSTRAINT task_instance_photo_storage_backend_check;
ALTER TABLE task_instance_photo
    ADD CONSTRAINT task_instance_photo_storage_backend_check
        CHECK (storage_backend IN ('local', 's3'));

-- +goose Down
-- Rolling back to a local-only schema: any 's3'-stamped rows are re-marked
-- 'local' before the CHECK narrows — the pre-NES-132 application can only
-- serve local refs, and leaving 's3' values in place would make this Down
-- fail outright (constraint violation), the same down-path failure class
-- migration 00018 hit. Operators rolling back an S3 deployment must run
-- the NES-133 storage migration back to local storage FIRST; this
-- re-stamp only keeps the schema rollback itself executable.
UPDATE photo SET storage_backend = 'local' WHERE storage_backend = 's3';
UPDATE task_instance_photo SET storage_backend = 'local' WHERE storage_backend = 's3';

ALTER TABLE photo DROP CONSTRAINT photo_storage_backend_check;
ALTER TABLE photo
    ADD CONSTRAINT photo_storage_backend_check
        CHECK (storage_backend IN ('local'));

ALTER TABLE task_instance_photo DROP CONSTRAINT task_instance_photo_storage_backend_check;
ALTER TABLE task_instance_photo
    ADD CONSTRAINT task_instance_photo_storage_backend_check
        CHECK (storage_backend IN ('local'));
