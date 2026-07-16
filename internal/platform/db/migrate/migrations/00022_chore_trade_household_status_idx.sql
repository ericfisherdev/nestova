-- +goose NO TRANSACTION
-- NES-122 CodeRabbit follow-up: chore_trade had no index on household_id at
-- all — only the id PRIMARY KEY and the partial chore_trade_expires_idx
-- (expires_at, WHERE status = 'proposed') used by the background sweep.
-- Three request-time reads now filter by household_id first:
--   - ChoreTradeRepository.ListPendingByMember: household_id = $1 AND
--     status = 'proposed' AND (proposer_id = $2 OR responder_id = $2) — the
--     dashboard's hot path, run on every page load.
--   - ChoreTradeRepository.ListHistory: household_id = $1, ORDER BY
--     created_at DESC LIMIT 50 — the parent-only history page.
--   - ChoreTradeRepository.Get: id = $1 AND household_id = $2 (already
--     served by the id PRIMARY KEY; household_id only narrows further).
--
-- (household_id, status, created_at DESC) serves ListPendingByMember
-- directly (both leading columns are equality-filtered; the proposer/
-- responder OR is then applied to an already-tiny per-household result).
-- It also serves ListHistory adequately: Postgres can use the household_id
-- prefix to narrow the scan before sorting by created_at, even though
-- ListHistory does not filter on status. A second index without the status
-- column would fit ListHistory's shape marginally better, but at this
-- application's scale (a single household's total trade count over its
-- entire lifetime is expected to stay in the low hundreds at most — this is
-- a family chore-tracking appliance, not a multi-tenant SaaS product) one
-- well-chosen composite index is enough; a dedicated second index would
-- only add write-side maintenance cost for a benefit that never becomes
-- measurable here.
--
-- No new task_instance index: the picker query
-- (TaskInstanceRepository.ListTradeableAssignedToOthers) filters
-- household_id = $1 AND status = 'pending' AND kind = 'scheduled' AND
-- claimed_by IS NULL AND due_on IS NOT NULL AND assignee_id <> $2, ORDER BY
-- due_on. The existing task_instance_due_idx (household_id, status, due_on)
-- from 00003_tasks.sql already covers this query's two leading equality
-- columns AND its ORDER BY column; the remaining predicates (kind,
-- claimed_by, assignee_id) are then applied to the already-small per-
-- household candidate set that index scan produces. Adding a wider
-- composite index here would not measurably improve this query at family
-- scale and was assessed and rejected rather than added speculatively.
--
-- NO TRANSACTION because the index is built CONCURRENTLY, matching
-- 00006/00020's rationale: CONCURRENTLY cannot run inside a transaction, so
-- this keeps the migration from blocking writes on chore_trade during
-- deploy. The index is DROPped before being (re)created rather than guarded
-- with CREATE INDEX CONCURRENTLY IF NOT EXISTS, for the same reason those
-- migrations do: an interrupted CONCURRENTLY build leaves behind an INVALID
-- index that IF NOT EXISTS would see and skip, permanently keeping the
-- unusable index.

-- +goose Up
DROP INDEX IF EXISTS chore_trade_household_status_created_idx;
CREATE INDEX CONCURRENTLY chore_trade_household_status_created_idx
    ON chore_trade (household_id, status, created_at DESC);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS chore_trade_household_status_created_idx;
