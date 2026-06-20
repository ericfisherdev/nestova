-- +goose Up
-- Calendar schema (NES-6 / NES-66): per-member connected calendar providers and
-- the cached external events that feed the unified calendar. OAuth tokens are
-- stored encrypted at rest (the *_enc bytea columns); the OAuth layer (NES-67)
-- owns the encryption, the domain carries only ciphertext. Tenant isolation
-- follows the composite-FK pattern: household_id on calendar_account, the member
-- referencing member(household_id, id) so an account can only belong to a member
-- in its own household.

CREATE TABLE calendar_account (
    id                 uuid        PRIMARY KEY,
    member_id          uuid        NOT NULL,
    household_id       uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    provider           text        NOT NULL CHECK (provider IN ('google')),
    -- Encrypted OAuth material (AES-GCM ciphertext); never stored in plaintext.
    -- Non-empty mirrors CalendarAccount.Validate, which rejects empty ciphertext.
    access_token_enc   bytea       NOT NULL CHECK (length(access_token_enc) > 0),
    refresh_token_enc  bytea       NOT NULL CHECK (length(refresh_token_enc) > 0),
    token_expiry       timestamptz NOT NULL,
    -- Google incremental-sync cursor; NULL until the first sync completes.
    sync_token         text,
    -- The provider calendar ids this account syncs; empty means none selected yet.
    calendar_ids       text[]      NOT NULL DEFAULT '{}',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    -- Tenant consistency: the account's member must belong to household_id. A
    -- member's accounts are removed with the member (ON DELETE CASCADE).
    CONSTRAINT calendar_account_member_fk FOREIGN KEY (household_id, member_id)
        REFERENCES member (household_id, id) ON DELETE CASCADE,
    -- One connected account per provider per member; reconnecting updates in place.
    CONSTRAINT calendar_account_member_provider_uniq UNIQUE (member_id, provider),
    -- Lets external_event compose a tenant FK on (household_id, id) if needed and
    -- mirrors the pattern used by the other tables.
    CONSTRAINT calendar_account_household_id_uniq UNIQUE (household_id, id)
);

-- Supports listing a household's accounts for the unified view and iterating a
-- household's accounts during sync.
CREATE INDEX calendar_account_household_idx ON calendar_account (household_id);

CREATE TABLE external_event (
    id                  uuid        PRIMARY KEY,
    calendar_account_id uuid        NOT NULL
        REFERENCES calendar_account (id) ON DELETE CASCADE,
    -- The provider's event id; the unique key below makes sync upserts idempotent.
    -- Reject empty AND whitespace-only ids to match ExternalEvent.Validate, which
    -- trims before the blank check.
    external_id         text        NOT NULL CHECK (external_id !~ '^[[:space:]]*$'),
    title               text        NOT NULL DEFAULT '',
    starts_at           timestamptz NOT NULL,
    ends_at             timestamptz NOT NULL,
    all_day             boolean     NOT NULL DEFAULT false,
    -- Provider color id; NULL when the event carries no explicit color.
    color               text,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    -- Mirror the domain invariant (ExternalEvent.Validate) so a direct insert
    -- cannot store an event whose end precedes its start.
    CONSTRAINT external_event_time_order CHECK (ends_at >= starts_at),
    -- Idempotent sync: re-syncing the same provider event upserts one cache row.
    CONSTRAINT external_event_account_external_uniq UNIQUE (calendar_account_id, external_id)
);

-- Serves the unified-calendar range query: events are household-scoped via their
-- account, so the range scan is keyed by account then start time (the household
-- has few accounts, so the join over them stays cheap).
CREATE INDEX external_event_account_starts_at_idx
    ON external_event (calendar_account_id, starts_at);

-- +goose Down
-- Drop in reverse dependency order so the FK is not violated.
DROP TABLE IF EXISTS external_event;
DROP TABLE IF EXISTS calendar_account;
