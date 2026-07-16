package adapter_test

import (
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/adapter"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

// seedRecurringTaskWithTitle mirrors seedRecurringTask but lets the caller
// choose the title and points, so tests asserting on resolved chore titles
// (NES-122) can distinguish the offered side from the requested side —
// seedRecurringTask's fixed "Vacuum living room" title is otherwise
// identical across every seeded task.
func seedRecurringTaskWithTitle(
	t *testing.T,
	repo *adapter.RecurringTaskRepository,
	householdID household.HouseholdID,
	title string,
	points int,
) *domain.RecurringTask {
	t.Helper()
	rt := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    householdID,
		Title:          title,
		Category:       domain.ChoreCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationRoundRobin,
		Points:         points,
		LeadTimeDays:   2,
		Active:         true,
	}
	if err := repo.Create(testCtx(t), rt); err != nil {
		t.Fatalf("seedRecurringTaskWithTitle: Create: %v", err)
	}
	return rt
}

// ---------------------------------------------------------------------------
// TaskInstanceRepository.ListTradeableAssignedToOthers
// ---------------------------------------------------------------------------

// TestInstance_ListTradeableAssignedToOthers_ExcludesOwnChore verifies the
// picker query behind "propose a trade": it returns tradeable chores
// assigned to OTHER members, never the caller's own.
func TestInstance_ListTradeableAssignedToOthers_ExcludesOwnChore(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)
	m3 := seedThirdMember(t, pool, h.ID)

	due := refDate.AddDate(0, 0, 5)
	rt1 := seedRecurringTask(t, taskRepo, h.ID)
	rt2 := seedRecurringTask(t, taskRepo, h.ID)
	rt3 := seedRecurringTask(t, taskRepo, h.ID)

	ownChore := seedAssignedTaskInstance(t, instRepo, rt1, due, m1)
	othersChore := seedAssignedTaskInstance(t, instRepo, rt2, due, m2)
	thirdChore := seedAssignedTaskInstance(t, instRepo, rt3, due, m3)

	got, err := instRepo.ListTradeableAssignedToOthers(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("ListTradeableAssignedToOthers: %v", err)
	}

	gotIDs := make(map[domain.TaskInstanceID]bool, len(got))
	for _, inst := range got {
		gotIDs[inst.ID] = true
	}
	if gotIDs[ownChore.ID] {
		t.Error("result includes the caller's own chore, want excluded")
	}
	if !gotIDs[othersChore.ID] {
		t.Error("result missing m2's tradeable chore")
	}
	if !gotIDs[thirdChore.ID] {
		t.Error("result missing m3's tradeable chore")
	}
}

// TestInstance_ListTradeableAssignedToOthers_ExcludesNonTradeable verifies
// that a claimed instance is excluded even though it is assigned to another
// member — IsInstanceTradeable requires claimed_by IS NULL.
func TestInstance_ListTradeableAssignedToOthers_ExcludesNonTradeable(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	due := refDate.AddDate(0, 0, 5)
	rt := seedRecurringTask(t, taskRepo, h.ID)
	claimable := seedTaskInstance(t, instRepo, rt, due)
	if err := instRepo.Claim(testCtx(t), h.ID, claimable.ID, m2); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	got, err := instRepo.ListTradeableAssignedToOthers(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("ListTradeableAssignedToOthers: %v", err)
	}
	for _, inst := range got {
		if inst.ID == claimable.ID {
			t.Error("result includes a claimed instance, want excluded (not tradeable)")
		}
	}
}

// TestInstance_ListTradeableAssignedToOthers_ExcludesStandingAndOverdue
// verifies that a standing (as-needed) instance and an overdue instance are
// both excluded, even when assigned to another member — IsInstanceTradeable
// requires kind=scheduled and status=pending.
func TestInstance_ListTradeableAssignedToOthers_ExcludesStandingAndOverdue(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	asNeededTask := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    h.ID,
		Title:          "Water plants",
		Category:       domain.ChoreCategory,
		Cadence:        newAsNeededCadence(),
		RotationPolicy: domain.RotationClaimable,
		Points:         5,
		Active:         true,
	}
	if err := taskRepo.Create(testCtx(t), asNeededTask); err != nil {
		t.Fatalf("create as-needed task: %v", err)
	}
	standing := seedStandingInstance(t, instRepo, asNeededTask)
	if err := instRepo.Claim(testCtx(t), h.ID, standing.ID, m2); err != nil {
		t.Fatalf("Claim standing: %v", err)
	}

	rt := seedRecurringTask(t, taskRepo, h.ID)
	overdueInst := seedAssignedTaskInstance(t, instRepo, rt, refDate.AddDate(0, 0, -1), m2)
	if _, err := instRepo.MarkPendingOverdue(testCtx(t), h.ID, refDate); err != nil {
		t.Fatalf("MarkPendingOverdue: %v", err)
	}

	got, err := instRepo.ListTradeableAssignedToOthers(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("ListTradeableAssignedToOthers: %v", err)
	}
	for _, inst := range got {
		if inst.ID == standing.ID {
			t.Error("result includes a standing instance, want excluded")
		}
		if inst.ID == overdueInst.ID {
			t.Error("result includes an overdue instance, want excluded")
		}
	}
}

// ---------------------------------------------------------------------------
// ChoreTradeRepository.ListPendingByMember
// ---------------------------------------------------------------------------

// TestTrade_ListPendingByMember_ReturnsLiveTradesForBothRoles verifies that
// ListPendingByMember returns a trade whether the member is the proposer or
// the responder, and excludes trades that don't involve the member at all.
func TestTrade_ListPendingByMember_ReturnsLiveTradesForBothRoles(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)
	m3 := seedThirdMember(t, pool, h.ID)

	due := refDate.AddDate(0, 0, 5)

	// trade1: m1 is the proposer.
	offered1, requested1 := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, due)
	trade1 := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered1.ID, requested1.ID)

	// trade2: m1 is the responder.
	offered2 := seedAssignedTaskInstance(t, instRepo, seedRecurringTask(t, taskRepo, h.ID), due, m3)
	requested2 := seedAssignedTaskInstance(t, instRepo, seedRecurringTask(t, taskRepo, h.ID), due, m1)
	trade2 := proposeTrade(t, tradeRepo, h.ID, m3, m1, offered2.ID, requested2.ID)

	// trade3: m1 is not a party at all — must not appear.
	offered3 := seedAssignedTaskInstance(t, instRepo, seedRecurringTask(t, taskRepo, h.ID), due, m2)
	requested3 := seedAssignedTaskInstance(t, instRepo, seedRecurringTask(t, taskRepo, h.ID), due, m3)
	proposeTrade(t, tradeRepo, h.ID, m2, m3, offered3.ID, requested3.ID)

	got, err := tradeRepo.ListPendingByMember(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("ListPendingByMember: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListPendingByMember = %d trades, want 2", len(got))
	}
	gotIDs := map[domain.ChoreTradeID]bool{got[0].TradeID: true, got[1].TradeID: true}
	if !gotIDs[trade1.ID] || !gotIDs[trade2.ID] {
		t.Errorf("ListPendingByMember missing an expected trade: got %v, want trade1=%v and trade2=%v", gotIDs, trade1.ID, trade2.ID)
	}
}

// TestTrade_ListPendingByMember_ProjectsChoreTitlesAndPoints verifies that
// ListPendingByMember's joined projection (NES-122 CodeRabbit follow-up)
// resolves both sides' titles and point values correctly attributed to
// their own side, without a separate per-trade lookup.
func TestTrade_ListPendingByMember_ProjectsChoreTitlesAndPoints(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	due := refDate.AddDate(0, 0, 5)
	offeredTask := seedRecurringTaskWithTitle(t, taskRepo, h.ID, "Mow the lawn", 15)
	requestedTask := seedRecurringTaskWithTitle(t, taskRepo, h.ID, "Wash dishes", 5)
	offered := seedAssignedTaskInstance(t, instRepo, offeredTask, due, m1)
	requested := seedAssignedTaskInstance(t, instRepo, requestedTask, due, m2)
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	got, err := tradeRepo.ListPendingByMember(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("ListPendingByMember: %v", err)
	}
	if len(got) != 1 || got[0].TradeID != trade.ID {
		t.Fatalf("ListPendingByMember = %v, want exactly [trade %v]", got, trade.ID)
	}
	if got[0].OfferedTitle != "Mow the lawn" || got[0].OfferedPoints != 15 {
		t.Errorf("offered summary = (%q, %d), want (%q, %d)", got[0].OfferedTitle, got[0].OfferedPoints, "Mow the lawn", 15)
	}
	if got[0].RequestedTitle != "Wash dishes" || got[0].RequestedPoints != 5 {
		t.Errorf("requested summary = (%q, %d), want (%q, %d)", got[0].RequestedTitle, got[0].RequestedPoints, "Wash dishes", 5)
	}
}

// TestTrade_ListPendingByMember_ExcludesResolvedTrades verifies that a
// declined trade no longer appears — ListPendingByMember is scoped to
// status='proposed' only.
func TestTrade_ListPendingByMember_ExcludesResolvedTrades(t *testing.T) {
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

	got, err := tradeRepo.ListPendingByMember(testCtx(t), h.ID, m1)
	if err != nil {
		t.Fatalf("ListPendingByMember: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListPendingByMember = %d trades, want 0 (trade was declined)", len(got))
	}
}

// ---------------------------------------------------------------------------
// ChoreTradeRepository.ListHistory
// ---------------------------------------------------------------------------

// TestTrade_ListHistory_ReturnsAllStatusesNewestFirst verifies that
// ListHistory returns every trade regardless of status, ordered by
// created_at descending.
func TestTrade_ListHistory_ReturnsAllStatusesNewestFirst(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	due := refDate.AddDate(0, 0, 5)
	offered1, requested1 := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, due)
	trade1 := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered1.ID, requested1.ID)
	if _, err := tradeRepo.Decline(testCtx(t), h.ID, trade1.ID, m2); err != nil {
		t.Fatalf("Decline: %v", err)
	}

	offered2, requested2 := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, due.AddDate(0, 0, 1))
	trade2 := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered2.ID, requested2.ID)

	got, err := tradeRepo.ListHistory(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListHistory = %d trades, want 2", len(got))
	}
	if got[0].TradeID != trade2.ID || got[1].TradeID != trade1.ID {
		t.Errorf("ListHistory order = [%v, %v], want [trade2 %v, trade1 %v] (newest first)",
			got[0].TradeID, got[1].TradeID, trade2.ID, trade1.ID)
	}
	if got[1].Status != domain.TradeDeclined {
		t.Errorf("trade1 status = %v, want TradeDeclined", got[1].Status)
	}
	if got[0].Status != domain.TradeProposed {
		t.Errorf("trade2 status = %v, want TradeProposed", got[0].Status)
	}
}

// TestTrade_ListHistory_TenantScoped verifies that a trade belonging to a
// different household never appears.
func TestTrade_ListHistory_TenantScoped(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h1, m1, m2 := seedHousehold(t, pool)
	h2, n1, n2 := seedHousehold(t, pool)

	due := refDate.AddDate(0, 0, 5)
	offered1, requested1 := seedTwoTradeableInstances(t, taskRepo, instRepo, h1.ID, m1, m2, due)
	trade1 := proposeTrade(t, tradeRepo, h1.ID, m1, m2, offered1.ID, requested1.ID)

	offered2, requested2 := seedTwoTradeableInstances(t, taskRepo, instRepo, h2.ID, n1, n2, due)
	proposeTrade(t, tradeRepo, h2.ID, n1, n2, offered2.ID, requested2.ID)

	got, err := tradeRepo.ListHistory(testCtx(t), h1.ID)
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(got) != 1 || got[0].TradeID != trade1.ID {
		t.Errorf("ListHistory(h1) = %v, want exactly [trade1 %v]", got, trade1.ID)
	}
}

// TestTrade_ListHistory_ArchivesInactiveRecurringTask verifies that a trade
// whose offered chore's recurring task was later deactivated still appears
// in history, with "(archived)"/0 in place of the real title/points for
// that side only — the joined projection's active-flag fallback (NES-122
// CodeRabbit follow-up), mirroring WebHandlers.buildInstanceRow's precedent.
func TestTrade_ListHistory_ArchivesInactiveRecurringTask(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	due := refDate.AddDate(0, 0, 5)
	offeredTask := seedRecurringTaskWithTitle(t, taskRepo, h.ID, "Mow the lawn", 15)
	requestedTask := seedRecurringTaskWithTitle(t, taskRepo, h.ID, "Wash dishes", 5)
	offered := seedAssignedTaskInstance(t, instRepo, offeredTask, due, m1)
	requested := seedAssignedTaskInstance(t, instRepo, requestedTask, due, m2)
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	// Simulate the offered task being deactivated after the trade was
	// proposed (no public API deactivates a task, so this goes straight to
	// the database, mirroring nes121_postgres_test.go's precedent for
	// simulating an out-of-band administrative change).
	if _, err := pool.Exec(testCtx(t), "UPDATE recurring_task SET active = false WHERE id = $1", offeredTask.ID.String()); err != nil {
		t.Fatalf("deactivate offered task: %v", err)
	}

	got, err := tradeRepo.ListHistory(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(got) != 1 || got[0].TradeID != trade.ID {
		t.Fatalf("ListHistory = %v, want exactly [trade %v]", got, trade.ID)
	}
	if got[0].OfferedTitle != "(archived)" {
		t.Errorf("OfferedTitle = %q, want %q (offered task was deactivated)", got[0].OfferedTitle, "(archived)")
	}
	if got[0].OfferedPoints != 0 {
		t.Errorf("OfferedPoints = %d, want 0 for an archived chore", got[0].OfferedPoints)
	}
	if got[0].RequestedTitle != "Wash dishes" || got[0].RequestedPoints != 5 {
		t.Errorf("requested summary = (%q, %d), want (%q, %d) (unaffected side)", got[0].RequestedTitle, got[0].RequestedPoints, "Wash dishes", 5)
	}
}

// TestTrade_ListHistory_RespectsLimit verifies that ListHistory never
// returns more than domain.TradeHistoryLimit rows, and that the returned
// rows are the MOST RECENT ones — the oldest trade proposed is evicted while
// the most recently proposed one is kept (NES-122 CodeRabbit follow-up: the
// original unbounded ListHistory could grow forever).
func TestTrade_ListHistory_RespectsLimit(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	due := refDate.AddDate(0, 0, 5)
	total := domain.TradeHistoryLimit + 3
	tradeIDs := make([]domain.ChoreTradeID, 0, total)
	for i := 0; i < total; i++ {
		offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, due)
		trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)
		tradeIDs = append(tradeIDs, trade.ID)
	}

	got, err := tradeRepo.ListHistory(testCtx(t), h.ID)
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(got) != domain.TradeHistoryLimit {
		t.Fatalf("ListHistory = %d trades, want %d (capped)", len(got), domain.TradeHistoryLimit)
	}

	gotIDs := make(map[domain.ChoreTradeID]bool, len(got))
	for _, g := range got {
		gotIDs[g.TradeID] = true
	}
	if !gotIDs[tradeIDs[len(tradeIDs)-1]] {
		t.Error("the most recently proposed trade is missing from the capped history")
	}
	if gotIDs[tradeIDs[0]] {
		t.Error("the oldest trade should have been evicted by the cap, but is present")
	}
}

// ---------------------------------------------------------------------------
// Propose / Decline notification payloads (NES-122)
// ---------------------------------------------------------------------------

// TestTrade_Propose_ReturnsProposedTradeWithTitles verifies that Propose
// resolves and returns both instances' titles, correctly attributed to their
// own side (offered vs requested) — the payload TradeService.Propose uses to
// build the "proposal received" notification.
func TestTrade_Propose_ReturnsProposedTradeWithTitles(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	due := refDate.AddDate(0, 0, 5)
	offeredTask := seedRecurringTaskWithTitle(t, taskRepo, h.ID, "Mow the lawn", 15)
	requestedTask := seedRecurringTaskWithTitle(t, taskRepo, h.ID, "Wash dishes", 5)
	offered := seedAssignedTaskInstance(t, instRepo, offeredTask, due, m1)
	requested := seedAssignedTaskInstance(t, instRepo, requestedTask, due, m2)

	trade := &domain.ChoreTrade{
		ID:                  domain.NewChoreTradeID(),
		ProposerID:          m1,
		ResponderID:         m2,
		OfferedInstanceID:   offered.ID,
		RequestedInstanceID: requested.ID,
	}
	proposed, err := tradeRepo.Propose(testCtx(t), h.ID, trade)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposed.TradeID != trade.ID {
		t.Errorf("ProposedTrade.TradeID = %v, want %v", proposed.TradeID, trade.ID)
	}
	if proposed.ProposerID != m1 || proposed.ResponderID != m2 {
		t.Errorf("ProposedTrade parties = (%v, %v), want (%v, %v)", proposed.ProposerID, proposed.ResponderID, m1, m2)
	}
	if proposed.OfferedTitle != "Mow the lawn" {
		t.Errorf("ProposedTrade.OfferedTitle = %q, want %q", proposed.OfferedTitle, "Mow the lawn")
	}
	if proposed.RequestedTitle != "Wash dishes" {
		t.Errorf("ProposedTrade.RequestedTitle = %q, want %q", proposed.RequestedTitle, "Wash dishes")
	}
}

// TestTrade_Decline_ReturnsDeclinedTradeWithTitles verifies that Decline
// resolves and returns the proposer id and both instances' titles — the
// payload TradeService.Decline uses to build the "trade declined"
// notification.
func TestTrade_Decline_ReturnsDeclinedTradeWithTitles(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	due := refDate.AddDate(0, 0, 5)
	offeredTask := seedRecurringTaskWithTitle(t, taskRepo, h.ID, "Mow the lawn", 15)
	requestedTask := seedRecurringTaskWithTitle(t, taskRepo, h.ID, "Wash dishes", 5)
	offered := seedAssignedTaskInstance(t, instRepo, offeredTask, due, m1)
	requested := seedAssignedTaskInstance(t, instRepo, requestedTask, due, m2)
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	declined, err := tradeRepo.Decline(testCtx(t), h.ID, trade.ID, m2)
	if err != nil {
		t.Fatalf("Decline: %v", err)
	}
	if declined.TradeID != trade.ID {
		t.Errorf("DeclinedTrade.TradeID = %v, want %v", declined.TradeID, trade.ID)
	}
	if declined.ProposerID != m1 {
		t.Errorf("DeclinedTrade.ProposerID = %v, want %v", declined.ProposerID, m1)
	}
	if declined.OfferedTitle != "Mow the lawn" {
		t.Errorf("DeclinedTrade.OfferedTitle = %q, want %q", declined.OfferedTitle, "Mow the lawn")
	}
	if declined.RequestedTitle != "Wash dishes" {
		t.Errorf("DeclinedTrade.RequestedTitle = %q, want %q", declined.RequestedTitle, "Wash dishes")
	}
}

// TestTrade_Decline_WrongResponder_ReturnsZeroValueDeclinedTrade verifies
// that a rejected Decline returns the zero-value DeclinedTrade alongside its
// error — callers must never build a notification from a failed Decline.
func TestTrade_Decline_WrongResponder_ReturnsZeroValueDeclinedTrade(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instRepo := adapter.NewTaskInstanceRepository(pool)
	tradeRepo := adapter.NewTradeRepository(pool)
	h, m1, m2 := seedHousehold(t, pool)

	offered, requested := seedTwoTradeableInstances(t, taskRepo, instRepo, h.ID, m1, m2, refDate.AddDate(0, 0, 5))
	trade := proposeTrade(t, tradeRepo, h.ID, m1, m2, offered.ID, requested.ID)

	declined, err := tradeRepo.Decline(testCtx(t), h.ID, trade.ID, household.NewMemberID())
	if err == nil {
		t.Fatal("Decline(wrong responder) error = nil, want non-nil")
	}
	if declined != (domain.DeclinedTrade{}) {
		t.Errorf("DeclinedTrade = %+v, want the zero value on error", declined)
	}
}
