-- +goose Up
-- Durable idempotency for the NES-129 kiosk QR deep-link redeem path: a
-- resubmitted POST (double-tap, browser refresh-and-resend, or a request
-- landing within the deep-link rate limiter's own burst) for the SAME
-- signed link must never redeem a reward twice. An in-process guard cannot
-- provide this — it does not survive a process restart and would not be
-- shared across multiple server instances — so the guard lives here, as a
-- database constraint enforced atomically by the SAME transaction that
-- inserts the redemption row (RewardPostgresRepository.RedeemWithDebit).
--
-- deep_link_signature_hash is NULL for every ordinary storefront redemption
-- (RewardService.Redeem) — only the deep-link path
-- (RewardService.RedeemViaDeepLink) ever sets it, to the SHA-256 hash
-- (hex-encoded) of the signed link's canonical decoded signature bytes, not
-- the raw signature itself: a fixed-length, opaque value that never
-- persists anything resembling a live credential.
ALTER TABLE reward_redemption
    ADD COLUMN deep_link_signature_hash text;

-- Partial unique index: only rows that actually came through the deep-link
-- path participate, so ordinary storefront redemptions (deep_link_signature_hash
-- IS NULL) are entirely unaffected and can never collide with one another —
-- Postgres treats every NULL as distinct for uniqueness purposes, but the
-- WHERE clause makes that non-participation explicit and self-documenting
-- rather than relying on that NULL-handling detail alone.
CREATE UNIQUE INDEX reward_redemption_deep_link_signature_uniq
    ON reward_redemption (household_id, deep_link_signature_hash)
    WHERE deep_link_signature_hash IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS reward_redemption_deep_link_signature_uniq;
ALTER TABLE reward_redemption DROP COLUMN IF EXISTS deep_link_signature_hash;
