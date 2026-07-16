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

-- chore_trade_reservation enforces "at most one LIVE (status = 'proposed')
-- trade may reference a given instance, in EITHER role (offered or
-- requested)" as a single schema-level invariant: instance_id's PRIMARY KEY
-- means at most one row — and therefore at most one live trade — can ever
-- reserve a given instance, full stop, regardless of which column
-- (offered_instance_id vs requested_instance_id) referenced it. This
-- supersedes an earlier design that used two separate per-column partial
-- unique indexes (chore_trade_offered_live_uniq / chore_trade_requested_live_
-- uniq): those only ever compared offered-vs-offered and requested-vs-
-- requested, so the SAME instance could still be offered by one live trade
-- and simultaneously requested by a different live trade — a real gap for
-- ANY writer, not just a repository-discipline concern (a raw INSERT INTO
-- chore_trade that bypassed ChoreTradeRepository.Propose entirely could
-- create exactly that inconsistent state). A single PRIMARY KEY on
-- instance_id closes the gap for every writer, present or future, without
-- needing the btree_gist extension an EXCLUDE constraint would require.
--
-- household_id is denormalized here purely so the composite tenant FK below
-- can enforce that a reservation's instance actually belongs to the
-- reservation's own household — it plays no role in the uniqueness
-- guarantee, which is instance_id alone.
CREATE TABLE chore_trade_reservation (
    instance_id  uuid NOT NULL,
    household_id uuid NOT NULL,
    trade_id     uuid NOT NULL,
    CONSTRAINT chore_trade_reservation_pkey PRIMARY KEY (instance_id),
    CONSTRAINT chore_trade_reservation_instance_fk FOREIGN KEY (household_id, instance_id)
        REFERENCES task_instance (household_id, id) ON DELETE CASCADE,
    CONSTRAINT chore_trade_reservation_trade_fk FOREIGN KEY (trade_id)
        REFERENCES chore_trade (id) ON DELETE CASCADE
);
-- Child-side index for the trade_id FK, so the AFTER UPDATE trigger's
-- "delete this trade's reservations" DELETE (keyed on trade_id) doesn't
-- table-scan, mirroring notification_household_member_idx's precedent
-- (00001_baseline.sql).
CREATE INDEX chore_trade_reservation_trade_id_idx ON chore_trade_reservation (trade_id);

-- chore_trade_reservation_sync keeps chore_trade_reservation in lockstep with
-- chore_trade.status, inside the SAME transaction as every status-changing
-- statement — this transactional coupling is what lets
-- ChoreTradeRepository.Propose's hasLiveTradeProposal pre-check (a plain,
-- non-locking read of chore_trade) stay perfectly synchronized with the
-- reservation table's hard PRIMARY KEY enforcement: whichever transaction
-- commits chore_trade.status = 'proposed' also commits that trade's two
-- reservation rows atomically, and whichever transaction commits chore_trade
-- leaving 'proposed' also atomically frees them in the same commit. There is
-- no instant where hasLiveTradeProposal could observe "not live" while a
-- reservation row for that same trade is still concurrently in flight (see
-- TestTrade_ProposeVsAccept_NoDeadlock's doc for the full argument this
-- relies on to stay deadlock-free).
--
--   - AFTER INSERT: a newly proposed trade (status = 'proposed', the only
--     status a fresh row is ever inserted with — the IF is defensive) reserves
--     both of its instances. A conflict here — one of the two instances
--     already reserved by a DIFFERENT live trade, in either role — raises a
--     unique_violation on chore_trade_reservation_pkey, which aborts the
--     entire proposing transaction (including the chore_trade insert itself).
--   - AFTER UPDATE: the moment a trade's status leaves 'proposed' (accept,
--     decline, cancel, or the expiry sweep — whichever caused it), its two
--     reservation rows are freed, making both instances available to a future
--     trade again.
-- +goose StatementBegin
CREATE FUNCTION chore_trade_reservation_sync() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status = 'proposed' THEN
            INSERT INTO chore_trade_reservation (instance_id, household_id, trade_id)
            VALUES (NEW.offered_instance_id, NEW.household_id, NEW.id),
                   (NEW.requested_instance_id, NEW.household_id, NEW.id);
        END IF;
        RETURN NEW;
    END IF;

    -- TG_OP = 'UPDATE'.
    IF OLD.status = 'proposed' AND NEW.status <> 'proposed' THEN
        DELETE FROM chore_trade_reservation WHERE trade_id = NEW.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER chore_trade_reservation_sync_trigger
    AFTER INSERT OR UPDATE ON chore_trade
    FOR EACH ROW
    EXECUTE FUNCTION chore_trade_reservation_sync();

-- Supports the background sweep's "find expired live trades" query
-- (ChoreTradeRepository.SweepExpiredTrades), mirroring
-- task_instance_claim_expires_idx's partial-index shape.
CREATE INDEX chore_trade_expires_idx
    ON chore_trade (expires_at)
    WHERE status = 'proposed';

-- +goose Down
DROP INDEX IF EXISTS chore_trade_expires_idx;
DROP TRIGGER IF EXISTS chore_trade_reservation_sync_trigger ON chore_trade;
DROP FUNCTION IF EXISTS chore_trade_reservation_sync();
DROP TABLE IF EXISTS chore_trade_reservation;
DROP TABLE IF EXISTS chore_trade;
ALTER TABLE task_instance DROP CONSTRAINT IF EXISTS task_instance_household_id_id_uniq;
