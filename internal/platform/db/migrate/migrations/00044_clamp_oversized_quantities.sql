-- +goose Up
-- NES-193 caps a Quantity at 1e9 (household.MaxQuantityAmount). Quantities were
-- previously unbounded, so a row above the cap would fail Quantity.Validate on
-- every consume, adjust and grocery-list generation, and pantry items cannot be
-- deleted to get rid of it. Bring such rows into range.
UPDATE pantry_item SET quantity = 1000000000 WHERE quantity > 1000000000;
UPDATE shopping_list_item SET quantity = 1000000000 WHERE quantity > 1000000000;
UPDATE recipe_ingredient SET quantity = 1000000000 WHERE quantity > 1000000000;

-- +goose Down
-- The clamped amounts are not recoverable, and rows within the cap are left
-- untouched, so there is nothing to undo.
SELECT 1;
