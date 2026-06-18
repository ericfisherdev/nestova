package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	tasksadapter "github.com/ericfisherdev/nestova/internal/tasks/adapter"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// Extended recurring-task repo fake that records CreateWithRotation calls
// ---------------------------------------------------------------------------

// recordingTaskRepo wraps fakeRecurringTaskRepo and records CreateWithRotation
// invocations so tests can assert that the service was (or was not) called.
type recordingTaskRepo struct {
	fakeRecurringTaskRepo
	createCalls []*tasksdomain.RecurringTask
	createErr   error
}

func (r *recordingTaskRepo) CreateWithRotation(
	_ context.Context,
	task *tasksdomain.RecurringTask,
	_ []household.MemberID,
) error {
	r.createCalls = append(r.createCalls, task)
	return r.createErr
}

// Compile-time assertion.
var _ tasksdomain.RecurringTaskRepository = (*recordingTaskRepo)(nil)

// ---------------------------------------------------------------------------
// authedHouseholdRepo extends testHouseholdRepo to return a known member for
// the GetMember call so the session member can be resolved.
// ---------------------------------------------------------------------------

// authedHouseholdRepo is a stub that returns a fixed household member for any
// GetMember call. It is used by tests that need an authenticated session.
type authedHouseholdRepo struct {
	testHouseholdRepo
	member *household.Member
}

func (r authedHouseholdRepo) GetMember(_ context.Context, _ household.MemberID) (*household.Member, error) {
	if r.member != nil {
		return r.member, nil
	}
	return nil, household.ErrMemberNotFound
}

func (r authedHouseholdRepo) ListMembers(_ context.Context, _ household.HouseholdID) ([]*household.Member, error) {
	if r.member != nil {
		return []*household.Member{r.member}, nil
	}
	return nil, nil
}

// Compile-time assertion.
var _ household.HouseholdRepository = authedHouseholdRepo{}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// buildCreateTaskHandler returns an http.Handler and a pointer to the
// recording task repo so each test can inspect calls. It injects a known
// household member so that auth-gated routes resolve the current member.
func buildCreateTaskHandler(
	taskRepo *recordingTaskRepo,
	householdRepo household.HouseholdRepository,
) (http.Handler, *scs.SessionManager) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	authn := authapp.New(testCredRepo{})
	authHandlers := authadapter.NewHandlers(sm, authn, logger)
	onboardingHandlers := authadapter.NewOnboardingHandlers(
		householdRepo, testCredStore{}, testProvisioner{}, sm, logger,
	)

	instanceRepo := &fakeTaskInstanceRepo{}
	taskService, err := tasksapp.NewTaskService(taskRepo, instanceRepo)
	if err != nil {
		panic("buildCreateTaskHandler: " + err.Error())
	}
	taskWebHandlers := tasksadapter.NewWebHandlers(
		taskService, taskRepo, instanceRepo, householdRepo, sm, logger,
	)

	mux := http.NewServeMux()
	registerWebRoutes(mux, logger, sm, authHandlers, onboardingHandlers, householdRepo, taskWebHandlers)

	return sm.LoadAndSave(
		authadapter.Authenticate(sm, householdRepo)(mux),
	), sm
}

// seedAuthedSession adds a member_id and csrf_token into a fresh session and
// returns the session cookie string so subsequent requests carry it.
func seedAuthedSession(t *testing.T, handler http.Handler, sm *scs.SessionManager, memberID string) (cookie, csrfToken string) {
	t.Helper()

	// Perform a GET /login to obtain a session cookie with a CSRF token.
	loginReq := httptest.NewRequest(http.MethodGet, "/login", nil)
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, loginReq)

	// Extract session cookie.
	var sessionCookie string
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == "session" {
			sessionCookie = c.Name + "=" + c.Value
		}
	}

	// Extract CSRF token from login page body.
	body := loginRec.Body.String()
	tokenStart := strings.Index(body, `name="csrf_token"`)
	var token string
	if tokenStart >= 0 {
		valStart := strings.Index(body[tokenStart:], `value="`)
		if valStart >= 0 {
			s := body[tokenStart+valStart+len(`value="`):]
			end := strings.Index(s, `"`)
			if end >= 0 {
				token = s[:end]
			}
		}
	}

	if sessionCookie == "" {
		t.Fatal("seedAuthedSession: no session cookie in GET /login response")
	}

	// Stamp member_id and csrf_token into the live session store by sending a
	// request through a one-shot LoadAndSave handler using the existing session
	// cookie. This avoids needing real credentials — the credential repo always
	// returns ErrInvalidCredentials in unit tests, so we bypass login entirely.
	injectHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sm.Put(r.Context(), "member_id", memberID)
		csrfInSession := sm.GetString(r.Context(), "csrf_token")
		if csrfInSession == "" && token != "" {
			sm.Put(r.Context(), "csrf_token", token)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	injectReq := httptest.NewRequest(http.MethodGet, "/", nil)
	if sessionCookie != "" {
		injectReq.Header.Set("Cookie", sessionCookie)
	}
	injectRec := httptest.NewRecorder()
	injectHandler.ServeHTTP(injectRec, injectReq)

	// Pick up the updated cookie from the inject response, if any.
	for _, c := range injectRec.Result().Cookies() {
		if c.Name == "session" {
			sessionCookie = c.Name + "=" + c.Value
		}
	}

	return sessionCookie, token
}

// ---------------------------------------------------------------------------
// Tests: GET /tasks/new
// ---------------------------------------------------------------------------

func TestGetNewTaskPageRequiresAuth(t *testing.T) {
	repo := &recordingTaskRepo{}
	householdRepo := authedHouseholdRepo{member: nil}
	handler, _ := buildCreateTaskHandler(repo, householdRepo)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tasks/new", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated GET /tasks/new: status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login") {
		t.Errorf("Location = %q, want /login...", loc)
	}
}

func TestGetNewTaskPageHTMXRequiresAuth(t *testing.T) {
	repo := &recordingTaskRepo{}
	householdRepo := authedHouseholdRepo{member: nil}
	handler, _ := buildCreateTaskHandler(repo, householdRepo)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tasks/new", nil)
	req.Header.Set("HX-Request", "true")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated HX GET /tasks/new: status = %d, want 401", rec.Code)
	}
}

func TestGetNewTaskPageRendersForm(t *testing.T) {
	fixedMember := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice",
		Color:       household.ColorSage,
	}
	repo := &recordingTaskRepo{}
	householdRepo := authedHouseholdRepo{member: fixedMember}
	handler, sm := buildCreateTaskHandler(repo, householdRepo)

	cookie, _ := seedAuthedSession(t, handler, sm, fixedMember.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/tasks/new", nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tasks/new: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="title"`,
		`name="category"`,
		`name="freq"`,
		`name="interval"`,
		`name="anchor"`,
		`name="rotation_policy"`,
		`name="points"`,
		`name="lead_time_days"`,
		`name="csrf_token"`,
		"Save chore",
		`href="/tasks"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /tasks/new response missing %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: POST /tasks
// ---------------------------------------------------------------------------

func TestPostTasksRejectsInvalidCSRF(t *testing.T) {
	fixedMember := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice",
		Color:       household.ColorSage,
	}
	repo := &recordingTaskRepo{}
	householdRepo := authedHouseholdRepo{member: fixedMember}
	handler, sm := buildCreateTaskHandler(repo, householdRepo)

	cookie, _ := seedAuthedSession(t, handler, sm, fixedMember.ID.String())

	form := url.Values{
		"csrf_token":      {"wrong-token"},
		"title":           {"Test chore"},
		"category":        {"chore"},
		"freq":            {"daily"},
		"interval":        {"1"},
		"rotation_policy": {"claimable"},
		"points":          {"0"},
		"lead_time_days":  {"0"},
	}
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /tasks with bad CSRF: status = %d, want 403", rec.Code)
	}
	if len(repo.createCalls) != 0 {
		t.Errorf("CreateWithRotation called despite bad CSRF; calls = %d", len(repo.createCalls))
	}
}

func TestPostTasksRequiresAuth(t *testing.T) {
	repo := &recordingTaskRepo{}
	householdRepo := authedHouseholdRepo{member: nil}
	handler, _ := buildCreateTaskHandler(repo, householdRepo)

	form := url.Values{"title": {"Chore"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("unauthenticated POST /tasks should not return 200")
	}
	if len(repo.createCalls) != 0 {
		t.Errorf("CreateWithRotation should not be called for unauthenticated request")
	}
}

func TestPostTasksValidFieldsCallsCreateAndRedirects(t *testing.T) {
	fixedMember := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice",
		Color:       household.ColorSage,
	}
	repo := &recordingTaskRepo{}
	householdRepo := authedHouseholdRepo{member: fixedMember}
	handler, sm := buildCreateTaskHandler(repo, householdRepo)

	cookie, csrfToken := seedAuthedSession(t, handler, sm, fixedMember.ID.String())

	form := url.Values{
		"csrf_token":      {csrfToken},
		"title":           {"Vacuum living room"},
		"category":        {"chore"},
		"freq":            {"weekly"},
		"interval":        {"1"},
		"rotation_policy": {"claimable"},
		"points":          {"3"},
		"lead_time_days":  {"0"},
		"anchor":          {"2026-07-01"},
	}
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("valid POST /tasks: status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc != "/tasks" {
		t.Errorf("redirect Location = %q, want /tasks", loc)
	}
	if len(repo.createCalls) != 1 {
		t.Errorf("CreateWithRotation calls = %d, want 1", len(repo.createCalls))
	}
	if len(repo.createCalls) > 0 {
		created := repo.createCalls[0]
		if created.Title != "Vacuum living room" {
			t.Errorf("created task title = %q, want %q", created.Title, "Vacuum living room")
		}
		if string(created.Category) != "chore" {
			t.Errorf("created task category = %q, want chore", created.Category)
		}
		if created.Points != 3 {
			t.Errorf("created task points = %d, want 3", created.Points)
		}
	}
}

func TestPostTasksInvalidIntervalRerendersForm422(t *testing.T) {
	fixedMember := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice",
		Color:       household.ColorSage,
	}
	repo := &recordingTaskRepo{}
	householdRepo := authedHouseholdRepo{member: fixedMember}
	handler, sm := buildCreateTaskHandler(repo, householdRepo)

	cookie, csrfToken := seedAuthedSession(t, handler, sm, fixedMember.ID.String())

	form := url.Values{
		"csrf_token":      {csrfToken},
		"title":           {"Sweep"},
		"category":        {"chore"},
		"freq":            {"daily"},
		"interval":        {"0"}, // invalid: must be >= 1
		"rotation_policy": {"claimable"},
		"points":          {"0"},
		"lead_time_days":  {"0"},
	}
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /tasks with interval=0: status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	// Service must NOT have been called.
	if len(repo.createCalls) != 0 {
		t.Errorf("CreateWithRotation must not be called when interval is 0; calls = %d", len(repo.createCalls))
	}
	// Form is re-rendered with an error message and the sticky title.
	body := rec.Body.String()
	if !strings.Contains(body, "Interval") {
		t.Errorf("422 re-render missing interval error message: %q", body)
	}
	if !strings.Contains(body, "Sweep") {
		t.Errorf("422 re-render missing sticky title value: %q", body)
	}
}

func TestPostTasksMissingTitleRerendersForm422(t *testing.T) {
	fixedMember := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice",
		Color:       household.ColorSage,
	}
	repo := &recordingTaskRepo{}
	householdRepo := authedHouseholdRepo{member: fixedMember}
	handler, sm := buildCreateTaskHandler(repo, householdRepo)

	cookie, csrfToken := seedAuthedSession(t, handler, sm, fixedMember.ID.String())

	form := url.Values{
		"csrf_token":      {csrfToken},
		"title":           {""}, // missing
		"category":        {"chore"},
		"freq":            {"daily"},
		"interval":        {"1"},
		"rotation_policy": {"claimable"},
		"points":          {"0"},
		"lead_time_days":  {"0"},
	}
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /tasks with empty title: status = %d, want 422", rec.Code)
	}
	if len(repo.createCalls) != 0 {
		t.Errorf("CreateWithRotation must not be called with missing title; calls = %d", len(repo.createCalls))
	}
	if !strings.Contains(rec.Body.String(), "required") {
		t.Errorf("422 re-render missing 'required' error text: %q", rec.Body.String())
	}
}
