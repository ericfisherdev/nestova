-- +goose Up
-- Unified shopping list (NES-43): manual and system-sourced items with a
-- needed → in_cart → purchased lifecycle. An item is identified by either a
-- catalogue ingredient_id (system-generated / catalogue pick) or a free-text
-- name (ad-hoc manual entry) — exactly one, enforced below.
CREATE TABLE shopping_list_item (
    id            uuid        PRIMARY KEY,
    household_id  uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    -- Catalogue ingredient (NES-38); NULL for ad-hoc manual items. No ON DELETE
    -- action: a still-listed ingredient must not be removed out from under it.
    ingredient_id uuid        REFERENCES ingredient (id),
    name          text,
    quantity      numeric     NOT NULL CHECK (quantity >= 0),
    unit          text        NOT NULL CHECK (unit IN ('count', 'g', 'kg', 'ml', 'l')),
    source        text        NOT NULL
        CHECK (source IN ('manual', 'restock', 'meal_plan', 'pantry_low')),
    status        text        NOT NULL DEFAULT 'needed'
        CHECK (status IN ('needed', 'in_cart', 'purchased')),
    -- Member who added a manual item; NULL for system-generated entries.
    added_by      uuid,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- Exactly one of ingredient_id / name identifies the item.
    CONSTRAINT shopping_list_item_identity_chk CHECK ((ingredient_id IS NULL) <> (name IS NULL)),
    -- Tenant consistency for the optional adder; SET NULL (added_by) preserves the
    -- item (and household_id, which is NOT NULL) when the member is removed.
    CONSTRAINT shopping_list_item_added_by_fk FOREIGN KEY (household_id, added_by)
        REFERENCES member (household_id, id) ON DELETE SET NULL (added_by)
);

-- Idempotency guard for the restock automation (NES-44): at most one OPEN
-- (non-purchased) restock entry per (household, ingredient). Purchased entries
-- are excluded so a new restock can be raised after the prior one is bought.
-- Partial UNIQUE so it never constrains manual or already-purchased rows.
CREATE UNIQUE INDEX shopping_list_item_open_restock_uniq
    ON shopping_list_item (household_id, ingredient_id)
    WHERE source = 'restock' AND status <> 'purchased';

-- Supports ListByStatus (the needed / in-cart / purchased views).
CREATE INDEX shopping_list_item_household_status_idx
    ON shopping_list_item (household_id, status);

-- +goose Down
DROP TABLE IF EXISTS shopping_list_item;
