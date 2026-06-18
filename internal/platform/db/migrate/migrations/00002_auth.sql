-- +goose Up
-- citext provides case-insensitive text type used for email storage so lookups
-- are case-insensitive without explicit lower() calls.
CREATE EXTENSION IF NOT EXISTS citext;

-- Add optional login credentials to member. Both columns are nullable so
-- members without login are unaffected; the CHECK keeps them consistent — a
-- member either has both an email and a password hash, or neither.
ALTER TABLE member
    ADD COLUMN email         citext UNIQUE,
    ADD COLUMN password_hash text,
    ADD CONSTRAINT member_credentials_complete CHECK (
        (email IS NULL AND password_hash IS NULL)
        OR (email IS NOT NULL AND password_hash IS NOT NULL)
    );

-- SCS server-side session store. The schema matches the exact layout required
-- by github.com/alexedwards/scs/pgxstore.
CREATE TABLE sessions (
    token  text        PRIMARY KEY,
    data   bytea       NOT NULL,
    expiry timestamptz NOT NULL
);

CREATE INDEX sessions_expiry_idx ON sessions (expiry);

-- +goose Down
DROP INDEX  IF EXISTS sessions_expiry_idx;
DROP TABLE  IF EXISTS sessions;

ALTER TABLE member
    DROP CONSTRAINT IF EXISTS member_credentials_complete,
    DROP COLUMN IF EXISTS password_hash,
    DROP COLUMN IF EXISTS email;

-- Intentionally NOT dropping the citext extension: it may be used by other
-- objects and removing extensions requires superuser privileges in many
-- hosted environments. If a clean teardown is needed, drop it manually.
