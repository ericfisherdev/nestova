package adapter_test

import (
	"context"
	"errors"
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
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
)

// ---------------------------------------------------------------------------
// Package-level tests for GET /federation/authorize (NSTR-105).
//
// cmd/server/federation_handler_test.go already exercises Authorize
// end-to-end through the real composition root, but Go's default coverage
// instrumentation (-cover with no -coverpkg) only attributes coverage to the
// package under test: since federation_http.go lives in this package
// (internal/auth/adapter) and cmd/server's tests run as package main, none
// of that exercise counts toward federation_http.go's own coverage. These
// tests cover the same handler directly, in-package, hermetically (no
// database — mirrors onboarding_test.go's fakeHouseholdRepo pattern), so the
// coverage actually lands where Sonar looks for it.
// ---------------------------------------------------------------------------

// fakeFederationCodeRepo is a minimal in-memory
// authdomain.AuthorizationCodeRepository for these tests. Consume is never
// invoked by Authorize (NSTR-110's token exchange owns that leg) so it just
// satisfies the interface; createErr, when set, makes Create fail the way a
// persistence error would.
type fakeFederationCodeRepo struct {
	created   []*authdomain.AuthorizationCode
	createErr error
}

func (f *fakeFederationCodeRepo) Create(_ context.Context, code *authdomain.AuthorizationCode) error {
	if f.createErr != nil {
		return f.createErr
	}
	code.CreatedAt = time.Now()
	cp := *code
	f.created = append(f.created, &cp)
	return nil
}

func (f *fakeFederationCodeRepo) Consume(_ context.Context, _ string, _ time.Time) (*authdomain.AuthorizationCode, error) {
	return nil, authdomain.ErrAuthorizationCodeNotFound
}

var _ authdomain.AuthorizationCodeRepository = (*fakeFederationCodeRepo)(nil)

// newTestFederationConfig returns a fixed, fully-configured registered
// client for the tests in this file.
func newTestFederationConfig() config.FederationConfig {
	return config.FederationConfig{
		ClientID:     "nestorage-household-1",
		ClientSecret: "federation-test-harness-client-secret-32",
		RedirectURL:  "https://nestorage.example.ts.net/federation/callback",
	}
}

// newFederationTestSessionManager returns an in-memory scs.SessionManager
// suitable for hermetic handler tests (mirrors onboarding_test.go's
// newOnboardingSessionManager).
func newFederationTestSessionManager() *scs.SessionManager {
	sm := scs.New()
	sm.Store = memstore.New()
	sm.Lifetime = 1 * time.Hour
	sm.Cookie.Secure = false
	return sm
}

// buildFederationHandler wires FederationHandlers behind Authenticate, the
// same middleware the real composition root uses, so CurrentMember resolves
// exactly as it does in production.
func buildFederationHandler(t *testing.T, cfg config.FederationConfig, codes authdomain.AuthorizationCodeRepository, hhRepo household.HouseholdRepository) (*scs.SessionManager, http.Handler) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newFederationTestSessionManager()
	h := adapter.NewFederationHandlers(cfg, sm, codes, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /federation/authorize", h.Authorize)

	return sm, sm.LoadAndSave(adapter.Authenticate(sm, hhRepo)(mux))
}

// seedFederationSession plants memberID as the session's authenticated
// member (mirrors onboarding_test.go's seeding of "member_id" via sm.Put
// inside a real request/response round trip) and returns the session
// cookies a subsequent request must carry to be recognized as that member.
func seedFederationSession(t *testing.T, sm *scs.SessionManager, hhRepo household.HouseholdRepository, memberID household.MemberID) []*http.Cookie {
	t.Helper()
	seed := sm.LoadAndSave(adapter.Authenticate(sm, hhRepo)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sm.Put(r.Context(), "member_id", memberID.String())
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "/seed", nil)
	rec := httptest.NewRecorder()
	seed.ServeHTTP(rec, req)
	return rec.Result().Cookies()
}

// federationAuthorizeURL builds a GET /federation/authorize query string,
// omitting any parameter that is empty (so a test can exercise a missing
// parameter, e.g. state, by passing "").
func federationAuthorizeURL(responseType, clientID, redirectURI, state string) string {
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

// testFederationMember returns a well-formed household.Member for tests that
// need an authenticated session.
func testFederationMember() *household.Member {
	return &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: household.NewHouseholdID(),
		DisplayName: "Adult",
		Role:        household.RoleAdult,
		Color:       household.ColorSage,
	}
}

// ---------------------------------------------------------------------------
// NewFederationHandlers
// ---------------------------------------------------------------------------

func TestNewFederationHandlers_PanicsOnMissingDependency(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newFederationTestSessionManager()
	codes := &fakeFederationCodeRepo{}
	cfg := newTestFederationConfig()

	tests := []struct {
		name string
		fn   func()
	}{
		{"nil session manager", func() { adapter.NewFederationHandlers(cfg, nil, codes, logger) }},
		{"nil code repository", func() { adapter.NewFederationHandlers(cfg, sm, nil, logger) }},
		{"nil logger", func() { adapter.NewFederationHandlers(cfg, sm, codes, nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("expected a panic, got none")
				}
			}()
			tt.fn()
		})
	}
}

func TestNewFederationHandlers_ConstructsWithAllDependencies(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newFederationTestSessionManager()
	codes := &fakeFederationCodeRepo{}

	h := adapter.NewFederationHandlers(newTestFederationConfig(), sm, codes, logger)
	if h == nil {
		t.Fatal("NewFederationHandlers returned nil with all dependencies present")
	}
}

// ---------------------------------------------------------------------------
// AC: "An unregistered client identifier or an unregistered redirect target
// is refused with a plain 400 before authentication is attempted — never a
// redirect."
// ---------------------------------------------------------------------------

func TestFederationAuthorize_UnregisteredClientOrRedirect_Returns400BeforeAuth(t *testing.T) {
	cfg := newTestFederationConfig()
	codes := &fakeFederationCodeRepo{}
	_, handler := buildFederationHandler(t, cfg, codes, &fakeHouseholdRepo{})

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
			req := httptest.NewRequest(http.MethodGet, federationAuthorizeURL("code", tt.clientID, tt.redirectURI, "s"), nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("Location = %q, want no redirect at all for an unregistered client/redirect", loc)
			}
			if len(codes.created) != 0 {
				t.Error("a code was persisted despite the unregistered client/redirect")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC (Implementation Plan): "a wrong response_type or missing state redirects
// back to the registered target with an RFC 6749 error parameter instead of
// rendering."
// ---------------------------------------------------------------------------

func TestFederationAuthorize_MalformedRequest_RedirectsToClientWithError(t *testing.T) {
	cfg := newTestFederationConfig()
	codes := &fakeFederationCodeRepo{}
	_, handler := buildFederationHandler(t, cfg, codes, &fakeHouseholdRepo{})

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
			req := httptest.NewRequest(http.MethodGet, federationAuthorizeURL(tt.responseType, cfg.ClientID, cfg.RedirectURL, tt.state), nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want 303; body: %s", rec.Code, rec.Body.String())
			}
			dest, err := url.Parse(rec.Header().Get("Location"))
			if err != nil {
				t.Fatalf("parse Location: %v", err)
			}
			if got := (&url.URL{Scheme: dest.Scheme, Host: dest.Host, Path: dest.Path}).String(); got != cfg.RedirectURL {
				t.Errorf("redirect target = %q, want %q", got, cfg.RedirectURL)
			}
			if got := dest.Query().Get("error"); got != tt.wantError {
				t.Errorf("error = %q, want %q", got, tt.wantError)
			}
			if len(codes.created) != 0 {
				t.Error("a code was persisted for a malformed request")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC: "A member without a session is sent through the ordinary /login flow
// with `next` set to this request's own URI."
// ---------------------------------------------------------------------------

func TestFederationAuthorize_NoSession_RedirectsToLogin(t *testing.T) {
	cfg := newTestFederationConfig()
	codes := &fakeFederationCodeRepo{}
	_, handler := buildFederationHandler(t, cfg, codes, &fakeHouseholdRepo{})

	authorizePath := federationAuthorizeURL("code", cfg.ClientID, cfg.RedirectURL, "xyz-state")
	req := httptest.NewRequest(http.MethodGet, authorizePath, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	wantLocation := "/login?next=" + url.QueryEscape(authorizePath)
	if got := rec.Header().Get("Location"); got != wantLocation {
		t.Errorf("Location = %q, want %q", got, wantLocation)
	}
	if len(codes.created) != 0 {
		t.Error("a code was persisted with no authenticated member")
	}
}

// ---------------------------------------------------------------------------
// AC: "A member with an active Nestova session is redirected back to the
// calling client with a code, without re-authenticating." Also asserts the
// persisted binding and that the raw code is hashed, never stored verbatim.
// ---------------------------------------------------------------------------

func TestFederationAuthorize_ActiveSession_RedirectsWithCodeAndPersistsHashedBinding(t *testing.T) {
	member := testFederationMember()
	hhRepo := &fakeHouseholdRepo{currentMember: member}
	cfg := newTestFederationConfig()
	codes := &fakeFederationCodeRepo{}
	sm, handler := buildFederationHandler(t, cfg, codes, hhRepo)
	cookies := seedFederationSession(t, sm, hhRepo, member.ID)

	authorizePath := federationAuthorizeURL("code", cfg.ClientID, cfg.RedirectURL, "xyz-state")
	req := httptest.NewRequest(http.MethodGet, authorizePath, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	dest, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
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

	if len(codes.created) != 1 {
		t.Fatalf("codes created = %d, want 1", len(codes.created))
	}
	stored := codes.created[0]
	if stored.MemberID != member.ID || stored.ClientID != cfg.ClientID || stored.RedirectURI != cfg.RedirectURL {
		t.Errorf("persisted code = %+v, want bound to member %s, client %q, redirect %q",
			stored, member.ID, cfg.ClientID, cfg.RedirectURL)
	}
	if stored.CodeHash != authdomain.HashAuthorizationCode(rawCode) {
		t.Error("persisted code hash does not match the issued raw code")
	}
	if stored.CodeHash == rawCode {
		t.Error("the raw code was persisted verbatim instead of hashed")
	}
	wantExpiry := time.Now().Add(authdomain.AuthorizationCodeTTL)
	if diff := wantExpiry.Sub(stored.ExpiresAt); diff < -5*time.Second || diff > 5*time.Second {
		t.Errorf("ExpiresAt = %v, want ~%v (AuthorizationCodeTTL from now)", stored.ExpiresAt, wantExpiry)
	}
}

// ---------------------------------------------------------------------------
// A persistence failure must not leak internal detail nor redirect anywhere.
// ---------------------------------------------------------------------------

func TestFederationAuthorize_CreateError_Returns500WithNoRedirect(t *testing.T) {
	member := testFederationMember()
	hhRepo := &fakeHouseholdRepo{currentMember: member}
	cfg := newTestFederationConfig()
	codes := &fakeFederationCodeRepo{createErr: errors.New("boom: persistence unavailable")}
	sm, handler := buildFederationHandler(t, cfg, codes, hhRepo)
	cookies := seedFederationSession(t, sm, hhRepo, member.ID)

	req := httptest.NewRequest(http.MethodGet, federationAuthorizeURL("code", cfg.ClientID, cfg.RedirectURL, "s"), nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "internal server error" {
		t.Errorf("body = %q, want %q (never leak the underlying persistence error)", got, "internal server error")
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q, want no redirect on a persistence failure", loc)
	}
}

// ---------------------------------------------------------------------------
// Defensive branches: Authorize's own doc notes that parsing the registered
// redirect URL is "defensive, not reachable in practice" because
// config.Load's startup validation normally guarantees RedirectURL parses,
// and redirect_uri is only ever compared to it for exact string equality.
// These tests bypass that startup validation (constructing FederationConfig
// directly, as a unit test legitimately can) to reach url.Parse's own
// failure branch in both Authorize and redirectWithError, so the defensive
// code itself is proven rather than left as dead-looking coverage.
// ---------------------------------------------------------------------------

const malformedRegisteredRedirect = "https://nestorage.example.ts.net/%zz"

func TestFederationAuthorize_UnparsableRegisteredRedirect_ReturnsDefensive500AfterCreate(t *testing.T) {
	member := testFederationMember()
	hhRepo := &fakeHouseholdRepo{currentMember: member}
	cfg := config.FederationConfig{
		ClientID:     "nestorage-household-1",
		ClientSecret: "irrelevant-to-authorize-32-bytes",
		RedirectURL:  malformedRegisteredRedirect,
	}
	codes := &fakeFederationCodeRepo{}
	sm, handler := buildFederationHandler(t, cfg, codes, hhRepo)
	cookies := seedFederationSession(t, sm, hhRepo, member.ID)

	req := httptest.NewRequest(http.MethodGet, federationAuthorizeURL("code", cfg.ClientID, malformedRegisteredRedirect, "s"), nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
	}
	// The code is already persisted by the time the redirect URL fails to
	// parse (Create happens first) — this documents that ordering rather
	// than asserting it is desirable.
	if len(codes.created) != 1 {
		t.Errorf("codes created = %d, want 1 (Create runs before the parse failure)", len(codes.created))
	}
}

func TestFederationAuthorize_UnparsableRegisteredRedirect_RedirectWithErrorReturnsDefensive500(t *testing.T) {
	cfg := config.FederationConfig{
		ClientID:     "nestorage-household-1",
		ClientSecret: "irrelevant-to-authorize-32-bytes",
		RedirectURL:  malformedRegisteredRedirect,
	}
	codes := &fakeFederationCodeRepo{}
	_, handler := buildFederationHandler(t, cfg, codes, &fakeHouseholdRepo{})

	// An unsupported response_type takes the redirectWithError path before
	// authentication is even consulted, so no session/member setup is
	// needed to reach it.
	req := httptest.NewRequest(http.MethodGet, federationAuthorizeURL("token", cfg.ClientID, malformedRegisteredRedirect, "s"), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
	}
}
