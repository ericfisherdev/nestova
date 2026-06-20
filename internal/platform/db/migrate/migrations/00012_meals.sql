-- +goose Up
-- Meals & Recipes (NES-5): the recipe box, recipe ingredient lines normalized to
-- the shared catalogue (NES-38), and the weekly meal planner. A recipe row is
-- either household-owned (household_id set, source='local') or external/cached and
-- shared across households (household_id NULL, source='external', external_ref
-- set), distinguished by the RecipeSourceKind enum mirrored as a CHECK.
CREATE TABLE recipe (
    id            uuid        PRIMARY KEY,
    -- NULL marks an external/cached recipe shared across households; a set value
    -- marks a household-owned box recipe. CASCADE so deleting a household drops its
    -- recipes (external rows, being household-agnostic, are unaffected).
    household_id  uuid        REFERENCES household (id) ON DELETE CASCADE,
    title         text        NOT NULL CHECK (btrim(title) <> ''),
    source        text        NOT NULL CHECK (source IN ('local', 'external')),
    -- Provider id for an external/cached recipe; NULL for local recipes. UNIQUE so
    -- the external cache upserts race-safely via ON CONFLICT (external_ref); NULLs
    -- are distinct, so local recipes never collide here.
    external_ref  text        UNIQUE,
    servings      integer     NOT NULL CHECK (servings > 0),
    instructions  text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    -- A local recipe is household-owned with no external ref; an external recipe is
    -- household-agnostic and carries one. Forbids either kind being malformed.
    CONSTRAINT recipe_source_identity_chk CHECK (
        (source = 'local'    AND household_id IS NOT NULL AND external_ref IS NULL)
     OR (source = 'external' AND household_id IS NULL     AND external_ref IS NOT NULL)
    ),
    -- external_ref is the cache key for external recipes (the upsert keys on it),
    -- so an untrimmed/empty value would split the cache: enforce it normalized,
    -- mirroring the ingredient.canonical_name CHECK.
    CONSTRAINT recipe_external_ref_normalized_chk CHECK (
        external_ref IS NULL OR (external_ref <> '' AND external_ref = btrim(external_ref))
    ),
    -- Backs the same-household composite FK from meal_plan_entry.
    CONSTRAINT recipe_household_id_uniq UNIQUE (household_id, id)
);

-- Supports the household recipe-box listing; partial so it ignores external rows.
CREATE INDEX recipe_household_idx ON recipe (household_id) WHERE household_id IS NOT NULL;

-- Normalized ingredient lines for a recipe, keyed to the shared catalogue
-- (NES-38). Composite PK so an ingredient appears at most once per recipe.
CREATE TABLE recipe_ingredient (
    recipe_id     uuid        NOT NULL REFERENCES recipe (id) ON DELETE CASCADE,
    -- Catalogue ingredient; no ON DELETE action — a referenced ingredient must not
    -- be removed out from under a recipe (mirrors pantry_item / shopping_list_item).
    ingredient_id uuid        NOT NULL REFERENCES ingredient (id),
    -- A recipe line uses a positive amount; zero would not describe an ingredient.
    quantity      numeric     NOT NULL CHECK (quantity > 0),
    unit          text        NOT NULL CHECK (unit IN ('count', 'g', 'kg', 'ml', 'l')),
    -- Optional lines never count against an ingredient-finder "missing" set.
    optional      boolean     NOT NULL DEFAULT false,
    PRIMARY KEY (recipe_id, ingredient_id)
);

-- Supports ingredient-driven recipe discovery (NES-58: find recipes that use
-- ingredient X). The composite PK leads with recipe_id, so reverse lookups by
-- ingredient_id need their own index.
CREATE INDEX recipe_ingredient_ingredient_idx ON recipe_ingredient (ingredient_id);

-- Weekly meal planner (NES-5): assigns one household recipe to a (date, meal) slot.
CREATE TABLE meal_plan_entry (
    id            uuid        PRIMARY KEY,
    household_id  uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    plan_date     date        NOT NULL,
    meal          text        NOT NULL
        CHECK (meal IN ('breakfast', 'lunch', 'dinner', 'snack')),
    recipe_id     uuid        NOT NULL,
    servings      integer     NOT NULL CHECK (servings > 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- One entry per slot: a (date, meal) holds a single recipe per household.
    CONSTRAINT meal_plan_entry_slot_uniq UNIQUE (household_id, plan_date, meal),
    -- Same-household planning: the recipe must belong to this household. Because
    -- external/cached recipes carry a NULL household_id they can never match the
    -- NOT NULL household_id here, so only box recipes are plannable. CASCADE so
    -- deleting a recipe clears its planned slots.
    CONSTRAINT meal_plan_entry_recipe_fk FOREIGN KEY (household_id, recipe_id)
        REFERENCES recipe (household_id, id) ON DELETE CASCADE
);

-- Supports reading a household's plan for a week (date-range scan).
CREATE INDEX meal_plan_entry_household_date_idx ON meal_plan_entry (household_id, plan_date);

-- Backs the recipe composite-FK ON DELETE CASCADE (deleting a recipe clears its
-- planned slots) and "which plans use this recipe" lookups; Postgres does not
-- auto-index the referencing columns.
CREATE INDEX meal_plan_entry_recipe_idx ON meal_plan_entry (recipe_id);

-- +goose Down
DROP TABLE IF EXISTS meal_plan_entry;
DROP TABLE IF EXISTS recipe_ingredient;
DROP TABLE IF EXISTS recipe;
