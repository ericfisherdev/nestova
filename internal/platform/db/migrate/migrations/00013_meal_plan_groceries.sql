-- +goose NO TRANSACTION
-- Meal-plan groceries (NES-61): generating a week's shopping from the meal plan
-- adds aggregated, ingredient-keyed items with source = 'meal_plan'. Mirror the
-- restock guards so generation is idempotent and the rows are always
-- ingredient-identified.
--
-- NO TRANSACTION because the partial index is built CONCURRENTLY (Postgres forbids
-- CONCURRENTLY inside a transaction) so creating it does not block reads/writes on
-- shopping_list_item during deploy. Because a non-transactional migration can stop
-- partway, each statement is made re-runnable (DROP ... IF EXISTS before ADD/CREATE)
-- so re-applying after an interruption always converges to a clean state.

-- +goose Up
-- Meal-plan items are always ingredient-based (they aggregate a recipe's
-- normalized lines). A name-only meal_plan row would carry a NULL ingredient_id
-- and, because UNIQUE treats NULLs as distinct, slip past the open-meal-plan
-- partial index below — so forbid it at the schema, as restock does.
ALTER TABLE shopping_list_item DROP CONSTRAINT IF EXISTS shopping_list_item_meal_plan_identity_chk;
ALTER TABLE shopping_list_item
    ADD CONSTRAINT shopping_list_item_meal_plan_identity_chk
        CHECK (source <> 'meal_plan' OR ingredient_id IS NOT NULL);

-- Idempotency guard for plan-to-grocery generation: at most one OPEN
-- (non-purchased) meal_plan entry per (household, ingredient, unit). The unit is
-- part of the key because generation aggregates per (ingredient, unit) and keeps
-- differing units as separate lines (Quantity does no unit conversion) — so the
-- same ingredient in, say, ml and l must be allowed to coexist while each (ingredient,
-- unit) line still de-duplicates on re-run. Purchased entries are excluded so a new
-- week's generation can re-add an ingredient once bought.
-- DROP-then-create CONCURRENTLY: an interrupted CONCURRENTLY build leaves an
-- INVALID index, so clear any leftover first to keep a re-run clean. The drop is
-- also CONCURRENTLY so a re-run never takes a blocking lock on shopping_list_item.
DROP INDEX CONCURRENTLY IF EXISTS shopping_list_item_open_meal_plan_uniq;
CREATE UNIQUE INDEX CONCURRENTLY shopping_list_item_open_meal_plan_uniq
    ON shopping_list_item (household_id, ingredient_id, unit)
    WHERE source = 'meal_plan' AND status <> 'purchased';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS shopping_list_item_open_meal_plan_uniq;
ALTER TABLE shopping_list_item DROP CONSTRAINT IF EXISTS shopping_list_item_meal_plan_identity_chk;
