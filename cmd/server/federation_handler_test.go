package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/crypto/cryptotest"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
)

// ---------------------------------------------------------------------------
// Test harness for GET /federation/authorize (NSTR-105): the provider side
// of the authorization-code redirect Nestorage's own login sends a person
// through. Mirrors buildLoginMFATestHandler's (login_mfa_handler_test.go)
// approach — real Handlers.Login + FederationHandlers against an in-memory
// session store — scoped to just what Authorize needs.
// ---------------------------------------------------------------------------

// fakeAuthorizationCodeRepo is an in-memory authdomain.AuthorizationCodeRepository,
// mirroring fakeActivationCodeRepo's (kiosk_handler_test.go) approach: close
// enough to the real adapter's contract for these HTTP-layer tests; true
// rollback-on-failure atomicity is covered by the gated adapter test
// (authorization_code_postgres_test.go).
type fakeAuthorizationCodeRepo struct {
	byHash map[string]*authdomain.AuthorizationCode
}

func newFakeAuthorizationCodeRepo() *fakeAuthorizationCodeRepo {
	return &fakeAuthorizationCodeRepo{byHash: make(map[string]*authdomain.AuthorizationCode)}
}

func (f *fakeAuthorizationCodeRepo) Create(_ context.Context, code *authdomain.AuthorizationCode) error {
	code.CreatedAt = time.Now()
	cp := *code
	f.byHash[code.CodeHash] = &cp
	return nil
}

func (f *fakeAuthorizationCodeRepo) Consume(_ context.Context, codeHash string, now time.Time) (*authdomain.AuthorizationCode, error) {
	code, ok := f.byHash[codeHash]
	if !ok {
		return nil, authdomain.ErrAuthorizationCodeNotFound
	}
	if code.UsedAt != nil {
		return nil, authdomain.ErrAuthorizationCodeUsed
	}
	if !now.Before(code.ExpiresAt) {
		return nil, authdomain.ErrAuthorizationCodeExpired
	}
	usedAt := now
	code.UsedAt = &usedAt
	cp := *code
	return &cp, nil
}

var _ authdomain.AuthorizationCodeRepository = (*fakeAuthorizationCodeRepo)(nil)

// testFederationConfig returns a fixed, fully-configured registered client
// for the tests in this file.
func testFederationConfig() config.FederationConfig {
	return config.FederationConfig{
		ClientID:     "nestorage-household-1",
		ClientSecret: "federation-test-harness-client-secret-32",
		RedirectURL:  "https://nestorage.example.ts.net/federation/callback",
	}
}

// buildFederationTestHandler wires just enough of the server to exercise
// GET /federation/authorize end to end: real Handlers.Login (so the
// no-session case can round-trip through the ordinary login form) plus
// FederationHandlers, against an in-memory session store.
func buildFederationTestHandler(t *testing.T, hhRepo household.HouseholdRepository, credRepo *loginTestCredRepo, cfg config.FederationConfig, codes authdomain.AuthorizationCodeRepository) (http.Handler, *scs.SessionManager) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()

	authn := authapp.New(credRepo, cryptotest.Hasher())
	authHandlers := authadapter.NewHandlers(sm, authn, nil, nil, nil, logger)
	federationHandlers := authadapter.NewFederationHandlers(cfg, sm, codes, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	mux.HandleFunc("POST /login", authHandlers.Login)
	mux.HandleFunc("GET /federation/authorize", federationHandlers.Authorize)

	handler := sm.LoadAndSave(authadapter.Authenticate(sm, hhRepo)(mux))
	return handler, sm
}

// authorizeURL builds a GET /federation/authorize query string from the
// given parameters, omitting any that are empty (so a test can exercise a
// missing parameter, e.g. state, by passing "").
func authorizeURL(responseType, clientID, redirectURI, state string) string {
	q := url.Values{}
	if responseType != "" {
		q.Set("response_type", responseType)
	}
	if clientID != "" {
		q.Set("client_id", clientID)
	}
	if redirectURI != "" {
		q.Set("redirect_uri", redirectURI)
	}
	if state != "" {
		q.Set("state", state)
	}
	return "/federation/authorize?" + q.Encode()
}

// redirectTarget returns the scheme+host+path of a redirect Location header,
// stripping the query string so a test can compare it against the registered
// redirect origin independently of the query parameters it carries.
func redirectTarget(t *testing.T, location string) *url.URL {
	t.Helper()
	dest, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse Location %q: %v", location, err)
	}
	return dest
}

// ---------------------------------------------------------------------------
// AC: "A member with an active Nestova session is redirected back to the
// calling client with a code, without re-authenticating."
// ---------------------------------------------------------------------------

func TestFederationAuthorize_ActiveSession_RedirectsWithCodeAndState(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	credRepo.seed(t, member.ID, "adult@example.com", loginMFATestPassword)
	cfg := testFederationConfig()
	codes := newFakeAuthorizationCodeRepo()
	handler, _ := buildFederationTestHandler(t, hhRepo, credRepo, cfg, codes)

	// Establish an active session first — no MFA is wired in this harness,
	// so POST /login promotes the session directly.
	flow := newLoginFlow(t, handler)
	loginRec := flow.login("adult@example.com", loginMFATestPassword, "/")
	if loginRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login: status = %d, want 303; body: %s", loginRec.Code, loginRec.Body.String())
	}

	rec := flow.do(http.MethodGet, authorizeURL("code", cfg.ClientID, cfg.RedirectURL, "xyz-state"), "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET /federation/authorize (active session): status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}

	dest := redirectTarget(t, rec.Header().Get("Location"))
	if got := (&url.URL{Scheme: dest.Scheme, Host: dest.Host, Path: dest.Path}).String(); got != cfg.RedirectURL {
		t.Errorf("redirect target = %q, want %q", got, cfg.RedirectURL)
	}
	if got := dest.Query().Get("state"); got != "xyz-state" {
		t.Errorf("state = %q, want the echoed xyz-state", got)
	}
	rawCode := dest.Query().Get("code")
	if rawCode == "" {
		t.Fatal("no code in the redirect")
	}

	// The persisted code must be bound to exactly this member, client, and
	// redirect, with an expiry ~AuthorizationCodeTTL from now — single-use/
	// expiry/binding enforcement itself is proven at the adapter level by
	// the gated Postgres tests (authorization_code_postgres_test.go); this
	// only checks Authorize populated the right fields before persisting.
	stored, ok := codes.byHash[authdomain.HashAuthorizationCode(rawCode)]
	if !ok {
		t.Fatal("no authorization code was persisted for the issued code")
	}
	if stored.MemberID != member.ID || stored.ClientID != cfg.ClientID || stored.RedirectURI != cfg.RedirectURL {
		t.Errorf("persisted code = %+v, want bound to member %s, client %q, redirect %q",
			stored, member.ID, cfg.ClientID, cfg.RedirectURL)
	}
	wantExpiry := time.Now().Add(authdomain.AuthorizationCodeTTL)
	if diff := wantExpiry.Sub(stored.ExpiresAt); diff < -5*time.Second || diff > 5*time.Second {
		t.Errorf("ExpiresAt = %v, want ~%v (AuthorizationCodeTTL from now)", stored.ExpiresAt, wantExpiry)
	}
}

// ---------------------------------------------------------------------------
// AC: "A member without a session authenticates through Nestova's normal
// login ... and is then redirected back with a code."
// ---------------------------------------------------------------------------

func TestFederationAuthorize_NoSession_RoundTripsThroughLoginThenRedirectsWithCode(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	credRepo.seed(t, member.ID, "adult@example.com", loginMFATestPassword)
	cfg := testFederationConfig()
	codes := newFakeAuthorizationCodeRepo()
	handler, _ := buildFederationTestHandler(t, hhRepo, credRepo, cfg, codes)

	authorizePath := authorizeURL("code", cfg.ClientID, cfg.RedirectURL, "round-trip-state")

	// No prior session at all: hit the protected authorize route directly.
	flow := &loginFlow{t: t, handler: handler}
	rec := flow.do(http.MethodGet, authorizePath, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET /federation/authorize (no session): status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	wantLoginRedirect := "/login?next=" + url.QueryEscape(authorizePath)
	if loc != wantLoginRedirect {
		t.Fatalf("Location = %q, want %q (RequireMember's own redirect shape)", loc, wantLoginRedirect)
	}
	flow.absorb(rec)

	loginPageRec := flow.followRedirect(rec)
	if loginPageRec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", loc, loginPageRec.Code)
	}

	loginRec := flow.login("adult@example.com", loginMFATestPassword, authorizePath)
	if loginRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login: status = %d, want 303; body: %s", loginRec.Code, loginRec.Body.String())
	}
	if got := loginRec.Header().Get("Location"); got != authorizePath {
		t.Fatalf("POST /login Location = %q, want the original authorize path %q", got, authorizePath)
	}

	finalRec := flow.followRedirect(loginRec)
	if finalRec.Code != http.StatusSeeOther {
		t.Fatalf("GET %s after completing login: status = %d, want 303 (redirect to client with code); body: %s",
			authorizePath, finalRec.Code, finalRec.Body.String())
	}
	dest := redirectTarget(t, finalRec.Header().Get("Location"))
	if got := dest.Query().Get("state"); got != "round-trip-state" {
		t.Errorf("state = %q, want the echoed round-trip-state", got)
	}
	if dest.Query().Get("code") == "" {
		t.Error("no code in the post-login redirect")
	}
}

// ---------------------------------------------------------------------------
// AC: "An unregistered client identifier or an unregistered redirect target
// is refused with a plain 400 before authentication is attempted — never a
// redirect."
// ---------------------------------------------------------------------------

func TestFederationAuthorize_UnregisteredClientOrRedirect_Returns400BeforeAuth(t *testing.T) {
	hhRepo := newMultiMemberHouseholdRepo()
	credRepo := newLoginTestCredRepo()
	cfg := testFederationConfig()
	codes := newFakeAuthorizationCodeRepo()
	handler, _ := buildFederationTestHandler(t, hhRepo, credRepo, cfg, codes)

	tests := []struct {
		name        string
		clientID    string
		redirectURI string
	}{
		{"unregistered client id", "someone-elses-client", cfg.RedirectURL},
		{"unregistered redirect uri", cfg.ClientID, "https://evil.example/callback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, authorizeURL("code", tt.clientID, tt.redirectURI, "s"), nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("Location = %q, want no redirect at all for an unregistered client/redirect", loc)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC (Implementation Plan): "a wrong response_type or missing state
// redirects back to the registered target with an RFC 6749 error
// parameter instead of rendering."
// ---------------------------------------------------------------------------

func TestFederationAuthorize_MalformedRequest_RedirectsToClientWithError(t *testing.T) {
	hhRepo := newMultiMemberHouseholdRepo()
	credRepo := newLoginTestCredRepo()
	cfg := testFederationConfig()
	codes := newFakeAuthorizationCodeRepo()
	handler, _ := buildFederationTestHandler(t, hhRepo, credRepo, cfg, codes)

	tests := []struct {
		name         string
		responseType string
		state        string
		wantError    string
	}{
		{"unsupported response_type", "token", "some-state", "unsupported_response_type"},
		{"missing state", "code", "", "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, authorizeURL(tt.responseType, cfg.ClientID, cfg.RedirectURL, tt.state), nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want 303; body: %s", rec.Code, rec.Body.String())
			}
			dest := redirectTarget(t, rec.Header().Get("Location"))
			if got := (&url.URL{Scheme: dest.Scheme, Host: dest.Host, Path: dest.Path}).String(); got != cfg.RedirectURL {
				t.Errorf("redirect target = %q, want %q", got, cfg.RedirectURL)
			}
			if got := dest.Query().Get("error"); got != tt.wantError {
				t.Errorf("error = %q, want %q", got, tt.wantError)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC: "Partial federation configuration fails startup naming the missing
// variable; an unconfigured install registers no federation route." The
// config-validation half is covered by internal/platform/config's own
// tests; this covers the wiring half — mirrors loginPasskeyHandlers' own
// nil-gated routes (already exercised implicitly by every other test file
// in this package, which all pass federationHandlers=nil to
// registerWebRoutes and compile/run unaffected).
// ---------------------------------------------------------------------------

func TestFederationAuthorize_UnconfiguredInstall_RegistersNoRoute(t *testing.T) {
	handler := buildTestHandler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/federation/authorize", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /federation/authorize with no federation client configured: status = %d, want 404", rec.Code)
	}
}
