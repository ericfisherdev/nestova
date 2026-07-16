package adapter_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	householdadapter "github.com/ericfisherdev/nestova/internal/household/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/adapter"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

// seedThirdMember adds a third member to householdID, for tests that need a
// member distinct from both trade parties within the SAME household — e.g.
// simulating an out-of-band reassignment, which must stay tenant-consistent
// with task_instance_assignee_fk.
func seedThirdMember(t *testing.T, pool *pgxpool.Pool, householdID household.HouseholdID) household.MemberID {
	t.Helper()
	hhRepo := householdadapter.NewPostgresRepository(pool)
	m := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: householdID,
		DisplayName: "Charlie",
		Role:        household.RoleAdult,
		Color:       household.ColorOchre,
	}
	if err := hhRepo.AddMember(testCtx(t), m); err != nil {
		t.Fatalf("seedThirdMember: AddMember: %v", err)
	}
	return m.ID
}

// proposeTrade builds a domain.ChoreTrade from the given parties/instances and
// persists it via tradeRepo.Propose, failing the test on error.
func proposeTrade(
	t *testing.T,
	tradeRepo *adapter.TradeRepository,
	householdID household.HouseholdID,
	proposerID, responderID household.MemberID,
	offeredID, requestedID domain.TaskInstanceID,
) *domain.ChoreTrade {
	t.Helper()
	trade := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          proposerID,
		ResponderID:         responderID,
		OfferedInstanceID:   offeredID,
		RequestedInstanceID: requestedID,
	}
	if _, err := tradeRepo.Propose(testCtx(t), householdID, trade); err != nil {
		t.Fatalf("proposeTrade: Propose: %v", err)
	}
	return trade
}

// seedTwoTradeableInstances seeds two separate recurring tasks and one
// pending, scheduled, unclaimed instance each — offered assigned to
// proposerID, requested assigned to responderID — both due on dueOn.
func seedTwoTradeableInstances(
	t *testing.T,
	taskRepo *adapter.RecurringTaskRepository,
	instRepo *adapter.TaskInstanceRepository,
	householdID household.HouseholdID,
	proposerID, responderID household.MemberID,
	dueOn time.Time,
) (offered, requested *domain.TaskInstance) {
	t.Helper()
	rt1 := seedRecurringTask(t, taskRepo, householdID)
	rt2 := seedRecurringTask(t, taskRepo, householdID)
	offered = seedAssignedTaskInstance(t, instRepo, rt1, dueOn, proposerID)
	requested = seedAssignedTaskInstance(t, instRepo, rt2, dueOn, responderID)
	return offered, requested
}

// seedThreeTradeableInstances extends seedTwoTradeableInstances with a third,
// unrelated tradeable instance (z) assigned to m3. Used by the cross-role
// collision and reservation-lifecycle tests, which each need a "somewhere
// else" instance beyond the pair a single trade references.
func seedThreeTradeableInstances(
	t *testing.T,
	taskRepo *adapter.RecurringTaskRepository,
	instRepo *adapter.TaskInstanceRepository,
	householdID household.HouseholdID,
	m1, m2, m3 household.MemberID,
	dueOn time.Time,
) (x, y, z *domain.TaskInstance) {
	t.Helper()
	x, y = seedTwoTradeableInstances(t, taskRepo, instRepo, householdID, m1, m2, dueOn)
	rt3 := seedRecurringTask(t, taskRepo, householdID)
	z = seedAssignedTaskInstance(t, instRepo, rt3, dueOn, m3)
	return x, y, z
}

// rawInsertChoreTrade INSERTs a chore_trade row directly via pool, bypassing
// ChoreTradeRepository.Propose (and therefore every Go-level validation and
// lock it performs) entirely. Used to prove the cross-role "at most one live
// reservation per instance" invariant is enforced by the schema itself —
// chore_trade_reservation_sync_trigger and chore_trade_reservation's PRIMARY
// KEY — for any writer, not just this repository.
func rawInsertChoreTrade(
	t *testing.T,
	pool *pgxpool.Pool,
	householdID household.HouseholdID,
	proposerID, responderID household.MemberID,
	offeredID, requestedID domain.TaskInstanceID,
	expiresAt time.Time,
) error {
	t.Helper()
	const q = `
		INSERT INTO chore_trade
			(id, household_id, proposer_id, responder_id, offered_instance_id, requested_instance_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := pool.Exec(testCtx(t), q,
		domain.NewChoreTradeID().String(),
		householdID.String(),
		proposerID.String(),
		responderID.String(),
		offeredID.String(),
		requestedID.String(),
		expiresAt,
	)
	return err
}

// pgSQLStateUniqueViolation is the PostgreSQL SQLSTATE for a unique-constraint
// violation. adapter.sqlstateUniqueViolation is unexported, so this test
// package (adapter_test) carries its own copy for asserting on raw pgconn
// errors from schema-level (repository-bypassing) writes.
const pgSQLStateUniqueViolation = "23505"

// assertReservationConflictRejected asserts err is a Postgres unique_violation
// (SQLSTATE 23505) on chore_trade_reservation's PRIMARY KEY — the schema-level
// signature of a cross-role (or same-role) live-reservation collision.
func assertReservationConflictRejected(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want a unique_violation on chore_trade_reservation_pkey")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgSQLStateUniqueViolation || pgErr.ConstraintName != "chore_trade_reservation_pkey" {
		t.Errorf("error = %v, want a 23505 unique_violation on chore_trade_reservation_pkey", err)
	}
}

// ---------------------------------------------------------------------------
// Propose
// ---------------------------------------------------------------------------

// TestTrade_Propose_SetsExpiresAtToEarlierDueDate verifies AC5's fixture: a
// trade's expires_at is the earlier of the two instances' due dates.
func TestTrade_Propose_SetsExpiresAtToEarlierDueDate(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	earlierDue := refDate.AddDate(0, 0, 3)
	laterDue := refDate.AddDate(0, 0, 9)
	rt1 := seedRecurringTask(t, taskRepo, h.ID)
	rt2 := seedRecurringTask(t, taskRepo, h.ID)
	offered := seedAssignedTaskInstance(t, instRepo, rt1, laterDue, m1)
	requested := seedAssignedTaskInstance(t, instRepo, rt2, earlierDue, m2)

	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	if trade.Status != domain.TradeProposed {
		t.Errorf("Status = %v, want TradeProposed", trade.Status)
	}
	if !trade.ExpiresAt.Equal(domain.DateOf(earlierDue)) {
		t.Errorf("ExpiresAt = %v, want the earlier due date %v", trade.ExpiresAt, domain.DateOf(earlierDue))
	}
}

// TestTrade_Propose_InstanceNotFound_ReturnsErrInstanceNotFound verifies that
// proposing over an unknown instance id fails cleanly.
func TestTrade_Propose_InstanceNotFound_ReturnsErrInstanceNotFound(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	rt := seedRecurringTask(t, taskRepo, h.ID)
	offered := seedAssignedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, 5), m1)

	trade := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          m1,
		ResponderID:         m2,
		OfferedInstanceID:   offered.ID,
		RequestedInstanceID: domain.NewTaskInstanceID(),
	}
	_, err := tradeRepo.Propose(testCtx(t), h.ID, trade)
	if !errors.Is(err, domain.ErrInstanceNotFound) {
		t.Errorf("Propose(unknown requested instance) error = %v, want ErrInstanceNotFound", err)
	}
}

// TestTrade_Propose_ClaimedOfferedInstance_ReturnsErrInstanceNotTradeable
// covers AC3: a claimed instance (NES-117) cannot be offered.
func TestTrade_Propose_ClaimedOfferedInstance_ReturnsErrInstanceNotTradeable(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	rt1 := seedRecurringTask(t, taskRepo, h.ID)
	rt2 := seedRecurringTask(t, taskRepo, h.ID)
	// A claimable instance claimed by m1: assignee_id and claimed_by both end
	// up set to m1, which is exactly the shape Propose must reject even
	// though m1 (the proposer) is the assignee.
	offered := seedTaskInstance(t, instRepo, rt1, refDate.AddDate(0, 0, 5))
	if err := instRepo.Claim(testCtx(t), h.ID, offered.ID, m1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	requested := seedAssignedTaskInstance(t, instRepo, rt2, refDate.AddDate(0, 0, 5), m2)

	trade := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          m1,
		ResponderID:         m2,
		OfferedInstanceID:   offered.ID,
		RequestedInstanceID: requested.ID,
	}
	_, err := tradeRepo.Propose(testCtx(t), h.ID, trade)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Propose(claimed offered instance) error = %v, want ErrInstanceNotTradeable", err)
	}
}

// TestTrade_Propose_ClaimedRequestedInstance_ReturnsErrInstanceNotTradeable
// covers AC3's other half: a claimed instance cannot be requested either.
func TestTrade_Propose_ClaimedRequestedInstance_ReturnsErrInstanceNotTradeable(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	rt1 := seedRecurringTask(t, taskRepo, h.ID)
	rt2 := seedRecurringTask(t, taskRepo, h.ID)
	offered := seedAssignedTaskInstance(t, instRepo, rt1, refDate.AddDate(0, 0, 5), m1)
	requested := seedTaskInstance(t, instRepo, rt2, refDate.AddDate(0, 0, 5))
	if err := instRepo.Claim(testCtx(t), h.ID, requested.ID, m2); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	trade := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          m1,
		ResponderID:         m2,
		OfferedInstanceID:   offered.ID,
		RequestedInstanceID: requested.ID,
	}
	_, err := tradeRepo.Propose(testCtx(t), h.ID, trade)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Propose(claimed requested instance) error = %v, want ErrInstanceNotTradeable", err)
	}
}

// TestTrade_Propose_OfferedInstanceAlreadyLive_ReturnsErrInstanceNotTradeable
// covers AC4's offered-instance-collision half.
func TestTrade_Propose_OfferedInstanceAlreadyLive_ReturnsErrInstanceNotTradeable(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	offered, requested1 := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	rt3 := seedRecurringTask(t, taskRepo, h.ID)
	requested2 := seedAssignedTaskInstance(t, instRepo, rt3, refDate.AddDate(0, 0, 5), m2)

	proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested1.ID)

	trade := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          m1,
		ResponderID:         m2,
		OfferedInstanceID:   offered.ID,
		RequestedInstanceID: requested2.ID,
	}
	_, err := tradeRepo.Propose(testCtx(t), h.ID, trade)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Propose(offered instance already live) error = %v, want ErrInstanceNotTradeable", err)
	}
}

// TestTrade_Propose_RequestedInstanceAlreadyLive_ReturnsErrInstanceNotTradeable
// covers AC4's requested-instance-collision half.
func TestTrade_Propose_RequestedInstanceAlreadyLive_ReturnsErrInstanceNotTradeable(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	offered1, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	rt3 := seedRecurringTask(t, taskRepo, h.ID)
	offered2 := seedAssignedTaskInstance(t, instRepo, rt3, refDate.AddDate(0, 0, 5), m1)

	proposeTrade(t, tradeRepo, h.ID, m1, m2, offered1.ID, requested.ID)

	trade := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          m1,
		ResponderID:         m2,
		OfferedInstanceID:   offered2.ID,
		RequestedInstanceID: requested.ID,
	}
	_, err := tradeRepo.Propose(testCtx(t), h.ID, trade)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Propose(requested instance already live) error = %v, want ErrInstanceNotTradeable", err)
	}
}

// ---------------------------------------------------------------------------
// Propose — cross-role collisions (the same instance offered in one live
// trade and requested in another). Distinct from the same-role tests above:
// those collide offered-vs-offered or requested-vs-requested; these collide
// offered-vs-requested, which the chore_trade_reservation table's PRIMARY KEY
// on instance_id (not the offered/requested columns individually) is what
// closes — see 00021_chore_trade.sql.
// ---------------------------------------------------------------------------

// TestTrade_Propose_OfferedAlreadyRequestedElsewhere_ReturnsErrInstanceNotTradeable
// covers the cross-role gap directly: an instance already the REQUESTED side
// of a live trade cannot be newly OFFERED by a different trade.
func TestTrade_Propose_OfferedAlreadyRequestedElsewhere_ReturnsErrInstanceNotTradeable(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)
	m3 := seedThirdMember(t, pool, h.ID)

	dueOn := refDate.AddDate(0, 0, 5)
	x, y, z := seedThreeTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, m3, dueOn)
	proposeTrade(t, tradeRepo, h.ID, m1, m2, x.ID, y.ID) // trade1: offers x, requests y

	trade2 := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          m2,
		ResponderID:         m3,
		OfferedInstanceID:   y.ID, // already requested by trade1
		RequestedInstanceID: z.ID,
	}
	_, err := tradeRepo.Propose(testCtx(t), h.ID, trade2)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Propose(offered already requested elsewhere) error = %v, want ErrInstanceNotTradeable", err)
	}
}

// TestTrade_Propose_RequestedAlreadyOfferedElsewhere_ReturnsErrInstanceNotTradeable
// covers the cross-role gap's other direction: an instance already the
// OFFERED side of a live trade cannot be newly REQUESTED by a different
// trade.
func TestTrade_Propose_RequestedAlreadyOfferedElsewhere_ReturnsErrInstanceNotTradeable(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)
	m3 := seedThirdMember(t, pool, h.ID)

	dueOn := refDate.AddDate(0, 0, 5)
	x, y, z := seedThreeTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, m3, dueOn)
	proposeTrade(t, tradeRepo, h.ID, m1, m2, x.ID, y.ID) // trade1: offers x, requests y

	trade2 := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          m3,
		ResponderID:         m1,
		OfferedInstanceID:   z.ID,
		RequestedInstanceID: x.ID, // already offered by trade1
	}
	_, err := tradeRepo.Propose(testCtx(t), h.ID, trade2)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Propose(requested already offered elsewhere) error = %v, want ErrInstanceNotTradeable", err)
	}
}

// ---------------------------------------------------------------------------
// Propose — schema-level enforcement (bypassing the repository entirely).
// These prove the cross-role invariant is a hard database guarantee, not
// just repository discipline: chore_trade_reservation_sync_trigger and
// chore_trade_reservation's PRIMARY KEY reject a colliding raw INSERT even
// when ChoreTradeRepository.Propose's Go-level checks are never consulted.
// ---------------------------------------------------------------------------

// TestTrade_SchemaLevel_RawInsert_OfferedAlreadyRequestedElsewhere_Rejected
// mirrors TestTrade_Propose_OfferedAlreadyRequestedElsewhere_ReturnsErrInstanceNotTradeable
// but via a raw INSERT INTO chore_trade instead of the repository.
func TestTrade_SchemaLevel_RawInsert_OfferedAlreadyRequestedElsewhere_Rejected(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)
	m3 := seedThirdMember(t, pool, h.ID)

	dueOn := refDate.AddDate(0, 0, 5)
	x, y, z := seedThreeTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, m3, dueOn)
	proposeTrade(t, tradeRepo, h.ID, m1, m2, x.ID, y.ID) // trade1: offers x, requests y (reserves both)

	err := rawInsertChoreTrade(t, pool, h.ID, m2, m3, y.ID, z.ID, dueOn)
	assertReservationConflictRejected(t, err)
}

// TestTrade_SchemaLevel_RawInsert_RequestedAlreadyOfferedElsewhere_Rejected
// mirrors TestTrade_Propose_RequestedAlreadyOfferedElsewhere_ReturnsErrInstanceNotTradeable
// but via a raw INSERT INTO chore_trade instead of the repository.
func TestTrade_SchemaLevel_RawInsert_RequestedAlreadyOfferedElsewhere_Rejected(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)
	m3 := seedThirdMember(t, pool, h.ID)

	dueOn := refDate.AddDate(0, 0, 5)
	x, y, z := seedThreeTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, m3, dueOn)
	proposeTrade(t, tradeRepo, h.ID, m1, m2, x.ID, y.ID) // trade1: offers x, requests y (reserves both)

	err := rawInsertChoreTrade(t, pool, h.ID, m3, m1, z.ID, x.ID, dueOn)
	assertReservationConflictRejected(t, err)
}

// ---------------------------------------------------------------------------
// Accept
// ---------------------------------------------------------------------------

// TestTrade_Accept_SwapsAssigneesAtomically covers AC1's accept half.
func TestTrade_Accept_SwapsAssigneesAtomically(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	resolved, err := tradeRepo.Accept(testCtx(t), h.ID, trade.ID, m2, refDate)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if resolved.ProposerID != m1 || resolved.ResponderID != m2 {
		t.Errorf("AcceptedTrade parties = (%v, %v), want (%v, %v)", resolved.ProposerID, resolved.ResponderID, m1, m2)
	}

	gotOffered, err := instRepo.Get(testCtx(t), h.ID, offered.ID)
	if err != nil {
		t.Fatalf("Get offered: %v", err)
	}
	if gotOffered.AssigneeID == nil || *gotOffered.AssigneeID != m2 {
		t.Errorf("offered.AssigneeID = %v, want responder %v", gotOffered.AssigneeID, m2)
	}
	gotRequested, err := instRepo.Get(testCtx(t), h.ID, requested.ID)
	if err != nil {
		t.Fatalf("Get requested: %v", err)
	}
	if gotRequested.AssigneeID == nil || *gotRequested.AssigneeID != m1 {
		t.Errorf("requested.AssigneeID = %v, want proposer %v", gotRequested.AssigneeID, m1)
	}

	gotTrade, err := tradeRepo.Get(testCtx(t), h.ID, trade.ID)
	if err != nil {
		t.Fatalf("Get trade: %v", err)
	}
	if gotTrade.Status != domain.TradeAccepted {
		t.Errorf("Status = %v, want TradeAccepted", gotTrade.Status)
	}
	if gotTrade.ResolvedAt == nil {
		t.Error("ResolvedAt is nil, want set")
	}
}

// TestTrade_Accept_WrongResponder_ReturnsErrTradeNotPending verifies Accept is
// scoped to the trade's actual responder.
func TestTrade_Accept_WrongResponder_ReturnsErrTradeNotPending(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	_, err := tradeRepo.Accept(testCtx(t), h.ID, trade.ID, m1, refDate)
	if !errors.Is(err, domain.ErrTradeNotPending) {
		t.Errorf("Accept(proposer as responder) error = %v, want ErrTradeNotPending", err)
	}
}

// TestTrade_Accept_InstanceCompletedAfterProposal_ReturnsErrInstanceNotTradeable
// covers AC2's first half, and verifies the whole accept — including the
// trade's own status flip — rolls back: the trade stays proposed and neither
// instance's assignee changes.
func TestTrade_Accept_InstanceCompletedAfterProposal_ReturnsErrInstanceNotTradeable(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	if err := instRepo.Complete(testCtx(t), h.ID, offered.ID, m1, time.Now()); err != nil {
		t.Fatalf("Complete(offered): %v", err)
	}

	_, err := tradeRepo.Accept(testCtx(t), h.ID, trade.ID, m2, refDate)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Accept(offered completed) error = %v, want ErrInstanceNotTradeable", err)
	}

	gotTrade, err := tradeRepo.Get(testCtx(t), h.ID, trade.ID)
	if err != nil {
		t.Fatalf("Get trade: %v", err)
	}
	if gotTrade.Status != domain.TradeProposed {
		t.Errorf("Status = %v, want TradeProposed (accept must roll back entirely)", gotTrade.Status)
	}

	gotRequested, err := instRepo.Get(testCtx(t), h.ID, requested.ID)
	if err != nil {
		t.Fatalf("Get requested: %v", err)
	}
	if gotRequested.AssigneeID == nil || *gotRequested.AssigneeID != m2 {
		t.Errorf("requested.AssigneeID = %v, want unchanged responder %v", gotRequested.AssigneeID, m2)
	}
}

// TestTrade_Accept_InstanceReassignedAfterProposal_ReturnsErrInstanceNotTradeable
// covers AC2's other half: an instance reassigned out from under the trade
// (simulated with a direct UPDATE, since no public mutation reassigns an
// already-assigned instance) also fails re-validation.
func TestTrade_Accept_InstanceReassignedAfterProposal_ReturnsErrInstanceNotTradeable(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)
	m3 := seedThirdMember(t, pool, h.ID)

	offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	// Simulate an out-of-band reassignment of the offered instance away from
	// the proposer (e.g. an administrative action) between propose and accept.
	if _, err := pool.Exec(testCtx(t),
		"UPDATE task_instance SET assignee_id = $1 WHERE id = $2", m3.String(), offered.ID.String(),
	); err != nil {
		t.Fatalf("simulate reassignment: %v", err)
	}

	_, err := tradeRepo.Accept(testCtx(t), h.ID, trade.ID, m2, refDate)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Accept(offered reassigned) error = %v, want ErrInstanceNotTradeable", err)
	}
}

// TestTrade_Accept_ExpiredButNotYetSwept_ReturnsErrTradeNotPending covers the
// CodeRabbit-flagged race: a trade whose expires_at has already passed, but
// whose status is still 'proposed' because SweepExpiredTrades has not run
// yet, must not be acceptable. Accept enforces the deadline synchronously
// (expires_at > at) rather than depending on the sweep having already run.
func TestTrade_Accept_ExpiredButNotYetSwept_ReturnsErrTradeNotPending(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	// Status is still 'proposed' — no sweep has run — but at is at the exact
	// deadline instant, which Accept's expires_at > at guard must reject.
	_, err := tradeRepo.Accept(testCtx(t), h.ID, trade.ID, m2, trade.ExpiresAt)
	if !errors.Is(err, domain.ErrTradeNotPending) {
		t.Errorf("Accept(at deadline, status still proposed) error = %v, want ErrTradeNotPending", err)
	}

	gotTrade, err := tradeRepo.Get(testCtx(t), h.ID, trade.ID)
	if err != nil {
		t.Fatalf("Get trade: %v", err)
	}
	if gotTrade.Status != domain.TradeProposed {
		t.Errorf("Status = %v, want unchanged TradeProposed (rejected accept must not resolve the trade)", gotTrade.Status)
	}
	assertAssigneeUnchanged(t, instRepo, h.ID, offered.ID, m1)
	assertAssigneeUnchanged(t, instRepo, h.ID, requested.ID, m2)
}

// ---------------------------------------------------------------------------
// Decline / Cancel / re-resolution
// ---------------------------------------------------------------------------

// TestTrade_Decline_NoAssignmentChange covers AC1's decline half.
func TestTrade_Decline_NoAssignmentChange(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	if _, err := tradeRepo.Decline(testCtx(t), h.ID, trade.ID, m2); err != nil {
		t.Fatalf("Decline: %v", err)
	}

	gotTrade, err := tradeRepo.Get(testCtx(t), h.ID, trade.ID)
	if err != nil {
		t.Fatalf("Get trade: %v", err)
	}
	if gotTrade.Status != domain.TradeDeclined {
		t.Errorf("Status = %v, want TradeDeclined", gotTrade.Status)
	}
	if gotTrade.ResolvedAt == nil {
		t.Error("ResolvedAt is nil, want set")
	}
	assertAssigneeUnchanged(t, instRepo, h.ID, offered.ID, m1)
	assertAssigneeUnchanged(t, instRepo, h.ID, requested.ID, m2)
}

// TestTrade_Decline_Twice_ReturnsErrTradeNotPending exercises the state
// machine's terminal-status guard at the adapter layer.
func TestTrade_Decline_Twice_ReturnsErrTradeNotPending(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	if _, err := tradeRepo.Decline(testCtx(t), h.ID, trade.ID, m2); err != nil {
		t.Fatalf("Decline (first): %v", err)
	}
	_, err := tradeRepo.Decline(testCtx(t), h.ID, trade.ID, m2)
	if !errors.Is(err, domain.ErrTradeNotPending) {
		t.Errorf("Decline (second) error = %v, want ErrTradeNotPending", err)
	}
}

// TestTrade_Cancel_NoAssignmentChange covers AC1's cancel half.
func TestTrade_Cancel_NoAssignmentChange(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	if err := tradeRepo.Cancel(testCtx(t), h.ID, trade.ID, m1); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	gotTrade, err := tradeRepo.Get(testCtx(t), h.ID, trade.ID)
	if err != nil {
		t.Fatalf("Get trade: %v", err)
	}
	if gotTrade.Status != domain.TradeCancelled {
		t.Errorf("Status = %v, want TradeCancelled", gotTrade.Status)
	}
	assertAssigneeUnchanged(t, instRepo, h.ID, offered.ID, m1)
	assertAssigneeUnchanged(t, instRepo, h.ID, requested.ID, m2)
}

// TestTrade_Cancel_WrongProposer_ReturnsErrTradeNotPending verifies Cancel is
// scoped to the trade's actual proposer.
func TestTrade_Cancel_WrongProposer_ReturnsErrTradeNotPending(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	err := tradeRepo.Cancel(testCtx(t), h.ID, trade.ID, m2)
	if !errors.Is(err, domain.ErrTradeNotPending) {
		t.Errorf("Cancel(responder as proposer) error = %v, want ErrTradeNotPending", err)
	}
}

// assertAssigneeUnchanged is a small helper asserting instance id's
// AssigneeID still equals want.
func assertAssigneeUnchanged(
	t *testing.T,
	instRepo *adapter.TaskInstanceRepository,
	householdID household.HouseholdID,
	id domain.TaskInstanceID,
	want household.MemberID,
) {
	t.Helper()
	got, err := instRepo.Get(testCtx(t), householdID, id)
	if err != nil {
		t.Fatalf("Get instance %s: %v", id, err)
	}
	if got.AssigneeID == nil || *got.AssigneeID != want {
		t.Errorf("instance %s AssigneeID = %v, want unchanged %v", id, got.AssigneeID, want)
	}
}

// ---------------------------------------------------------------------------
// SweepExpiredTrades
// ---------------------------------------------------------------------------

// TestTrade_SweepExpiredTrades_ExpiresAtOrBeforeEarlierDueDate covers AC5: an
// unresolved trade expires automatically at (not after) the earlier of the
// two due dates, and no instance assignment changes.
func TestTrade_SweepExpiredTrades_ExpiresAtOrBeforeEarlierDueDate(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	earlierDue := refDate.AddDate(0, 0, 3)
	laterDue := refDate.AddDate(0, 0, 9)
	rt1 := seedRecurringTask(t, taskRepo, h.ID)
	rt2 := seedRecurringTask(t, taskRepo, h.ID)
	offered := seedAssignedTaskInstance(t, instRepo, rt1, laterDue, m1)
	requested := seedAssignedTaskInstance(t, instRepo, rt2, earlierDue, m2)
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	// Before the earlier due date: not yet expired.
	before, err := tradeRepo.SweepExpiredTrades(testCtx(t), domain.DateOf(earlierDue).Add(-time.Hour))
	if err != nil {
		t.Fatalf("SweepExpiredTrades (before): %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("SweepExpiredTrades (before earlier due date) = %d expired, want 0", len(before))
	}

	// At the earlier due date: expires.
	expired, err := tradeRepo.SweepExpiredTrades(testCtx(t), domain.DateOf(earlierDue))
	if err != nil {
		t.Fatalf("SweepExpiredTrades (at): %v", err)
	}
	if len(expired) != 1 {
		t.Fatalf("SweepExpiredTrades (at earlier due date) = %d expired, want 1", len(expired))
	}
	if expired[0].TradeID != trade.ID {
		t.Errorf("expired trade id = %v, want %v", expired[0].TradeID, trade.ID)
	}
	if expired[0].ProposerID != m1 {
		t.Errorf("expired trade proposer = %v, want %v", expired[0].ProposerID, m1)
	}

	gotTrade, err := tradeRepo.Get(testCtx(t), h.ID, trade.ID)
	if err != nil {
		t.Fatalf("Get trade: %v", err)
	}
	if gotTrade.Status != domain.TradeExpired {
		t.Errorf("Status = %v, want TradeExpired", gotTrade.Status)
	}
	assertAssigneeUnchanged(t, instRepo, h.ID, offered.ID, m1)
	assertAssigneeUnchanged(t, instRepo, h.ID, requested.ID, m2)
}

// TestTrade_SweepExpiredTrades_AcceptedTradeNeverExpires verifies a resolved
// (accepted) trade is excluded from the sweep even once its expires_at has
// passed.
func TestTrade_SweepExpiredTrades_AcceptedTradeNeverExpires(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	due := refDate.AddDate(0, 0, 3)
	offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, due)
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)
	if _, err := tradeRepo.Accept(testCtx(t), h.ID, trade.ID, m2, refDate); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	expired, err := tradeRepo.SweepExpiredTrades(testCtx(t), domain.DateOf(due).AddDate(1, 0, 0))
	if err != nil {
		t.Fatalf("SweepExpiredTrades: %v", err)
	}
	if len(expired) != 0 {
		t.Errorf("SweepExpiredTrades (accepted trade) = %d expired, want 0", len(expired))
	}
}

// ---------------------------------------------------------------------------
// Reservation lifecycle — chore_trade_reservation_sync_trigger must free a
// trade's two reservation rows the moment its status leaves 'proposed',
// regardless of which of the four resolution paths caused it. Each test below
// resolves a trade one way, then proposes a completely independent follow-up
// trade referencing one of the same instances — success proves the
// reservation was actually freed (a still-held reservation would make the
// follow-up Propose fail with ErrInstanceNotTradeable, per the cross-role
// tests above).
// ---------------------------------------------------------------------------

// TestTrade_Accept_FreesReservationForFollowUpTrade covers the accept path.
// x's assignee has moved to m2 by the time of the follow-up (Accept's own
// swap), so the follow-up is proposed by m2, not m1.
func TestTrade_Accept_FreesReservationForFollowUpTrade(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)
	m3 := seedThirdMember(t, pool, h.ID)

	dueOn := refDate.AddDate(0, 0, 5)
	x, y, z := seedThreeTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, m3, dueOn)
	trade1 := proposeTrade(t, tradeRepo, h.ID, m1, m2, x.ID, y.ID)
	if _, err := tradeRepo.Accept(testCtx(t), h.ID, trade1.ID, m2, refDate); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	trade2 := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          m2, // x's new owner, post-swap
		ResponderID:         m3,
		OfferedInstanceID:   x.ID,
		RequestedInstanceID: z.ID,
	}
	if _, err := tradeRepo.Propose(testCtx(t), h.ID, trade2); err != nil {
		t.Errorf("Propose(follow-up after accept) error = %v, want nil (reservation must be freed)", err)
	}
}

// TestTrade_Decline_FreesReservationForFollowUpTrade covers the decline path.
// Decline never changes assignees, so the follow-up is proposed by the same
// original proposer, m1.
func TestTrade_Decline_FreesReservationForFollowUpTrade(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)
	m3 := seedThirdMember(t, pool, h.ID)

	dueOn := refDate.AddDate(0, 0, 5)
	x, y, z := seedThreeTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, m3, dueOn)
	trade1 := proposeTrade(t, tradeRepo, h.ID, m1, m2, x.ID, y.ID)
	if _, err := tradeRepo.Decline(testCtx(t), h.ID, trade1.ID, m2); err != nil {
		t.Fatalf("Decline: %v", err)
	}

	trade2 := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          m1,
		ResponderID:         m3,
		OfferedInstanceID:   x.ID,
		RequestedInstanceID: z.ID,
	}
	if _, err := tradeRepo.Propose(testCtx(t), h.ID, trade2); err != nil {
		t.Errorf("Propose(follow-up after decline) error = %v, want nil (reservation must be freed)", err)
	}
}

// TestTrade_Cancel_FreesReservationForFollowUpTrade covers the cancel path.
func TestTrade_Cancel_FreesReservationForFollowUpTrade(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)
	m3 := seedThirdMember(t, pool, h.ID)

	dueOn := refDate.AddDate(0, 0, 5)
	x, y, z := seedThreeTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, m3, dueOn)
	trade1 := proposeTrade(t, tradeRepo, h.ID, m1, m2, x.ID, y.ID)
	if err := tradeRepo.Cancel(testCtx(t), h.ID, trade1.ID, m1); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	trade2 := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          m1,
		ResponderID:         m3,
		OfferedInstanceID:   x.ID,
		RequestedInstanceID: z.ID,
	}
	if _, err := tradeRepo.Propose(testCtx(t), h.ID, trade2); err != nil {
		t.Errorf("Propose(follow-up after cancel) error = %v, want nil (reservation must be freed)", err)
	}
}

// TestTrade_Expire_FreesReservationForFollowUpTrade covers the background
// sweep's expiry path.
func TestTrade_Expire_FreesReservationForFollowUpTrade(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)
	m3 := seedThirdMember(t, pool, h.ID)

	dueOn := refDate.AddDate(0, 0, 5)
	x, y, z := seedThreeTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, m3, dueOn)
	trade1 := proposeTrade(t, tradeRepo, h.ID, m1, m2, x.ID, y.ID)
	expired, err := tradeRepo.SweepExpiredTrades(testCtx(t), trade1.ExpiresAt)
	if err != nil {
		t.Fatalf("SweepExpiredTrades: %v", err)
	}
	if len(expired) != 1 {
		t.Fatalf("SweepExpiredTrades = %d expired, want 1", len(expired))
	}

	trade2 := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          m1,
		ResponderID:         m3,
		OfferedInstanceID:   x.ID,
		RequestedInstanceID: z.ID,
	}
	if _, err := tradeRepo.Propose(testCtx(t), h.ID, trade2); err != nil {
		t.Errorf("Propose(follow-up after expiry) error = %v, want nil (reservation must be freed)", err)
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// TestTrade_Accept_ConcurrentAcceptsOnlyOneSucceeds races two goroutines
// accepting the same trade: exactly one must succeed and the instances must
// be swapped exactly once (never double-swapped back to their originals).
func TestTrade_Accept_ConcurrentAcceptsOnlyOneSucceeds(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	var (
		wg   sync.WaitGroup
		errs [2]error
	)
	wg.Add(2)
	for i := range 2 {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = tradeRepo.Accept(context.Background(), h.ID, trade.ID, m2, refDate)
		}(i)
	}
	wg.Wait()

	successes, notPendingErrs := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, domain.ErrTradeNotPending):
			notPendingErrs++
		default:
			t.Errorf("unexpected error racing accept: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want 1", successes)
	}
	if notPendingErrs != 1 {
		t.Errorf("ErrTradeNotPending count = %d, want 1", notPendingErrs)
	}

	assertAssigneeUnchanged(t, instRepo, h.ID, offered.ID, m2)
	assertAssigneeUnchanged(t, instRepo, h.ID, requested.ID, m1)
}

// TestTrade_Accept_RaceAgainstComplete races Accept against Complete on the
// offered instance. Complete never depends on the trade or the assignee, so
// it always succeeds; Accept either wins (the swap commits before Complete's
// UPDATE takes the row) or loses to Complete (which flips status away from
// pending first, so Accept's re-validation fails and its entire transaction —
// including the trade's own status flip — rolls back). Both outcomes are
// individually asserted below; the key invariant is that the two operations
// never leave an inconsistent mix (e.g. the trade marked accepted while the
// swap only partially applied).
func TestTrade_Accept_RaceAgainstComplete(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	var (
		wg          sync.WaitGroup
		acceptErr   error
		completeErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, acceptErr = tradeRepo.Accept(context.Background(), h.ID, trade.ID, m2, refDate)
	}()
	go func() {
		defer wg.Done()
		completeErr = instRepo.Complete(context.Background(), h.ID, offered.ID, m1, time.Now())
	}()
	wg.Wait()

	// Complete never depends on the trade or the current assignee, so it must
	// always succeed regardless of ordering.
	if completeErr != nil {
		t.Errorf("Complete error = %v, want nil", completeErr)
	}

	gotOffered, err := instRepo.Get(testCtx(t), h.ID, offered.ID)
	if err != nil {
		t.Fatalf("Get offered: %v", err)
	}
	if gotOffered.Status != domain.StatusDone {
		t.Errorf("offered.Status = %v, want done", gotOffered.Status)
	}

	gotTrade, err := tradeRepo.Get(testCtx(t), h.ID, trade.ID)
	if err != nil {
		t.Fatalf("Get trade: %v", err)
	}

	switch {
	case acceptErr == nil:
		// Accept won the race: the trade is accepted and the swap applied —
		// requested must have moved to the proposer even though offered later
		// (harmlessly) transitioned to done.
		if gotTrade.Status != domain.TradeAccepted {
			t.Errorf("Accept succeeded but Status = %v, want TradeAccepted", gotTrade.Status)
		}
		gotRequested, err := instRepo.Get(testCtx(t), h.ID, requested.ID)
		if err != nil {
			t.Fatalf("Get requested: %v", err)
		}
		if gotRequested.AssigneeID == nil || *gotRequested.AssigneeID != m1 {
			t.Errorf("requested.AssigneeID = %v, want proposer %v", gotRequested.AssigneeID, m1)
		}
	case errors.Is(acceptErr, domain.ErrInstanceNotTradeable):
		// Complete won the race: Accept's entire transaction — including the
		// trade's own status flip — must have rolled back.
		if gotTrade.Status != domain.TradeProposed {
			t.Errorf("Accept lost the race but Status = %v, want TradeProposed (full rollback)", gotTrade.Status)
		}
		assertAssigneeUnchanged(t, instRepo, h.ID, requested.ID, m2)
	default:
		t.Fatalf("Accept error = %v, want nil or ErrInstanceNotTradeable", acceptErr)
	}
}

// TestTrade_ProposeVsAccept_NoDeadlock is the CodeRabbit-flagged regression
// test: Propose and Accept acquire their locks in intentionally different
// orders (Propose: task_instance rows only; Accept: its chore_trade row,
// then task_instance rows — see the lock-ordering convention documented on
// domain.ChoreTradeRepository). Before the fix (hasLiveTradeProposal's now-
// removed FOR UPDATE), racing an Accept of an already-live trade against a
// concurrent Propose that references the SAME offered instance could
// deadlock: Accept holds its chore_trade row and waits on the instance;
// Propose holds the instance (via lockTradeInstances) and waits on the
// chore_trade row.
//
// trade1 (X offered by m1, Y requested from m2) is already proposed. The two
// goroutines race:
//   - Accept(trade1, m2)
//   - Propose(a NEW trade2: proposer=m1, responder=m3, offered=X ⚠ same
//     instance as trade1's offered, requested=Z)
//
// Neither goroutine may block forever or surface a raw/unmapped database
// error (a deadlock, once detected, would otherwise propagate as an opaque
// wrapped pgconn error rather than one of the two calls' own domain
// sentinels). Accept must always succeed in this specific interleaving
// (nothing in trade2's failed Propose can invalidate trade1), while Propose
// must always fail — with ErrInstanceNotTradeable if it observed trade1 still
// live, or ErrNotYourChore if Accept had already committed and swapped X's
// assignee away from m1 by the time Propose's ownership check ran.
func TestTrade_ProposeVsAccept_NoDeadlock(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)
	m3 := seedThirdMember(t, pool, h.ID)

	dueOn := refDate.AddDate(0, 0, 5)
	x, y := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, dueOn)
	rtZ := seedRecurringTask(t, taskRepo, h.ID)
	z := seedAssignedTaskInstance(t, instRepo, rtZ, dueOn, m3)

	trade1 := proposeTrade(t, tradeRepo, h.ID, m1, m2, x.ID, y.ID)

	var (
		wg         sync.WaitGroup
		acceptErr  error
		proposeErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, acceptErr = tradeRepo.Accept(context.Background(), h.ID, trade1.ID, m2, refDate)
	}()
	go func() {
		defer wg.Done()
		trade2 := &domain.ChoreTrade{
			ID:                  domain.NewChoreTradeID(),
			ProposerID:          m1,
			ResponderID:         m3,
			OfferedInstanceID:   x.ID,
			RequestedInstanceID: z.ID,
		}
		_, proposeErr = tradeRepo.Propose(context.Background(), h.ID, trade2)
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Propose and Accept did not both complete within 10s — likely deadlocked")
	}

	if acceptErr != nil {
		t.Errorf("Accept error = %v, want nil (nothing in the concurrent Propose attempt can invalidate trade1)", acceptErr)
	}
	if !errors.Is(proposeErr, domain.ErrInstanceNotTradeable) && !errors.Is(proposeErr, domain.ErrNotYourChore) {
		t.Errorf("Propose error = %v, want ErrInstanceNotTradeable or ErrNotYourChore", proposeErr)
	}

	gotTrade1, err := tradeRepo.Get(testCtx(t), h.ID, trade1.ID)
	if err != nil {
		t.Fatalf("Get trade1: %v", err)
	}
	if gotTrade1.Status != domain.TradeAccepted {
		t.Errorf("trade1 Status = %v, want TradeAccepted", gotTrade1.Status)
	}
	assertAssigneeUnchanged(t, instRepo, h.ID, x.ID, m2)
	assertAssigneeUnchanged(t, instRepo, h.ID, y.ID, m1)
}

// TestTrade_ProposeVsPropose_CrossRoleRace_ExactlyOneWins races two brand-new
// Propose calls that collide on instance x in DIFFERENT roles: tradeA offers
// x, tradeB requests x. Before the reservation table (chore_trade_offered_
// live_uniq / chore_trade_requested_live_uniq alone), both could succeed,
// since neither indexed column overlaps with the other trade's role for x.
// With chore_trade_reservation's single PRIMARY KEY on instance_id, exactly
// one may still win regardless of role. This is expected to already be
// race-free rather than reveal a NEW hazard: lockTradeInstances' FOR UPDATE
// on x fully serializes the two Propose calls before either reaches the
// trigger, so the loser's hasLiveTradeProposal check sees the winner's
// already-committed trade and exits cleanly — see hasLiveTradeProposal's doc
// for why. The test exists to pin that behavior empirically, the same way
// TestTrade_ProposeVsAccept_NoDeadlock pins the propose/accept case.
func TestTrade_ProposeVsPropose_CrossRoleRace_ExactlyOneWins(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)
	m3 := seedThirdMember(t, pool, h.ID)

	dueOn := refDate.AddDate(0, 0, 5)
	x, y, z := seedThreeTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, m3, dueOn)

	var (
		wg         sync.WaitGroup
		errA, errB error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		tradeA := &domain.ChoreTrade{
			ID: domain.NewChoreTradeID(), ProposerID: m1, ResponderID: m2,
			OfferedInstanceID: x.ID, RequestedInstanceID: y.ID,
		}
		_, errA = tradeRepo.Propose(context.Background(), h.ID, tradeA)
	}()
	go func() {
		defer wg.Done()
		tradeB := &domain.ChoreTrade{
			ID: domain.NewChoreTradeID(), ProposerID: m3, ResponderID: m1,
			OfferedInstanceID: z.ID, RequestedInstanceID: x.ID,
		}
		_, errB = tradeRepo.Propose(context.Background(), h.ID, tradeB)
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Propose A and Propose B did not both complete within 10s — possibly deadlocked")
	}

	successes, notTradeableErrs := 0, 0
	for _, err := range []error{errA, errB} {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, domain.ErrInstanceNotTradeable):
			notTradeableErrs++
		default:
			t.Errorf("unexpected error racing cross-role propose: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want 1", successes)
	}
	if notTradeableErrs != 1 {
		t.Errorf("ErrInstanceNotTradeable count = %d, want 1", notTradeableErrs)
	}
}
