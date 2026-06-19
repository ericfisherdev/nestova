-- +goose Up
-- Shared ingredient catalogue (NES-38). Ingredients are household-agnostic: a
-- single canonical entry ("flour", "tomato") is referenced by every household's
-- pantry and shopping list. canonical_name is the normalized primary name and is
-- unique so EnsureIngredient can upsert race-safely via ON CONFLICT; aliases
-- holds additional normalized names that resolve to the same ingredient.
CREATE TABLE ingredient (
    id             uuid        PRIMARY KEY,
    canonical_name text        NOT NULL UNIQUE,
    aliases        text[]      NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Supports the resolver's alias/plural lookup, which matches a candidate set
-- against aliases with the array-overlap operator (aliases && $1). A GIN index
-- keeps that overlap test from scanning the whole table as the catalogue grows.
CREATE INDEX ingredient_aliases_gin ON ingredient USING gin (aliases);

-- +goose Down
DROP TABLE IF EXISTS ingredient;
