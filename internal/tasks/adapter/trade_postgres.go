package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// Constraint names from 00021_chore_trade.sql used to map database errors to
// domain sentinels (NES-121).
const (
	// constraintChoreTradeOfferedLiveUniq is the partial unique index on
	// offered_instance_id WHERE status = 'proposed' whose violation maps to
	// domain.ErrInstanceNotTradeable.
	constraintChoreTradeOfferedLiveUniq = "chore_trade_offered_live_uniq"
	// constraintChoreTradeRequestedLiveUniq is the partial unique index on
	// requested_instance_id WHERE status = 'proposed' whose violation maps to
	// domain.ErrInstanceNotTradeable.
	constraintChoreTradeRequestedLiveUniq = "chore_trade_requested_live_uniq"
)

// TradeRepository is the pgx-backed implementation of
// domain.ChoreTradeRepository (NES-121). UUIDs are passed and scanned as text
// so no pgx UUID codec registration is required.
type TradeRepository struct {
	dbtx db.TX
}

// Compile-time assurance that TradeRepository satisfies the port.
var _ domain.ChoreTradeRepository = (*TradeRepository)(nil)

// NewTradeRepository constructs a TradeRepository with an injected query
// executor. The executor is a db.TX, satisfied by both *pgxpool.Pool (the
// default composition) and pgx.Tx.
func NewTradeRepository(dbtx db.TX) *TradeRepository {
	if dbtx == nil {
		panic("adapter: NewTradeRepository requires a non-nil db.TX")
	}
	return &TradeRepository{dbtx: dbtx}
}

// Propose validates and persists a new trade proposal in a single
// transaction. See domain.ChoreTradeRepository.Propose for the full
// contract. The two referenced instances are locked FOR UPDATE, ordered by
// id, so two concurrent Propose calls that reference overlapping instances
// (in either order) cannot deadlock against each other. Propose never takes
// an explicit lock on any chore_trade row (see hasLiveTradeProposal and the
// lock-ordering convention documented on domain.ChoreTradeRepository), so it
// also cannot deadlock against a concurrent Accept, which locks its
// chore_trade row before the instances it swaps.
func (r *TradeRepository) Propose(
	ctx context.Context,
	householdID household.HouseholdID,
	trade *domain.ChoreTrade,
) error {
	if trade == nil {
		return errors.New("adapter: propose trade: nil trade")
	}

	tx, err := beginTx(ctx, r.dbtx, "propose trade")
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	offered, requested, err := lockTradeInstances(ctx, tx, householdID, trade.OfferedInstanceID, trade.RequestedInstanceID)
	if err != nil {
		return fmt.Errorf("propose trade: %w", err)
	}

	if !domain.IsInstanceTradeable(offered) || !domain.IsInstanceTradeable(requested) {
		return fmt.Errorf("propose trade: %w", domain.ErrInstanceNotTradeable)
	}
	if offered.AssigneeID == nil || *offered.AssigneeID != trade.ProposerID {
		return fmt.Errorf("propose trade: %w", domain.ErrNotYourChore)
	}
	if requested.AssigneeID == nil || *requested.AssigneeID != trade.ResponderID {
		return fmt.Errorf("propose trade: %w", domain.ErrNotYourChore)
	}

	live, err := hasLiveTradeProposal(ctx, tx, householdID, trade.OfferedInstanceID, trade.RequestedInstanceID)
	if err != nil {
		return fmt.Errorf("propose trade: %w", err)
	}
	if live {
		return fmt.Errorf("propose trade: %w", domain.ErrInstanceNotTradeable)
	}

	expiresAt := earlierDueOn(offered, requested)

	const insertQ = `
		INSERT INTO chore_trade
			(id, household_id, proposer_id, responder_id, offered_instance_id, requested_instance_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING status, created_at`
	var statusStr string
	err = tx.QueryRow(ctx, insertQ,
		trade.ID.String(),
		householdID.String(),
		trade.ProposerID.String(),
		trade.ResponderID.String(),
		trade.OfferedInstanceID.String(),
		trade.RequestedInstanceID.String(),
		expiresAt,
	).Scan(&statusStr, &trade.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == sqlstateUniqueViolation &&
			(pgErr.ConstraintName == constraintChoreTradeOfferedLiveUniq ||
				pgErr.ConstraintName == constraintChoreTradeRequestedLiveUniq) {
			return fmt.Errorf("propose trade: %w", domain.ErrInstanceNotTradeable)
		}
		return fmt.Errorf("propose trade: insert: %w", err)
	}

	status, err := domain.ParseTradeStatus(statusStr)
	if err != nil {
		return fmt.Errorf("propose trade: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("propose trade: commit: %w", err)
	}

	trade.HouseholdID = householdID
	trade.Status = status
	trade.ExpiresAt = expiresAt
	return nil
}

// lockTradeInstances SELECTs and FOR UPDATE-locks the two task_instance rows
// identified by offeredID and requestedID, scoped to householdID, ordered by
// id to give every caller (concurrent or not) a consistent lock-acquisition
// order and so avoid deadlocking against another Propose call that references
// an overlapping pair of instances.
//
// Returns domain.ErrInstanceNotFound when either id is unknown, belongs to
// another household, or (degenerately) offeredID equals requestedID and no
// second row exists to satisfy the pair.
func lockTradeInstances(
	ctx context.Context,
	tx pgx.Tx,
	householdID household.HouseholdID,
	offeredID, requestedID domain.TaskInstanceID,
) (*domain.TaskInstance, *domain.TaskInstance, error) {
	const q = `
		SELECT id, household_id, recurring_task_id, assignee_id,
		       due_on, status, completed_at, completed_by,
		       created_at, updated_at, kind,
		       claimed_by, claimed_at, claim_expires_at, claim_warned_at
		  FROM task_instance
		 WHERE household_id = $1
		   AND id = ANY($2::uuid[])
		 ORDER BY id
		 FOR UPDATE`
	rows, err := tx.Query(ctx, q, householdID.String(), []string{offeredID.String(), requestedID.String()})
	if err != nil {
		return nil, nil, fmt.Errorf("lock trade instances: %w", err)
	}
	defer rows.Close()

	found := make(map[domain.TaskInstanceID]*domain.TaskInstance, 2)
	for rows.Next() {
		inst, err := scanTaskInstance(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("lock trade instances: scan: %w", err)
		}
		found[inst.ID] = inst
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("lock trade instances: %w", err)
	}

	offered, ok := found[offeredID]
	if !ok {
		return nil, nil, domain.ErrInstanceNotFound
	}
	requested, ok := found[requestedID]
	if !ok {
		return nil, nil, domain.ErrInstanceNotFound
	}
	return offered, requested, nil
}

// hasLiveTradeProposal reports whether either offeredID or requestedID
// already appears — in EITHER role (offered or requested) — on a live
// (status = 'proposed') chore_trade row within householdID.
//
// This is a plain (non-locking) read: it exists only to give Propose a
// friendly, early ErrInstanceNotTradeable rather than always falling through
// to a raw unique-constraint violation. It is NOT the source of correctness
// for "at most one live proposal per instance" — chore_trade_offered_live_uniq
// and chore_trade_requested_live_uniq (the two partial unique indexes) are the
// sole atomic backstop for that guarantee, catching same-column collisions
// even when this early read races another transaction's not-yet-committed
// insert (see the migration's doc comment for the known cross-column gap
// those indexes don't close).
//
// Deliberately no FOR UPDATE: an earlier version locked the matched rows,
// which meant Propose — already holding locks on the two task_instance rows
// from lockTradeInstances — would then try to acquire a lock on an existing
// chore_trade row too. Accept acquires those same two lock types in the
// OPPOSITE order (its own chore_trade row via its UPDATE, then the
// task_instance rows via the swap), so a concurrent Propose referencing an
// already-live trade's instance and a concurrent Accept of that same trade
// could deadlock. A plain, non-blocking MVCC read never enters that lock
// wait at all, which is what the lock-ordering convention documented on
// domain.ChoreTradeRepository requires of Propose. See
// TestTrade_ProposeVsAccept_NoDeadlock for the regression coverage.
func hasLiveTradeProposal(
	ctx context.Context,
	tx pgx.Tx,
	householdID household.HouseholdID,
	offeredID, requestedID domain.TaskInstanceID,
) (bool, error) {
	const q = `
		SELECT 1
		  FROM chore_trade
		 WHERE household_id = $1
		   AND status = 'proposed'
		   AND (offered_instance_id = ANY($2::uuid[]) OR requested_instance_id = ANY($2::uuid[]))
		 LIMIT 1`
	ids := []string{offeredID.String(), requestedID.String()}
	var found int
	err := tx.QueryRow(ctx, q, householdID.String(), ids).Scan(&found)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("check live trade proposal: %w", err)
	}
	return true, nil
}

// earlierDueOn returns the earlier of offered's and requested's DueOn. Callers
// must only invoke this after confirming domain.IsInstanceTradeable for both
// instances, which guarantees kind = scheduled and therefore a non-nil DueOn
// on each (see validateInstanceKindDueOn's insert-time invariant).
func earlierDueOn(offered, requested *domain.TaskInstance) time.Time {
	o, req := *offered.DueOn, *requested.DueOn
	if o.Before(req) {
		return o
	}
	return req
}

// Get returns the trade identified by id within the household, or
// domain.ErrTradeNotFound when id is unknown or belongs to another household.
func (r *TradeRepository) Get(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.ChoreTradeID,
) (*domain.ChoreTrade, error) {
	const q = `
		SELECT id, household_id, proposer_id, responder_id,
		       offered_instance_id, requested_instance_id,
		       status, created_at, resolved_at, expires_at
		  FROM chore_trade
		 WHERE id = $1
		   AND household_id = $2`
	trade, err := scanChoreTrade(r.dbtx.QueryRow(ctx, q, id.String(), householdID.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTradeNotFound
		}
		return nil, fmt.Errorf("get trade: %w", err)
	}
	return trade, nil
}

// Accept atomically re-validates both instances and, if they still qualify,
// swaps their assignees, marks the trade accepted, and returns the
// notification payload — all within one transaction. See
// domain.ChoreTradeRepository.Accept for the full contract, including the
// expires_at > at deadline check this method's UPDATE enforces.
func (r *TradeRepository) Accept(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.ChoreTradeID,
	responderID household.MemberID,
	at time.Time,
) (domain.AcceptedTrade, error) {
	tx, err := beginTx(ctx, r.dbtx, "accept trade")
	if err != nil {
		return domain.AcceptedTrade{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// expires_at > $4 rejects an accept attempted at or after the trade's
	// deadline, without waiting for SweepExpiredTrades to flip status to
	// 'expired' first — see the lock-ordering/deadline doc on
	// domain.ChoreTradeRepository.Accept for why the two predicates
	// (this one and SweepExpiredTrades' expires_at <= asOf) are exact
	// complements.
	const resolveQ = `
		UPDATE chore_trade
		   SET status = 'accepted', resolved_at = $4
		 WHERE id           = $1
		   AND household_id = $2
		   AND responder_id = $3
		   AND status       = 'proposed'
		   AND expires_at   > $4
		RETURNING proposer_id, offered_instance_id, requested_instance_id`
	var proposerIDStr, offeredIDStr, requestedIDStr string
	err = tx.QueryRow(ctx, resolveQ, id.String(), householdID.String(), responderID.String(), at).
		Scan(&proposerIDStr, &offeredIDStr, &requestedIDStr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.AcceptedTrade{}, fmt.Errorf("accept trade: %w", domain.ErrTradeNotPending)
		}
		return domain.AcceptedTrade{}, fmt.Errorf("accept trade: resolve: %w", err)
	}

	proposerID, err := household.ParseMemberID(proposerIDStr)
	if err != nil {
		return domain.AcceptedTrade{}, fmt.Errorf("accept trade: parse proposer id: %w", err)
	}
	offeredID, err := domain.ParseTaskInstanceID(offeredIDStr)
	if err != nil {
		return domain.AcceptedTrade{}, fmt.Errorf("accept trade: parse offered instance id: %w", err)
	}
	requestedID, err := domain.ParseTaskInstanceID(requestedIDStr)
	if err != nil {
		return domain.AcceptedTrade{}, fmt.Errorf("accept trade: parse requested instance id: %w", err)
	}

	swaps, err := swapTradeInstances(ctx, tx, householdID, offeredID, requestedID, proposerID, responderID)
	if err != nil {
		return domain.AcceptedTrade{}, fmt.Errorf("accept trade: %w", err)
	}
	// Both instances must still have been in a tradeable, correctly-assigned
	// state at accept time. Returning here rolls back the trade's own status
	// flip too (deferred tx.Rollback), so a failed re-validation is a full
	// no-op — Accept is all-or-nothing.
	if len(swaps) != 2 {
		return domain.AcceptedTrade{}, fmt.Errorf("accept trade: %w", domain.ErrInstanceNotTradeable)
	}

	titles, err := fetchInstanceTitles(ctx, tx, []string{offeredID.String(), requestedID.String()})
	if err != nil {
		return domain.AcceptedTrade{}, fmt.Errorf("accept trade: fetch titles: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.AcceptedTrade{}, fmt.Errorf("accept trade: commit: %w", err)
	}

	return domain.AcceptedTrade{
		TradeID:        id,
		HouseholdID:    householdID,
		ProposerID:     proposerID,
		ResponderID:    responderID,
		OfferedTitle:   titles[offeredID.String()],
		RequestedTitle: titles[requestedID.String()],
	}, nil
}

// swapTradeInstances swaps offeredID's and requestedID's assignee_id
// (offered → responderID, requested → proposerID) in one statement, guarded
// by the same tradeability predicate as domain.IsInstanceTradeable
// (status = pending, kind = scheduled, claimed_by IS NULL) plus a check that
// each instance is still assigned to the party that held it when the trade
// was proposed. Returns the ids of the rows actually updated — callers must
// treat anything other than exactly 2 as a failed re-validation.
func swapTradeInstances(
	ctx context.Context,
	tx pgx.Tx,
	householdID household.HouseholdID,
	offeredID, requestedID domain.TaskInstanceID,
	proposerID, responderID household.MemberID,
) ([]domain.TaskInstanceID, error) {
	const swapQ = `
		UPDATE task_instance
		   SET assignee_id = CASE WHEN id = $3 THEN $5 ELSE $4 END,
		       updated_at  = now()
		 WHERE household_id = $1
		   AND status        = 'pending'
		   AND kind          = 'scheduled'
		   AND claimed_by   IS NULL
		   AND id = ANY($2::uuid[])
		   AND ((id = $3 AND assignee_id = $4) OR (id = $6 AND assignee_id = $5))
		RETURNING id`
	rows, err := tx.Query(ctx, swapQ,
		householdID.String(),
		[]string{offeredID.String(), requestedID.String()},
		offeredID.String(),
		proposerID.String(),
		responderID.String(),
		requestedID.String(),
	)
	if err != nil {
		return nil, fmt.Errorf("swap instances: %w", err)
	}
	defer rows.Close()

	var swapped []domain.TaskInstanceID
	for rows.Next() {
		var idStr string
		if err := rows.Scan(&idStr); err != nil {
			return nil, fmt.Errorf("swap instances: scan: %w", err)
		}
		id, err := domain.ParseTaskInstanceID(idStr)
		if err != nil {
			return nil, fmt.Errorf("swap instances: parse id: %w", err)
		}
		swapped = append(swapped, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("swap instances: %w", err)
	}
	return swapped, nil
}

// fetchInstanceTitles performs a single SELECT, scoped to the open
// transaction tx, joining task_instance to recurring_task to look up the
// chore title for each instance id in instanceIDs. Returns a map keyed by the
// instance id's string form.
func fetchInstanceTitles(ctx context.Context, tx pgx.Tx, instanceIDs []string) (map[string]string, error) {
	const q = `
		SELECT ti.id, rt.title
		  FROM task_instance ti
		  JOIN recurring_task rt ON rt.id = ti.recurring_task_id
		 WHERE ti.id = ANY($1::uuid[])`
	rows, err := tx.Query(ctx, q, instanceIDs)
	if err != nil {
		return nil, fmt.Errorf("fetch instance titles: %w", err)
	}
	defer rows.Close()

	titles := make(map[string]string, len(instanceIDs))
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, fmt.Errorf("fetch instance titles: scan: %w", err)
		}
		titles[id] = title
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fetch instance titles: %w", err)
	}
	return titles, nil
}

// Decline transitions the trade from proposed to declined. On 0 rows
// affected, domain.ErrTradeNotPending is returned without disambiguation —
// see domain.ErrTradeNotPending's doc for why unknown/wrong-member/
// already-resolved are not distinguished.
func (r *TradeRepository) Decline(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.ChoreTradeID,
	responderID household.MemberID,
) error {
	const q = `
		UPDATE chore_trade
		   SET status = 'declined', resolved_at = now()
		 WHERE id           = $1
		   AND household_id = $2
		   AND responder_id = $3
		   AND status       = 'proposed'`
	tag, err := r.dbtx.Exec(ctx, q, id.String(), householdID.String(), responderID.String())
	if err != nil {
		return fmt.Errorf("decline trade: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("decline trade: %w", domain.ErrTradeNotPending)
	}
	return nil
}

// Cancel transitions the trade from proposed to cancelled. On 0 rows
// affected, domain.ErrTradeNotPending is returned — mirroring Decline.
func (r *TradeRepository) Cancel(
	ctx context.Context,
	householdID household.HouseholdID,
	id domain.ChoreTradeID,
	proposerID household.MemberID,
) error {
	const q = `
		UPDATE chore_trade
		   SET status = 'cancelled', resolved_at = now()
		 WHERE id           = $1
		   AND household_id = $2
		   AND proposer_id  = $3
		   AND status       = 'proposed'`
	tag, err := r.dbtx.Exec(ctx, q, id.String(), householdID.String(), proposerID.String())
	if err != nil {
		return fmt.Errorf("cancel trade: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("cancel trade: %w", domain.ErrTradeNotPending)
	}
	return nil
}

// SweepExpiredTrades atomically transitions every live trade whose
// expires_at is at or before asOf to expired, using FOR UPDATE SKIP LOCKED so
// two concurrent sweeps never process the same row twice, mirroring
// revertExpiredClaims. No task_instance row is touched — an expiry never
// changes an assignee, unlike Accept.
func (r *TradeRepository) SweepExpiredTrades(ctx context.Context, asOf time.Time) ([]domain.ExpiredTrade, error) {
	tx, err := beginTx(ctx, r.dbtx, "sweep expired trades")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const sweepQ = `
		WITH expired AS (
			SELECT id, household_id, proposer_id, offered_instance_id, requested_instance_id
			  FROM chore_trade
			 WHERE status = 'proposed'
			   AND expires_at <= $1
			   FOR UPDATE SKIP LOCKED
		)
		UPDATE chore_trade ct
		   SET status = 'expired', resolved_at = now()
		  FROM expired
		 WHERE ct.id = expired.id
		RETURNING ct.id, expired.household_id, expired.proposer_id,
		          expired.offered_instance_id, expired.requested_instance_id`
	rows, err := tx.Query(ctx, sweepQ, asOf)
	if err != nil {
		return nil, fmt.Errorf("sweep expired trades: %w", err)
	}

	type expiredRow struct {
		tradeID     domain.ChoreTradeID
		householdID household.HouseholdID
		proposerID  household.MemberID
		offeredID   domain.TaskInstanceID
		requestedID domain.TaskInstanceID
	}
	var expired []expiredRow
	for rows.Next() {
		var tradeIDStr, hhStr, proposerIDStr, offeredIDStr, requestedIDStr string
		if err := rows.Scan(&tradeIDStr, &hhStr, &proposerIDStr, &offeredIDStr, &requestedIDStr); err != nil {
			rows.Close()
			return nil, fmt.Errorf("sweep expired trades: scan: %w", err)
		}
		tradeID, err := domain.ParseChoreTradeID(tradeIDStr)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("sweep expired trades: parse trade id: %w", err)
		}
		hhID, err := household.ParseHouseholdID(hhStr)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("sweep expired trades: parse household id: %w", err)
		}
		proposerID, err := household.ParseMemberID(proposerIDStr)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("sweep expired trades: parse proposer id: %w", err)
		}
		offeredID, err := domain.ParseTaskInstanceID(offeredIDStr)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("sweep expired trades: parse offered instance id: %w", err)
		}
		requestedID, err := domain.ParseTaskInstanceID(requestedIDStr)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("sweep expired trades: parse requested instance id: %w", err)
		}
		expired = append(expired, expiredRow{
			tradeID:     tradeID,
			householdID: hhID,
			proposerID:  proposerID,
			offeredID:   offeredID,
			requestedID: requestedID,
		})
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return nil, fmt.Errorf("sweep expired trades: %w", rowsErr)
	}

	if len(expired) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("sweep expired trades: commit: %w", err)
		}
		return nil, nil
	}

	instanceIDs := make([]string, 0, len(expired)*2)
	for _, e := range expired {
		instanceIDs = append(instanceIDs, e.offeredID.String(), e.requestedID.String())
	}
	titles, err := fetchInstanceTitles(ctx, tx, instanceIDs)
	if err != nil {
		return nil, fmt.Errorf("sweep expired trades: %w", err)
	}

	result := make([]domain.ExpiredTrade, 0, len(expired))
	for _, e := range expired {
		result = append(result, domain.ExpiredTrade{
			TradeID:        e.tradeID,
			HouseholdID:    e.householdID,
			ProposerID:     e.proposerID,
			OfferedTitle:   titles[e.offeredID.String()],
			RequestedTitle: titles[e.requestedID.String()],
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("sweep expired trades: commit: %w", err)
	}
	return result, nil
}

// scanChoreTrade scans a single chore_trade row (id, household_id,
// proposer_id, responder_id, offered_instance_id, requested_instance_id,
// status, created_at, resolved_at, expires_at) into a domain.ChoreTrade.
func scanChoreTrade(r row) (*domain.ChoreTrade, error) {
	var (
		idStr, householdIDStr, proposerIDStr, responderIDStr string
		offeredIDStr, requestedIDStr, statusStr              string
		createdAt, expiresAt                                 time.Time
		resolvedAt                                           *time.Time
	)
	err := r.Scan(
		&idStr, &householdIDStr, &proposerIDStr, &responderIDStr,
		&offeredIDStr, &requestedIDStr, &statusStr,
		&createdAt, &resolvedAt, &expiresAt,
	)
	if err != nil {
		return nil, err
	}

	id, err := domain.ParseChoreTradeID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan trade: %w", err)
	}
	householdID, err := household.ParseHouseholdID(householdIDStr)
	if err != nil {
		return nil, fmt.Errorf("scan trade: %w", err)
	}
	proposerID, err := household.ParseMemberID(proposerIDStr)
	if err != nil {
		return nil, fmt.Errorf("scan trade: %w", err)
	}
	responderID, err := household.ParseMemberID(responderIDStr)
	if err != nil {
		return nil, fmt.Errorf("scan trade: %w", err)
	}
	offeredID, err := domain.ParseTaskInstanceID(offeredIDStr)
	if err != nil {
		return nil, fmt.Errorf("scan trade: %w", err)
	}
	requestedID, err := domain.ParseTaskInstanceID(requestedIDStr)
	if err != nil {
		return nil, fmt.Errorf("scan trade: %w", err)
	}
	status, err := domain.ParseTradeStatus(statusStr)
	if err != nil {
		return nil, fmt.Errorf("scan trade: %w", err)
	}

	return &domain.ChoreTrade{
		ID:                  id,
		HouseholdID:         householdID,
		ProposerID:          proposerID,
		ResponderID:         responderID,
		OfferedInstanceID:   offeredID,
		RequestedInstanceID: requestedID,
		Status:              status,
		CreatedAt:           createdAt,
		ResolvedAt:          resolvedAt,
		ExpiresAt:           expiresAt,
	}, nil
}
