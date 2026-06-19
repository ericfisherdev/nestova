-- +goose Up
-- Pantry inventory (NES-42): on-hand quantities per household, optionally with an
-- expiry date. quantity/unit store the shared Quantity value object; unit is
-- CHECK-constrained to the Unit enum (household/domain). Tenant isolation follows
-- the composite-FK pattern. A household may hold multiple entries for the same
-- ingredient (different batches/expiries), so there is no per-ingredient unique.
CREATE TABLE pantry_item (
    id            uuid        PRIMARY KEY,
    household_id  uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    -- Catalogue ingredient (NES-38). No ON DELETE action: an ingredient that is
    -- still stocked must not be removed out from under the pantry.
    ingredient_id uuid        NOT NULL REFERENCES ingredient (id),
    quantity      numeric     NOT NULL CHECK (quantity >= 0),
    unit          text        NOT NULL CHECK (unit IN ('count', 'g', 'kg', 'ml', 'l')),
    expires_on    date,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    -- Reserved for future child composite FKs (mirrors tracked_item).
    CONSTRAINT pantry_item_household_id_uniq UNIQUE (household_id, id)
);

-- Supports ListByHousehold.
CREATE INDEX pantry_item_household_idx ON pantry_item (household_id);

-- Supports ListExpiringWithin (household + expiry window); partial so it ignores
-- the no-expiry rows the query also excludes.
CREATE INDEX pantry_item_expiring_idx
    ON pantry_item (household_id, expires_on)
    WHERE expires_on IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS pantry_item;
