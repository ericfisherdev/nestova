package adapter_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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
	if err := tradeRepo.Propose(testCtx(t), householdID, trade); err != nil {
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
	err := tradeRepo.Propose(testCtx(t), h.ID, trade)
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
	err := tradeRepo.Propose(testCtx(t), h.ID, trade)
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
	err := tradeRepo.Propose(testCtx(t), h.ID, trade)
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
	err := tradeRepo.Propose(testCtx(t), h.ID, trade)
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
	err := tradeRepo.Propose(testCtx(t), h.ID, trade)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Propose(requested instance already live) error = %v, want ErrInstanceNotTradeable", err)
	}
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

	if err := tradeRepo.Decline(testCtx(t), h.ID, trade.ID, m2); err != nil {
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

	if err := tradeRepo.Decline(testCtx(t), h.ID, trade.ID, m2); err != nil {
		t.Fatalf("Decline (first): %v", err)
	}
	err := tradeRepo.Decline(testCtx(t), h.ID, trade.ID, m2)
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
		proposeErr = tradeRepo.Propose(context.Background(), h.ID, trade2)
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
