-- +goose Up
-- Nestova's full cutover onto the shared identity schema (NSTR-115):
-- household, member, member_mfa, member_recovery_code, member_credential,
-- and sessions are ALL dropped from this (nestova) schema — they now live
-- in identity, owned and migrated separately by nestcore (a companion
-- migration runner against the same "nest" database; see
-- docs/deployment.md and cmd/migrate's own wiring). This migration assumes
-- identity.household / identity.member already exist when it runs.
--
-- No data-migration or data-preservation step exists anywhere in this
-- migration (Eric, 2026-07-29 decision record on NSTR-115): neither app is
-- live, so local dev databases are disposable — reset and recreate local
-- dev accounts after this cutover via onboarding, documented in
-- docs/deployment.md.
--
-- +goose StatementBegin

-- nestova.member_profile: color_key is presentation, not identity — the
-- 5-member Hearth palette is Nestova's own design system (Nestorage has a
-- different one), so it stays app-owned rather than migrating onto
-- identity.member, which must remain app-agnostic. One row per member; the
-- PK doubles as the FK to identity.member, so a member profile can never
-- outlive its identity row.
CREATE TABLE member_profile (
    member_id  uuid PRIMARY KEY REFERENCES identity.member (id) ON DELETE CASCADE,
    color_key  text NOT NULL CHECK (color_key IN ('sage', 'clay', 'ochre', 'blue', 'plum')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- nestova.member_contact: phone_e164/sms_opted_in_at rehome here from
-- member (originally added by 00036_member_notification_pref.sql) — the
-- same reasoning as member_profile's color_key: identity must stay
-- app-agnostic and channel-agnostic, and NES-139's own SMS contact info is
-- squarely a notify concern, not an identity one. household.Member (and
-- now identity.member) never gains these fields; only notify's own
-- adapter (ContactDirectory) reads/writes this table. Sparse by design —
-- a member who has never touched their phone/opt-in settings has no row
-- here at all, exactly like member_profile's color_key before AddMember
-- writes it.
CREATE TABLE member_contact (
    member_id       uuid PRIMARY KEY REFERENCES identity.member (id) ON DELETE CASCADE,
    phone_e164      text,
    sms_opted_in_at timestamptz,
    CONSTRAINT member_contact_sms_opt_in_requires_phone
        CHECK (sms_opted_in_at IS NULL OR phone_e164 IS NOT NULL)
);

-- nestova.notification_quiet_hours: quiet hours rehome here from the
-- dissolving household domain (00036_member_notification_pref.sql
-- originally added them to household). Notifications stay strictly
-- per-app (the shared-identity decision record), and identity is strictly
-- authentication/authorization, so delivery policy — quiet hours — cannot
-- follow household into the identity schema; it moves into notify's own
-- schema instead, keyed by the identity household id. Semantics are
-- unchanged from the original household.quiet_hours_start/end columns:
-- both null means disabled, and the paired-bounds CHECK is ported
-- verbatim from household_quiet_hours_bounds_paired.
CREATE TABLE notification_quiet_hours (
    household_id      uuid PRIMARY KEY REFERENCES identity.household (id) ON DELETE CASCADE,
    quiet_hours_start time,
    quiet_hours_end   time,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT notification_quiet_hours_bounds_paired
        CHECK ((quiet_hours_start IS NULL) = (quiet_hours_end IS NULL))
);

-- Repoint every surviving table's FK from local household/member onto
-- identity.household/identity.member. Constraint names, local/referenced
-- columns, and ON DELETE behavior (including a per-column SET NULL list,
-- e.g. task_instance's assignee_id/completed_by/claimed_by) are read from
-- the catalog and preserved exactly, rather than hand-enumerated: roughly
-- twenty of Nestova's tables carry such an FK, and several adapters
-- (household, calendar, media, tracking, kiosk, subscriptions, meals,
-- notify) map specific FK-violation constraint names to domain sentinel
-- errors — those names must keep matching after this migration. member_mfa
-- and member_credential are excluded: both are dropped a few statements
-- below, so repointing their own household_id/member FKs here first would
-- be pure churn.
DO $$
DECLARE
    fk RECORD;
    local_cols TEXT;
    ref_cols TEXT;
    ref_table TEXT;
    set_null_cols TEXT;
    action TEXT;
BEGIN
    FOR fk IN
        SELECT
            con.conname,
            con.conrelid::regclass::text AS local_table,
            con.confrelid::regclass::text AS referenced_table,
            con.confdeltype,
            con.confdelsetcols,
            con.conkey,
            con.confkey,
            con.conrelid,
            con.confrelid
        FROM pg_constraint con
        WHERE con.contype = 'f'
          AND con.confrelid IN ('household'::regclass, 'member'::regclass)
          AND con.conrelid::regclass::text NOT IN ('member_mfa', 'member_credential')
    LOOP
        SELECT string_agg(quote_ident(a.attname), ', ' ORDER BY u.ord)
          INTO local_cols
          FROM unnest(fk.conkey) WITH ORDINALITY AS u(attnum, ord)
          JOIN pg_attribute a ON a.attrelid = fk.conrelid AND a.attnum = u.attnum;

        SELECT string_agg(quote_ident(a.attname), ', ' ORDER BY u.ord)
          INTO ref_cols
          FROM unnest(fk.confkey) WITH ORDINALITY AS u(attnum, ord)
          JOIN pg_attribute a ON a.attrelid = fk.confrelid AND a.attnum = u.attnum;

        ref_table := CASE fk.referenced_table
            WHEN 'household' THEN 'identity.household'
            WHEN 'member' THEN 'identity.member'
        END;

        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', fk.local_table, fk.conname);

        action := CASE fk.confdeltype
            WHEN 'c' THEN 'CASCADE'
            WHEN 'n' THEN 'SET NULL'
            ELSE NULL
        END;
        IF action IS NULL THEN
            RAISE EXCEPTION 'unexpected ON DELETE action % on constraint %', fk.confdeltype, fk.conname;
        END IF;

        IF fk.confdeltype = 'n' AND fk.confdelsetcols IS NOT NULL AND array_length(fk.confdelsetcols, 1) > 0 THEN
            SELECT string_agg(quote_ident(a.attname), ', ')
              INTO set_null_cols
              FROM unnest(fk.confdelsetcols) AS u(attnum)
              JOIN pg_attribute a ON a.attrelid = fk.conrelid AND a.attnum = u.attnum;
            action := action || format(' (%s)', set_null_cols);
        END IF;

        EXECUTE format(
            'ALTER TABLE %s ADD CONSTRAINT %I FOREIGN KEY (%s) REFERENCES %s (%s) ON DELETE %s',
            fk.local_table, fk.conname, local_cols, ref_table, ref_cols, action
        );
    END LOOP;
END $$;

-- +goose StatementEnd

-- Drop in dependency order: recovery codes reference member_mfa; both
-- member_mfa and member_credential reference household/member directly;
-- sessions references nothing. member and household drop last, once every
-- surviving reference has been repointed away above.
DROP TABLE IF EXISTS member_recovery_code;
DROP TABLE IF EXISTS member_mfa;
DROP TABLE IF EXISTS member_credential;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS member;
DROP TABLE IF EXISTS household;

-- +goose Down
-- +goose StatementBegin

CREATE TABLE household (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name              text NOT NULL,
    quiet_hours_start time,
    quiet_hours_end   time,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT household_quiet_hours_bounds_paired
        CHECK ((quiet_hours_start IS NULL) = (quiet_hours_end IS NULL))
);

CREATE TABLE member (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  uuid NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    display_name  text NOT NULL,
    role          text NOT NULL CHECK (role IN ('owner', 'adult', 'child')),
    color_key     text NOT NULL CHECK (color_key IN ('sage', 'clay', 'ochre', 'blue', 'plum')),
    email         citext,
    password_hash text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT member_email_unique UNIQUE (email),
    CONSTRAINT member_credentials_complete CHECK (
        (email IS NULL AND password_hash IS NULL)
        OR (email IS NOT NULL AND password_hash IS NOT NULL)
    )
);
CREATE INDEX member_household_id_idx ON member (household_id);
CREATE UNIQUE INDEX member_household_name_uniq ON member (household_id, lower(display_name));
CREATE UNIQUE INDEX member_household_id_id_uniq ON member (household_id, id);

CREATE TABLE member_mfa (
    member_id       uuid        PRIMARY KEY,
    household_id    uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    totp_secret_enc bytea       NOT NULL CHECK (length(totp_secret_enc) > 0),
    confirmed_at    timestamptz,
    last_totp_step  bigint,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT member_mfa_member_fk FOREIGN KEY (household_id, member_id)
        REFERENCES member (household_id, id) ON DELETE CASCADE
);
CREATE INDEX member_mfa_household_idx ON member_mfa (household_id);

CREATE TABLE member_recovery_code (
    id         uuid        PRIMARY KEY,
    member_id  uuid        NOT NULL REFERENCES member_mfa (member_id) ON DELETE CASCADE,
    code_hash  text        NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX member_recovery_code_member_idx ON member_recovery_code (member_id);

CREATE TABLE member_credential (
    id            uuid        PRIMARY KEY,
    household_id  uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    member_id     uuid        NOT NULL,
    credential_id bytea       NOT NULL UNIQUE,
    public_key    bytea       NOT NULL,
    sign_count    bigint      NOT NULL DEFAULT 0,
    transports    text[],
    aaguid        uuid,
    nickname      text        NOT NULL,
    user_handle   bytea       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    CONSTRAINT member_credential_member_fk FOREIGN KEY (household_id, member_id)
        REFERENCES member (household_id, id) ON DELETE CASCADE
);
CREATE INDEX member_credential_member_idx ON member_credential (member_id);
CREATE INDEX member_credential_user_handle_idx ON member_credential (user_handle);

CREATE TABLE sessions (
    token  text        PRIMARY KEY,
    data   bytea       NOT NULL,
    expiry timestamptz NOT NULL
);
CREATE INDEX sessions_expiry_idx ON sessions (expiry);

-- Repoint every surviving table's FK back from identity.household/
-- identity.member onto the just-recreated local household/member —
-- the mirror of the Up block above, run after household/member exist
-- again so the new local FKs have a target. Constraints whose OWN table
-- lives in the identity schema are excluded: nestcore's identity schema
-- has its own member_mfa/member_credential tables (NSTR-130) with FKs to
-- identity.household/identity.member — those belong to nestcore, not
-- nestova, and must never be touched by this migration in either
-- direction.
DO $$
DECLARE
    fk RECORD;
    local_cols TEXT;
    ref_cols TEXT;
    ref_table TEXT;
    set_null_cols TEXT;
    action TEXT;
BEGIN
    FOR fk IN
        SELECT
            con.conname,
            con.conrelid::regclass::text AS local_table,
            con.confrelid::regclass::text AS referenced_table,
            con.confdeltype,
            con.confdelsetcols,
            con.conkey,
            con.confkey,
            con.conrelid,
            con.confrelid
        FROM pg_constraint con
        WHERE con.contype = 'f'
          AND con.confrelid IN ('identity.household'::regclass, 'identity.member'::regclass)
          AND con.connamespace != 'identity'::regnamespace
    LOOP
        SELECT string_agg(quote_ident(a.attname), ', ' ORDER BY u.ord)
          INTO local_cols
          FROM unnest(fk.conkey) WITH ORDINALITY AS u(attnum, ord)
          JOIN pg_attribute a ON a.attrelid = fk.conrelid AND a.attnum = u.attnum;

        SELECT string_agg(quote_ident(a.attname), ', ' ORDER BY u.ord)
          INTO ref_cols
          FROM unnest(fk.confkey) WITH ORDINALITY AS u(attnum, ord)
          JOIN pg_attribute a ON a.attrelid = fk.confrelid AND a.attnum = u.attnum;

        ref_table := CASE fk.referenced_table
            WHEN 'identity.household' THEN 'household'
            WHEN 'identity.member' THEN 'member'
        END;

        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', fk.local_table, fk.conname);

        action := CASE fk.confdeltype
            WHEN 'c' THEN 'CASCADE'
            WHEN 'n' THEN 'SET NULL'
            ELSE NULL
        END;
        IF action IS NULL THEN
            RAISE EXCEPTION 'unexpected ON DELETE action % on constraint %', fk.confdeltype, fk.conname;
        END IF;

        IF fk.confdeltype = 'n' AND fk.confdelsetcols IS NOT NULL AND array_length(fk.confdelsetcols, 1) > 0 THEN
            SELECT string_agg(quote_ident(a.attname), ', ')
              INTO set_null_cols
              FROM unnest(fk.confdelsetcols) AS u(attnum)
              JOIN pg_attribute a ON a.attrelid = fk.conrelid AND a.attnum = u.attnum;
            action := action || format(' (%s)', set_null_cols);
        END IF;

        -- NOT VALID: this Down runs against whatever data a test (or a
        -- local dev session) has already written against identity.* FKs
        -- since this migration's Up ran — household/member are recreated
        -- above as brand-new EMPTY tables, so a fully-validated ADD
        -- CONSTRAINT would reject every existing row in fk.local_table
        -- outright. Down is a dev/test rollback convenience, never a
        -- preserved-data path (see this file's own "no data migration"
        -- doc at the top) — the schema is either being torn down further
        -- or immediately re-Up'd, so skipping retroactive validation here
        -- is safe. New writes are still constrained normally; only
        -- pre-existing rows are exempt.
        EXECUTE format(
            'ALTER TABLE %s ADD CONSTRAINT %I FOREIGN KEY (%s) REFERENCES %s (%s) ON DELETE %s NOT VALID',
            fk.local_table, fk.conname, local_cols, ref_table, ref_cols, action
        );
    END LOOP;
END $$;

-- +goose StatementEnd

DROP TABLE IF EXISTS notification_quiet_hours;
DROP TABLE IF EXISTS member_contact;
DROP TABLE IF EXISTS member_profile;
