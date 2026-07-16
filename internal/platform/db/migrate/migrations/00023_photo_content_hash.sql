-- +goose Up
-- Bulk-upload hardening (NES-123): content-hash dedup so re-dropping the same
-- photo is a no-op instead of a second row. content_sha256 is the sha256 the
-- upload path already computes to build the content-addressed storage_ref
-- (see LocalPhotoStore.Put); size_bytes/content_type are the other
-- server-verified upload facts (never the client's declared values).
--
-- content_sha256 is nullable — existing rows predate this column and their
-- bytes are not re-read here — and the uniqueness guard below is a PARTIAL
-- index (WHERE content_sha256 IS NOT NULL) so those legacy NULL rows never
-- collide with each other or block a new upload. Every new upload always
-- populates content_sha256, so from this migration forward the guard is
-- fully enforced for new photos.
-- size_bytes/content_type get a temporary DEFAULT purely so this ADD COLUMN
-- can populate every pre-existing row without a table rewrite error; the
-- DEFAULT is dropped below once every row (old and new) has a concrete
-- value, so a future INSERT that omits either column fails fast instead of
-- silently persisting a placeholder 0/''.
ALTER TABLE photo
    ADD COLUMN content_sha256 text,
    ADD COLUMN size_bytes     bigint NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    ADD COLUMN content_type   text   NOT NULL DEFAULT '';

-- content_sha256 must be a full lowercase hex sha256 (the shape
-- PhotoStore.Put always produces, mirrored by domain.Photo.Validate's
-- contentHashPattern) whenever it is set; NULL (unknown/legacy) is exempt and
-- handled by the partial index below.
ALTER TABLE photo ADD CONSTRAINT photo_content_sha256_format
    CHECK (content_sha256 IS NULL OR content_sha256 ~ '^[0-9a-f]{64}$');

-- Backfill: storage_ref is already content-addressed as
-- "<household_id>/<first-2-hex-chars>/<sha256>.<ext>" (LocalPhotoStore.Put),
-- so the hash is recoverable from existing rows without touching the stored
-- bytes. Content-addressing dedupes bytes on disk but, before this ticket,
-- NOT photo rows — so two existing rows in the same household can already
-- share a storage_ref (the same image uploaded twice). Backfilling every
-- such row would violate the unique index below, so only the earliest row
-- per (household_id, hash) is backfilled; any other pre-existing duplicate
-- keeps content_sha256 NULL and simply does not participate retroactively in
-- dedup (nothing is merged or deleted).
WITH extracted AS (
    SELECT id, household_id, storage_ref,
           split_part(split_part(storage_ref, '/', 3), '.', 1) AS hash,
           row_number() OVER (
               PARTITION BY household_id, split_part(split_part(storage_ref, '/', 3), '.', 1)
               ORDER BY created_at, id
           ) AS rn
    FROM photo
    WHERE storage_ref ~ '^[^/]+/[0-9a-f]{2}/[0-9a-f]{64}\.[a-z0-9]+$'
)
UPDATE photo
SET content_sha256 = extracted.hash
FROM extracted
WHERE photo.id = extracted.id AND extracted.rn = 1;

-- Every row (old and new/backfilled) now has a concrete size_bytes/
-- content_type; drop the temporary DEFAULT so a future INSERT must supply
-- real values instead of silently getting 0/''.
ALTER TABLE photo
    ALTER COLUMN size_bytes   DROP DEFAULT,
    ALTER COLUMN content_type DROP DEFAULT;

-- At most one photo per household may carry a given content hash; NULLs
-- (legacy rows, and anything backfill skipped) are excluded so they never
-- collide.
CREATE UNIQUE INDEX photo_household_content_hash_uniq
    ON photo (household_id, content_sha256)
    WHERE content_sha256 IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS photo_household_content_hash_uniq;
ALTER TABLE photo DROP CONSTRAINT IF EXISTS photo_content_sha256_format;
ALTER TABLE photo
    DROP COLUMN IF EXISTS content_sha256,
    DROP COLUMN IF EXISTS size_bytes,
    DROP COLUMN IF EXISTS content_type;
