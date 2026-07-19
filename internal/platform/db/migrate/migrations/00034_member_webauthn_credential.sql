-- +goose Up
-- WebAuthn passkey registration (NES-136): opt-in per-member platform
-- passkey enrollment (Face ID, Android fingerprint, Windows Hello). LOGIN
-- CEREMONY IS A FOLLOW-UP TICKET (NES-137) — this schema only tracks
-- registered credentials and the durable, per-member handle NES-137's
-- usernameless lookup needs; nothing here changes login behavior.
--
-- member_credential is a 1:many extension of member (a member may register
-- multiple passkeys — phone, laptop, security key), mirroring member_mfa's
-- (00031) tenant-isolation pattern: household_id lets
-- member_credential_member_fk verify the member actually belongs to that
-- household without a second join at query time.
CREATE TABLE member_credential (
    id            uuid        PRIMARY KEY,
    household_id  uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    member_id     uuid        NOT NULL,
    -- The WebAuthn credential id (an opaque handle the authenticator itself
    -- generates), globally unique by construction — the UNIQUE constraint is
    -- defense-in-depth, not the primary replay guard: FinishRegistration's
    -- single-use challenge (cleared from the scs session immediately after
    -- one use, win or lose — see authapp.WebAuthnService) is what actually
    -- makes a replayed registration response fail.
    credential_id bytea       NOT NULL UNIQUE,
    -- CBOR-encoded public key (go-webauthn's Credential.PublicKey). Unlike
    -- member_mfa's totp_secret_enc, this is NOT encrypted at rest: a public
    -- key is, by definition, not a secret — it is useless to an attacker
    -- without the authenticator's private key, which never leaves the
    -- device.
    public_key    bytea       NOT NULL,
    -- The authenticator's signature counter as of the last successful
    -- ceremony (0 at registration for an authenticator that does not
    -- implement counters at all — never NULL, since "counter unsupported"
    -- and "counter is genuinely zero" are handled identically by NES-137's
    -- clone-detection check either way). NES-137 updates this on every
    -- login.
    sign_count    bigint      NOT NULL DEFAULT 0,
    -- Transport hints the authenticator reported at registration (e.g.
    -- {internal}, {hybrid,usb}) — advisory only, used to pre-filter which
    -- transports the browser tries first on a future login; never a
    -- security boundary.
    transports    text[],
    -- The authenticator model's AAGUID, when reported (NULL for models that
    -- report none — treated as "unknown model", not "no authenticator").
    aaguid        uuid,
    -- Member-chosen label shown in the "Your devices" settings list (e.g.
    -- "iPhone", "Work laptop") so a member with several passkeys can tell
    -- them apart when revoking one.
    nickname      text        NOT NULL,
    -- The HMAC-derived WebAuthn user handle (authapp.WebAuthnUserHandleDeriver)
    -- stored on EVERY credential row for this member — deliberately
    -- redundant per-row (not normalized onto member) so NES-137's
    -- usernameless login lookup is a single indexed equality query against
    -- this table alone, with no join back to member required.
    user_handle   bytea       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- NULL until NES-137's login ceremony first uses this credential.
    last_used_at  timestamptz,
    -- Tenant consistency: the credential's member must belong to
    -- household_id, mirroring member_mfa_member_fk.
    CONSTRAINT member_credential_member_fk FOREIGN KEY (household_id, member_id)
        REFERENCES member (household_id, id) ON DELETE CASCADE
);

-- Supports the settings page's "Your devices" list (all of one member's
-- registered passkeys) and rename/revoke's tenant-scoped lookups.
CREATE INDEX member_credential_member_idx ON member_credential (member_id);

-- Supports NES-137's usernameless login lookup: the authenticator returns a
-- user handle (not a member id) during a discoverable-credential login, and
-- that handle must resolve back to a member via a single indexed equality
-- query against this column.
CREATE INDEX member_credential_user_handle_idx ON member_credential (user_handle);

-- +goose Down
DROP TABLE IF EXISTS member_credential;
