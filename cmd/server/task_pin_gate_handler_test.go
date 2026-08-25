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

	"github.com/ericfisherdev/nestcore/crypto/cryptotest"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	tasksadapter "github.com/ericfisherdev/nestova/internal/tasks/adapter"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// NES-166: the PIN gate on POST /tasks/{id}/complete and /tasks/{id}/skip
// ---------------------------------------------------------------------------

// newTestPINService builds a PINService over an empty in-memory repo — the
// "nobody has enrolled a PIN" state every pre-NES-166 handler harness is
// implicitly in, where the gate is a no-op and behavior is unchanged.
func newTestPINService(logger *slog.Logger) *authapp.PINService {
	return newTestPINServiceWithRepo(newFakePINRepo(), logger)
}

// newTestPINServiceWithRepo builds a PINService over repo, so a test can
// enrol a member's PIN (via PINService.Set) and then exercise the gate.
func newTestPINServiceWithRepo(repo *fakePINRepo, logger *slog.Logger) *authapp.PINService {
	pinService, err := authapp.NewPINService(repo, cryptotest.Hasher(), time.Now, logger)
	if err != nil {
		panic("newTestPINServiceWithRepo: " + err.Error())
	}
	return pinService
}

// recordingInstanceRepo wraps fakeTaskInstanceRepo to capture the member id
// each completion is credited to — the whole point of the gate is that this
// is the ASSIGNEE, not whoever's session sent the request.
type recordingInstanceRepo struct {
	*fakeTaskInstanceRepo
	completedBy []household.MemberID
}

func (r *recordingInstanceRepo) CompleteAndAward(ctx context.Context, householdID household.HouseholdID, id tasksdomain.TaskInstanceID, by household.MemberID, at time.Time) error {
	r.completedBy = append(r.completedBy, by)
	return r.fakeTaskInstanceRepo.CompleteAndAward(ctx, householdID, id, by, at)
}

// Compile-time assertion.
var _ tasksdomain.TaskInstanceRepository = (*recordingInstanceRepo)(nil)

// twoMemberHouseholdRepo resolves the signed-in member for authentication
// and both household members for row rendering, so a chore assigned to the
// OTHER member renders (and gates) exactly as it does in production.
type twoMemberHouseholdRepo struct {
	testHouseholdRepo
	viewer   *household.Member
	assignee *household.Member
}

func (r twoMemberHouseholdRepo) GetMember(_ context.Context, id household.MemberID) (*household.Member, error) {
	switch id {
	case r.viewer.ID:
		return r.viewer, nil
	case r.assignee.ID:
		return r.assignee, nil
	default:
		return nil, household.ErrMemberNotFound
	}
}

func (r twoMemberHouseholdRepo) EnsureMemberProfile(ctx context.Context, id household.MemberID) (*household.Member, error) {
	return r.GetMember(ctx, id)
}

func (r twoMemberHouseholdRepo) ListMembers(_ context.Context, _ household.HouseholdID) ([]*household.Member, error) {
	return []*household.Member{r.viewer, r.assignee}, nil
}

// Compile-time assertion.
var _ household.HouseholdRepository = twoMemberHouseholdRepo{}

// pinGateFixture bundles a /tasks handler wired with a real PINService over
// an in-memory PIN repo, one pending chore assigned to assignee, and the
// session manager needed to authenticate as viewer.
type pinGateFixture struct {
	handler    http.Handler
	sm         *scs.SessionManager
	instanceID tasksdomain.TaskInstanceID
	instances  *recordingInstanceRepo
	pinService *authapp.PINService
	viewer     *household.Member
	assignee   *household.Member
}

// buildPINGateFixture wires the /tasks routes for a household of two members
// with one pending chore assigned to the second. The viewer is signed in;
// whether the chore is gated depends entirely on whether the test enrols a
// PIN for the assignee.
func buildPINGateFixture(t *testing.T) *pinGateFixture {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()

	viewer := testMember()
	assignee := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: viewer.HouseholdID,
		DisplayName: "Assignee",
		Color:       household.ColorClay,
	}
	householdRepo := twoMemberHouseholdRepo{viewer: viewer, assignee: assignee}

	task := &tasksdomain.RecurringTask{
		ID:          tasksdomain.NewRecurringTaskID(),
		HouseholdID: viewer.HouseholdID,
		Title:       "Take out the trash",
		Category:    tasksdomain.ChoreCategory,
		Active:      true,
	}
	recurringRepo := photoPolicyTaskRepo{task: task}

	due := time.Now()
	inst := &tasksdomain.TaskInstance{
		ID:              tasksdomain.NewTaskInstanceID(),
		RecurringTaskID: task.ID,
		HouseholdID:     viewer.HouseholdID,
		AssigneeID:      &assignee.ID,
		DueOn:           &due,
		Status:          tasksdomain.StatusPending,
		Kind:            tasksdomain.KindScheduled,
	}
	instances := &recordingInstanceRepo{
		fakeTaskInstanceRepo: &fakeTaskInstanceRepo{getInst: inst, listByHousehold: []*tasksdomain.TaskInstance{inst}},
	}

	taskService, err := tasksapp.NewTaskService(recurringRepo, instances, nil)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	pinService := newTestPINServiceWithRepo(newFakePINRepo(), logger)
	taskWebHandlers := tasksadapter.NewWebHandlers(taskService, recurringRepo, instances, householdRepo, pinService, sm, logger, nil)

	authn := authapp.New(testCredRepo{}, cryptotest.Hasher())
	authHandlers := authadapter.NewHandlers(sm, authn, nil, nil, nil, logger)
	onboardingHandlers := authadapter.NewOnboardingHandlers(householdRepo, testCredStore{}, testProvisioner{}, sm, logger)

	mux := http.NewServeMux()
	registerWebRoutes(mux, logger, sm, authHandlers, nil, nil, onboardingHandlers, householdRepo, taskWebHandlers,
		newTestTradeHandlers(taskWebHandlers, instances, householdRepo, sm, logger),
		newTestGamificationHandlers(instances, householdRepo, sm, logger),
		newTestGroceryHandlers(householdRepo, sm, logger),
		newTestMealsHandlers(sm, logger), newTestCalendarHandlers(sm, logger), config.PeerConfig{}, nil)

	return &pinGateFixture{
		handler:    sm.LoadAndSave(authadapter.Authenticate(sm, householdRepo)(mux)),
		sm:         sm,
		instanceID: inst.ID,
		instances:  instances,
		pinService: pinService,
		viewer:     viewer,
		assignee:   assignee,
	}
}

// enrolAssigneePIN gives the fixture's assignee a PIN, turning the gate on
// for their chore.
func (f *pinGateFixture) enrolAssigneePIN(t *testing.T, pin string) {
	t.Helper()
	if err := f.pinService.Set(context.Background(), f.assignee.ID, f.assignee.HouseholdID, pin); err != nil {
		t.Fatalf("enrol assignee PIN: %v", err)
	}
}

// post sends an authenticated, CSRF-valid HTMX POST to action ("complete" or
// "skip") with the supplied PIN (omitted entirely when empty).
func (f *pinGateFixture) post(t *testing.T, action, pin string) *httptest.ResponseRecorder {
	t.Helper()
	cookie, csrfToken := seedAuthedSession(t, f.handler, f.sm, f.viewer.ID.String())

	form := "csrf_token=" + csrfToken
	if pin != "" {
		form += "&pin=" + pin
	}
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+f.instanceID.String()+"/"+action, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("HX-Request", "true")

	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// TestTasksComplete_EnrolledAssignee_CorrectPINCreditsAssignee covers the
// primary AC: the assignee's own PIN completes the chore, and the completion
// is credited to the ASSIGNEE — not to whoever's session sent the request —
// so the point ledger and the instance's completed-by agree.
func TestTasksComplete_EnrolledAssignee_CorrectPINCreditsAssignee(t *testing.T) {
	f := buildPINGateFixture(t)
	f.enrolAssigneePIN(t, "4821")

	rec := f.post(t, "complete", "4821")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if f.instances.completeCalls != 1 {
		t.Fatalf("completeCalls = %d, want 1", f.instances.completeCalls)
	}
	if got := f.instances.completedBy[0]; got != f.assignee.ID {
		t.Errorf("completed by %v, want the assignee %v (the session member was %v)", got, f.assignee.ID, f.viewer.ID)
	}
}

// TestTasksComplete_EnrolledAssignee_WrongPINLeavesInstanceUntouched covers
// "a member cannot complete another member's chore": a wrong PIN (including
// the requester's own) mutates nothing, awards nothing, and re-renders the
// row inline at 422 with a message that never says whether anyone is
// enrolled.
func TestTasksComplete_EnrolledAssignee_WrongPINLeavesInstanceUntouched(t *testing.T) {
	f := buildPINGateFixture(t)
	f.enrolAssigneePIN(t, "4821")

	rec := f.post(t, "complete", "9999")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if f.instances.completeCalls != 0 {
		t.Errorf("completeCalls = %d, want 0 — a refused PIN must not complete the chore", f.instances.completeCalls)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="task-`+f.instanceID.String()+`"`) {
		t.Errorf("response is not an in-place row swap: %s", body)
	}
	if !strings.Contains(body, "That PIN could not be verified.") {
		t.Errorf("response missing the non-disclosing PIN error: %s", body)
	}
	for _, disclosure := range []string{"not enrolled", "no PIN", "enrolled"} {
		if strings.Contains(body, disclosure) {
			t.Errorf("response discloses enrolment state (%q): %s", disclosure, body)
		}
	}
}

// TestTasksComplete_EnrolledAssignee_MissingPINIsRefused proves an omitted
// PIN field is refused exactly like a wrong one — the gate is not something
// a client can opt out of by simply not submitting the input.
func TestTasksComplete_EnrolledAssignee_MissingPINIsRefused(t *testing.T) {
	f := buildPINGateFixture(t)
	f.enrolAssigneePIN(t, "4821")

	rec := f.post(t, "complete", "")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if f.instances.completeCalls != 0 {
		t.Errorf("completeCalls = %d, want 0", f.instances.completeCalls)
	}
}

// TestTasksComplete_NonHTMXRefusalIs403 covers the plain-form fallback: a
// non-HTMX POST cannot swap a row, so a refused PIN is a 403 with the same
// non-disclosing text.
func TestTasksComplete_NonHTMXRefusalIs403(t *testing.T) {
	f := buildPINGateFixture(t)
	f.enrolAssigneePIN(t, "4821")

	cookie, csrfToken := seedAuthedSession(t, f.handler, f.sm, f.viewer.ID.String())
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+f.instanceID.String()+"/complete",
		strings.NewReader("csrf_token="+csrfToken+"&pin=9999"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)

	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	if f.instances.completeCalls != 0 {
		t.Errorf("completeCalls = %d, want 0", f.instances.completeCalls)
	}
}

// TestTasksSkip_EnrolledAssignee_GatedTheSameWay proves skipping is gated
// identically — disposing of somebody else's chore needs the same proof
// completing it does.
func TestTasksSkip_EnrolledAssignee_GatedTheSameWay(t *testing.T) {
	f := buildPINGateFixture(t)
	f.enrolAssigneePIN(t, "4821")

	rec := f.post(t, "skip", "9999")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong PIN: status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if f.instances.skipCalls != 0 {
		t.Fatalf("skipCalls = %d, want 0 after a refused PIN", f.instances.skipCalls)
	}

	if rec := f.post(t, "skip", "4821"); rec.Code != http.StatusOK {
		t.Fatalf("correct PIN: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if f.instances.skipCalls != 1 {
		t.Errorf("skipCalls = %d, want 1 after the assignee's own PIN", f.instances.skipCalls)
	}
}

// TestTasksComplete_UnenrolledAssignee_BehavesExactlyAsBefore is the upgrade
// path AC: with no PIN enrolled the gate is a no-op — no PIN is asked for,
// the completion succeeds, and it is credited to the session member exactly
// as it was before NES-166.
func TestTasksComplete_UnenrolledAssignee_BehavesExactlyAsBefore(t *testing.T) {
	f := buildPINGateFixture(t)

	rec := f.post(t, "complete", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if f.instances.completeCalls != 1 {
		t.Fatalf("completeCalls = %d, want 1", f.instances.completeCalls)
	}
	if got := f.instances.completedBy[0]; got != f.viewer.ID {
		t.Errorf("completed by %v, want the session member %v when no PIN is enrolled", got, f.viewer.ID)
	}
}

// TestTasksList_RendersPINFieldOnlyForEnrolledAssignee proves the row's PIN
// input appears exactly when the gate is on, so a household that has not
// enrolled any PIN sees the pre-NES-166 UI unchanged.
func TestTasksList_RendersPINFieldOnlyForEnrolledAssignee(t *testing.T) {
	f := buildPINGateFixture(t)
	cookie, _ := seedAuthedSession(t, f.handler, f.sm, f.viewer.ID.String())

	get := func() string {
		req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
		req.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /tasks: status = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}

	if body := get(); strings.Contains(body, `data-testid="task-pin-input"`) {
		t.Errorf("PIN field rendered with nobody enrolled: %s", body)
	}

	f.enrolAssigneePIN(t, "4821")

	if body := get(); !strings.Contains(body, `data-testid="task-pin-input"`) {
		t.Errorf("PIN field missing for an enrolled assignee: %s", body)
	}
}
