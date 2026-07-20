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

	"github.com/ericfisherdev/nestova/internal/platform/crypto/cryptotest"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	tasksadapter "github.com/ericfisherdev/nestova/internal/tasks/adapter"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// NES-120: photo policy completion gate — HTTP handler test (AC2)
// ---------------------------------------------------------------------------

// photoPolicyTaskRepo wraps fakeRecurringTaskRepo, overriding Get to return a
// single configured task regardless of id — enough for a focused test where
// exactly one recurring task is in play.
type photoPolicyTaskRepo struct {
	fakeRecurringTaskRepo
	task *tasksdomain.RecurringTask
}

func (r photoPolicyTaskRepo) Get(_ context.Context, _ household.HouseholdID, _ tasksdomain.RecurringTaskID) (*tasksdomain.RecurringTask, error) {
	return r.task, nil
}

// ListActive overrides fakeRecurringTaskRepo's own (always-empty) stub so
// buildTaskRows' taskMeta map is actually populated — needed for the
// GET /tasks list-rendering tests (NES-120's batch lookup), which the
// Complete-flow tests above never exercise (buildInstanceRow calls Get, not
// ListActive).
func (r photoPolicyTaskRepo) ListActive(_ context.Context, _ household.HouseholdID) ([]*tasksdomain.RecurringTask, error) {
	if r.task == nil {
		return nil, nil
	}
	return []*tasksdomain.RecurringTask{r.task}, nil
}

// Compile-time assertion.
var _ tasksdomain.RecurringTaskRepository = photoPolicyTaskRepo{}

// fakePhotoChecker is a configurable tasksdomain.ProofPhotoChecker for unit
// tests. beforeID/afterID are returned verbatim by ProofPhotos for every
// instance — a focused single-instance test never needs more. batchIDs,
// when set, is what ProofPhotosByInstances (NES-120's batch counterpart)
// returns, keyed by instance id. proofPhotosCalls/batchCalls count each
// method's invocations so a test can assert the list builder uses ONLY the
// batch method when rendering a page (never one ProofPhotos call per row).
type fakePhotoChecker struct {
	beforeID, afterID string
	batchIDs          map[tasksdomain.TaskInstanceID]tasksdomain.ProofPhotoIDs
	proofPhotosCalls  int
	batchCalls        int
}

func (c *fakePhotoChecker) ProofPhotos(_ context.Context, _ household.HouseholdID, _ tasksdomain.TaskInstanceID) (string, string, error) {
	c.proofPhotosCalls++
	return c.beforeID, c.afterID, nil
}

func (c *fakePhotoChecker) ProofPhotosByInstances(_ context.Context, _ household.HouseholdID, _ []tasksdomain.TaskInstanceID) (map[tasksdomain.TaskInstanceID]tasksdomain.ProofPhotoIDs, error) {
	c.batchCalls++
	return c.batchIDs, nil
}

// Compile-time assertion.
var _ tasksdomain.ProofPhotoChecker = (*fakePhotoChecker)(nil)

// buildPhotoPolicyTestHandler wires a minimal /tasks handler with a single
// pending instance whose parent recurring task carries policy, and whose
// captured photos are reported by photoChecker. member is pre-registered so
// seedAuthedSession can authenticate as them.
func buildPhotoPolicyTestHandler(
	t *testing.T,
	member *household.Member,
	policy tasksdomain.PhotoPolicy,
	photoChecker tasksdomain.ProofPhotoChecker,
) (handler http.Handler, sm *scs.SessionManager, instanceID tasksdomain.TaskInstanceID, instanceRepo *fakeTaskInstanceRepo) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm = newTestSessionManager()
	householdRepo := authedHouseholdRepo{member: member}
	authn := authapp.New(testCredRepo{}, cryptotest.Hasher())
	authHandlers := authadapter.NewHandlers(sm, authn, nil, nil, nil, logger)
	onboardingHandlers := authadapter.NewOnboardingHandlers(householdRepo, testCredStore{}, testProvisioner{}, sm, logger)

	task := &tasksdomain.RecurringTask{
		ID:          tasksdomain.NewRecurringTaskID(),
		HouseholdID: member.HouseholdID,
		Title:       "Clean garage",
		Category:    tasksdomain.ChoreCategory,
		Active:      true,
		PhotoPolicy: policy,
	}
	recurringRepo := photoPolicyTaskRepo{task: task}

	due := time.Now()
	inst := &tasksdomain.TaskInstance{
		ID:              tasksdomain.NewTaskInstanceID(),
		RecurringTaskID: task.ID,
		HouseholdID:     member.HouseholdID,
		AssigneeID:      &member.ID,
		DueOn:           &due,
		Status:          tasksdomain.StatusPending,
		Kind:            tasksdomain.KindScheduled,
	}
	instanceRepo = &fakeTaskInstanceRepo{getInst: inst}

	taskService, err := tasksapp.NewTaskService(recurringRepo, instanceRepo, photoChecker)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}
	taskWebHandlers := tasksadapter.NewWebHandlers(taskService, recurringRepo, instanceRepo, householdRepo, sm, logger, photoChecker)
	gamificationHandlers := newTestGamificationHandlers(instanceRepo, householdRepo, sm, logger)
	groceryHandlers := newTestGroceryHandlers(householdRepo, sm, logger)

	mux := http.NewServeMux()
	registerWebRoutes(mux, logger, sm, authHandlers, nil, nil, onboardingHandlers, householdRepo, taskWebHandlers,
		newTestTradeHandlers(taskWebHandlers, instanceRepo, householdRepo, sm, logger),
		gamificationHandlers, groceryHandlers, newTestMealsHandlers(sm, logger), newTestCalendarHandlers(sm, logger))

	handler = sm.LoadAndSave(authadapter.Authenticate(sm, householdRepo)(mux))
	return handler, sm, inst.ID, instanceRepo
}

// TestTasksComplete_PhotoPolicyBlocked_RendersInlineErrorViaHTMX verifies
// AC2: an HTMX completion request blocked by an unmet photo policy re-renders
// the SAME chore row in place (status 422, the row's stable anchor id and a
// friendly message), with no HX-Redirect / full page reload signaled.
func TestTasksComplete_PhotoPolicyBlocked_RendersInlineErrorViaHTMX(t *testing.T) {
	member := testMember()
	// No photos captured for a before_after policy — before is checked first.
	handler, sm, instanceID, instanceRepo := buildPhotoPolicyTestHandler(t, member, tasksdomain.PhotoPolicyBeforeAfter, &fakePhotoChecker{})
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+instanceID.String()+"/complete",
		strings.NewReader("csrf_token="+csrfToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("HX-Request", "true")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="task-`+instanceID.String()+`"`) {
		t.Errorf("response missing the row's stable anchor id (not an in-place row swap): %s", body)
	}
	if !strings.Contains(body, "Take a before photo before marking this chore complete.") {
		t.Errorf("response missing the friendly inline error message: %s", body)
	}
	if !strings.Contains(body, `role="alert"`) {
		t.Errorf("response missing role=alert on the inline error: %s", body)
	}
	// No page reload: no HX-Redirect header, and the mutation never actually
	// reached the repository (the gate rejected it before CompleteAndAward).
	if rec.Header().Get("HX-Redirect") != "" {
		t.Errorf("HX-Redirect = %q, want empty (no page reload for an inline-swappable error)", rec.Header().Get("HX-Redirect"))
	}
	if instanceRepo.completeCalls != 0 {
		t.Errorf("completeCalls = %d, want 0 (photo gate must reject before CompleteAndAward runs)", instanceRepo.completeCalls)
	}
}

// TestTasksComplete_PhotoPolicyBlocked_AfterOnlySatisfiedSucceeds verifies
// that once the required photo exists, the SAME HTMX request path succeeds
// normally (200, no inline error) — confirming the 422 case above is
// genuinely policy-driven, not a permanent block.
func TestTasksComplete_PhotoPolicyBlocked_AfterOnlySatisfiedSucceeds(t *testing.T) {
	member := testMember()
	handler, sm, instanceID, instanceRepo := buildPhotoPolicyTestHandler(t, member, tasksdomain.PhotoPolicyAfterOnly,
		&fakePhotoChecker{afterID: "after-photo-id"})
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+instanceID.String()+"/complete",
		strings.NewReader("csrf_token="+csrfToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("HX-Request", "true")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	// The photo gate let the request through to the repository (unlike the
	// blocked case above, where completeCalls stays 0) — fakeTaskInstanceRepo
	// is a thin stub that records the call rather than mutating instance
	// state, so this is the strongest success signal it can offer.
	if instanceRepo.completeCalls != 1 {
		t.Errorf("completeCalls = %d, want 1 (satisfied policy must reach CompleteAndAward)", instanceRepo.completeCalls)
	}
	if strings.Contains(rec.Body.String(), "role=\"alert\"") {
		t.Errorf("response should carry no inline photo error once the policy is satisfied: %s", rec.Body.String())
	}
}

// TestTasksComplete_PhotoPolicyBlocked_NonHTMXFallsBackToPlainError verifies
// that a non-HTMX (plain form) request blocked by the photo gate still gets
// a clear error response rather than a silently-swapped row fragment — the
// inline-row behavior above is HTMX-specific.
func TestTasksComplete_PhotoPolicyBlocked_NonHTMXFallsBackToPlainError(t *testing.T) {
	member := testMember()
	handler, sm, instanceID, _ := buildPhotoPolicyTestHandler(t, member, tasksdomain.PhotoPolicyBeforeAfter, &fakePhotoChecker{})
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/tasks/"+instanceID.String()+"/complete",
		strings.NewReader("csrf_token="+csrfToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	// No HX-Request header.

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `id="task-`) {
		t.Errorf("non-HTMX error response should be plain text, not an HTML row fragment: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// N+1 avoidance — CodeRabbit finding (batched photo lookup across rows)
// ---------------------------------------------------------------------------

// TestTasksList_PhotoPolicy_BatchesProofPhotoLookupAcrossRows verifies that
// rendering a page with MULTIPLE photo-policy-requiring rows costs exactly
// ONE ProofPhotoChecker call (the batch method), never one call per row —
// the N+1 a naive per-row lookup would otherwise introduce on an unbounded
// overdue list.
func TestTasksList_PhotoPolicy_BatchesProofPhotoLookupAcrossRows(t *testing.T) {
	member := testMember()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	householdRepo := authedHouseholdRepo{member: member}
	authn := authapp.New(testCredRepo{}, cryptotest.Hasher())
	authHandlers := authadapter.NewHandlers(sm, authn, nil, nil, nil, logger)
	onboardingHandlers := authadapter.NewOnboardingHandlers(householdRepo, testCredStore{}, testProvisioner{}, sm, logger)

	task := &tasksdomain.RecurringTask{
		ID:          tasksdomain.NewRecurringTaskID(),
		HouseholdID: member.HouseholdID,
		Title:       "Clean garage",
		Category:    tasksdomain.ChoreCategory,
		Active:      true,
		PhotoPolicy: tasksdomain.PhotoPolicyBeforeAfter,
	}
	recurringRepo := photoPolicyTaskRepo{task: task}

	due := time.Now()
	var instances []*tasksdomain.TaskInstance
	for range 3 {
		instances = append(instances, &tasksdomain.TaskInstance{
			ID:              tasksdomain.NewTaskInstanceID(),
			RecurringTaskID: task.ID,
			HouseholdID:     member.HouseholdID,
			AssigneeID:      &member.ID,
			DueOn:           &due,
			Status:          tasksdomain.StatusPending,
			Kind:            tasksdomain.KindScheduled,
		})
	}
	instanceRepo := &fakeTaskInstanceRepo{listByHousehold: instances}
	photoChecker := &fakePhotoChecker{}

	taskService, err := tasksapp.NewTaskService(recurringRepo, instanceRepo, photoChecker)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}
	taskWebHandlers := tasksadapter.NewWebHandlers(taskService, recurringRepo, instanceRepo, householdRepo, sm, logger, photoChecker)
	gamificationHandlers := newTestGamificationHandlers(instanceRepo, householdRepo, sm, logger)
	groceryHandlers := newTestGroceryHandlers(householdRepo, sm, logger)

	mux := http.NewServeMux()
	registerWebRoutes(mux, logger, sm, authHandlers, nil, nil, onboardingHandlers, householdRepo, taskWebHandlers,
		newTestTradeHandlers(taskWebHandlers, instanceRepo, householdRepo, sm, logger),
		gamificationHandlers, groceryHandlers, newTestMealsHandlers(sm, logger), newTestCalendarHandlers(sm, logger))
	handler := sm.LoadAndSave(authadapter.Authenticate(sm, householdRepo)(mux))

	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tasks status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if photoChecker.batchCalls != 1 {
		t.Errorf("batchCalls = %d, want 1 (one batched lookup for the whole page build)", photoChecker.batchCalls)
	}
	if photoChecker.proofPhotosCalls != 0 {
		t.Errorf("proofPhotosCalls = %d, want 0 (list rendering must never use the single-instance method)", photoChecker.proofPhotosCalls)
	}
	for _, inst := range instances {
		if !strings.Contains(rec.Body.String(), inst.ID.String()) {
			t.Errorf("response missing row for instance %s", inst.ID)
		}
	}
}
