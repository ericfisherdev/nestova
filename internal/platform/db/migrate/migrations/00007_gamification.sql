-- +goose Up
-- Gamification schema: point ledger, reward catalogue, and reward redemptions.
-- All tables carry household_id and use composite FKs to member and (where
-- applicable) to other gamification tables, matching the tenant-isolation
-- pattern established in 00003/00004. Named constraints and indexes throughout
-- follow the project convention.

-- Append-only ledger: every point award (+) or redemption debit (-) is a row.
-- The adapter (NES-36) reads this table to compute balances and leaderboards.
CREATE TABLE point_ledger (
    id           uuid        PRIMARY KEY,
    household_id uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    member_id    uuid        NOT NULL,
    -- source_type distinguishes the origin: 'task_instance' for task-completion
    -- awards, 'redemption' for reward debits, etc.
    source_type  text        NOT NULL,
    -- source_id is the id of the originating row (task_instance.id,
    -- reward_redemption.id, …). NULL is allowed for manual point adjustments
    -- that have no associated source row.
    source_id    uuid,
    -- points may be negative: redemption debits carry a negative value.
    points       int         NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- Tenant consistency: member must belong to the same household.
    CONSTRAINT point_ledger_member_fk FOREIGN KEY (household_id, member_id)
        REFERENCES member (household_id, id) ON DELETE CASCADE
);

-- Supports per-member balance queries and the leaderboard (household + member +
-- time window). Ordering by created_at inside the index avoids a sort on the
-- common "sum since date" query shape.
CREATE INDEX point_ledger_household_member_idx
    ON point_ledger (household_id, member_id, created_at);

-- Idempotency guard for NES-36's award-on-completion path: at most one
-- task-completion ledger row per task_instance. Partial index so non-task rows
-- (redemptions, manual adjustments with NULL source_id) are never affected.
CREATE UNIQUE INDEX point_ledger_task_completion_uniq
    ON point_ledger (source_id)
    WHERE source_type = 'task_instance';

-- Reward catalogue entry. A reward is household-scoped and can be deactivated
-- (active = false) without deletion so redemption history stays intact.
CREATE TABLE reward (
    id           uuid        PRIMARY KEY,
    household_id uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    cost_points  int         NOT NULL CHECK (cost_points > 0),
    active       boolean     NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    -- Referenced by reward_redemption's composite FK: the child can only point
    -- at a reward in its own household (same pattern as recurring_task_household_id_uniq).
    CONSTRAINT reward_household_id_uniq UNIQUE (household_id, id)
);

-- Supports RewardRepository.ListActiveRewards (active rewards for a household).
-- Partial index keeps it small as rewards are retired.
CREATE INDEX reward_household_active_idx
    ON reward (household_id)
    WHERE active = true;

-- A member's request to redeem a reward. The point debit is a separate
-- point_ledger row appended by the NES-36 use-case in the same transaction.
CREATE TABLE reward_redemption (
    id           uuid        PRIMARY KEY,
    household_id uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    reward_id    uuid        NOT NULL,
    member_id    uuid        NOT NULL,
    -- Status lifecycle: requested → fulfilled or cancelled.
    status       text        NOT NULL DEFAULT 'requested'
        CHECK (status IN ('requested', 'fulfilled', 'cancelled')),
    -- created_at is when the redemption was requested; updated_at records the
    -- most recent status transition (fulfilled/cancelled) for the audit trail.
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    -- Tenant consistency: reward must belong to the same household.
    CONSTRAINT reward_redemption_reward_fk FOREIGN KEY (household_id, reward_id)
        REFERENCES reward (household_id, id) ON DELETE CASCADE,
    -- Tenant consistency: member must belong to the same household.
    CONSTRAINT reward_redemption_member_fk FOREIGN KEY (household_id, member_id)
        REFERENCES member (household_id, id) ON DELETE CASCADE
);

-- Supports per-member redemption history queries ordered by recency.
CREATE INDEX reward_redemption_member_idx
    ON reward_redemption (household_id, member_id, created_at);

-- +goose Down
-- Drop in reverse dependency order so FK constraints are not violated.
DROP TABLE IF EXISTS reward_redemption;
DROP TABLE IF EXISTS reward;
DROP TABLE IF EXISTS point_ledger;
