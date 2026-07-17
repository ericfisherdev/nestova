-- +goose Up
-- Reward redemption fulfillment (NES-127): parents fulfill or deny pending
-- redemptions from an inbox; denial and member self-cancellation refund the
-- debited points via a compensating point_ledger entry (source_type
-- 'redemption_refund') rather than mutating the original debit row.
--
-- status gains 'denied' alongside a rename of the original 'requested' value
-- to 'pending' — the value is renamed, not the semantics: 'requested' always
-- meant exactly what 'pending' now spells out, "awaiting a parent's
-- decision".
--
-- The OLD constraint must be dropped BEFORE the backfill below, not after: on
-- a database that already has reward_redemption rows (i.e. everywhere but a
-- fresh test database), setting status = 'pending' while the old constraint
-- (status IN ('requested', 'fulfilled', 'cancelled')) is still active would
-- itself violate that constraint — 'pending' was never in its allow-list. A
-- migration written the other way round passes every gated test run here
-- (which always starts from an empty schema, so the backfill's UPDATE
-- matches zero rows and has nothing to violate) while still failing outright
-- against the production appliance's real data. See
-- TestUpTo_BackfillsPreExistingRequestedRows for the regression coverage a
-- fresh-database test cannot provide.
ALTER TABLE reward_redemption
    DROP CONSTRAINT reward_redemption_status_check;

UPDATE reward_redemption SET status = 'pending' WHERE status = 'requested';

ALTER TABLE reward_redemption
    ALTER COLUMN status SET DEFAULT 'pending';

ALTER TABLE reward_redemption
    ADD CONSTRAINT reward_redemption_status_check
        CHECK (status IN ('pending', 'fulfilled', 'denied', 'cancelled'));

-- denied_reason is nullable free text set only by RewardRepository.Deny;
-- every other transition leaves it NULL.
ALTER TABLE reward_redemption
    ADD COLUMN denied_reason text;

-- Supports RewardRepository.ListPendingRedemptions (the parent fulfillment
-- inbox): household_id + status equality filter, created_at for the
-- oldest-first ordering. A regular (non-CONCURRENTLY) index is sufficient
-- here — this ALTER TABLE already takes a brief exclusive lock on the table,
-- and reward_redemption lives at the same family-appliance scale
-- chore_trade_household_status_created_idx's rationale (00022) already
-- established for this codebase: a single household's total redemption count
-- over its entire lifetime is expected to stay in the low hundreds at most.
CREATE INDEX reward_redemption_household_status_idx
    ON reward_redemption (household_id, status, created_at);

-- +goose Down
DROP INDEX IF EXISTS reward_redemption_household_status_idx;
ALTER TABLE reward_redemption DROP COLUMN IF EXISTS denied_reason;

-- The currently-active (NES-127) CHECK constraint must be dropped BEFORE the
-- data is backfilled below — 'requested' satisfies neither the current
-- constraint (which doesn't list it) nor the pre-NES-127 one (which isn't
-- restored yet), so no CHECK constraint can be active while this UPDATE runs.
ALTER TABLE reward_redemption DROP CONSTRAINT reward_redemption_status_check;

-- Best-effort inverse, split by what each status actually means:
--   - 'pending' folds back to 'requested' — same meaning, still awaiting a
--     decision, still actionable, points still owed.
--   - 'denied' must NOT fold back to 'requested': a denied redemption's
--     points have already been refunded via a compensating point_ledger
--     entry (RewardRepository.Deny). Restoring it as 'requested' would make
--     it look actionable again to the pre-NES-127 app, which could then
--     fulfill it (delivering a reward whose points were already returned) or
--     — if a future migration ever re-added deny/refund logic downstream —
--     refund it a second time. 'cancelled' is the correct pre-NES-127
--     status for "resolved, no longer actionable, points already settled",
--     which is exactly what a denied-and-refunded redemption is.
UPDATE reward_redemption SET status = 'requested' WHERE status = 'pending';
UPDATE reward_redemption SET status = 'cancelled' WHERE status = 'denied';

ALTER TABLE reward_redemption
    ADD CONSTRAINT reward_redemption_status_check
        CHECK (status IN ('requested', 'fulfilled', 'cancelled'));
ALTER TABLE reward_redemption ALTER COLUMN status SET DEFAULT 'requested';
