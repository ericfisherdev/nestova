-- +goose Up
-- TOTP MFA enrollment (NES-134): opt-in per-member authenticator-app
-- enrollment with recovery codes. LOGIN ENFORCEMENT IS A FOLLOW-UP TICKET
-- (NES-135) — this schema only tracks enrollment/management state; nothing
-- here changes login behavior.
--
-- member_mfa is a 1:1 extension of member (member_id is both the primary key
-- and, via the composite FK below, tenant-checked against household_id),
-- mirroring the calendar_account tenant-isolation pattern (NES-6/66):
-- household_id lets member_mfa_member_fk verify the member actually belongs
-- to that household without a second join at query time.
--
-- An enrollment starts unconfirmed (confirmed_at IS NULL) the moment a member
-- generates a secret, and becomes active only once they prove control of the
-- authenticator app by submitting one valid code back. Re-enrolling before
-- confirming simply overwrites the still-unconfirmed row in place (the
-- application layer's contract, not a DB constraint) — there is deliberately
-- no separate "pending enrollment" table and no sweep/cleanup job: an
-- abandoned unconfirmed row is inert (ignored by login, and by this ticket's
-- own confirm/disable/disenroll checks, which all require CONFIRMED state)
-- until either confirmed or replaced by a fresh BeginEnrollment call.
CREATE TABLE member_mfa (
    member_id       uuid        PRIMARY KEY,
    household_id    uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    -- AES-256-GCM ciphertext (internal/platform/crypto.Cipher, the same
    -- ENCRYPTION_KEY-derived cipher that protects calendar OAuth tokens);
    -- never stored or logged in plaintext. Non-empty mirrors
    -- MFAEnrollment.Validate.
    totp_secret_enc bytea       NOT NULL CHECK (length(totp_secret_enc) > 0),
    confirmed_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- Tenant consistency: the enrollment's member must belong to
    -- household_id, mirroring calendar_account_member_fk.
    CONSTRAINT member_mfa_member_fk FOREIGN KEY (household_id, member_id)
        REFERENCES member (household_id, id) ON DELETE CASCADE
);

-- Supports the household-owner admin reset flow, which needs to find/verify
-- a target member's enrollment scoped to the acting owner's own household.
CREATE INDEX member_mfa_household_idx ON member_mfa (household_id);

-- Ten single-use recovery codes are generated once, immediately after
-- confirmation (never before — an unconfirmed enrollment never has recovery
-- codes), and displayed exactly once; only the argon2id hash is persisted
-- (internal/platform/crypto, the same KDF used for member passwords).
-- Regenerating replaces the full set atomically (delete-then-insert in one
-- transaction); disenrolling or an owner reset removes the member_mfa row,
-- cascading here.
CREATE TABLE member_recovery_code (
    id         uuid        PRIMARY KEY,
    member_id  uuid        NOT NULL REFERENCES member_mfa (member_id) ON DELETE CASCADE,
    code_hash  text        NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Serves both ListUnusedRecoveryCodes (verifying a submitted code against a
-- member's unused set) and ReplaceRecoveryCodes (deleting a member's full set
-- to regenerate).
CREATE INDEX member_recovery_code_member_idx ON member_recovery_code (member_id);

-- +goose Down
-- Drop in reverse dependency order so the FK is not violated.
DROP TABLE IF EXISTS member_recovery_code;
DROP TABLE IF EXISTS member_mfa;
