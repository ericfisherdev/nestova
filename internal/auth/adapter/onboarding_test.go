package adapter_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"

	"github.com/ericfisherdev/nestova/internal/auth/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// fakeHouseholdRepo is a minimal in-memory HouseholdRepository for onboarding
// handler tests. hasAny controls the return value of HasAnyHousehold, and
// currentMember (when set) is returned from GetMember so the Authenticate
// middleware can inject an authenticated member into the request context.
type fakeHouseholdRepo struct {
	hasAny        bool
	currentMember *household.Member
}

func (r *fakeHouseholdRepo) HasAnyHousehold(_ context.Context) (bool, error) {
	return r.hasAny, nil
}

func (r *fakeHouseholdRepo) CreateHousehold(_ context.Context, _ *household.Household) error {
	r.hasAny = true
	return nil
}

func (r *fakeHouseholdRepo) GetHousehold(_ context.Context, _ household.HouseholdID) (*household.Household, error) {
	return nil, household.ErrHouseholdNotFound
}

func (r *fakeHouseholdRepo) AddMember(_ context.Context, _ *household.Member) error { return nil }

func (r *fakeHouseholdRepo) GetMember(_ context.Context, _ household.MemberID) (*household.Member, error) {
	if r.currentMember != nil {
		return r.currentMember, nil
	}
	return nil, household.ErrMemberNotFound
}

func (r *fakeHouseholdRepo) ListMembers(_ context.Context, _ household.HouseholdID) ([]*household.Member, error) {
	return nil, nil
}

// Compile-time assertion.
var _ household.HouseholdRepository = (*fakeHouseholdRepo)(nil)

// fakeCredStore is a configurable credentialStore for hermetic tests.
// emailExists controls the EmailExists return value.
type fakeCredStore struct {
	emailExists bool
}

func (s fakeCredStore) EmailExists(_ context.Context, _ string) (bool, error) {
	return s.emailExists, nil
}

// fakeProvisioner records the arguments passed to its methods so tests can
// assert what was actually provisioned. err, when set, is returned from both
// methods to simulate a provisioning failure.
type fakeProvisioner struct {
	householdCalls int
	memberCalls    int
	lastHousehold  *household.Household
	lastOwner      *household.Member
	lastMember     *household.Member
	lastEmail      string
	err            error
}

func (p *fakeProvisioner) ProvisionHousehold(
	_ context.Context,
	hh *household.Household,
	owner *household.Member,
	email, _ string,
) error {
	p.householdCalls++
	p.lastHousehold = hh
	p.lastOwner = owner
	p.lastEmail = email
	return p.err
}

func (p *fakeProvisioner) ProvisionMember(
	_ context.Context,
	m *household.Member,
	email, _ string,
) error {
	p.memberCalls++
	p.lastMember = m
	p.lastEmail = email
	return p.err
}

// Compile-time assertion.
var _ adapter.Provisioner = (*fakeProvisioner)(nil)

// newOnboardingSessionManager creates an scs session manager backed by the
// in-memory store, suitable for hermetic handler tests.
func newOnboardingSessionManager() *scs.SessionManager {
	sm := scs.New()
	sm.Store = memstore.New()
	sm.Lifetime = 1 * time.Hour
	sm.Cookie.Secure = false
	return sm
}

// buildOnboardingHandler wraps OnboardingHandlers in session middleware and
// routes it onto a mux so tests can exercise the full HTTP path.
func buildOnboardingHandler(repo *fakeHouseholdRepo, prov *fakeProvisioner) (*scs.SessionManager, http.Handler) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newOnboardingSessionManager()
	h := adapter.NewOnboardingHandlers(repo, fakeCredStore{}, prov, sm, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /onboarding", h.OnboardingPage)
	mux.HandleFunc("POST /onboarding", h.Onboard)

	return sm, sm.LoadAndSave(mux)
}

// TestVerifyCSRF_Match confirms VerifyCSRF returns true when the form token
// matches the one stored in the session.
func TestVerifyCSRF_Match(t *testing.T) {
	sm := newOnboardingSessionManager()

	// Seed a CSRF token into a real session via a GET handler so the session
	// store can persist it correctly.
	var capturedToken string
	seedHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedToken = adapter.GetCSRFToken(r.Context(), sm)
		w.WriteHeader(http.StatusOK)
	}))

	seedReq := httptest.NewRequest(http.MethodGet, "/", nil)
	seedRec := httptest.NewRecorder()
	seedHandler.ServeHTTP(seedRec, seedReq)

	// The token must be a 32-byte value hex-encoded to 64 chars; a fixed length
	// keeps the constant-time comparison meaningful.
	if len(capturedToken) != 64 {
		t.Errorf("CSRF token length = %d, want 64", len(capturedToken))
	}

	// Extract the session cookie from the seed response.
	cookies := seedRec.Result().Cookies()

	// Verify: build a POST that presents the same token in the form.
	form := url.Values{"csrf_token": {capturedToken}}
	verifyReq := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	verifyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		verifyReq.AddCookie(c)
	}

	var got bool
	verifyHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		got = adapter.VerifyCSRF(r, sm)
		w.WriteHeader(http.StatusOK)
	}))
	verifyRec := httptest.NewRecorder()
	verifyHandler.ServeHTTP(verifyRec, verifyReq)

	if !got {
		t.Error("VerifyCSRF returned false, want true on matching token")
	}
}

// TestVerifyCSRF_Mismatch confirms VerifyCSRF returns false when the form
// token does not match the session token.
func TestVerifyCSRF_Mismatch(t *testing.T) {
	sm := newOnboardingSessionManager()

	// Seed a real session token.
	seedHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adapter.GetCSRFToken(r.Context(), sm)
		w.WriteHeader(http.StatusOK)
	}))
	seedReq := httptest.NewRequest(http.MethodGet, "/", nil)
	seedRec := httptest.NewRecorder()
	seedHandler.ServeHTTP(seedRec, seedReq)
	cookies := seedRec.Result().Cookies()

	// Present a deliberately wrong token.
	form := url.Values{"csrf_token": {"wrong-token-value"}}
	verifyReq := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	verifyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		verifyReq.AddCookie(c)
	}

	var got bool
	verifyHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		got = adapter.VerifyCSRF(r, sm)
		w.WriteHeader(http.StatusOK)
	}))
	verifyHandler.ServeHTTP(httptest.NewRecorder(), verifyReq)

	if got {
		t.Error("VerifyCSRF returned true, want false on mismatched token")
	}
}

// TestVerifyCSRF_Empty confirms VerifyCSRF returns false when no token is in
// the form (empty string).
func TestVerifyCSRF_Empty(t *testing.T) {
	sm := newOnboardingSessionManager()

	// Seed a session so the session token is non-empty.
	seedHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adapter.GetCSRFToken(r.Context(), sm)
		w.WriteHeader(http.StatusOK)
	}))
	seedReq := httptest.NewRequest(http.MethodGet, "/", nil)
	seedRec := httptest.NewRecorder()
	seedHandler.ServeHTTP(seedRec, seedReq)
	cookies := seedRec.Result().Cookies()

	// Submit with no csrf_token field at all.
	verifyReq := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	verifyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		verifyReq.AddCookie(c)
	}

	var got bool
	verifyHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		got = adapter.VerifyCSRF(r, sm)
		w.WriteHeader(http.StatusOK)
	}))
	verifyHandler.ServeHTTP(httptest.NewRecorder(), verifyReq)

	if got {
		t.Error("VerifyCSRF returned true, want false when form token is empty")
	}
}

// TestVerifyCSRF_HeaderMatch confirms VerifyCSRF returns true when the
// X-CSRF-Token HEADER matches the session token, with no form field at all
// (NES-136: JSON endpoints like WebAuthnWebHandlers' registration ceremony
// have no form to carry the token in — see VerifyCSRF's own doc).
func TestVerifyCSRF_HeaderMatch(t *testing.T) {
	sm := newOnboardingSessionManager()

	var capturedToken string
	seedHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedToken = adapter.GetCSRFToken(r.Context(), sm)
		w.WriteHeader(http.StatusOK)
	}))
	seedReq := httptest.NewRequest(http.MethodGet, "/", nil)
	seedRec := httptest.NewRecorder()
	seedHandler.ServeHTTP(seedRec, seedReq)
	cookies := seedRec.Result().Cookies()

	verifyReq := httptest.NewRequest(http.MethodPost, "/", nil)
	verifyReq.Header.Set("X-CSRF-Token", capturedToken)
	for _, c := range cookies {
		verifyReq.AddCookie(c)
	}

	var got bool
	verifyHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = adapter.VerifyCSRF(r, sm)
		w.WriteHeader(http.StatusOK)
	}))
	verifyHandler.ServeHTTP(httptest.NewRecorder(), verifyReq)

	if !got {
		t.Error("VerifyCSRF returned false, want true on a matching X-CSRF-Token header with no form field present")
	}
}

// TestVerifyCSRF_HeaderMismatch confirms VerifyCSRF returns false when the
// X-CSRF-Token header is present but wrong.
func TestVerifyCSRF_HeaderMismatch(t *testing.T) {
	sm := newOnboardingSessionManager()

	seedHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adapter.GetCSRFToken(r.Context(), sm)
		w.WriteHeader(http.StatusOK)
	}))
	seedReq := httptest.NewRequest(http.MethodGet, "/", nil)
	seedRec := httptest.NewRecorder()
	seedHandler.ServeHTTP(seedRec, seedReq)
	cookies := seedRec.Result().Cookies()

	verifyReq := httptest.NewRequest(http.MethodPost, "/", nil)
	verifyReq.Header.Set("X-CSRF-Token", "wrong-token-value")
	for _, c := range cookies {
		verifyReq.AddCookie(c)
	}

	var got bool
	verifyHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = adapter.VerifyCSRF(r, sm)
		w.WriteHeader(http.StatusOK)
	}))
	verifyHandler.ServeHTTP(httptest.NewRecorder(), verifyReq)

	if got {
		t.Error("VerifyCSRF returned true, want false on a mismatched X-CSRF-Token header")
	}
}

// TestVerifyCSRF_HeaderTakesPrecedenceOverForm confirms that a PRESENT
// X-CSRF-Token header is used directly (and, if wrong, fails the check)
// rather than silently falling back to a correct form field when the
// header is merely wrong rather than absent — the fallback in VerifyCSRF's
// implementation is conditioned on the header being EMPTY, not on the
// header failing to match.
func TestVerifyCSRF_HeaderTakesPrecedenceOverForm(t *testing.T) {
	sm := newOnboardingSessionManager()

	var capturedToken string
	seedHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedToken = adapter.GetCSRFToken(r.Context(), sm)
		w.WriteHeader(http.StatusOK)
	}))
	seedReq := httptest.NewRequest(http.MethodGet, "/", nil)
	seedRec := httptest.NewRecorder()
	seedHandler.ServeHTTP(seedRec, seedReq)
	cookies := seedRec.Result().Cookies()

	// The FORM field carries the correct token, but the HEADER carries a
	// wrong one — the header must win (and therefore fail the check).
	form := url.Values{"csrf_token": {capturedToken}}
	verifyReq := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	verifyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	verifyReq.Header.Set("X-CSRF-Token", "wrong-token-value")
	for _, c := range cookies {
		verifyReq.AddCookie(c)
	}

	var got bool
	verifyHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		got = adapter.VerifyCSRF(r, sm)
		w.WriteHeader(http.StatusOK)
	}))
	verifyHandler.ServeHTTP(httptest.NewRecorder(), verifyReq)

	if got {
		t.Error("VerifyCSRF returned true, want false: a present-but-wrong header must not fall back to a correct form field")
	}
}

// TestOnboardingGET_RendersFormWhenNoHousehold confirms that GET /onboarding
// returns 200 with the form when no household exists yet.
func TestOnboardingGET_RendersFormWhenNoHousehold(t *testing.T) {
	repo := &fakeHouseholdRepo{hasAny: false}
	_, handler := buildOnboardingHandler(repo, &fakeProvisioner{})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/onboarding", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Set up your household",
		`name="household_name"`,
		`name="display_name"`,
		`name="email"`,
		`name="password"`,
		`name="csrf_token"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("onboarding page missing %q", want)
		}
	}
}

// TestOnboardingGET_RedirectsWhenHouseholdExists confirms that GET /onboarding
// redirects to /login when a household already exists (first-run guard).
func TestOnboardingGET_RedirectsWhenHouseholdExists(t *testing.T) {
	repo := &fakeHouseholdRepo{hasAny: true}
	_, handler := buildOnboardingHandler(repo, &fakeProvisioner{})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/onboarding", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 redirect to /login", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

// TestOnboardingPOST_RejectsBadCSRF confirms that POST /onboarding with a
// wrong CSRF token returns 403.
func TestOnboardingPOST_RejectsBadCSRF(t *testing.T) {
	repo := &fakeHouseholdRepo{hasAny: false}
	prov := &fakeProvisioner{}
	sm, handler := buildOnboardingHandler(repo, prov)

	// Seed a real session so the session has a CSRF token.
	seedReq := httptest.NewRequest(http.MethodGet, "/onboarding", nil)
	seedRec := httptest.NewRecorder()
	handler.ServeHTTP(seedRec, seedReq)
	cookies := seedRec.Result().Cookies()

	// Post a valid-looking form body but with an incorrect CSRF token.
	form := url.Values{
		"csrf_token":     {"deliberately-wrong"},
		"household_name": {"The Smiths"},
		"display_name":   {"Alex"},
		"email":          {"alex@example.com"},
		"password":       {"supersecret"},
	}
	postReq := httptest.NewRequest(http.MethodPost, "/onboarding", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		postReq.AddCookie(c)
	}

	_ = sm // ensure sm is reachable for the session store lookup
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, postReq)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 on bad CSRF token", rec.Code)
	}
	// A rejected CSRF must not have provisioned anything.
	if prov.householdCalls != 0 {
		t.Errorf("ProvisionHousehold called %d times on bad CSRF, want 0", prov.householdCalls)
	}
}

// TestOnboardingPOST_CreatesHouseholdAndRedirects is an end-to-end hermetic
// test of the happy path: valid CSRF + valid form creates a household and signs
// the user in, redirecting to /.
func TestOnboardingPOST_CreatesHouseholdAndRedirects(t *testing.T) {
	repo := &fakeHouseholdRepo{hasAny: false}
	prov := &fakeProvisioner{}
	_, handler := buildOnboardingHandler(repo, prov)

	// GET /onboarding to obtain a session cookie and CSRF token.
	getReq := httptest.NewRequest(http.MethodGet, "/onboarding", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /onboarding: status = %d, want 200", getRec.Code)
	}
	cookies := getRec.Result().Cookies()

	// Extract the CSRF token from the rendered form.
	body := getRec.Body.String()
	csrfToken := extractCSRF(t, body)

	// POST the onboarding form with the correct token.
	form := url.Values{
		"csrf_token":     {csrfToken},
		"household_name": {"The Smiths"},
		"display_name":   {"Alex Smith"},
		"email":          {"alex@example.com"},
		"password":       {"supersecret"},
	}
	postReq := httptest.NewRequest(http.MethodPost, "/onboarding", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		postReq.AddCookie(c)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, postReq)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /onboarding: status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}

	// RenewToken must rotate the session cookie on sign-in (session-fixation
	// defense): the post-onboarding session token differs from the pre-onboarding one.
	var beforeTok, afterTok string
	for _, c := range cookies {
		if c.Name == "session" {
			beforeTok = c.Value
		}
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "session" {
			afterTok = c.Value
		}
	}
	if afterTok == "" || afterTok == beforeTok {
		t.Errorf("session cookie not rotated by RenewToken: before=%q after=%q", beforeTok, afterTok)
	}

	// The household and owner must have been provisioned atomically (one call),
	// with the submitted values.
	if prov.householdCalls != 1 {
		t.Fatalf("ProvisionHousehold called %d times, want 1", prov.householdCalls)
	}
	if prov.lastHousehold == nil || prov.lastHousehold.Name != "The Smiths" {
		t.Errorf("provisioned household = %+v, want name %q", prov.lastHousehold, "The Smiths")
	}
	if prov.lastOwner == nil {
		t.Fatal("provisioned owner is nil")
	}
	if prov.lastOwner.DisplayName != "Alex Smith" {
		t.Errorf("owner DisplayName = %q, want %q", prov.lastOwner.DisplayName, "Alex Smith")
	}
	if prov.lastOwner.Role != household.RoleOwner {
		t.Errorf("owner Role = %q, want %q", prov.lastOwner.Role, household.RoleOwner)
	}
	if prov.lastOwner.HouseholdID != prov.lastHousehold.ID {
		t.Error("owner HouseholdID does not match the provisioned household ID")
	}
	if prov.lastEmail != "alex@example.com" {
		t.Errorf("provisioned email = %q, want %q", prov.lastEmail, "alex@example.com")
	}
}

// TestAddMember_EmailInUse confirms that when the email is already taken, the
// add-member flow renders the in-use error and does NOT create any member
// (ProvisionMember is never called).
func TestAddMember_EmailInUse(t *testing.T) {
	owner := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: household.NewHouseholdID(),
		DisplayName: "Owner",
		Role:        household.RoleOwner,
		Color:       household.ColorSage,
	}
	repo := &fakeHouseholdRepo{currentMember: owner}
	prov := &fakeProvisioner{}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newOnboardingSessionManager()
	// EmailExists returns true: the address is already taken.
	h := adapter.NewOnboardingHandlers(repo, fakeCredStore{emailExists: true}, prov, sm, logger)

	mux := http.NewServeMux()
	// Seed an authenticated session, then route the POST /members handler with
	// props/nav supplied (as the composition root does).
	mux.HandleFunc("POST /members", func(w http.ResponseWriter, r *http.Request) {
		h.AddMember(w, r, components.ShellProps{}, nil)
	})
	mux.HandleFunc("GET /seed", func(w http.ResponseWriter, r *http.Request) {
		sm.Put(r.Context(), "member_id", owner.ID.String())
		_ = adapter.GetCSRFToken(r.Context(), sm)
		w.WriteHeader(http.StatusOK)
	})

	handler := sm.LoadAndSave(adapter.Authenticate(sm, repo)(mux))

	// Seed the session (member_id + CSRF token).
	seedReq := httptest.NewRequest(http.MethodGet, "/seed", nil)
	seedRec := httptest.NewRecorder()
	handler.ServeHTTP(seedRec, seedReq)
	cookies := seedRec.Result().Cookies()

	// Read the CSRF token straight from the session store using a throwaway
	// request carrying the same cookie.
	csrfToken := readSessionCSRF(t, sm, cookies)

	form := url.Values{
		"csrf_token":   {csrfToken},
		"display_name": {"Jamie"},
		"role":         {"adult"},
		"email":        {"taken@example.com"},
		"password":     {"supersecret"},
	}
	postReq := httptest.NewRequest(http.MethodPost, "/members", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		postReq.AddCookie(c)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, postReq)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 when email is in use", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "already in use") {
		t.Error("response body missing the in-use error message")
	}
	// Critically: nothing was provisioned.
	if prov.memberCalls != 0 {
		t.Errorf("ProvisionMember called %d times, want 0 when email is in use", prov.memberCalls)
	}
}

// readSessionCSRF returns the CSRF token stored in the session identified by
// cookies, by invoking a throwaway GET that echoes it.
func readSessionCSRF(t *testing.T, sm *scs.SessionManager, cookies []*http.Cookie) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /echo-csrf", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(adapter.GetCSRFToken(r.Context(), sm)))
	})
	echo := sm.LoadAndSave(mux)

	req := httptest.NewRequest(http.MethodGet, "/echo-csrf", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	echo.ServeHTTP(rec, req)
	token := rec.Body.String()
	if token == "" {
		t.Fatal("session CSRF token is empty")
	}
	return token
}

// extractCSRF pulls the csrf_token hidden field value from raw HTML. It fails
// the test when no token is found.
func extractCSRF(t *testing.T, html string) string {
	t.Helper()
	const prefix = `name="csrf_token" value="`
	idx := strings.Index(html, prefix)
	if idx == -1 {
		t.Fatal("csrf_token hidden field not found in rendered HTML")
	}
	rest := html[idx+len(prefix):]
	end := strings.Index(rest, `"`)
	if end == -1 {
		t.Fatal("csrf_token value not terminated")
	}
	token := rest[:end]
	if token == "" {
		t.Fatal("csrf_token value is empty")
	}
	return token
}
