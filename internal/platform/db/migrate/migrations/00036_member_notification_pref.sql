-- +goose Up
-- SMS routing by member preference + quiet hours (NES-139). Three additive
-- changes; none of them narrows an existing constraint against already-
-- populated data, so — unlike 00035's DOWN, which had to delete 'sms' rows
-- before re-narrowing notification_channel_check — this migration's DOWN
-- can simply drop what Up added: every new column is nullable and every
-- new CHECK is on a brand-new table, so no existing row can ever violate
-- what the DOWN below restores.
--
-- member gains an optional contact channel for SMS: phone_e164 (validated
-- application-side by domain.ParseE164Phone before ever being written —
-- see that type's own doc; no DB-level format CHECK, mirroring how every
-- other validated-format column in this schema is enforced only in the
-- domain layer) and sms_opted_in_at, the express-written-consent record
-- (NULL until a member explicitly opts in; see docs/aws-sms.md's
-- production gate). These live on member, not a channel-agnostic-domain
-- extension, deliberately: the household bounded context's own Go domain
-- model (internal/household/domain.Member) does NOT gain corresponding
-- fields — only the notify context's own adapter reads/writes these two
-- columns (see notify/domain.ContactDirectory), keeping household
-- itself channel-agnostic per this ticket's own plan.
--
-- member_sms_opt_in_requires_phone is the schema-level backstop for the
-- SAME invariant app.SettingsService.SetPreference and
-- ContactDirectory.SetOptedIn already enforce in application code
-- (ErrPhoneRequiredForOptIn): consent cannot outlive the number it was
-- given for. Without it, a race or a future direct-SQL caller could leave
-- sms_opted_in_at set after phone_e164 is cleared — the exact
-- "opted-in-but-undeliverable" state the app layer already prevents on
-- its own normal paths.
ALTER TABLE member
    ADD COLUMN phone_e164      text,
    ADD COLUMN sms_opted_in_at timestamptz,
    ADD CONSTRAINT member_sms_opt_in_requires_phone
        CHECK (sms_opted_in_at IS NULL OR phone_e164 IS NOT NULL);

-- member_notification_pref: a member's chosen delivery channel per event
-- type (e.g. "claim expiring" -> sms). Sparse by design — a member with no
-- row for a given event_type gets that event type's default (in-app),
-- resolved application-side (internal/notify/app/routing.go), not by a
-- DEFAULT here: the "no preference row = in-app" rule is a routing
-- decision, not a storage default, and keeping it out of the schema means
-- introducing a new default channel later needs no migration.
--
-- The composite tenant FK mirrors 00031_member_mfa.sql:33-36's identical
-- pattern: household_id lets member_notification_pref_member_fk verify
-- the member actually belongs to household_id without a second join at
-- query/write time — the same defense-in-depth this schema already uses
-- for member_mfa and calendar_account.
--
-- NES-141 reads and writes this same table (email preferences) — this
-- ticket owns the migration; NES-141 adds no competing one.
CREATE TABLE member_notification_pref (
    member_id    uuid NOT NULL,
    household_id uuid NOT NULL,
    event_type   text NOT NULL,
    channel      text NOT NULL CHECK (channel IN ('push', 'email', 'inapp', 'sms')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (member_id, event_type),
    CONSTRAINT member_notification_pref_member_fk FOREIGN KEY (household_id, member_id)
        REFERENCES member (household_id, id) ON DELETE CASCADE
);

-- Supports listing every preference a household's members have set (e.g.
-- an owner-facing overview, or a future household-wide default sweep) —
-- mirrors member_mfa_household_idx's identical rationale.
CREATE INDEX member_notification_pref_household_idx ON member_notification_pref (household_id);

-- household gains an optional quiet-hours window: both NULL means quiet
-- hours are disabled (SMS may be sent at any time). A window may cross
-- midnight (e.g. 22:00-07:00) — application-side logic
-- (household.Household's own methods), not a CHECK here, decides whether
-- a given clock time falls inside it, since a CHECK constraint cannot
-- express "start may be after end, meaning the window wraps past
-- midnight" without needlessly special-casing the non-wrapping case too.
--
-- household_quiet_hours_bounds_paired DOES belong at the schema level,
-- unlike the wrap-around rule above: "both set or both NULL" is a simple,
-- constraint-expressible invariant, and the app layer's own
-- both-or-neither validation (PostgresRepository.SetQuietHours,
-- NotifyWebHandlers.UpdateQuietHours) should not be the only thing
-- preventing a half-set window from silently disabling quiet hours (a
-- one-sided bound has no defined meaning in Household.InQuietHours).
ALTER TABLE household
    ADD COLUMN quiet_hours_start time,
    ADD COLUMN quiet_hours_end   time,
    ADD CONSTRAINT household_quiet_hours_bounds_paired
        CHECK ((quiet_hours_start IS NULL) = (quiet_hours_end IS NULL));

-- +goose Down
-- Constraints dropped explicitly, before their columns, rather than relied
-- on to disappear implicitly alongside a multi-column DROP COLUMN: explicit
-- here costs nothing and removes any doubt about it.
ALTER TABLE household
    DROP CONSTRAINT IF EXISTS household_quiet_hours_bounds_paired,
    DROP COLUMN IF EXISTS quiet_hours_start,
    DROP COLUMN IF EXISTS quiet_hours_end;

DROP TABLE IF EXISTS member_notification_pref;

ALTER TABLE member
    DROP CONSTRAINT IF EXISTS member_sms_opt_in_requires_phone,
    DROP COLUMN IF EXISTS phone_e164,
    DROP COLUMN IF EXISTS sms_opted_in_at;
