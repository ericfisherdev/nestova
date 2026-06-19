-- +goose Up
-- Usage-tracking schema (NES-39): tracked consumables, their usage events, and
-- the cached restock prediction per item. Tenant isolation follows the
-- composite-FK pattern from 00003/00007 — household_id on every table, children
-- referencing (household_id, id) so a row can only point at parents in its own
-- household.

CREATE TABLE tracked_item (
    id                uuid        PRIMARY KEY,
    household_id      uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    name              text        NOT NULL CHECK (name <> ''),
    -- Free-text grouping (e.g. "pantry", "cleaning"); '' means uncategorized.
    category          text        NOT NULL DEFAULT '',
    -- Days before predicted depletion that the item should hit the shopping list.
    restock_lead_days int         NOT NULL DEFAULT 0 CHECK (restock_lead_days >= 0),
    active            boolean     NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    -- Lets usage_event compose a tenant FK on (household_id, id), the same way
    -- reward_household_id_uniq backs reward_redemption's composite FK.
    CONSTRAINT tracked_item_household_id_uniq UNIQUE (household_id, id)
);

-- Supports TrackedItemRepository.ListActiveByHousehold; partial so it stays small
-- as items are deactivated.
CREATE INDEX tracked_item_household_active_idx
    ON tracked_item (household_id) WHERE active = true;

CREATE TABLE usage_event (
    id              uuid        PRIMARY KEY,
    household_id    uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    tracked_item_id uuid        NOT NULL,
    type            text        NOT NULL
        CHECK (type IN ('replaced', 'refilled', 'depleted', 'opened')),
    occurred_at     timestamptz NOT NULL,
    -- NULL for system-generated events; otherwise the member who logged it.
    member_id       uuid,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Tenant consistency: the event's item must belong to the same household.
    CONSTRAINT usage_event_tracked_item_fk FOREIGN KEY (household_id, tracked_item_id)
        REFERENCES tracked_item (household_id, id) ON DELETE CASCADE,
    -- Tenant consistency: an attributed member must belong to the same household.
    -- member_id is nullable; with MATCH SIMPLE a NULL member_id skips this check,
    -- so system events (no member) are allowed.
    CONSTRAINT usage_event_member_fk FOREIGN KEY (household_id, member_id)
        REFERENCES member (household_id, id) ON DELETE CASCADE
);

-- Serves the depletion-history read (ListDepletionEvents): all events for an
-- item ordered by occurrence. Ordering in the index avoids a sort on that query.
CREATE INDEX usage_event_item_occurred_idx
    ON usage_event (tracked_item_id, occurred_at);

CREATE TABLE restock_prediction (
    -- One prediction per item; Upsert replaces it (PK = tracked_item_id).
    tracked_item_id        uuid        PRIMARY KEY
        REFERENCES tracked_item (id) ON DELETE CASCADE,
    avg_interval_days      numeric     NOT NULL CHECK (avg_interval_days >= 0),
    last_event_at          timestamptz NOT NULL,
    predicted_depletion_on date        NOT NULL,
    confidence             numeric     NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    updated_at             timestamptz NOT NULL DEFAULT now()
);

-- Supports the due-for-restock scan (ListDueForRestock), which filters on
-- predicted_depletion_on within an item's lead window.
CREATE INDEX restock_prediction_depletion_idx
    ON restock_prediction (predicted_depletion_on);

-- +goose Down
-- Drop in reverse dependency order so FK constraints are not violated.
DROP TABLE IF EXISTS restock_prediction;
DROP TABLE IF EXISTS usage_event;
DROP TABLE IF EXISTS tracked_item;
