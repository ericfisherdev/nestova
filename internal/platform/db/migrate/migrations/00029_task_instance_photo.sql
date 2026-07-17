-- +goose Up
-- Chore proof photos (NES-119): before/after photos attached to a task
-- instance as evidence of work, EXIF-timestamp-validated by the application
-- layer before a row is ever written here (taken_at is therefore NOT NULL —
-- unlike photo.taken_at, which is nullable because an album upload may carry
-- no EXIF date at all; a chore-proof upload without one is rejected upstream
-- with domain.ErrPhotoMissingTimestamp and never reaches this table).
--
-- task_instance_photo is a DELIBERATELY SEPARATE table from photo (00017),
-- not a shared table with a discriminator column: class separation between
-- an album/gallery photo and a chore-proof photo is structural (mirroring
-- the storage-key class-namespacing principle NES-131 established for
-- PhotoStore), so an album/gallery query can never surface a chore photo —
-- there is no shared table or column filter to ever get wrong. The two
-- tables intentionally duplicate several columns (storage_ref,
-- storage_backend, content_sha256, size_bytes, content_type, taken_at,
-- uploaded_by) rather than share a base table, for the same reason.
--
-- content_sha256/size_bytes/content_type are NOT NULL from the start (unlike
-- photo's 00023 migration, which had to backfill pre-existing rows): this is
-- a brand-new table with no legacy data, so every row is written by the
-- NES-119 upload path, which always has these upload facts from
-- PhotoStore.Put.
--
-- No content-hash uniqueness constraint (unlike photo_household_content_hash_
-- uniq, 00023): a chore-proof upload is a distinct proof-of-work event, not a
-- media-library item to deduplicate — two different task instances (or two
-- legitimate re-shoots of the same instance) coincidentally producing
-- byte-identical bytes is not an error case worth guarding against, and
-- de-duplicating would destroy the audit trail of "a photo was taken at this
-- moment" the freshness-window check depends on.
CREATE TABLE task_instance_photo (
    id                uuid        PRIMARY KEY,
    household_id      uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    task_instance_id  uuid        NOT NULL,
    kind              text        NOT NULL CHECK (kind IN ('before', 'after')),
    -- Opaque key into the PhotoStore, class-namespaced under
    -- domain.PhotoClassChoreProof (NES-131) so chore-proof bytes can never
    -- collide with, or be resolved as, an album photo's bytes even if the
    -- content happens to be identical. Non-empty mirrors
    -- TaskInstancePhoto.Validate.
    storage_ref       text        NOT NULL CHECK (storage_ref !~ '^[[:space:]]*$'),
    -- Mirrors photo.storage_backend (00028): only 'local' exists today.
    storage_backend   text        NOT NULL DEFAULT 'local' CHECK (storage_backend IN ('local')),
    -- The sha256 PhotoStore.Put computed while streaming the (already
    -- EXIF-scrubbed — see ChoreProofPhotoService.Upload) bytes to storage.
    content_sha256    text        NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    size_bytes        bigint      NOT NULL CHECK (size_bytes > 0),
    content_type      text        NOT NULL,
    -- EXIF capture time (UTC). NOT NULL: see the migration's top comment.
    taken_at          timestamptz NOT NULL,
    -- The uploader; nulled (not deleted) when the member is removed so the
    -- photo survives, mirroring photo_uploader_fk (00017).
    uploaded_by       uuid,
    uploaded_at       timestamptz NOT NULL DEFAULT now(),
    -- Tenant consistency: the instance and its parent task_instance share a
    -- household. task_instance_household_id_id_uniq (added in 00021 for
    -- chore_trade) is the composite unique key this FK targets.
    CONSTRAINT task_instance_photo_instance_fk FOREIGN KEY (household_id, task_instance_id)
        REFERENCES task_instance (household_id, id) ON DELETE CASCADE,
    -- Tenant consistency for the uploader, mirroring photo_uploader_fk
    -- (00017): uploaded_by is nullable, so MATCH SIMPLE skips the check when
    -- it is NULL, and ON DELETE SET NULL (uploaded_by) nulls only that
    -- column when the member is removed.
    CONSTRAINT task_instance_photo_uploader_fk FOREIGN KEY (household_id, uploaded_by)
        REFERENCES member (household_id, id) ON DELETE SET NULL (uploaded_by)
);

-- Serves ChoreProofPhotoService's before/after ordering check (the most
-- recent photo of a given kind for an instance) and a future NES-120 detail
-- view (every photo for an instance, ordered by capture time).
CREATE INDEX task_instance_photo_instance_kind_idx
    ON task_instance_photo (household_id, task_instance_id, kind, taken_at);

-- +goose Down
DROP TABLE IF EXISTS task_instance_photo;
