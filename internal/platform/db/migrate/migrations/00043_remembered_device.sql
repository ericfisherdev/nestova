-- +goose Up
-- Server-side "remember this device" tokens (NES-200). The cookie used to be a
-- stateless HMAC over member id and expiry, so a copy of it skipped the login
-- MFA prompt for its whole lifetime and nothing could revoke it. It is now a
-- random opaque token; only its SHA-256 is stored here, so a database read
-- cannot be replayed as a cookie, and revoking a member's devices is a DELETE.
--
-- The table lives in nestova, not identity: the nestova_remember cookie is
-- app-scoped, so the record is too, and no identity migration is needed. The
-- FK cascades so a removed member takes their remembered devices with them.
--
-- A device is only as live as the MFA enrollment it bypasses, so it also
-- references identity.member_mfa. Deleting the enrollment (disenrol or owner
-- reset) removes the member's devices in the same statement, and an insert
-- racing that delete fails the FK instead of surviving it.
--
-- token_hash is UNIQUE (the lookup key). The 32-byte check pins the SHA-256
-- length the domain always writes; user_agent is display-only metadata, bounded
-- to match the domain's truncation.
CREATE TABLE remembered_device (
    id           uuid        PRIMARY KEY,
    member_id    uuid        NOT NULL REFERENCES identity.member (id) ON DELETE CASCADE,
    token_hash   bytea       NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    user_agent   text        NOT NULL DEFAULT '' CHECK (char_length(user_agent) <= 255),
    created_at   timestamptz NOT NULL,
    expires_at   timestamptz NOT NULL,
    last_used_at timestamptz NOT NULL,
    CONSTRAINT remembered_device_expiry_chk CHECK (expires_at > created_at),
    CONSTRAINT remembered_device_enrollment_fkey FOREIGN KEY (member_id)
        REFERENCES identity.member_mfa (member_id) ON DELETE CASCADE
);

-- Supports RevokeAllForMember and the per-member expired-row sweep.
CREATE INDEX remembered_device_member_idx ON remembered_device (member_id);

-- +goose Down
DROP TABLE IF EXISTS remembered_device;
