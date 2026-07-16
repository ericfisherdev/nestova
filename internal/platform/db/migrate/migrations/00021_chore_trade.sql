-- +goose Up
-- Chore trade domain (NES-121): a household member proposes trading their
-- pending chore (offered_instance_id) for another member's pending chore
-- (requested_instance_id); the other member must accept before either
-- assignee changes. status walks a small state machine mirrored by
-- domain.TradeStatus.CanTransitionTo: proposed -> {accepted, declined,
-- cancelled, expired}; every other status is terminal.

-- Referenced by the composite tenant FKs below, mirroring
-- recurring_task_household_id_uniq (00003_tasks.sql). task_instance had no
-- such constraint before this ticket — nothing previously needed to
-- reference a task_instance row by composite key.
ALTER TABLE task_instance
    ADD CONSTRAINT task_instance_household_id_id_uniq UNIQUE (household_id, id);

CREATE TABLE chore_trade (
    id                     uuid        PRIMARY KEY,
    household_id           uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    proposer_id            uuid        NOT NULL,
    responder_id           uuid        NOT NULL,
    offered_instance_id    uuid        NOT NULL,
    requested_instance_id  uuid        NOT NULL,
    status                 text        NOT NULL DEFAULT 'proposed'
        CHECK (status IN ('proposed', 'accepted', 'declined', 'cancelled', 'expired')),
    created_at             timestamptz NOT NULL DEFAULT now(),
    -- resolved_at is populated the moment status leaves 'proposed' (accept,
    -- decline, cancel, or the expiry sweep) and stays NULL while a trade is
    -- still live.
    resolved_at            timestamptz,
    -- expires_at is computed once at propose time as the earlier of the two
    -- instances' due_on (both instances are kind='scheduled' per
    -- IsInstanceTradeable, so both are guaranteed to have one). It is never
    -- recomputed: a per-instance due-date change after proposing does not
    -- retroactively move an already-ticking trade's expiry.
    expires_at             timestamptz NOT NULL,
    -- Tenant consistency: both members and both instances must belong to the
    -- trade's own household. NOT NULL member columns use ON DELETE CASCADE
    -- (a deleted member's trades are removed with them), matching
    -- rotation_member_member_fk's precedent for a NOT NULL member reference
    -- (00003_tasks.sql) rather than task_instance's ON DELETE SET NULL
    -- pattern, which only applies to nullable assignee/claim columns.
    CONSTRAINT chore_trade_proposer_fk FOREIGN KEY (household_id, proposer_id)
        REFERENCES member (household_id, id) ON DELETE CASCADE,
    CONSTRAINT chore_trade_responder_fk FOREIGN KEY (household_id, responder_id)
        REFERENCES member (household_id, id) ON DELETE CASCADE,
    CONSTRAINT chore_trade_offered_instance_fk FOREIGN KEY (household_id, offered_instance_id)
        REFERENCES task_instance (household_id, id) ON DELETE CASCADE,
    CONSTRAINT chore_trade_requested_instance_fk FOREIGN KEY (household_id, requested_instance_id)
        REFERENCES task_instance (household_id, id) ON DELETE CASCADE,
    -- A member cannot trade with themselves (domain.ErrTradeSelf enforces
    -- this ahead of the database at the TradeService layer; this CHECK is the
    -- schema-level backstop).
    CONSTRAINT chore_trade_proposer_not_responder CHECK (proposer_id <> responder_id),
    -- An instance cannot be offered and requested by the same trade.
    CONSTRAINT chore_trade_offered_not_requested CHECK (offered_instance_id <> requested_instance_id),
    -- resolved_at is set if and only if the trade has left the proposed state,
    -- mirroring task_instance_done_completed_at's directional-pair pattern.
    CONSTRAINT chore_trade_resolved_matches_status
        CHECK ((status = 'proposed') = (resolved_at IS NULL))
);

-- At most one LIVE (status = 'proposed') trade may reference a given instance
-- as the offered side, and likewise for the requested side. This is a
-- per-column pair rather than a single cross-column constraint: an instance
-- offered in one live trade is not additionally prevented from being
-- requested by a different live trade at the exact same instant by these
-- indexes alone. ChoreTradeRepository.Propose closes most of that gap with an
-- explicit locking SELECT across both columns before inserting; these indexes
-- remain the atomic, always-correct backstop for the common case (the same
-- instance proposed twice in the same role), which is what NES-121's
-- acceptance criteria require.
CREATE UNIQUE INDEX chore_trade_offered_live_uniq
    ON chore_trade (offered_instance_id)
    WHERE status = 'proposed';
CREATE UNIQUE INDEX chore_trade_requested_live_uniq
    ON chore_trade (requested_instance_id)
    WHERE status = 'proposed';

-- Supports the background sweep's "find expired live trades" query
-- (ChoreTradeRepository.SweepExpiredTrades), mirroring
-- task_instance_claim_expires_idx's partial-index shape.
CREATE INDEX chore_trade_expires_idx
    ON chore_trade (expires_at)
    WHERE status = 'proposed';

-- +goose Down
DROP INDEX IF EXISTS chore_trade_expires_idx;
DROP INDEX IF EXISTS chore_trade_requested_live_uniq;
DROP INDEX IF EXISTS chore_trade_offered_live_uniq;
DROP TABLE IF EXISTS chore_trade;
ALTER TABLE task_instance DROP CONSTRAINT IF EXISTS task_instance_household_id_id_uniq;
