-- +goose Up
-- Federation authorization codes (NSTR-105/NSTR-110): the provider side of
-- Nestova's authorize-then-exchange identity hand-off to Nestorage. GET
-- /federation/authorize (NSTR-105) mints a short-lived, single-use code once
-- a member is authenticated on THIS origin (including MFA and passkeys,
-- which only work here); NSTR-110's POST /federation/token exchanges it for
-- that member's identity document. This is deliberately NOT OAuth token
-- issuance — no access_token is ever produced, and there is no PKCE
-- code_challenge/code_verifier — so this table, unlike a textbook OAuth
-- schema, carries no scope or challenge columns.
--
-- code_hash is SHA-256, mirroring kiosk_activation_code.code_hash (00026):
-- the raw code is 256 bits of crypto/rand output, not a human-chosen
-- secret, so it already has full entropy and there is no dictionary/
-- rainbow-table risk to defend against with KDF stretching.
--
-- client_id and redirect_uri persist exactly what the code was issued for,
-- so NSTR-110's exchange can reject a code redeemed with a different client
-- or redirect than the one Authorize validated it against — a code is bound
-- to its original request, not just to the member.
--
-- expires_at is a short, fixed 2-minute window (auth/domain.AuthorizationCodeTTL):
-- the hop this code covers is a browser redirect straight into a
-- back-channel exchange, both at machine speed, unlike kiosk_activation_code's
-- 15-minute window, which waits on a person walking to a device.
CREATE TABLE federation_authorization_code (
    id           uuid        PRIMARY KEY,
    member_id    uuid        NOT NULL REFERENCES member (id) ON DELETE CASCADE,
    client_id    text        NOT NULL CHECK (client_id !~ '^[[:space:]]*$'),
    redirect_uri text        NOT NULL CHECK (redirect_uri !~ '^[[:space:]]*$'),
    code_hash    text        NOT NULL CHECK (code_hash !~ '^[[:space:]]*$'),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    used_at      timestamptz,
    CONSTRAINT federation_authorization_code_code_hash_uniq UNIQUE (code_hash)
);

-- Supports AuthorizationCodeRepository.Consume's lookup-by-hash (the
-- uniqueness constraint above already indexes it) and a future
-- per-member listing if one is ever needed.
CREATE INDEX federation_authorization_code_member_idx ON federation_authorization_code (member_id, created_at);

-- +goose Down
DROP TABLE IF EXISTS federation_authorization_code;
