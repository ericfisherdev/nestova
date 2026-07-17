-- +goose Up
-- PhotoStore port extension (NES-131): storage_backend records which
-- PhotoStore implementation persisted a photo's bytes. Only 'local'
-- (LocalPhotoStore) exists today — the app selects a single backend once at
-- startup via config (cmd/server), so every row this migration touches, and
-- every row a new upload creates until NES-132 ships an object-store
-- adapter, is 'local'. The DEFAULT is intentionally permanent (not dropped
-- like 00023's temporary size_bytes/content_type defaults): there genuinely
-- is only one legitimate value for this column right now, so relying on it
-- for every existing and future row is correct, not a placeholder.
--
-- This migration does NOT touch storage_ref. NES-131 also introduces
-- class-namespaced, content-addressed keys for new uploads
-- (households/<household_id>/<class-prefix>/<aa>/<hash>.<ext>, see
-- LocalPhotoStore), but existing rows keep their pre-existing
-- <household_id>/<aa>/<hash>.<ext> refs unrewritten: LocalPhotoStore treats
-- storage_ref as an opaque relative path in Open/Delete and never assumes
-- its shape, so a legacy ref keeps resolving to its file exactly as before.
-- Rewriting refs (and physically relocating the underlying files, which live
-- outside the database) is out of scope here — NES-133's planned
-- migrate/verify tooling is the deliberate, auditable place for that if it
-- is ever done.
ALTER TABLE photo
    ADD COLUMN storage_backend text NOT NULL DEFAULT 'local'
        CHECK (storage_backend IN ('local', 's3'));

-- +goose Down
ALTER TABLE photo DROP COLUMN IF EXISTS storage_backend;
