-- +goose Up
-- Reward catalogue admin (NES-126): parents can create/edit/archive rewards,
-- and the member storefront gains a description, an optional image/emoji
-- reference, and finite stock tracking.
--
-- description defaults to '' (NOT NULL) so every existing row (and every
-- future INSERT that omits it) has a concrete, renderable value — matching
-- the temporary-DEFAULT backfill convention from 00023.
-- image_ref is nullable: a reward may have no image/emoji token at all (v1
-- keeps this a simple optional text field per NES-126 — see reward.go).
-- quantity_available is nullable INT where NULL means "unlimited stock"; a
-- non-NULL value must be zero or greater (0 = currently sold out).
ALTER TABLE reward
    ADD COLUMN description        text NOT NULL DEFAULT '',
    ADD COLUMN image_ref          text,
    ADD COLUMN quantity_available int  CHECK (quantity_available IS NULL OR quantity_available >= 0);

-- Data-integrity fix (NES-126 AC5): reward_redemption_reward_fk was created in
-- 00007 as ON DELETE CASCADE, which would silently destroy a household's
-- redemption history the moment a reward row was deleted. The reward admin
-- (NES-126) never hard-deletes a reward that has redemptions — only archives
-- (active = false) — so the FK is tightened to ON DELETE RESTRICT: any
-- attempt to delete a reward with existing redemptions now fails at the
-- database, and RewardPostgresRepository.DeleteReward maps that failure to
-- domain.ErrRewardHasRedemptions.
ALTER TABLE reward_redemption DROP CONSTRAINT reward_redemption_reward_fk;
ALTER TABLE reward_redemption
    ADD CONSTRAINT reward_redemption_reward_fk FOREIGN KEY (household_id, reward_id)
        REFERENCES reward (household_id, id) ON DELETE RESTRICT;

-- +goose Down
ALTER TABLE reward_redemption DROP CONSTRAINT reward_redemption_reward_fk;
ALTER TABLE reward_redemption
    ADD CONSTRAINT reward_redemption_reward_fk FOREIGN KEY (household_id, reward_id)
        REFERENCES reward (household_id, id) ON DELETE CASCADE;

ALTER TABLE reward
    DROP COLUMN IF EXISTS description,
    DROP COLUMN IF EXISTS image_ref,
    DROP COLUMN IF EXISTS quantity_available;
