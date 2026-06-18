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
)

// testCredRepo is a no-op CredentialRepository used in unit tests that have no
// database. All lookups return ErrInvalidCredentials.
type testCredRepo struct{}

func (testCredRepo) FindByEmail(_ context.Context, _ string) (*authdomain.Credential, error) {
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

// Compile-time assertion.
var _ household.HouseholdRepository = testHouseholdRepo{}

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
	authn := authapp.New(testCredRepo{})
	authHandlers := authadapter.NewHandlers(sm, authn, logger)

	mux := http.NewServeMux()
	registerWebRoutes(mux, logger, sm, authHandlers)

	// Apply the session middleware so CSRF tokens and member lookups work.
	return sm.LoadAndSave(
		authadapter.Authenticate(sm, testHouseholdRepo{})(mux),
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
	nav := primaryNav("/chores")
	var activeCount int
	for _, item := range nav {
		if item.Active {
			activeCount++
			if item.Href != "/chores" {
				t.Errorf("active item = %q, want /chores", item.Href)
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
