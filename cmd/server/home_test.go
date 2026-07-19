package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	tasksadapter "github.com/ericfisherdev/nestova/internal/tasks/adapter"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// testCredRepo is a no-op CredentialRepository used in unit tests that have no
// database. All lookups return ErrInvalidCredentials.
type testCredRepo struct{}

func (testCredRepo) FindByEmail(_ context.Context, _ string) (*authdomain.Credential, error) {
	return nil, authdomain.ErrInvalidCredentials
}

func (testCredRepo) FindByMemberID(_ context.Context, _ household.MemberID) (*authdomain.Credential, error) {
	return nil, authdomain.ErrInvalidCredentials
}

func (testCredRepo) SetPassword(_ context.Context, _ household.MemberID, _, _ string) error {
	return nil
}

// Compile-time assertion.
var _ authdomain.CredentialRepository = testCredRepo{}

// testHouseholdRepo is a minimal stub that satisfies household.HouseholdRepository
// for the Authenticate middleware in unit tests where no real DB is available.
type testHouseholdRepo struct{}

func (testHouseholdRepo) CreateHousehold(_ context.Context, _ *household.Household) error {
	return nil
}

func (testHouseholdRepo) GetHousehold(_ context.Context, _ household.HouseholdID) (*household.Household, error) {
	return nil, household.ErrHouseholdNotFound
}

func (testHouseholdRepo) AddMember(_ context.Context, _ *household.Member) error { return nil }

func (testHouseholdRepo) GetMember(_ context.Context, _ household.MemberID) (*household.Member, error) {
	return nil, household.ErrMemberNotFound
}

func (testHouseholdRepo) ListMembers(_ context.Context, _ household.HouseholdID) ([]*household.Member, error) {
	return nil, nil
}

func (testHouseholdRepo) HasAnyHousehold(_ context.Context) (bool, error) {
	return false, nil
}

// Compile-time assertion.
var _ household.HouseholdRepository = testHouseholdRepo{}

// testCredStore is a no-op credentialStore (EmailExists always false) for unit
// tests that have no database.
type testCredStore struct{}

func (testCredStore) EmailExists(_ context.Context, _ string) (bool, error) { return false, nil }

// testProvisioner is a no-op Provisioner for unit tests that have no database.
type testProvisioner struct{}

func (testProvisioner) ProvisionHousehold(_ context.Context, _ *household.Household, _ *household.Member, _, _ string) error {
	return nil
}

func (testProvisioner) ProvisionMember(_ context.Context, _ *household.Member, _, _ string) error {
	return nil
}

// Compile-time assertion.
var _ authadapter.Provisioner = testProvisioner{}

// ---------------------------------------------------------------------------
// Gamification fakes — no-op implementations of the gamification ports used
// to construct a GamificationWebHandlers in tests that do not exercise those
// routes.
// ---------------------------------------------------------------------------

// fakePointLedgerRepo is a no-op PointLedgerRepository for unit tests.
type fakePointLedgerRepo struct{}

func (fakePointLedgerRepo) Append(_ context.Context, _ *tasksdomain.PointEntry) error {
	return nil
}

func (fakePointLedgerRepo) Balance(_ context.Context, _ household.HouseholdID, _ household.MemberID) (int, error) {
	return 0, nil
}

func (fakePointLedgerRepo) Leaderboard(_ context.Context, _ household.HouseholdID, _ time.Time) ([]tasksdomain.MemberPoints, error) {
	return nil, nil
}

func (fakePointLedgerRepo) History(_ context.Context, _ household.HouseholdID, _ household.MemberID, _ int) ([]tasksdomain.PointHistoryEntry, error) {
	return nil, nil
}

// Compile-time assertion.
var _ tasksdomain.PointLedgerRepository = fakePointLedgerRepo{}

// fakeRewardRepo is a no-op RewardRepository for unit tests.
type fakeRewardRepo struct{}

func (fakeRewardRepo) CreateReward(_ context.Context, _ *tasksdomain.Reward) error { return nil }

func (fakeRewardRepo) GetReward(_ context.Context, _ household.HouseholdID, _ tasksdomain.RewardID) (*tasksdomain.Reward, error) {
	return nil, tasksdomain.ErrRewardNotFound
}

func (fakeRewardRepo) ListActiveRewards(_ context.Context, _ household.HouseholdID) ([]*tasksdomain.Reward, error) {
	return nil, nil
}

func (fakeRewardRepo) ListStorefrontRewards(_ context.Context, _ household.HouseholdID) ([]tasksdomain.StorefrontReward, error) {
	return nil, nil
}

func (fakeRewardRepo) ListAllRewards(_ context.Context, _ household.HouseholdID) ([]*tasksdomain.Reward, error) {
	return nil, nil
}

func (fakeRewardRepo) UpdateReward(_ context.Context, _ *tasksdomain.Reward) error { return nil }

func (fakeRewardRepo) ArchiveReward(_ context.Context, _ household.HouseholdID, _ tasksdomain.RewardID) error {
	return nil
}

func (fakeRewardRepo) DeleteReward(_ context.Context, _ household.HouseholdID, _ tasksdomain.RewardID) error {
	return nil
}

func (fakeRewardRepo) Redeem(_ context.Context, _ *tasksdomain.RewardRedemption) error {
	return nil
}

// RedeemWithDebit satisfies the tasksapp.RewardRedeemer interface so this fake
// can be passed to NewRewardService.
func (fakeRewardRepo) RedeemWithDebit(_ context.Context, _ *tasksdomain.RewardRedemption) (int, error) {
	return 0, nil
}

func (fakeRewardRepo) ListPendingRedemptions(_ context.Context, _ household.HouseholdID) ([]tasksdomain.RedemptionDetail, error) {
	return nil, nil
}

func (fakeRewardRepo) ListMemberRedemptions(
	_ context.Context, _ household.HouseholdID, _ household.MemberID, _ int,
) ([]tasksdomain.RedemptionDetail, error) {
	return nil, nil
}

func (fakeRewardRepo) Fulfill(
	_ context.Context, _ household.HouseholdID, _ tasksdomain.RewardRedemptionID,
) (tasksdomain.ResolvedRedemption, error) {
	return tasksdomain.ResolvedRedemption{}, tasksdomain.ErrRedemptionNotFound
}

func (fakeRewardRepo) Deny(
	_ context.Context, _ household.HouseholdID, _ tasksdomain.RewardRedemptionID, _ string,
) (tasksdomain.ResolvedRedemption, error) {
	return tasksdomain.ResolvedRedemption{}, tasksdomain.ErrRedemptionNotFound
}

func (fakeRewardRepo) Cancel(
	_ context.Context, _ household.HouseholdID, _ tasksdomain.RewardRedemptionID, _ household.MemberID,
) (tasksdomain.ResolvedRedemption, error) {
	return tasksdomain.ResolvedRedemption{}, tasksdomain.ErrRedemptionNotPending
}

// Compile-time assertion.
var _ tasksdomain.RewardRepository = fakeRewardRepo{}

// newTestGamificationHandlers builds a GamificationWebHandlers wired with all
// no-op fakes. Used by tests that need registerWebRoutes to compile but do not
// exercise /rewards routes.
func newTestGamificationHandlers(
	instanceRepo tasksdomain.TaskInstanceRepository,
	householdRepo household.HouseholdRepository,
	sm *scs.SessionManager,
	logger *slog.Logger,
) *tasksadapter.GamificationWebHandlers {
	rewardRepo := fakeRewardRepo{}
	rewardSvc := tasksapp.NewRewardService(rewardRepo, householdRepo, fakeEnqueuer{}, logger)
	rewardAdminSvc := tasksapp.NewRewardAdminService(rewardRepo, logger)
	redemptionSvc, err := tasksapp.NewRedemptionService(rewardRepo, fakeEnqueuer{}, logger)
	if err != nil {
		panic("newTestGamificationHandlers: " + err.Error())
	}
	return tasksadapter.NewGamificationWebHandlers(
		fakePointLedgerRepo{},
		rewardRepo,
		rewardSvc,
		rewardAdminSvc,
		redemptionSvc,
		instanceRepo,
		householdRepo,
		sm,
		logger,
	)
}

func newTestSessionManager() *scs.SessionManager {
	sm := scs.New()
	sm.Store = memstore.New()
	sm.Lifetime = 1 * time.Hour
	sm.Cookie.Secure = false // httptest serves over plain HTTP, not HTTPS
	return sm
}

// buildTestHandler returns an http.Handler with the session and auth middleware
// applied on top of the route mux, using in-memory stubs (no DB required).
func buildTestHandler() http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	repo := testHouseholdRepo{}
	authn := authapp.New(testCredRepo{})
	authHandlers := authadapter.NewHandlers(sm, authn, nil, nil, logger)
	onboardingHandlers := authadapter.NewOnboardingHandlers(repo, testCredStore{}, testProvisioner{}, sm, logger)

	// Stub task dependencies so the composition root builds without a database.
	taskRecurringRepo := fakeRecurringTaskRepo{}
	taskInstanceRepo := &fakeTaskInstanceRepo{}
	taskService, err := tasksapp.NewTaskService(taskRecurringRepo, taskInstanceRepo, nil)
	if err != nil {
		panic("buildTestHandler: " + err.Error())
	}
	taskWebHandlers := tasksadapter.NewWebHandlers(taskService, taskRecurringRepo, taskInstanceRepo, repo, sm, logger, nil)
	gamificationHandlers := newTestGamificationHandlers(taskInstanceRepo, repo, sm, logger)
	groceryHandlers := newTestGroceryHandlers(repo, sm, logger)

	mux := http.NewServeMux()
	registerWebRoutes(mux, logger, sm, authHandlers, nil, onboardingHandlers, repo, taskWebHandlers, newTestTradeHandlers(taskWebHandlers, taskInstanceRepo, repo, sm, logger), gamificationHandlers, groceryHandlers, newTestMealsHandlers(sm, logger), newTestCalendarHandlers(sm, logger))

	// Apply the session middleware so CSRF tokens and member lookups work.
	return sm.LoadAndSave(
		authadapter.Authenticate(sm, repo)(mux),
	)
}

func TestDashboardRequiresAuth(t *testing.T) {
	handler := buildTestHandler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	// Unauthenticated GET / must redirect to /login, not serve the dashboard.
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (redirect to /login)", rec.Code, http.StatusSeeOther)
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/login") {
		t.Errorf("Location = %q, want /login...", location)
	}
	// The original path must be preserved so the user returns to it after login.
	if !strings.Contains(location, "next=") {
		t.Errorf("Location = %q, want a next= return path", location)
	}
}

func TestDashboardHTMXUnauthorized(t *testing.T) {
	handler := buildTestHandler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("HX-Request", "true")
	handler.ServeHTTP(rec, req)

	// An unauthenticated HTMX request must get 401 (not a redirect HTMX cannot
	// follow into a navigation).
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d for unauthenticated HX request", rec.Code, http.StatusUnauthorized)
	}
}

func TestLoginPageRendersForm(t *testing.T) {
	handler := buildTestHandler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Sign in",
		`name="email"`,
		`name="password"`,
		`name="csrf_token"`,
		"Nestova",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login page missing %q", want)
		}
	}
	// The CSRF field must carry a real token, not be empty.
	m := regexp.MustCompile(`name="csrf_token"[^>]*value="([^"]+)"`).FindStringSubmatch(body)
	if m == nil || m[1] == "" {
		t.Error("login page csrf_token field has no non-empty value")
	}
}

// TestPrimaryNavActive verifies only the matching nav item is marked active.
func TestPrimaryNavActive(t *testing.T) {
	// The Chores nav item now links to /tasks (NES-32).
	nav := primaryNav("/tasks")
	var activeCount int
	for _, item := range nav {
		if item.Active {
			activeCount++
			if item.Href != "/tasks" {
				t.Errorf("active item = %q, want /tasks", item.Href)
			}
		}
	}
	if activeCount != 1 {
		t.Errorf("active nav items = %d, want 1", activeCount)
	}
	for _, item := range primaryNav("") {
		if item.Active {
			t.Errorf("no item should be active for empty selection, got %q", item.Href)
		}
	}
}
