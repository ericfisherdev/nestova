-- +goose Up
-- Media schema (NES-7 / NES-71): the rotating photo album. An album is a named,
-- household-scoped slideshow with a rotation cadence and an optional filter
-- (jsonb) that narrows which of the household's photos it shows; photos store
-- only a storage_ref (the bytes live behind the PhotoStore port, NES-72), the
-- optional EXIF capture time, a caption, and the uploader. album_photo is the
-- ordered membership join. Tenant isolation follows the composite-FK pattern:
-- household_id on album and photo, both exposing UNIQUE (household_id, id).

CREATE TABLE album (
    id               uuid        PRIMARY KEY,
    household_id     uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    name             text        NOT NULL,
    -- Seconds each photo is shown before advancing; strictly positive, mirroring
    -- the RotationInterval value-object invariant.
    rotation_seconds integer     NOT NULL CHECK (rotation_seconds > 0),
    -- AlbumFilter (member ids / taken_at range); '{}' means "all household photos".
    filter           jsonb       NOT NULL DEFAULT '{}',
    created_at       timestamptz NOT NULL DEFAULT now(),
    -- Mirrors the other tables so a future composite FK on (household_id, id) is possible.
    CONSTRAINT album_household_id_uniq UNIQUE (household_id, id)
);

CREATE INDEX album_household_idx ON album (household_id);

CREATE TABLE photo (
    id           uuid        PRIMARY KEY,
    household_id uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    -- Opaque key into the PhotoStore; the bytes are never stored in the DB.
    -- Non-empty mirrors Photo.Validate, which rejects an empty StorageRef.
    storage_ref  text        NOT NULL CHECK (storage_ref !~ '^[[:space:]]*$'),
    -- EXIF capture time (UTC); NULL when the upload carried no EXIF date.
    taken_at     timestamptz,
    caption      text        NOT NULL DEFAULT '',
    -- The uploader; nulled (not deleted) when the member is removed so the photo
    -- and its album memberships survive.
    uploaded_by  uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT photo_household_id_uniq UNIQUE (household_id, id),
    -- Tenant consistency: an uploader must belong to the photo's household.
    -- uploaded_by is nullable; MATCH SIMPLE skips the check when it is NULL.
    -- ON DELETE SET NULL (uploaded_by) nulls only the uploader column (not
    -- household_id, which is NOT NULL), the same column-specific pattern the
    -- subscription payer FK uses in 00014.
    CONSTRAINT photo_uploader_fk FOREIGN KEY (household_id, uploaded_by)
        REFERENCES member (household_id, id) ON DELETE SET NULL (uploaded_by)
);

CREATE INDEX photo_household_idx ON photo (household_id);

CREATE TABLE album_photo (
    -- Carried so the composite FKs below can bind the album and the photo to the
    -- SAME household, making a cross-household link impossible at the schema level.
    household_id uuid    NOT NULL,
    album_id     uuid    NOT NULL,
    photo_id     uuid    NOT NULL,
    -- Display order within the album; gap-free and unique per album. The reorder
    -- path updates positions inside a single transaction.
    position     integer NOT NULL,
    PRIMARY KEY (album_id, photo_id),
    CONSTRAINT album_photo_position_uniq UNIQUE (album_id, position),
    -- Tenant consistency: the album and the photo must belong to household_id.
    -- Composite FKs to the (household_id, id) unique keys mean an album_photo row
    -- can only link an album and a photo from the same household; deleting either
    -- parent (or the household) cascades the membership away.
    CONSTRAINT album_photo_album_fk FOREIGN KEY (household_id, album_id)
        REFERENCES album (household_id, id) ON DELETE CASCADE,
    CONSTRAINT album_photo_photo_fk FOREIGN KEY (household_id, photo_id)
        REFERENCES photo (household_id, id) ON DELETE CASCADE
);

-- Serves ListByAlbumOrdered (ORDER BY position) and the album-side FK.
CREATE INDEX album_photo_album_position_idx ON album_photo (album_id, position);
-- Supports the photo-side composite FK's cascade (delete a photo -> remove its memberships).
CREATE INDEX album_photo_photo_idx ON album_photo (household_id, photo_id);

-- +goose Down
-- Drop in reverse dependency order so the FKs are not violated.
DROP TABLE IF EXISTS album_photo;
DROP TABLE IF EXISTS photo;
DROP TABLE IF EXISTS album;
