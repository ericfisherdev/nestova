-- +goose Up
-- Federation instance link (NSTR-106): the binding an administrator creates
-- from Nestova's /settings page by entering a Nestorage instance's URL and
-- API key. This row is what every later push and reconciliation call
-- (NSTR-107/108) is scoped by — the credential is verified against the live
-- instance BEFORE this row ever exists (LinkService.Attach), so a row here
-- always represents a proven-reachable instance at the time it was created.
--
-- api_key_enc is bytea, not text: unlike kiosk_device.token_hash or
-- federation_authorization_code.code_hash (which store only a one-way
-- hash), the Nestorage API key must be recovered in full to authenticate
-- later push/reconciliation calls, so it is encrypted at rest (AES-256-GCM
-- via the same tokenCipher instance protecting calendar OAuth tokens and
-- TOTP secrets) rather than hashed.
--
-- UNIQUE (household_id): at most one Nestorage instance per household — a
-- Nestova install serving more than one household must never let one
-- household's members reach another household's Nestorage data, and this
-- constraint (plus the lower(base_url) index below) makes that
-- many-to-many ambiguity unrepresentable at the storage layer, not just in
-- service code (LinkService.Attach's own pre-check is a race-safe
-- convenience on top of this, not a substitute for it).
--
-- attached_by is nullable with ON DELETE SET NULL, mirroring task's own
-- assignee_id/completed_by and subscriptions' payer_id (00003, 00014): the
-- member who performed the attach may later leave the household, but the
-- link itself — and the audit trail of who created it while they were
-- still a member — survives.
CREATE TABLE federation_instance_link (
    id           uuid        PRIMARY KEY,
    household_id uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    base_url     text        NOT NULL CHECK (btrim(base_url) <> ''),
    api_key_enc  bytea       NOT NULL,
    attached_by  uuid        REFERENCES member (id) ON DELETE SET NULL,
    attached_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT federation_instance_link_household_id_uniq UNIQUE (household_id)
);

-- One household per Nestorage instance across the whole install — the
-- other half of the many-to-many-ambiguity guard UNIQUE(household_id)
-- starts above. lower() mirrors member_household_name_uniq's own
-- case-insensitive convention and matches domain.NormalizeBaseURL's
-- lowercase-host canonicalization, so this index and the domain's own
-- equality checks agree bit-for-bit.
CREATE UNIQUE INDEX federation_instance_link_base_url_uniq ON federation_instance_link (lower(base_url));

-- +goose Down
DROP TABLE IF EXISTS federation_instance_link;
