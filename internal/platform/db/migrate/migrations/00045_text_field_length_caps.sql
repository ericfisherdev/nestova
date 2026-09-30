-- +goose Up
-- Bound every remaining user-entered text column in the nestova schema
-- (NES-194). NES-172 capped recurring_task.title only; a 10,000-character
-- reward name, album name, recipe title, ingredient, subscription name,
-- shopping item name or kiosk device name was accepted and stored in full.
--
-- The constraints are the backstop, not the primary guard: each domain
-- package rejects an over-length value first and returns a readable message.
-- These exist so a caller that bypasses the service layer still cannot write
-- one. Every number here must match its Go constant:
--   reward.name                     tasks.MaxRewardNameLength
--   reward.description              tasks.MaxRewardDescriptionLength
--   album.name                      media.MaxAlbumNameLength
--   recipe.title                    meals.MaxRecipeTitleLength
--   ingredient.canonical_name       tracking.MaxIngredientNameLength
--   tracked_item.name               tracking.MaxTrackedItemNameLength
--   subscription.name               subscriptions.MaxNameLength
--   shopping_list_item.name         tracking.MaxShoppingListItemNameLength
--   kiosk_device.name and
--   kiosk_activation_code.name      kiosk.MaxDeviceNameLength
--
-- identity.household.name, identity.member.display_name and
-- identity.member_credential.nickname are owned by nestcore, whose migrations
-- carry their constraints; the domain caps for those three live in Go.
--
-- char_length counts characters, not bytes, matching the domain's rune count.

-- Existing rows come first: ADD CONSTRAINT validates immediately, so a database
-- already holding an over-length value would fail this migration on exactly
-- the installations that need it. Truncating to the new bound is the only
-- forward-compatible choice; left() counts characters, so a multi-byte value
-- keeps whole runes.
UPDATE reward SET name = left(name, 200) WHERE char_length(name) > 200;
UPDATE reward SET description = left(description, 1000) WHERE char_length(description) > 1000;
UPDATE album SET name = left(name, 200) WHERE char_length(name) > 200;
UPDATE recipe SET title = left(title, 200) WHERE char_length(title) > 200;
UPDATE subscription SET name = left(name, 200) WHERE char_length(name) > 200;
UPDATE tracked_item SET name = left(name, 200) WHERE char_length(name) > 200;
UPDATE shopping_list_item SET name = left(name, 200) WHERE char_length(name) > 200;
UPDATE kiosk_device SET name = left(name, 200) WHERE char_length(name) > 200;
UPDATE kiosk_activation_code SET name = left(name, 200) WHERE char_length(name) > 200;

ALTER TABLE reward
    ADD CONSTRAINT reward_name_length CHECK (char_length(name) <= 200),
    ADD CONSTRAINT reward_description_length CHECK (char_length(description) <= 1000);
ALTER TABLE album
    ADD CONSTRAINT album_name_length CHECK (char_length(name) <= 200);
ALTER TABLE recipe
    ADD CONSTRAINT recipe_title_length CHECK (char_length(title) <= 200);
ALTER TABLE subscription
    ADD CONSTRAINT subscription_name_length CHECK (char_length(name) <= 200);
ALTER TABLE shopping_list_item
    ADD CONSTRAINT shopping_list_item_name_length CHECK (char_length(name) <= 200);
ALTER TABLE tracked_item
    ADD CONSTRAINT tracked_item_name_length CHECK (char_length(name) <= 200);
ALTER TABLE kiosk_device
    ADD CONSTRAINT kiosk_device_name_length CHECK (char_length(name) <= 200);
ALTER TABLE kiosk_activation_code
    ADD CONSTRAINT kiosk_activation_code_name_length CHECK (char_length(name) <= 200);

-- ingredient.canonical_name is the exception: it is UNIQUE and carries a
-- canonical-form CHECK (no edge whitespace), so truncating an existing row
-- could collide with another or leave a trailing space. NOT VALID enforces the
-- bound on every new write and skips the rows already stored.
ALTER TABLE ingredient
    ADD CONSTRAINT ingredient_canonical_name_length
    CHECK (char_length(canonical_name) <= 200) NOT VALID;

-- +goose Down
-- Dropping the constraints restores the old unbounded columns; the truncation
-- above is not reversible, and this deliberately does not try to fake it.
ALTER TABLE ingredient DROP CONSTRAINT IF EXISTS ingredient_canonical_name_length;
ALTER TABLE kiosk_activation_code DROP CONSTRAINT IF EXISTS kiosk_activation_code_name_length;
ALTER TABLE tracked_item DROP CONSTRAINT IF EXISTS tracked_item_name_length;
ALTER TABLE kiosk_device DROP CONSTRAINT IF EXISTS kiosk_device_name_length;
ALTER TABLE shopping_list_item DROP CONSTRAINT IF EXISTS shopping_list_item_name_length;
ALTER TABLE subscription DROP CONSTRAINT IF EXISTS subscription_name_length;
ALTER TABLE recipe DROP CONSTRAINT IF EXISTS recipe_title_length;
ALTER TABLE album DROP CONSTRAINT IF EXISTS album_name_length;
ALTER TABLE reward DROP CONSTRAINT IF EXISTS reward_description_length;
ALTER TABLE reward DROP CONSTRAINT IF EXISTS reward_name_length;
