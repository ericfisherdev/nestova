-- +goose Up
-- Kiosk device auth (NES-128): the wall-mounted touchscreen authenticates as a
-- DEVICE, never as a member, so a LAN guest cannot browse family data by
-- walking up to the kiosk. A parent provisions one kiosk_device per household
-- from the settings page; the long-lived bearer token is generated only at
-- redemption time (see kiosk_activation_code below) and stored on the device
-- as an HttpOnly cookie — only its hash is persisted here, and the settings
-- page itself never displays it (that would leak a long-lived credential via
-- browser history, access logs, or the Referer header).
--
-- token_hash is SHA-256 (not argon2id, unlike credential.password_hash): the
-- kiosk token is a 256-bit value drawn from crypto/rand, not a human-chosen
-- password, so it already has full entropy and there is no dictionary/rainbow
-- table risk to defend against with KDF stretching — a single fast hash is
-- the right tool for comparing a high-entropy random secret, and avoids
-- paying argon2's deliberate CPU/memory cost on every kiosk page load.
--
-- revoked_at (nullable) marks a token invalidated by an explicit parent
-- revoke or superseded by a fresh redemption; the row is kept (not deleted)
-- as an audit trail of which devices were ever issued a token.
CREATE TABLE kiosk_device (
    id           uuid        PRIMARY KEY,
    household_id uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    token_hash   text        NOT NULL CHECK (btrim(token_hash) <> ''),
    name         text        NOT NULL CHECK (btrim(name) <> ''),
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz,
    CONSTRAINT kiosk_device_token_hash_uniq UNIQUE (token_hash)
);

-- Supports KioskDeviceRepository.GetByTokenHash — the per-request device
-- lookup on every /kiosk/* page load, keyed by the token's hash alone (the
-- uniqueness constraint above already gives this an index; this comment
-- documents that the constraint is deliberately serving double duty rather
-- than a missing index).

-- Supports KioskDeviceRepository.ListByHousehold (the settings page's status
-- display): household_id equality, created_at for a stable newest-first order.
CREATE INDEX kiosk_device_household_idx ON kiosk_device (household_id, created_at);

-- Kiosk activation codes: the provisioning credential a parent actually sees.
-- Settings generates a short-lived (15-minute), single-use code — never the
-- long-lived kiosk_device token — and the kiosk device redeems it at
-- GET/POST /kiosk/activate. Redemption (ActivationCodeRepository.Redeem) is
-- one atomic transaction: mark the code used, revoke the household's
-- previously active device (if any), and insert the newly minted device —
-- so a failure at any step leaves the code unused and the previous device
-- (if any) still active, never a half-provisioned state.
--
-- code_hash is SHA-256 for the same reason token_hash is: the code is
-- crypto/rand-derived, not human-chosen. Its shorter length (10 characters
-- from a 32-symbol alphabet, ~50 bits) trades some entropy for being
-- hand-typeable, which is safe specifically because the code is single-use
-- and expires in 15 minutes — the exposure window a brute-force or leaked-hash
-- attack would need is far too short to matter at that entropy.
CREATE TABLE kiosk_activation_code (
    id           uuid        PRIMARY KEY,
    household_id uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    code_hash    text        NOT NULL CHECK (btrim(code_hash) <> ''),
    name         text        NOT NULL CHECK (btrim(name) <> ''),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    used_at      timestamptz,
    CONSTRAINT kiosk_activation_code_code_hash_uniq UNIQUE (code_hash)
);

-- Supports ActivationCodeRepository.Redeem's lookup-by-hash (the uniqueness
-- constraint above already indexes it) and ListByHousehold-style household
-- scoping if a future settings view lists outstanding codes.
CREATE INDEX kiosk_activation_code_household_idx ON kiosk_activation_code (household_id, created_at);

-- +goose Down
DROP TABLE IF EXISTS kiosk_activation_code;
DROP TABLE IF EXISTS kiosk_device;
