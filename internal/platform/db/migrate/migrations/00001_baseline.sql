-- +goose Up
-- Baseline foundation schema: household, member, and the cross-cutting
-- notification outbox. gen_random_uuid() requires pgcrypto.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE household (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE member (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id uuid NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    display_name text NOT NULL,
    role         text NOT NULL CHECK (role IN ('owner', 'adult', 'child')),
    color_key    text NOT NULL CHECK (color_key IN ('sage', 'clay', 'ochre', 'blue', 'plum')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX member_household_id_idx ON member (household_id);
-- Member display names are unique (case-insensitively) within a household; this
-- backs the household context's ErrDuplicateMember. Member color is NOT unique
-- here: the domain reuses palette colors gracefully beyond five members.
CREATE UNIQUE INDEX member_household_name_uniq ON member (household_id, lower(display_name));
-- Target for the notification composite foreign key below, which enforces that a
-- notification's member belongs to the notification's household.
CREATE UNIQUE INDEX member_household_id_id_uniq ON member (household_id, id);

CREATE TABLE notification (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id  uuid NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    -- member_id is nullable for household-wide notifications. When set, the
    -- composite FK below enforces the member belongs to household_id (tenant
    -- consistency). Default MATCH SIMPLE skips the composite FK when member_id
    -- is NULL, so household-wide rows are still validated by the household FK.
    member_id     uuid,
    channel       text NOT NULL CHECK (channel IN ('push', 'email', 'inapp')),
    title         text NOT NULL,
    body          text NOT NULL,
    scheduled_for timestamptz NOT NULL,
    sent_at       timestamptz,
    status        text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'sent', 'failed', 'cancelled')),
    source_type   text,
    source_id     uuid,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- Deleting a member removes their member-targeted notifications. (SET NULL
    -- is not usable here: it would also null the NOT NULL household_id.)
    CONSTRAINT notification_member_in_household_fk
        FOREIGN KEY (household_id, member_id)
        REFERENCES member (household_id, id) ON DELETE CASCADE
);
-- Child-side index for the composite FK above, so deleting a member/household
-- does not table-scan notification during the cascade.
CREATE INDEX notification_household_member_idx ON notification (household_id, member_id);
-- Supports the outbox dispatcher's "claim due, pending notifications" query.
CREATE INDEX notification_due_idx ON notification (status, scheduled_for);

-- +goose Down
DROP TABLE IF EXISTS notification;
DROP TABLE IF EXISTS member;
DROP TABLE IF EXISTS household;
