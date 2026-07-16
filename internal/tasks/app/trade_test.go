package app_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// fakeChoreTradeRepo is an in-memory implementation of
// domain.ChoreTradeRepository for use in hermetic tests (NES-121). Propose and
// Accept mirror the real adapter's validation contract (tradeability,
// ownership, self-trade already checked by TradeService, live-proposal
// collision) against the fake's own in-memory instances slice, so
// TradeService unit tests can exercise every domain error without a
// database.
// ---------------------------------------------------------------------------

type fakeChoreTradeRepo struct {
	mu        sync.Mutex
	trades    []*domain.ChoreTrade
	instances []*domain.TaskInstance
	titles    map[domain.TaskInstanceID]string

	// sweepCalls/sweepErr/sweepCount let scheduler tests assert that
	// RunOnce's NES-121 step ran and surface its errors, mirroring
	// callCountingInstanceRepo's claimExpiry* fields.
	sweepCalls atomic.Int64
	sweepErr   error
	sweepCount int
}

func newFakeChoreTradeRepo() *fakeChoreTradeRepo {
	return &fakeChoreTradeRepo{titles: make(map[domain.TaskInstanceID]string)}
}

// seedInstance registers inst (with its display title) so Propose/Accept can
// resolve tradeability and ownership against it. Tests own inst's lifecycle
// (e.g. mutating AssigneeID/Status/ClaimedBy) via the pointer returned here.
func (r *fakeChoreTradeRepo) seedInstance(inst *domain.TaskInstance, title string) *domain.TaskInstance {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.instances = append(r.instances, inst)
	r.titles[inst.ID] = title
	return inst
}

// findInstanceLocked looks up id, scoped to householdID — mirroring the real
// adapter's tenant-scoped lockTradeInstances query — so an instance seeded
// under a different household is treated as not found, exactly like an
// unknown id (NES-121 tenant isolation).
func (r *fakeChoreTradeRepo) findInstanceLocked(householdID household.HouseholdID, id domain.TaskInstanceID) *domain.TaskInstance {
	for _, inst := range r.instances {
		if inst.ID == id && inst.HouseholdID == householdID {
			return inst
		}
	}
	return nil
}

func (r *fakeChoreTradeRepo) hasLiveProposalLocked(offeredID, requestedID domain.TaskInstanceID) bool {
	for _, tr := range r.trades {
		if tr.Status != domain.TradeProposed {
			continue
		}
		if tr.OfferedInstanceID == offeredID || tr.OfferedInstanceID == requestedID ||
			tr.RequestedInstanceID == offeredID || tr.RequestedInstanceID == requestedID {
			return true
		}
	}
	return false
}

func (r *fakeChoreTradeRepo) Propose(_ context.Context, householdID household.HouseholdID, trade *domain.ChoreTrade) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	offered := r.findInstanceLocked(householdID, trade.OfferedInstanceID)
	requested := r.findInstanceLocked(householdID, trade.RequestedInstanceID)
	if offered == nil || requested == nil {
		return domain.ErrInstanceNotFound
	}
	if !domain.IsInstanceTradeable(offered) || !domain.IsInstanceTradeable(requested) {
		return domain.ErrInstanceNotTradeable
	}
	if offered.AssigneeID == nil || *offered.AssigneeID != trade.ProposerID {
		return domain.ErrNotYourChore
	}
	if requested.AssigneeID == nil || *requested.AssigneeID != trade.ResponderID {
		return domain.ErrNotYourChore
	}
	if r.hasLiveProposalLocked(trade.OfferedInstanceID, trade.RequestedInstanceID) {
		return domain.ErrInstanceNotTradeable
	}

	expiresAt := *offered.DueOn
	if requested.DueOn.Before(expiresAt) {
		expiresAt = *requested.DueOn
	}

	trade.HouseholdID = householdID
	trade.Status = domain.TradeProposed
	trade.CreatedAt = time.Now()
	trade.ExpiresAt = expiresAt
	snapshot := *trade
	r.trades = append(r.trades, &snapshot)
	return nil
}

func (r *fakeChoreTradeRepo) Get(_ context.Context, householdID household.HouseholdID, id domain.ChoreTradeID) (*domain.ChoreTrade, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, tr := range r.trades {
		if tr.ID == id && tr.HouseholdID == householdID {
			return tr, nil
		}
	}
	return nil, domain.ErrTradeNotFound
}

func (r *fakeChoreTradeRepo) findTradeLocked(householdID household.HouseholdID, id domain.ChoreTradeID) *domain.ChoreTrade {
	for _, tr := range r.trades {
		if tr.ID == id && tr.HouseholdID == householdID {
			return tr
		}
	}
	return nil
}

func (r *fakeChoreTradeRepo) Accept(
	_ context.Context,
	householdID household.HouseholdID,
	id domain.ChoreTradeID,
	responderID household.MemberID,
	at time.Time,
) (domain.AcceptedTrade, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tr := r.findTradeLocked(householdID, id)
	// !tr.ExpiresAt.After(at) means expires_at <= at: the deadline has passed
	// as of at, even though status is still 'proposed' because the sweep
	// hasn't run yet — mirrors the real adapter's "expires_at > $4" guard.
	if tr == nil || tr.Status != domain.TradeProposed || tr.ResponderID != responderID || !tr.ExpiresAt.After(at) {
		return domain.AcceptedTrade{}, domain.ErrTradeNotPending
	}

	offered := r.findInstanceLocked(householdID, tr.OfferedInstanceID)
	requested := r.findInstanceLocked(householdID, tr.RequestedInstanceID)
	if offered == nil || requested == nil ||
		!domain.IsInstanceTradeable(offered) || !domain.IsInstanceTradeable(requested) ||
		offered.AssigneeID == nil || *offered.AssigneeID != tr.ProposerID ||
		requested.AssigneeID == nil || *requested.AssigneeID != tr.ResponderID {
		return domain.AcceptedTrade{}, domain.ErrInstanceNotTradeable
	}

	resolvedAt := at
	tr.Status = domain.TradeAccepted
	tr.ResolvedAt = &resolvedAt
	proposerID := tr.ProposerID
	offered.AssigneeID = &tr.ResponderID
	requested.AssigneeID = &proposerID

	return domain.AcceptedTrade{
		TradeID:        tr.ID,
		HouseholdID:    tr.HouseholdID,
		ProposerID:     tr.ProposerID,
		ResponderID:    tr.ResponderID,
		OfferedTitle:   r.titles[tr.OfferedInstanceID],
		RequestedTitle: r.titles[tr.RequestedInstanceID],
	}, nil
}

func (r *fakeChoreTradeRepo) Decline(
	_ context.Context,
	householdID household.HouseholdID,
	id domain.ChoreTradeID,
	responderID household.MemberID,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	tr := r.findTradeLocked(householdID, id)
	if tr == nil || tr.Status != domain.TradeProposed || tr.ResponderID != responderID {
		return domain.ErrTradeNotPending
	}
	now := time.Now()
	tr.Status = domain.TradeDeclined
	tr.ResolvedAt = &now
	return nil
}

func (r *fakeChoreTradeRepo) Cancel(
	_ context.Context,
	householdID household.HouseholdID,
	id domain.ChoreTradeID,
	proposerID household.MemberID,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	tr := r.findTradeLocked(householdID, id)
	if tr == nil || tr.Status != domain.TradeProposed || tr.ProposerID != proposerID {
		return domain.ErrTradeNotPending
	}
	now := time.Now()
	tr.Status = domain.TradeCancelled
	tr.ResolvedAt = &now
	return nil
}

func (r *fakeChoreTradeRepo) SweepExpiredTrades(_ context.Context, asOf time.Time) ([]domain.ExpiredTrade, error) {
	r.sweepCalls.Add(1)
	if r.sweepErr != nil {
		return nil, r.sweepErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.sweepCount > 0 {
		return make([]domain.ExpiredTrade, r.sweepCount), nil
	}

	var expired []domain.ExpiredTrade
	for _, tr := range r.trades {
		if tr.Status == domain.TradeProposed && !tr.ExpiresAt.After(asOf) {
			now := time.Now()
			tr.Status = domain.TradeExpired
			tr.ResolvedAt = &now
			expired = append(expired, domain.ExpiredTrade{
				TradeID:        tr.ID,
				HouseholdID:    tr.HouseholdID,
				ProposerID:     tr.ProposerID,
				OfferedTitle:   r.titles[tr.OfferedInstanceID],
				RequestedTitle: r.titles[tr.RequestedInstanceID],
			})
		}
	}
	return expired, nil
}

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

var tradeTestHousehold = household.NewHouseholdID()

// newTradeTestInstance returns a pending, scheduled, unclaimed task instance
// assigned to assignee, due on due, ready to be seeded into a
// fakeChoreTradeRepo via seedInstance.
func newTradeTestInstance(assignee household.MemberID, due time.Time) *domain.TaskInstance {
	return &domain.TaskInstance{
		ID:          domain.NewTaskInstanceID(),
		HouseholdID: tradeTestHousehold,
		AssigneeID:  &assignee,
		DueOn:       domain.DueOnPtr(due),
		Status:      domain.StatusPending,
		Kind:        domain.KindScheduled,
	}
}

func newTestTradeService(t *testing.T, tradeRepo domain.ChoreTradeRepository, enqueuer notifydomain.Enqueuer) *app.TradeService {
	t.Helper()
	s, err := app.NewTradeService(tradeRepo, enqueuer, discardLogger())
	if err != nil {
		t.Fatalf("NewTradeService: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Constructor validation
// ---------------------------------------------------------------------------

func TestNewTradeService_NilTradeRepo_ReturnsError(t *testing.T) {
	_, err := app.NewTradeService(nil, newFakeEnqueuer(), discardLogger())
	if err == nil {
		t.Error("NewTradeService(nil tradeRepo) error = nil, want non-nil")
	}
}

func TestNewTradeService_NilEnqueuer_ReturnsError(t *testing.T) {
	_, err := app.NewTradeService(newFakeChoreTradeRepo(), nil, discardLogger())
	if err == nil {
		t.Error("NewTradeService(nil enqueuer) error = nil, want non-nil")
	}
}

func TestNewTradeService_NilLogger_ReturnsError(t *testing.T) {
	_, err := app.NewTradeService(newFakeChoreTradeRepo(), newFakeEnqueuer(), nil)
	if err == nil {
		t.Error("NewTradeService(nil logger) error = nil, want non-nil")
	}
}

// ---------------------------------------------------------------------------
// Propose
// ---------------------------------------------------------------------------

func TestTradeService_Propose_Success(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	proposer, responder := household.NewMemberID(), household.NewMemberID()
	due := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	offered := repo.seedInstance(newTradeTestInstance(proposer, due), "Vacuum")
	requested := repo.seedInstance(newTradeTestInstance(responder, due.AddDate(0, 0, 2)), "Dishes")

	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	trade, err := svc.Propose(context.Background(), tradeTestHousehold, proposer, responder, offered.ID, requested.ID)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if trade.Status != domain.TradeProposed {
		t.Errorf("Status = %v, want TradeProposed", trade.Status)
	}
	if !trade.ExpiresAt.Equal(due) {
		t.Errorf("ExpiresAt = %v, want the earlier due date %v", trade.ExpiresAt, due)
	}
}

func TestTradeService_Propose_Self_ReturnsErrTradeSelf(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	member := household.NewMemberID()
	due := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	offered := repo.seedInstance(newTradeTestInstance(member, due), "Vacuum")
	requested := repo.seedInstance(newTradeTestInstance(member, due), "Dishes")

	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	_, err := svc.Propose(context.Background(), tradeTestHousehold, member, member, offered.ID, requested.ID)
	if !errors.Is(err, domain.ErrTradeSelf) {
		t.Errorf("Propose(self) error = %v, want ErrTradeSelf", err)
	}
}

func TestTradeService_Propose_SameInstanceBothSides_ReturnsErrInstanceNotTradeable(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	proposer, responder := household.NewMemberID(), household.NewMemberID()
	inst := repo.seedInstance(newTradeTestInstance(proposer, time.Now()), "Vacuum")

	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	_, err := svc.Propose(context.Background(), tradeTestHousehold, proposer, responder, inst.ID, inst.ID)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Propose(same instance both sides) error = %v, want ErrInstanceNotTradeable", err)
	}
}

func TestTradeService_Propose_OfferedNotProposers_ReturnsErrNotYourChore(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	proposer, responder, someoneElse := household.NewMemberID(), household.NewMemberID(), household.NewMemberID()
	due := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	offered := repo.seedInstance(newTradeTestInstance(someoneElse, due), "Vacuum")
	requested := repo.seedInstance(newTradeTestInstance(responder, due), "Dishes")

	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	_, err := svc.Propose(context.Background(), tradeTestHousehold, proposer, responder, offered.ID, requested.ID)
	if !errors.Is(err, domain.ErrNotYourChore) {
		t.Errorf("Propose(offered not proposer's) error = %v, want ErrNotYourChore", err)
	}
}

func TestTradeService_Propose_RequestedNotResponders_ReturnsErrNotYourChore(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	proposer, responder, someoneElse := household.NewMemberID(), household.NewMemberID(), household.NewMemberID()
	due := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	offered := repo.seedInstance(newTradeTestInstance(proposer, due), "Vacuum")
	requested := repo.seedInstance(newTradeTestInstance(someoneElse, due), "Dishes")

	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	_, err := svc.Propose(context.Background(), tradeTestHousehold, proposer, responder, offered.ID, requested.ID)
	if !errors.Is(err, domain.ErrNotYourChore) {
		t.Errorf("Propose(requested not responder's) error = %v, want ErrNotYourChore", err)
	}
}

func TestTradeService_Propose_ClaimedInstance_ReturnsErrInstanceNotTradeable(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	proposer, responder := household.NewMemberID(), household.NewMemberID()
	due := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	offered := newTradeTestInstance(proposer, due)
	offered.ClaimedBy = &proposer
	repo.seedInstance(offered, "Vacuum")
	requested := repo.seedInstance(newTradeTestInstance(responder, due), "Dishes")

	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	_, err := svc.Propose(context.Background(), tradeTestHousehold, proposer, responder, offered.ID, requested.ID)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Propose(claimed offered instance) error = %v, want ErrInstanceNotTradeable", err)
	}
}

func TestTradeService_Propose_StandingInstance_ReturnsErrInstanceNotTradeable(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	proposer, responder := household.NewMemberID(), household.NewMemberID()
	standing := &domain.TaskInstance{
		ID:          domain.NewTaskInstanceID(),
		HouseholdID: tradeTestHousehold,
		AssigneeID:  &proposer,
		Status:      domain.StatusPending,
		Kind:        domain.KindStanding,
	}
	repo.seedInstance(standing, "Anytime chore")
	requested := repo.seedInstance(newTradeTestInstance(responder, time.Now()), "Dishes")

	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	_, err := svc.Propose(context.Background(), tradeTestHousehold, proposer, responder, standing.ID, requested.ID)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Propose(standing offered instance) error = %v, want ErrInstanceNotTradeable", err)
	}
}

func TestTradeService_Propose_LiveProposalOnOfferedInstance_ReturnsErrInstanceNotTradeable(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	proposer, responder, other := household.NewMemberID(), household.NewMemberID(), household.NewMemberID()
	due := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	offered := repo.seedInstance(newTradeTestInstance(proposer, due), "Vacuum")
	requested1 := repo.seedInstance(newTradeTestInstance(responder, due), "Dishes")
	requested2 := repo.seedInstance(newTradeTestInstance(other, due), "Laundry")

	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	if _, err := svc.Propose(context.Background(), tradeTestHousehold, proposer, responder, offered.ID, requested1.ID); err != nil {
		t.Fatalf("first Propose: %v", err)
	}

	// Same offered instance, different requested instance and responder —
	// still blocked because offered.ID already has a live proposal.
	_, err := svc.Propose(context.Background(), tradeTestHousehold, proposer, other, offered.ID, requested2.ID)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Propose(offered instance already live) error = %v, want ErrInstanceNotTradeable", err)
	}
}

func TestTradeService_Propose_LiveProposalOnRequestedInstance_ReturnsErrInstanceNotTradeable(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	proposer1, proposer2, responder := household.NewMemberID(), household.NewMemberID(), household.NewMemberID()
	due := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	offered1 := repo.seedInstance(newTradeTestInstance(proposer1, due), "Vacuum")
	offered2 := repo.seedInstance(newTradeTestInstance(proposer2, due), "Laundry")
	requested := repo.seedInstance(newTradeTestInstance(responder, due), "Dishes")

	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	if _, err := svc.Propose(context.Background(), tradeTestHousehold, proposer1, responder, offered1.ID, requested.ID); err != nil {
		t.Fatalf("first Propose: %v", err)
	}

	// Same requested instance, different offered instance and proposer —
	// still blocked because requested.ID already has a live proposal.
	_, err := svc.Propose(context.Background(), tradeTestHousehold, proposer2, responder, offered2.ID, requested.ID)
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Propose(requested instance already live) error = %v, want ErrInstanceNotTradeable", err)
	}
}

// TestTradeService_Propose_CrossHouseholdInstance_ReturnsErrInstanceNotFound
// covers tenant isolation: an instance that exists but belongs to a
// DIFFERENT household than the one Propose is called with must be treated as
// unknown, exactly like an instance id that doesn't exist at all — mirroring
// the real adapter's household-scoped lockTradeInstances query.
func TestTradeService_Propose_CrossHouseholdInstance_ReturnsErrInstanceNotFound(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	proposer, responder := household.NewMemberID(), household.NewMemberID()
	due := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

	// requested belongs to a different household than tradeTestHousehold.
	otherHousehold := household.NewHouseholdID()
	offered := repo.seedInstance(newTradeTestInstance(proposer, due), "Vacuum")
	requested := &domain.TaskInstance{
		ID:          domain.NewTaskInstanceID(),
		HouseholdID: otherHousehold,
		AssigneeID:  &responder,
		DueOn:       domain.DueOnPtr(due),
		Status:      domain.StatusPending,
		Kind:        domain.KindScheduled,
	}
	repo.seedInstance(requested, "Dishes (other household)")

	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	_, err := svc.Propose(context.Background(), tradeTestHousehold, proposer, responder, offered.ID, requested.ID)
	if !errors.Is(err, domain.ErrInstanceNotFound) {
		t.Errorf("Propose(cross-household requested instance) error = %v, want ErrInstanceNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Accept
// ---------------------------------------------------------------------------

// seedProposedTrade seeds two tradeable instances and proposes a trade
// between them, returning the trade, the two instances, and both member ids.
func seedProposedTrade(t *testing.T, repo *fakeChoreTradeRepo, svc *app.TradeService) (
	trade *domain.ChoreTrade, offered, requested *domain.TaskInstance, proposer, responder household.MemberID,
) {
	t.Helper()
	proposer, responder = household.NewMemberID(), household.NewMemberID()
	due := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	offered = repo.seedInstance(newTradeTestInstance(proposer, due), "Vacuum")
	requested = repo.seedInstance(newTradeTestInstance(responder, due), "Dishes")

	trade, err := svc.Propose(context.Background(), tradeTestHousehold, proposer, responder, offered.ID, requested.ID)
	if err != nil {
		t.Fatalf("seedProposedTrade: Propose: %v", err)
	}
	return trade, offered, requested, proposer, responder
}

func TestTradeService_Accept_SwapsAssignees(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	trade, offered, requested, _, responder := seedProposedTrade(t, repo, svc)

	if err := svc.Accept(context.Background(), tradeTestHousehold, trade.ID, responder, trade.ExpiresAt.Add(-time.Hour)); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if offered.AssigneeID == nil || *offered.AssigneeID != responder {
		t.Errorf("offered.AssigneeID = %v, want responder %v", offered.AssigneeID, responder)
	}
	if requested.AssigneeID == nil || *requested.AssigneeID != trade.ProposerID {
		t.Errorf("requested.AssigneeID = %v, want proposer %v", requested.AssigneeID, trade.ProposerID)
	}

	got, err := repo.Get(context.Background(), tradeTestHousehold, trade.ID)
	if err != nil {
		t.Fatalf("Get after accept: %v", err)
	}
	if got.Status != domain.TradeAccepted {
		t.Errorf("Status = %v, want TradeAccepted", got.Status)
	}
	if got.ResolvedAt == nil {
		t.Error("ResolvedAt is nil, want set")
	}
}

func TestTradeService_Accept_EnqueuesBothPartyNotifications(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	enqueuer := newFakeEnqueuer()
	svc := newTestTradeService(t, repo, enqueuer)
	trade, _, _, proposer, responder := seedProposedTrade(t, repo, svc)

	if err := svc.Accept(context.Background(), tradeTestHousehold, trade.ID, responder, trade.ExpiresAt.Add(-time.Hour)); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if len(enqueuer.notifications) != 2 {
		t.Fatalf("enqueued notifications = %d, want 2", len(enqueuer.notifications))
	}
	addressees := map[household.MemberID]bool{}
	for _, n := range enqueuer.notifications {
		if n.MemberID == nil {
			t.Fatal("notification MemberID is nil, want set")
		}
		addressees[*n.MemberID] = true
	}
	if !addressees[proposer] {
		t.Error("no notification addressed to the proposer")
	}
	if !addressees[responder] {
		t.Error("no notification addressed to the responder")
	}
}

func TestTradeService_Accept_WrongResponder_ReturnsErrTradeNotPending(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	trade, _, _, _, _ := seedProposedTrade(t, repo, svc)

	err := svc.Accept(context.Background(), tradeTestHousehold, trade.ID, household.NewMemberID(), trade.ExpiresAt.Add(-time.Hour))
	if !errors.Is(err, domain.ErrTradeNotPending) {
		t.Errorf("Accept(wrong responder) error = %v, want ErrTradeNotPending", err)
	}
}

func TestTradeService_Accept_AlreadyResolved_ReturnsErrTradeNotPending(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	trade, _, _, _, responder := seedProposedTrade(t, repo, svc)

	if err := svc.Decline(context.Background(), tradeTestHousehold, trade.ID, responder); err != nil {
		t.Fatalf("Decline: %v", err)
	}

	err := svc.Accept(context.Background(), tradeTestHousehold, trade.ID, responder, trade.ExpiresAt.Add(-time.Hour))
	if !errors.Is(err, domain.ErrTradeNotPending) {
		t.Errorf("Accept(already declined) error = %v, want ErrTradeNotPending", err)
	}
}

// TestTradeService_Accept_InstanceCompletedAfterProposal_ReturnsErrInstanceNotTradeable
// covers AC2: an instance completed after the trade was proposed fails
// re-validation on accept, and neither instance's assignee changes.
func TestTradeService_Accept_InstanceCompletedAfterProposal_ReturnsErrInstanceNotTradeable(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	trade, offered, requested, proposer, responder := seedProposedTrade(t, repo, svc)

	offered.Status = domain.StatusDone

	err := svc.Accept(context.Background(), tradeTestHousehold, trade.ID, responder, trade.ExpiresAt.Add(-time.Hour))
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Accept(offered completed) error = %v, want ErrInstanceNotTradeable", err)
	}
	if offered.AssigneeID == nil || *offered.AssigneeID != proposer {
		t.Errorf("offered.AssigneeID = %v, want unchanged proposer %v", offered.AssigneeID, proposer)
	}
	if requested.AssigneeID == nil || *requested.AssigneeID != responder {
		t.Errorf("requested.AssigneeID = %v, want unchanged responder %v", requested.AssigneeID, responder)
	}
}

// TestTradeService_Accept_InstanceReassignedAfterProposal_ReturnsErrInstanceNotTradeable
// covers AC2's other half: an instance reassigned to a different member after
// the trade was proposed also fails re-validation.
func TestTradeService_Accept_InstanceReassignedAfterProposal_ReturnsErrInstanceNotTradeable(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	trade, offered, _, _, responder := seedProposedTrade(t, repo, svc)

	someoneElse := household.NewMemberID()
	offered.AssigneeID = &someoneElse

	err := svc.Accept(context.Background(), tradeTestHousehold, trade.ID, responder, trade.ExpiresAt.Add(-time.Hour))
	if !errors.Is(err, domain.ErrInstanceNotTradeable) {
		t.Errorf("Accept(offered reassigned) error = %v, want ErrInstanceNotTradeable", err)
	}
}

// TestTradeService_Accept_ExpiredButNotYetSwept_ReturnsErrTradeNotPending is
// the hermetic (service-layer) counterpart to the gated
// TestTrade_Accept_ExpiredButNotYetSwept_ReturnsErrTradeNotPending: a trade
// whose ExpiresAt has passed as of at, but whose status is still
// TradeProposed because no sweep has run, must not be acceptable, and no
// instance assignment may change.
func TestTradeService_Accept_ExpiredButNotYetSwept_ReturnsErrTradeNotPending(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	trade, offered, requested, proposer, responder := seedProposedTrade(t, repo, svc)

	err := svc.Accept(context.Background(), tradeTestHousehold, trade.ID, responder, trade.ExpiresAt)
	if !errors.Is(err, domain.ErrTradeNotPending) {
		t.Errorf("Accept(at deadline, status still proposed) error = %v, want ErrTradeNotPending", err)
	}
	if offered.AssigneeID == nil || *offered.AssigneeID != proposer {
		t.Errorf("offered.AssigneeID = %v, want unchanged proposer %v", offered.AssigneeID, proposer)
	}
	if requested.AssigneeID == nil || *requested.AssigneeID != responder {
		t.Errorf("requested.AssigneeID = %v, want unchanged responder %v", requested.AssigneeID, responder)
	}
}

// ---------------------------------------------------------------------------
// Decline / Cancel
// ---------------------------------------------------------------------------

func TestTradeService_Decline_NoAssignmentChange(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	trade, offered, requested, proposer, responder := seedProposedTrade(t, repo, svc)

	if err := svc.Decline(context.Background(), tradeTestHousehold, trade.ID, responder); err != nil {
		t.Fatalf("Decline: %v", err)
	}

	got, err := repo.Get(context.Background(), tradeTestHousehold, trade.ID)
	if err != nil {
		t.Fatalf("Get after decline: %v", err)
	}
	if got.Status != domain.TradeDeclined {
		t.Errorf("Status = %v, want TradeDeclined", got.Status)
	}
	if *offered.AssigneeID != proposer || *requested.AssigneeID != responder {
		t.Error("Decline must not change either instance's assignee")
	}
}

func TestTradeService_Decline_WrongResponder_ReturnsErrTradeNotPending(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	trade, _, _, _, _ := seedProposedTrade(t, repo, svc)

	err := svc.Decline(context.Background(), tradeTestHousehold, trade.ID, household.NewMemberID())
	if !errors.Is(err, domain.ErrTradeNotPending) {
		t.Errorf("Decline(wrong responder) error = %v, want ErrTradeNotPending", err)
	}
}

func TestTradeService_Cancel_NoAssignmentChange(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	trade, offered, requested, proposer, responder := seedProposedTrade(t, repo, svc)

	if err := svc.Cancel(context.Background(), tradeTestHousehold, trade.ID, proposer); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	got, err := repo.Get(context.Background(), tradeTestHousehold, trade.ID)
	if err != nil {
		t.Fatalf("Get after cancel: %v", err)
	}
	if got.Status != domain.TradeCancelled {
		t.Errorf("Status = %v, want TradeCancelled", got.Status)
	}
	if *offered.AssigneeID != proposer || *requested.AssigneeID != responder {
		t.Error("Cancel must not change either instance's assignee")
	}
}

func TestTradeService_Cancel_WrongProposer_ReturnsErrTradeNotPending(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	trade, _, _, _, _ := seedProposedTrade(t, repo, svc)

	err := svc.Cancel(context.Background(), tradeTestHousehold, trade.ID, household.NewMemberID())
	if !errors.Is(err, domain.ErrTradeNotPending) {
		t.Errorf("Cancel(wrong proposer) error = %v, want ErrTradeNotPending", err)
	}
}

// ---------------------------------------------------------------------------
// ExpireTrades
// ---------------------------------------------------------------------------

func TestTradeService_ExpireTrades_NoAssignmentChangeAndNotifiesProposer(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	enqueuer := newFakeEnqueuer()
	svc := newTestTradeService(t, repo, enqueuer)
	trade, offered, requested, proposer, responder := seedProposedTrade(t, repo, svc)

	if err := svc.ExpireTrades(context.Background(), trade.ExpiresAt.Add(time.Second)); err != nil {
		t.Fatalf("ExpireTrades: %v", err)
	}

	got, err := repo.Get(context.Background(), tradeTestHousehold, trade.ID)
	if err != nil {
		t.Fatalf("Get after expire: %v", err)
	}
	if got.Status != domain.TradeExpired {
		t.Errorf("Status = %v, want TradeExpired", got.Status)
	}
	if *offered.AssigneeID != proposer || *requested.AssigneeID != responder {
		t.Error("ExpireTrades must not change either instance's assignee")
	}

	if len(enqueuer.notifications) != 1 {
		t.Fatalf("enqueued notifications = %d, want 1", len(enqueuer.notifications))
	}
	if got := enqueuer.notifications[0].MemberID; got == nil || *got != proposer {
		t.Errorf("notification MemberID = %v, want proposer %v", got, proposer)
	}
}

func TestTradeService_ExpireTrades_NotYetExpired_NoOp(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	svc := newTestTradeService(t, repo, newFakeEnqueuer())
	trade, _, _, _, _ := seedProposedTrade(t, repo, svc)

	if err := svc.ExpireTrades(context.Background(), trade.ExpiresAt.Add(-time.Hour)); err != nil {
		t.Fatalf("ExpireTrades: %v", err)
	}

	got, err := repo.Get(context.Background(), tradeTestHousehold, trade.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != domain.TradeProposed {
		t.Errorf("Status = %v, want unchanged TradeProposed", got.Status)
	}
}

func TestTradeService_ExpireTrades_SweepError_ReturnsError(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	repo.sweepErr = errors.New("db: sweep failed")
	svc := newTestTradeService(t, repo, newFakeEnqueuer())

	err := svc.ExpireTrades(context.Background(), time.Now())
	if !errors.Is(err, repo.sweepErr) {
		t.Errorf("ExpireTrades error = %v, want %v", err, repo.sweepErr)
	}
}

// ---------------------------------------------------------------------------
// Post-commit enqueue failure contracts
// ---------------------------------------------------------------------------

// TestTradeService_Accept_SucceedsAndAttemptsBothRecipientsWhenEnqueueFails
// verifies that a failing notification enqueue never short-circuits the
// other recipient's notification, and never turns a successful accept into a
// reported failure — the swap has already committed by the time
// notification-building starts, so Accept's own error must stay nil (see
// Accept's doc).
func TestTradeService_Accept_SucceedsAndAttemptsBothRecipientsWhenEnqueueFails(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	failingEnqueuer := &fakeEnqueuerWithError{errOnCall: 1} // fail only the first attempt
	svc := newTestTradeService(t, repo, failingEnqueuer)
	trade, _, _, _, responder := seedProposedTrade(t, repo, svc)

	err := svc.Accept(context.Background(), tradeTestHousehold, trade.ID, responder, trade.ExpiresAt.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Accept: %v, want nil even though the first notification enqueue failed", err)
	}

	if failingEnqueuer.callCount != 2 {
		t.Errorf("Enqueue call count = %d, want 2 (both recipients attempted despite the first failing)", failingEnqueuer.callCount)
	}
	// Only the second (successful) call's notification was recorded by the
	// embedded fakeEnqueuer; the first failed before being appended.
	if len(failingEnqueuer.notifications) != 1 {
		t.Errorf("recorded notifications = %d, want 1 (the one successful enqueue)", len(failingEnqueuer.notifications))
	}

	got, err := repo.Get(context.Background(), tradeTestHousehold, trade.ID)
	if err != nil {
		t.Fatalf("Get after accept: %v", err)
	}
	if got.Status != domain.TradeAccepted {
		t.Errorf("Status = %v, want TradeAccepted (notification failure must not affect the already-committed swap)", got.Status)
	}
}

// TestTradeService_ExpireTrades_EnqueueFails_ReturnsErrorButTradeStaysExpired
// verifies that a failing notification enqueue during the expiry sweep is
// surfaced as an error (unlike Accept — see ExpireTrades' doc, which mirrors
// EmitClaimExpiry), but the already-committed sweep is never rolled back: the
// trade must still read back as TradeExpired.
func TestTradeService_ExpireTrades_EnqueueFails_ReturnsErrorButTradeStaysExpired(t *testing.T) {
	repo := newFakeChoreTradeRepo()
	failingEnqueuer := &fakeEnqueuerWithError{errOnCall: 1}
	svc := newTestTradeService(t, repo, failingEnqueuer)
	// Propose never touches the enqueuer, so seeding through svc itself
	// (rather than a separate non-failing service) is safe here.
	trade, _, _, _, _ := seedProposedTrade(t, repo, svc)

	err := svc.ExpireTrades(context.Background(), trade.ExpiresAt.Add(time.Second))
	if err == nil {
		t.Fatal("ExpireTrades error = nil, want non-nil (the enqueue failed)")
	}

	got, err := repo.Get(context.Background(), tradeTestHousehold, trade.ID)
	if err != nil {
		t.Fatalf("Get after expire: %v", err)
	}
	if got.Status != domain.TradeExpired {
		t.Errorf("Status = %v, want TradeExpired (the sweep already committed; a failed notification must not roll it back)", got.Status)
	}
}
