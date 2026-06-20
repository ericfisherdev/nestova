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
	"golang.org/x/oauth2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	calendaradapter "github.com/ericfisherdev/nestova/internal/calendar/adapter"
	calendarapp "github.com/ericfisherdev/nestova/internal/calendar/app"
	calendardomain "github.com/ericfisherdev/nestova/internal/calendar/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	tasksadapter "github.com/ericfisherdev/nestova/internal/tasks/adapter"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
)

// testStateKey is the fixed HMAC key the calendar test harness signs OAuth state
// with, so tests can mint a valid state the handler will accept.
const testStateKey = "calendar-test-oauth-state-key-0001"

// fakeCalAccountRepo is an in-memory domain.CalendarAccountRepository.
type fakeCalAccountRepo struct {
	created          []*calendardomain.CalendarAccount
	byMemberProvider *calendardomain.CalendarAccount
	getResult        *calendardomain.CalendarAccount
	updatedSync      int
}

func (f *fakeCalAccountRepo) Create(_ context.Context, a *calendardomain.CalendarAccount) error {
	f.created = append(f.created, a)
	return nil
}

func (f *fakeCalAccountRepo) Get(context.Context, calendardomain.CalendarAccountID) (*calendardomain.CalendarAccount, error) {
	if f.getResult != nil {
		return f.getResult, nil
	}
	return nil, calendardomain.ErrCalendarAccountNotFound
}

func (f *fakeCalAccountRepo) GetByMemberProvider(context.Context, household.MemberID, calendardomain.Provider) (*calendardomain.CalendarAccount, error) {
	if f.byMemberProvider != nil {
		return f.byMemberProvider, nil
	}
	return nil, calendardomain.ErrCalendarAccountNotFound
}

func (f *fakeCalAccountRepo) UpdateTokens(context.Context, calendardomain.CalendarAccountID, []byte, []byte, time.Time, []string) error {
	return nil
}

func (f *fakeCalAccountRepo) UpdateSyncState(context.Context, calendardomain.CalendarAccountID, []byte, []byte, time.Time, *string) error {
	f.updatedSync++
	return nil
}

func (f *fakeCalAccountRepo) ListByHousehold(context.Context, household.HouseholdID) ([]*calendardomain.CalendarAccount, error) {
	return nil, nil
}

func (f *fakeCalAccountRepo) ListAll(context.Context) ([]*calendardomain.CalendarAccount, error) {
	return nil, nil
}

// fakeOAuthExchanger is an in-memory tokenExchanger.
type fakeOAuthExchanger struct {
	token       *oauth2.Token
	exchangeErr error
}

func (f *fakeOAuthExchanger) AuthCodeURL(state string) string {
	return "https://accounts.google.com/o/oauth2/auth?state=" + state
}

func (f *fakeOAuthExchanger) Exchange(_ context.Context, code string) (*oauth2.Token, error) {
	if f.exchangeErr != nil {
		return nil, f.exchangeErr
	}
	if f.token != nil {
		return f.token, nil
	}
	return &oauth2.Token{AccessToken: "access-" + code, RefreshToken: "refresh-tok", Expiry: time.Now().Add(time.Hour)}, nil
}

func (f *fakeOAuthExchanger) TokenSource(_ context.Context, tok *oauth2.Token) oauth2.TokenSource {
	return oauth2.StaticTokenSource(tok)
}

func buildCalendarTestService(t *testing.T, repo calendardomain.CalendarAccountRepository, exchanger *fakeOAuthExchanger, logger *slog.Logger) *calendarapp.AccountService {
	t.Helper()
	cipher, err := crypto.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	signer, err := calendarapp.NewOAuthStateSigner([]byte(testStateKey))
	if err != nil {
		t.Fatalf("NewOAuthStateSigner: %v", err)
	}
	svc, err := calendarapp.NewAccountService(repo, cipher, exchanger, signer, logger)
	if err != nil {
		t.Fatalf("NewAccountService: %v", err)
	}
	return svc
}

// newTestCalendarHandlers builds a no-op calendar WebHandlers for tests that need
// registerWebRoutes to compile but do not exercise the calendar routes.
func newTestCalendarHandlers(sm *scs.SessionManager, logger *slog.Logger) *calendaradapter.WebHandlers {
	cipher, _ := crypto.NewCipher(make([]byte, 32))
	signer, _ := calendarapp.NewOAuthStateSigner([]byte(testStateKey))
	svc, _ := calendarapp.NewAccountService(&fakeCalAccountRepo{}, cipher, &fakeOAuthExchanger{}, signer, logger)
	return calendaradapter.NewWebHandlers(svc, sm, logger)
}

func buildCalendarTestHandler(t *testing.T, member *household.Member, repo calendardomain.CalendarAccountRepository, exchanger *fakeOAuthExchanger) (http.Handler, *scs.SessionManager) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	householdRepo := authedHouseholdRepo{member: member}
	authn := authapp.New(testCredRepo{})
	authHandlers := authadapter.NewHandlers(sm, authn, logger)
	onboardingHandlers := authadapter.NewOnboardingHandlers(householdRepo, testCredStore{}, testProvisioner{}, sm, logger)

	recurringRepo := fakeRecurringTaskRepo{}
	instanceRepo := &fakeTaskInstanceRepo{}
	taskService, err := tasksapp.NewTaskService(recurringRepo, instanceRepo)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}
	taskWebHandlers := tasksadapter.NewWebHandlers(taskService, recurringRepo, instanceRepo, householdRepo, sm, logger)
	gamificationHandlers := newTestGamificationHandlers(instanceRepo, householdRepo, sm, logger)
	groceryHandlers := buildGroceryHandlers(newGroceryFakes(), householdRepo, sm, logger)

	calendarHandlers := calendaradapter.NewWebHandlers(buildCalendarTestService(t, repo, exchanger, logger), sm, logger)

	mux := http.NewServeMux()
	registerWebRoutes(mux, logger, sm, authHandlers, onboardingHandlers, householdRepo, taskWebHandlers, gamificationHandlers, groceryHandlers, newTestMealsHandlers(sm, logger), calendarHandlers)
	return sm.LoadAndSave(authadapter.Authenticate(sm, householdRepo)(mux)), sm
}

func TestCalendarConnectRejectsBadCSRF(t *testing.T) {
	member := testMember()
	repo := &fakeCalAccountRepo{}
	handler, sm := buildCalendarTestHandler(t, member, repo, &fakeOAuthExchanger{})
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/calendar/google/connect", strings.NewReader("csrf_token=wrong"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("connect with bad CSRF: status = %d, want 403", rec.Code)
	}
}

func TestCalendarConnectRedirectsToGoogle(t *testing.T) {
	member := testMember()
	handler, sm := buildCalendarTestHandler(t, member, &fakeCalAccountRepo{}, &fakeOAuthExchanger{})
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/calendar/google/connect", strings.NewReader("csrf_token="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("connect: status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "accounts.google.com") {
		t.Fatalf("connect Location = %q, want a Google consent URL", loc)
	}
}

func TestCalendarCallbackRejectsBadState(t *testing.T) {
	member := testMember()
	repo := &fakeCalAccountRepo{}
	handler, sm := buildCalendarTestHandler(t, member, repo, &fakeOAuthExchanger{})
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/calendar/google/callback?code=abc&state=tampered", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("callback with bad state: status = %d, want 403", rec.Code)
	}
	if len(repo.created) != 0 {
		t.Errorf("bad-state callback must not persist an account, got %d", len(repo.created))
	}
}

func TestCalendarCallbackHandlesDeniedConsent(t *testing.T) {
	member := testMember()
	repo := &fakeCalAccountRepo{}
	handler, sm := buildCalendarTestHandler(t, member, repo, &fakeOAuthExchanger{})
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/calendar/google/callback?error=access_denied", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("denied callback: status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "connect=denied") {
		t.Fatalf("denied callback Location = %q, want connect=denied", loc)
	}
	if len(repo.created) != 0 {
		t.Errorf("denied callback must not persist an account, got %d", len(repo.created))
	}
}

func TestCalendarCallbackPersistsAccount(t *testing.T) {
	member := testMember()
	repo := &fakeCalAccountRepo{}
	handler, sm := buildCalendarTestHandler(t, member, repo, &fakeOAuthExchanger{})
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	signer, err := calendarapp.NewOAuthStateSigner([]byte(testStateKey))
	if err != nil {
		t.Fatalf("NewOAuthStateSigner: %v", err)
	}
	state := signer.Sign(member.ID.String(), time.Now())

	req := httptest.NewRequest(http.MethodGet, "/calendar/google/callback?code=auth-code&state="+state, nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "connect=ok") {
		t.Fatalf("callback Location = %q, want connect=ok", loc)
	}
	if len(repo.created) != 1 {
		t.Fatalf("callback persisted %d accounts, want 1", len(repo.created))
	}
	if got := repo.created[0]; got.MemberID != member.ID || got.HouseholdID != member.HouseholdID || got.Provider != calendardomain.ProviderGoogle {
		t.Fatalf("persisted account = %+v, want member %s household %s / google", got, member.ID, member.HouseholdID)
	}
	if len(repo.created[0].AccessTokenEnc) == 0 || len(repo.created[0].RefreshTokenEnc) == 0 {
		t.Error("persisted account is missing encrypted tokens")
	}
}
