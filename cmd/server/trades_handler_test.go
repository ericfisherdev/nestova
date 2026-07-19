package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	tasksadapter "github.com/ericfisherdev/nestova/internal/tasks/adapter"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// Trade domain fakes (NES-122)
// ---------------------------------------------------------------------------

// fakeEnqueuer is a no-op notifydomain.Enqueuer for tests that need
// registerWebRoutes to compile but do not assert on enqueued notifications.
type fakeEnqueuer struct{}

func (fakeEnqueuer) Enqueue(_ context.Context, _ *notifydomain.Notification) error { return nil }

// Compile-time assertion.
var _ notifydomain.Enqueuer = fakeEnqueuer{}

// fakeChoreTradeRepo is a configurable ChoreTradeRepository for unit tests.
// proposeErr/acceptErr/declineErr/cancelErr let individual tests inject
// domain errors from each mutation without a database. pending/history
// (NES-122) hold pre-joined TradeSummary values, mirroring the real
// adapter's joined projection.
type fakeChoreTradeRepo struct {
	proposeErr error
	acceptErr  error
	declineErr error
	cancelErr  error
	pending    []tasksdomain.TradeSummary
	history    []tasksdomain.TradeSummary
}

func (f *fakeChoreTradeRepo) Propose(_ context.Context, householdID household.HouseholdID, trade *tasksdomain.ChoreTrade) (tasksdomain.ProposedTrade, error) {
	if f.proposeErr != nil {
		return tasksdomain.ProposedTrade{}, f.proposeErr
	}
	trade.HouseholdID = householdID
	trade.Status = tasksdomain.TradeProposed
	trade.CreatedAt = time.Now()
	return tasksdomain.ProposedTrade{
		TradeID:     trade.ID,
		HouseholdID: householdID,
		ProposerID:  trade.ProposerID,
		ResponderID: trade.ResponderID,
	}, nil
}

func (f *fakeChoreTradeRepo) Get(_ context.Context, _ household.HouseholdID, _ tasksdomain.ChoreTradeID) (*tasksdomain.ChoreTrade, error) {
	return nil, tasksdomain.ErrTradeNotFound
}

func (f *fakeChoreTradeRepo) Accept(_ context.Context, householdID household.HouseholdID, id tasksdomain.ChoreTradeID, responderID household.MemberID, _ time.Time) (tasksdomain.AcceptedTrade, error) {
	if f.acceptErr != nil {
		return tasksdomain.AcceptedTrade{}, f.acceptErr
	}
	return tasksdomain.AcceptedTrade{TradeID: id, HouseholdID: householdID, ResponderID: responderID}, nil
}

func (f *fakeChoreTradeRepo) Decline(_ context.Context, householdID household.HouseholdID, id tasksdomain.ChoreTradeID, _ household.MemberID) (tasksdomain.DeclinedTrade, error) {
	if f.declineErr != nil {
		return tasksdomain.DeclinedTrade{}, f.declineErr
	}
	return tasksdomain.DeclinedTrade{TradeID: id, HouseholdID: householdID}, nil
}

func (f *fakeChoreTradeRepo) Cancel(_ context.Context, _ household.HouseholdID, _ tasksdomain.ChoreTradeID, _ household.MemberID) error {
	return f.cancelErr
}

func (f *fakeChoreTradeRepo) SweepExpiredTrades(_ context.Context, _ time.Time) ([]tasksdomain.ExpiredTrade, error) {
	return nil, nil
}

func (f *fakeChoreTradeRepo) ListPendingByMember(_ context.Context, _ household.HouseholdID, _ household.MemberID) ([]tasksdomain.TradeSummary, error) {
	return f.pending, nil
}

func (f *fakeChoreTradeRepo) ListHistory(_ context.Context, _ household.HouseholdID) ([]tasksdomain.TradeSummary, error) {
	return f.history, nil
}

// Compile-time assertion.
var _ tasksdomain.ChoreTradeRepository = (*fakeChoreTradeRepo)(nil)

// newTestTradeHandlers builds a TradeWebHandlers wired with no-op fakes.
// taskWebHandlers is reused as the taskGroupsBuilder dependency (it already
// satisfies the interface via its BuildGroups method) so tests that need
// registerWebRoutes to compile do not require a second fake.
func newTestTradeHandlers(
	taskWebHandlers *tasksadapter.WebHandlers,
	instanceRepo tasksdomain.TaskInstanceRepository,
	householdRepo household.HouseholdRepository,
	sm *scs.SessionManager,
	logger *slog.Logger,
) *tasksadapter.TradeWebHandlers {
	tradeRepo := &fakeChoreTradeRepo{}
	svc, err := tasksapp.NewTradeService(tradeRepo, fakeEnqueuer{}, logger)
	if err != nil {
		panic("newTestTradeHandlers: " + err.Error())
	}
	return tasksadapter.NewTradeWebHandlers(
		svc, tradeRepo, instanceRepo, fakeRecurringTaskRepo{}, householdRepo, taskWebHandlers, sm, logger,
	)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// buildTradeTestHandler returns an http.Handler wired with the supplied
// trade/instance repo fakes so each test can control mutation outcomes and
// inspect calls, mirroring buildTaskTestHandler's shape.
func buildTradeTestHandler(
	tradeRepo *fakeChoreTradeRepo,
	instanceRepo *fakeTaskInstanceRepo,
	householdRepo household.HouseholdRepository,
) (http.Handler, *scs.SessionManager) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	authn := authapp.New(testCredRepo{})
	authHandlers := authadapter.NewHandlers(sm, authn, nil, nil, nil, logger)
	onboardingHandlers := authadapter.NewOnboardingHandlers(householdRepo, testCredStore{}, testProvisioner{}, sm, logger)

	recurringRepo := fakeRecurringTaskRepo{}
	taskService, err := tasksapp.NewTaskService(recurringRepo, instanceRepo, nil)
	if err != nil {
		panic("buildTradeTestHandler: " + err.Error())
	}
	taskWebHandlers := tasksadapter.NewWebHandlers(taskService, recurringRepo, instanceRepo, householdRepo, sm, logger, nil)
	gamificationHandlers := newTestGamificationHandlers(instanceRepo, householdRepo, sm, logger)
	groceryHandlers := newTestGroceryHandlers(householdRepo, sm, logger)

	svc, err := tasksapp.NewTradeService(tradeRepo, fakeEnqueuer{}, logger)
	if err != nil {
		panic("buildTradeTestHandler: " + err.Error())
	}
	tradeWebHandlers := tasksadapter.NewTradeWebHandlers(
		svc, tradeRepo, instanceRepo, recurringRepo, householdRepo, taskWebHandlers, sm, logger,
	)

	mux := http.NewServeMux()
	registerWebRoutes(mux, logger, sm, authHandlers, nil, nil, onboardingHandlers, householdRepo, taskWebHandlers, tradeWebHandlers, gamificationHandlers, groceryHandlers, newTestMealsHandlers(sm, logger), newTestCalendarHandlers(sm, logger))

	return sm.LoadAndSave(
		authadapter.Authenticate(sm, householdRepo)(mux),
	), sm
}

// ---------------------------------------------------------------------------
// Tests: auth guards
// ---------------------------------------------------------------------------

func TestTradeHistoryRequiresAuth(t *testing.T) {
	handler, _ := buildTradeTestHandler(&fakeChoreTradeRepo{}, &fakeTaskInstanceRepo{}, authedHouseholdRepo{member: nil})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/trades/history", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated GET /trades/history: status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("Location = %q, want /login...", loc)
	}
}

func TestProposeTradePickerRequiresAuth(t *testing.T) {
	handler, _ := buildTradeTestHandler(&fakeChoreTradeRepo{}, &fakeTaskInstanceRepo{}, authedHouseholdRepo{member: nil})
	fakeID := tasksdomain.NewTaskInstanceID().String()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tasks/"+fakeID+"/propose-trade", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated GET /tasks/{id}/propose-trade: status = %d, want 303", rec.Code)
	}
}

func TestTradeAcceptRequiresAuth(t *testing.T) {
	handler, _ := buildTradeTestHandler(&fakeChoreTradeRepo{}, &fakeTaskInstanceRepo{}, authedHouseholdRepo{member: nil})
	fakeID := tasksdomain.NewChoreTradeID().String()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/trades/"+fakeID+"/accept", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Error("unauthenticated POST /trades/{id}/accept should not return 200")
	}
}

// ---------------------------------------------------------------------------
// Tests: CSRF guard
// ---------------------------------------------------------------------------

func TestTradeAcceptRejectsInvalidCSRF(t *testing.T) {
	fixedMember := &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice", Role: household.RoleAdult, Color: household.ColorSage,
	}
	tradeRepo := &fakeChoreTradeRepo{}
	handler, sm := buildTradeTestHandler(tradeRepo, &fakeTaskInstanceRepo{}, authedHouseholdRepo{member: fixedMember})
	cookie, _ := seedAuthedSession(t, handler, sm, fixedMember.ID.String())

	fakeID := tasksdomain.NewChoreTradeID().String()
	req := httptest.NewRequest(http.MethodPost, "/trades/"+fakeID+"/accept", strings.NewReader("csrf_token=wrong-token"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /trades/{id}/accept with wrong CSRF: status = %d, want 403", rec.Code)
	}
}

func TestProposeTradeRejectsInvalidCSRF(t *testing.T) {
	fixedMember := &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice", Role: household.RoleAdult, Color: household.ColorSage,
	}
	handler, sm := buildTradeTestHandler(&fakeChoreTradeRepo{}, &fakeTaskInstanceRepo{}, authedHouseholdRepo{member: fixedMember})
	cookie, _ := seedAuthedSession(t, handler, sm, fixedMember.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/trades", strings.NewReader("csrf_token=wrong-token"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /trades with wrong CSRF: status = %d, want 403", rec.Code)
	}
}

// TestProposeTradeRebuildFailure_PreservesMappedStatus is the CodeRabbit-
// flagged regression test: when svc.Propose fails and the subsequent picker
// rebuild (to re-render the form with the error message) ALSO fails with a
// mapped domain error, the response must carry THAT mapped status
// (404/403/409) rather than collapsing to a generic 500. Here the initial
// Propose fails with ErrNotYourChore (→ 403), then the rebuild's own
// instanceRepo.Get call (for the offered instance) fails with
// ErrInstanceNotFound — simulating the offered chore vanishing between the
// original failure and the rebuild — which must surface as 404, not 500.
func TestProposeTradeRebuildFailure_PreservesMappedStatus(t *testing.T) {
	fixedMember := &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice", Role: household.RoleAdult, Color: household.ColorSage,
	}
	responder := household.NewMemberID()
	due := time.Now().Add(48 * time.Hour)
	// Call #1 (ProposeTrade's own lookup of the requested instance) succeeds
	// via getInst; call #2 (buildProposeTradeForm's rebuild, looking up the
	// offered instance) fails via getErrOnCall.
	instanceRepo := &fakeTaskInstanceRepo{
		getInst: &tasksdomain.TaskInstance{
			ID: tasksdomain.NewTaskInstanceID(), HouseholdID: fixedMember.HouseholdID,
			AssigneeID: &responder, DueOn: tasksdomain.DueOnPtr(due),
			Status: tasksdomain.StatusPending, Kind: tasksdomain.KindScheduled,
		},
		getErrOnCall: 2,
	}
	tradeRepo := &fakeChoreTradeRepo{proposeErr: tasksdomain.ErrNotYourChore}
	handler, sm := buildTradeTestHandler(tradeRepo, instanceRepo, authedHouseholdRepo{member: fixedMember})
	cookie, csrfToken := seedAuthedSession(t, handler, sm, fixedMember.ID.String())

	offeredID := tasksdomain.NewTaskInstanceID().String()
	requestedID := tasksdomain.NewTaskInstanceID().String()
	body := "csrf_token=" + csrfToken + "&offered_instance_id=" + offeredID + "&requested_instance_id=" + requestedID
	req := httptest.NewRequest(http.MethodPost, "/trades", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("rebuild failure collapsed to 500, want the mapped status from the rebuild's own error; body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (ErrInstanceNotFound from the rebuild's own Get call)", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: trade history role gate (NES-122 AC4)
// ---------------------------------------------------------------------------

func TestTradeHistoryForbiddenForChild(t *testing.T) {
	child := &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Kiddo", Role: household.RoleChild, Color: household.ColorSage,
	}
	handler, sm := buildTradeTestHandler(&fakeChoreTradeRepo{}, &fakeTaskInstanceRepo{}, authedHouseholdRepo{member: child})
	cookie, _ := seedAuthedSession(t, handler, sm, child.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/trades/history", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /trades/history as a child: status = %d, want 403", rec.Code)
	}
}

func TestTradeHistoryAllowedForAdult(t *testing.T) {
	adult := &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice", Role: household.RoleAdult, Color: household.ColorSage,
	}
	handler, sm := buildTradeTestHandler(&fakeChoreTradeRepo{}, &fakeTaskInstanceRepo{}, authedHouseholdRepo{member: adult})
	cookie, _ := seedAuthedSession(t, handler, sm, adult.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/trades/history", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /trades/history as an adult: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Trade History") {
		t.Errorf("trade history page missing heading: %s", rec.Body.String())
	}
}

func TestTradeHistoryAllowedForOwner(t *testing.T) {
	owner := &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice", Role: household.RoleOwner, Color: household.ColorSage,
	}
	handler, sm := buildTradeTestHandler(&fakeChoreTradeRepo{}, &fakeTaskInstanceRepo{}, authedHouseholdRepo{member: owner})
	cookie, _ := seedAuthedSession(t, handler, sm, owner.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/trades/history", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /trades/history as an owner: status = %d, want 200", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: Accept's HTMX response wiring (NES-122 AC2)
// ---------------------------------------------------------------------------

// TestTradeAcceptHTMXResponseIncludesOOBGroupsRefresh verifies, end-to-end
// through the real route, that a successful HTMX accept responds with both
// the resolved card's own removal and an out-of-band #task-groups refresh —
// the wiring AC2 requires so both traded chores show their new assignees
// immediately.
func TestTradeAcceptHTMXResponseIncludesOOBGroupsRefresh(t *testing.T) {
	fixedMember := &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice", Role: household.RoleAdult, Color: household.ColorSage,
	}
	tradeRepo := &fakeChoreTradeRepo{}
	handler, sm := buildTradeTestHandler(tradeRepo, &fakeTaskInstanceRepo{}, authedHouseholdRepo{member: fixedMember})
	cookie, csrfToken := seedAuthedSession(t, handler, sm, fixedMember.ID.String())

	fakeID := tasksdomain.NewChoreTradeID().String()
	req := httptest.NewRequest(http.MethodPost, "/trades/"+fakeID+"/accept", strings.NewReader("csrf_token="+csrfToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HTMX POST /trades/{id}/accept: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="trade-`+fakeID+`"`) {
		t.Errorf("accept response missing the resolved card's own removal: %s", body)
	}
	if !strings.Contains(body, `id="task-groups"`) {
		t.Errorf("accept response missing the #task-groups container: %s", body)
	}
	if !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Errorf("accept response missing hx-swap-oob=\"true\" on #task-groups: %s", body)
	}
}

// TestTradeDeclineHTMXResponseRemovesCardOnly verifies that Decline's HTMX
// response removes the resolved card but carries no #task-groups refresh —
// Decline never changes an instance's assignee.
func TestTradeDeclineHTMXResponseRemovesCardOnly(t *testing.T) {
	fixedMember := &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Bob", Role: household.RoleAdult, Color: household.ColorClay,
	}
	tradeRepo := &fakeChoreTradeRepo{}
	handler, sm := buildTradeTestHandler(tradeRepo, &fakeTaskInstanceRepo{}, authedHouseholdRepo{member: fixedMember})
	cookie, csrfToken := seedAuthedSession(t, handler, sm, fixedMember.ID.String())

	fakeID := tasksdomain.NewChoreTradeID().String()
	req := httptest.NewRequest(http.MethodPost, "/trades/"+fakeID+"/decline", strings.NewReader("csrf_token="+csrfToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HTMX POST /trades/{id}/decline: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="trade-`+fakeID+`"`) {
		t.Errorf("decline response missing the resolved card's own removal: %s", body)
	}
	if strings.Contains(body, "task-groups") {
		t.Errorf("decline response must not refresh #task-groups (no assignee change): %s", body)
	}
}

// TestTradeAcceptMutationError verifies that a rejected accept (trade no
// longer pending) surfaces as 409, mirroring the tasks list's mutation error
// mapping precedent.
func TestTradeAcceptMutationError(t *testing.T) {
	fixedMember := &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice", Role: household.RoleAdult, Color: household.ColorSage,
	}
	tradeRepo := &fakeChoreTradeRepo{acceptErr: tasksdomain.ErrTradeNotPending}
	handler, sm := buildTradeTestHandler(tradeRepo, &fakeTaskInstanceRepo{}, authedHouseholdRepo{member: fixedMember})
	cookie, csrfToken := seedAuthedSession(t, handler, sm, fixedMember.ID.String())

	fakeID := tasksdomain.NewChoreTradeID().String()
	req := httptest.NewRequest(http.MethodPost, "/trades/"+fakeID+"/accept", strings.NewReader("csrf_token="+csrfToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("POST /trades/{id}/accept (not pending): status = %d, want 409", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: propose-trade picker and action
// ---------------------------------------------------------------------------

// TestProposeTradePickerForbiddenForNotOwnChore verifies that a member
// cannot open the picker for a chore that is not currently assigned to them.
func TestProposeTradePickerForbiddenForNotOwnChore(t *testing.T) {
	fixedMember := &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice", Role: household.RoleAdult, Color: household.ColorSage,
	}
	someoneElse := household.NewMemberID()
	due := time.Now().Add(48 * time.Hour)
	instanceRepo := &fakeTaskInstanceRepo{getInst: &tasksdomain.TaskInstance{
		ID:          tasksdomain.NewTaskInstanceID(),
		HouseholdID: fixedMember.HouseholdID,
		AssigneeID:  &someoneElse,
		DueOn:       tasksdomain.DueOnPtr(due),
		Status:      tasksdomain.StatusPending,
		Kind:        tasksdomain.KindScheduled,
	}}
	handler, sm := buildTradeTestHandler(&fakeChoreTradeRepo{}, instanceRepo, authedHouseholdRepo{member: fixedMember})
	cookie, _ := seedAuthedSession(t, handler, sm, fixedMember.ID.String())

	fakeID := tasksdomain.NewTaskInstanceID().String()
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+fakeID+"/propose-trade", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /tasks/{id}/propose-trade (not my chore): status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
}

// TestProposeTradePickerRendersOwnChore verifies that a member CAN open the
// picker for their own pending, tradeable chore.
func TestProposeTradePickerRendersOwnChore(t *testing.T) {
	fixedMember := &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice", Role: household.RoleAdult, Color: household.ColorSage,
	}
	due := time.Now().Add(48 * time.Hour)
	instanceRepo := &fakeTaskInstanceRepo{getInst: &tasksdomain.TaskInstance{
		ID:          tasksdomain.NewTaskInstanceID(),
		HouseholdID: fixedMember.HouseholdID,
		AssigneeID:  &fixedMember.ID,
		DueOn:       tasksdomain.DueOnPtr(due),
		Status:      tasksdomain.StatusPending,
		Kind:        tasksdomain.KindScheduled,
	}}
	handler, sm := buildTradeTestHandler(&fakeChoreTradeRepo{}, instanceRepo, authedHouseholdRepo{member: fixedMember})
	cookie, _ := seedAuthedSession(t, handler, sm, fixedMember.ID.String())

	fakeID := tasksdomain.NewTaskInstanceID().String()
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+fakeID+"/propose-trade", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tasks/{id}/propose-trade (own chore): status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Propose a chore trade") {
		t.Errorf("picker page missing heading: %s", rec.Body.String())
	}
}
