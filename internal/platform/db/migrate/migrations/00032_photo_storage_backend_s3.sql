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
-- Rolling back to a local-only schema requires that NO row is currently
-- stamped 's3'. An earlier version of this migration silently re-stamped
-- any 's3' row 'local' here so the narrowed CHECK below would never fail —
-- deliberately REJECTED in favor of the executable precondition below,
-- because that re-stamp erases the only record of where an s3-backed
-- row's bytes actually live: the pre-NES-132 application reading a
-- "local" ref for an object that only ever existed in S3 would find
-- nothing there — every one of those photos silently breaks, with no
-- error and no trace of what happened. Aborting loudly here, before any
-- damage, is the only option that keeps this Down honest about what it can
-- and cannot safely do; the alternative failure mode this replaces (a bare
-- CHECK-constraint violation, the same down-path failure class migration
-- 00018 hit) at least does not corrupt data, but gives no guidance either.
--
-- Operators rolling back an S3 deployment must run NES-133's planned
-- storage migrate-back procedure FIRST — copying every 's3'-stamped row's
-- bytes to local storage and re-stamping the row 'local' THROUGH THE
-- APPLICATION, where the object copy actually happens — before running
-- this Down.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM photo WHERE storage_backend = 's3')
        OR EXISTS (SELECT 1 FROM task_instance_photo WHERE storage_backend = 's3')
    THEN
        RAISE EXCEPTION 'cannot roll back migration 00032: photo and/or task_instance_photo rows are still stamped ''s3''. Run NES-133''s storage migrate-back procedure to move those rows'' bytes to local storage and re-stamp them ''local'' BEFORE rolling back this migration.';
    END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE photo DROP CONSTRAINT photo_storage_backend_check;
ALTER TABLE photo
    ADD CONSTRAINT photo_storage_backend_check
        CHECK (storage_backend IN ('local'));

ALTER TABLE task_instance_photo DROP CONSTRAINT task_instance_photo_storage_backend_check;
ALTER TABLE task_instance_photo
    ADD CONSTRAINT task_instance_photo_storage_backend_check
        CHECK (storage_backend IN ('local'));
