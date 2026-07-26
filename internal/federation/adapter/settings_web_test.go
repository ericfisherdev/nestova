package adapter_test

// ---------------------------------------------------------------------------
// Package-level tests for the /settings page's federation section handlers
// (NSTR-106): FederationWebHandlers.SectionView/Attach/Detach.
//
// cmd/server/federation_settings_handler_test.go already exercises this
// section end-to-end through the real composition root (recomposing the
// whole settings page). But Go's default coverage instrumentation (-cover
// with no -coverpkg) only attributes coverage to the package under test:
// since settings_web.go lives in this package (internal/federation/adapter)
// and the cmd/server tests run as package main, none of that exercise
// counts toward settings_web.go's own coverage — mirrors the identical
// rationale documented in internal/auth/adapter/federation_http_test.go for
// FederationHandlers.Authorize. These tests cover the same handlers
// directly, in-package, hermetically (no database — mirrors
// onboarding_test.go's fakeHouseholdRepo pattern), so the coverage actually
// lands where Sonar looks for it.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	"github.com/ericfisherdev/nestova/internal/federation/adapter"
	"github.com/ericfisherdev/nestova/internal/federation/app"
	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	"github.com/ericfisherdev/nestova/web/components"
)

// discardLogger returns a logger that writes nowhere, for tests that don't
// assert on log output.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testCipher returns a fixed-key *crypto.Cipher, mirroring federation/app's
// own test helper of the same name (a distinct package, so no collision).
func testCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

// stubLinkRepo is an in-memory domain.InstanceLinkRepository, mirroring
// federation/app's own fakeLinkRepo (a distinct package, so no collision).
// createErr/deleteErr, when set, override the normal conflict-checking
// behavior so tests can force LinkService.Attach/Detach into their
// wrapped-generic-error branches.
type stubLinkRepo struct {
	byHousehold map[household.HouseholdID]*domain.InstanceLink
	byBaseURL   map[string]*domain.InstanceLink
	createErr   error
	deleteErr   error
}

func newStubLinkRepo() *stubLinkRepo {
	return &stubLinkRepo{
		byHousehold: map[household.HouseholdID]*domain.InstanceLink{},
		byBaseURL:   map[string]*domain.InstanceLink{},
	}
}

func (r *stubLinkRepo) Create(_ context.Context, link *domain.InstanceLink) error {
	if r.createErr != nil {
		return r.createErr
	}
	if _, ok := r.byHousehold[link.HouseholdID]; ok {
		return domain.ErrHouseholdAlreadyBound
	}
	if _, ok := r.byBaseURL[link.BaseURL]; ok {
		return domain.ErrInstanceAlreadyBound
	}
	link.AttachedAt = time.Now()
	r.byHousehold[link.HouseholdID] = link
	r.byBaseURL[link.BaseURL] = link
	return nil
}

func (r *stubLinkRepo) GetByHousehold(_ context.Context, id household.HouseholdID) (*domain.InstanceLink, error) {
	if link, ok := r.byHousehold[id]; ok {
		return link, nil
	}
	return nil, domain.ErrLinkNotFound
}

func (r *stubLinkRepo) GetByBaseURL(_ context.Context, baseURL string) (*domain.InstanceLink, error) {
	if link, ok := r.byBaseURL[baseURL]; ok {
		return link, nil
	}
	return nil, domain.ErrLinkNotFound
}

func (r *stubLinkRepo) DeleteByHousehold(_ context.Context, id household.HouseholdID) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	if link, ok := r.byHousehold[id]; ok {
		delete(r.byHousehold, id)
		delete(r.byBaseURL, link.BaseURL)
	}
	return nil
}

var _ domain.InstanceLinkRepository = (*stubLinkRepo)(nil)

// stubVerifier is a scripted instanceVerifier (structurally satisfying
// federation/app's unexported instanceVerifier interface): it records every
// call's arguments and returns err, so tests can assert both what Attach
// passed through and that a failure never reaches the repository at all.
type stubVerifier struct {
	err     error
	calls   int
	seenKey string
	seenHH  household.HouseholdID
	seenURL string
}

func (v *stubVerifier) Verify(_ context.Context, baseURL, apiKey string, householdID household.HouseholdID) error {
	v.calls++
	v.seenURL = baseURL
	v.seenKey = apiKey
	v.seenHH = householdID
	return v.err
}

// stubHouseholdRepo is a minimal household.HouseholdRepository, mirroring
// auth/adapter's onboarding_test.go fakeHouseholdRepo: currentMember (when
// set) is what authadapter.Authenticate's middleware resolves via GetMember,
// letting a seeded session inject a real *household.Member into the request
// context exactly as production does.
type stubHouseholdRepo struct {
	currentMember *household.Member
}

func (r *stubHouseholdRepo) HasAnyHousehold(_ context.Context) (bool, error) { return true, nil }

func (r *stubHouseholdRepo) CreateHousehold(_ context.Context, _ *household.Household) error {
	return nil
}

func (r *stubHouseholdRepo) GetHousehold(_ context.Context, _ household.HouseholdID) (*household.Household, error) {
	return nil, household.ErrHouseholdNotFound
}

func (r *stubHouseholdRepo) AddMember(_ context.Context, _ *household.Member) error { return nil }

func (r *stubHouseholdRepo) GetMember(_ context.Context, _ household.MemberID) (*household.Member, error) {
	if r.currentMember != nil {
		return r.currentMember, nil
	}
	return nil, household.ErrMemberNotFound
}

func (r *stubHouseholdRepo) ListMembers(_ context.Context, _ household.HouseholdID) ([]*household.Member, error) {
	return nil, nil
}

var _ household.HouseholdRepository = (*stubHouseholdRepo)(nil)

func newOwner() *household.Member {
	return &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: household.NewHouseholdID(),
		DisplayName: "Owner",
		Role:        household.RoleOwner,
		Color:       household.ColorSage,
	}
}

func newAdultInHousehold(hh household.HouseholdID) *household.Member {
	return &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: hh,
		DisplayName: "Adult",
		Role:        household.RoleAdult,
		Color:       household.ColorClay,
	}
}

func newSettingsSessionManager() *scs.SessionManager {
	sm := scs.New()
	sm.Store = memstore.New()
	sm.Lifetime = 1 * time.Hour
	sm.Cookie.Secure = false
	return sm
}

func mustLinkService(t *testing.T, repo domain.InstanceLinkRepository, verifier *stubVerifier) *app.LinkService {
	t.Helper()
	svc, err := app.NewLinkService(repo, testCipher(t), verifier, discardLogger())
	if err != nil {
		t.Fatalf("NewLinkService: %v", err)
	}
	return svc
}

// attachResult captures Attach's full return tuple. Attach — unlike a plain
// http.HandlerFunc — never writes a response on its ok=true path (the
// composition root, cmd/server/home.go, decides success-redirect vs.
// inline-error recomposition from this exact tuple), so asserting on it
// directly is what "real behavior" means at this layer.
type attachResult struct {
	member *household.Member
	errMsg string
	status int
	ok     bool
}

type detachResult struct {
	member *household.Member
	ok     bool
}

// webTestHarness wires FederationWebHandlers behind the real Authenticate
// middleware, exactly as the composition root does, so CurrentMember/
// requireOwner resolve exactly as they do in production. GET /seed sets the
// session's authenticated member (when memberID is non-empty) and returns
// the session's CSRF token via a test-only response header, avoiding a
// second round trip just to read it back.
type webTestHarness struct {
	sm      *scs.SessionManager
	handler http.Handler
	attach  attachResult
	detach  detachResult
}

func newWebTestHarness(hhRepo household.HouseholdRepository, h *adapter.FederationWebHandlers, sm *scs.SessionManager) *webTestHarness {
	th := &webTestHarness{sm: sm}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /seed", func(w http.ResponseWriter, r *http.Request) {
		if id := r.URL.Query().Get("member_id"); id != "" {
			sm.Put(r.Context(), "member_id", id)
		}
		w.Header().Set("X-Test-CSRF", authadapter.GetCSRFToken(r.Context(), sm))
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /attach", func(w http.ResponseWriter, r *http.Request) {
		th.attach = attachResult{}
		th.attach.member, th.attach.errMsg, th.attach.status, th.attach.ok = h.Attach(w, r)
	})
	mux.HandleFunc("POST /detach", func(w http.ResponseWriter, r *http.Request) {
		th.detach = detachResult{}
		th.detach.member, th.detach.ok = h.Detach(w, r)
	})
	th.handler = sm.LoadAndSave(authadapter.Authenticate(sm, hhRepo)(mux))
	return th
}

func (th *webTestHarness) do(method, path string, cookies []*http.Cookie, body string) *httptest.ResponseRecorder {
	var r io.Reader
	if method == http.MethodPost {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	th.handler.ServeHTTP(rec, req)
	return rec
}

// seed authenticates memberID into a fresh session and returns the cookies
// a subsequent request must carry, plus that session's CSRF token.
func (th *webTestHarness) seed(memberID string) ([]*http.Cookie, string) {
	rec := th.do(http.MethodGet, "/seed?member_id="+memberID, nil, "")
	return rec.Result().Cookies(), rec.Result().Header.Get("X-Test-CSRF")
}

// ---------------------------------------------------------------------------
// NewFederationWebHandlers
// ---------------------------------------------------------------------------

func TestNewFederationWebHandlers_PanicsOnMissingDependency(t *testing.T) {
	sm := newSettingsSessionManager()
	link := mustLinkService(t, newStubLinkRepo(), &stubVerifier{})
	logger := discardLogger()

	tests := []struct {
		name string
		fn   func()
	}{
		{"nil link service", func() { adapter.NewFederationWebHandlers(nil, sm, logger) }},
		{"nil session manager", func() { adapter.NewFederationWebHandlers(link, nil, logger) }},
		{"nil logger", func() { adapter.NewFederationWebHandlers(link, sm, nil) }},
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

// ---------------------------------------------------------------------------
// SectionView
// ---------------------------------------------------------------------------

func TestFederationWebHandlers_SectionView_HiddenFromNonOwner(t *testing.T) {
	sm := newSettingsSessionManager()
	h := adapter.NewFederationWebHandlers(mustLinkService(t, newStubLinkRepo(), &stubVerifier{}), sm, discardLogger())
	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("sm.Load: %v", err)
	}

	adult := newAdultInHousehold(household.NewHouseholdID())
	view, show, err := h.SectionView(ctx, adult, "")
	if err != nil {
		t.Fatalf("SectionView: %v", err)
	}
	if show {
		t.Error("show = true, want false for a non-owner adult")
	}
	if view != (components.FederationSettingsView{}) {
		t.Errorf("view = %+v, want the zero value for a hidden section", view)
	}
}

func TestFederationWebHandlers_SectionView_OwnerNotAttached(t *testing.T) {
	sm := newSettingsSessionManager()
	h := adapter.NewFederationWebHandlers(mustLinkService(t, newStubLinkRepo(), &stubVerifier{}), sm, discardLogger())
	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("sm.Load: %v", err)
	}

	owner := newOwner()
	view, show, err := h.SectionView(ctx, owner, "a prior inline error")
	if err != nil {
		t.Fatalf("SectionView: %v", err)
	}
	if !show {
		t.Error("show = false, want true for an owner")
	}
	if view.Attached {
		t.Error("Attached = true, want false when no link exists")
	}
	if view.ErrorMessage != "a prior inline error" {
		t.Errorf("ErrorMessage = %q, want the passed-through errMsg", view.ErrorMessage)
	}
	if view.CSRFToken == "" {
		t.Error("CSRFToken is empty, want a generated token")
	}
}

func TestFederationWebHandlers_SectionView_OwnerAttached(t *testing.T) {
	sm := newSettingsSessionManager()
	repo := newStubLinkRepo()
	h := adapter.NewFederationWebHandlers(mustLinkService(t, repo, &stubVerifier{}), sm, discardLogger())
	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("sm.Load: %v", err)
	}

	owner := newOwner()
	if err := repo.Create(ctx, &domain.InstanceLink{
		ID:          domain.NewInstanceLinkID(),
		HouseholdID: owner.HouseholdID,
		BaseURL:     "https://nestorage.example.ts.net",
		APIKeyEnc:   []byte("encrypted-bytes"),
	}); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	view, show, err := h.SectionView(ctx, owner, "")
	if err != nil {
		t.Fatalf("SectionView: %v", err)
	}
	if !show {
		t.Fatal("show = false, want true for an owner")
	}
	if !view.Attached {
		t.Error("Attached = false, want true")
	}
	if view.InstanceHost != "nestorage.example.ts.net" {
		t.Errorf("InstanceHost = %q, want %q", view.InstanceHost, "nestorage.example.ts.net")
	}
	if view.AttachedAtDisplay == "" {
		t.Error("AttachedAtDisplay is empty, want a formatted timestamp")
	}
	if view.CSRFToken == "" {
		t.Error("CSRFToken is empty, want a generated token")
	}
}

func TestFederationWebHandlers_SectionView_PropagatesUnexpectedError(t *testing.T) {
	sm := newSettingsSessionManager()
	repo := &erroringStatusRepo{stubLinkRepo: newStubLinkRepo()}
	h := adapter.NewFederationWebHandlers(mustLinkService(t, repo, &stubVerifier{}), sm, discardLogger())
	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("sm.Load: %v", err)
	}

	owner := newOwner()
	_, show, err := h.SectionView(ctx, owner, "")
	if err == nil {
		t.Fatal("SectionView() error = nil, want the underlying repository error")
	}
	if show {
		t.Error("show = true, want false when Status fails")
	}
}

// erroringStatusRepo wraps stubLinkRepo but always fails GetByHousehold with
// a non-ErrLinkNotFound error, exercising SectionView's error-propagation
// branch (distinct from the "not yet attached" ErrLinkNotFound branch).
type erroringStatusRepo struct {
	*stubLinkRepo
}

func (r *erroringStatusRepo) GetByHousehold(_ context.Context, _ household.HouseholdID) (*domain.InstanceLink, error) {
	return nil, errors.New("connection reset")
}

var _ domain.InstanceLinkRepository = (*erroringStatusRepo)(nil)

// ---------------------------------------------------------------------------
// Attach: owner-only gate, CSRF, malformed requests.
// ---------------------------------------------------------------------------

func TestFederationWebHandlers_Attach_Unauthenticated(t *testing.T) {
	sm := newSettingsSessionManager()
	h := adapter.NewFederationWebHandlers(mustLinkService(t, newStubLinkRepo(), &stubVerifier{}), sm, discardLogger())
	th := newWebTestHarness(&stubHouseholdRepo{}, h, sm)

	rec := th.do(http.MethodPost, "/attach", nil, "base_url=https://nestorage.example.ts.net&api_key=key")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if th.attach.ok {
		t.Error("ok = true, want false for an unauthenticated request")
	}
}

func TestFederationWebHandlers_Attach_ForbiddenForNonOwner(t *testing.T) {
	adult := newAdultInHousehold(household.NewHouseholdID())
	hhRepo := &stubHouseholdRepo{currentMember: adult}
	sm := newSettingsSessionManager()
	repo := newStubLinkRepo()
	h := adapter.NewFederationWebHandlers(mustLinkService(t, repo, &stubVerifier{}), sm, discardLogger())
	th := newWebTestHarness(hhRepo, h, sm)
	cookies, csrfToken := th.seed(adult.ID.String())

	rec := th.do(http.MethodPost, "/attach", cookies, "csrf_token="+csrfToken+"&base_url=https://nestorage.example.ts.net&api_key=key")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if th.attach.ok {
		t.Error("ok = true, want false for a non-owner adult")
	}
	if _, err := repo.GetByHousehold(context.Background(), adult.HouseholdID); err == nil {
		t.Error("a forbidden attach must not have stored a link")
	}
}

func TestFederationWebHandlers_Attach_MalformedFormIsBadRequest(t *testing.T) {
	owner := newOwner()
	hhRepo := &stubHouseholdRepo{currentMember: owner}
	sm := newSettingsSessionManager()
	h := adapter.NewFederationWebHandlers(mustLinkService(t, newStubLinkRepo(), &stubVerifier{}), sm, discardLogger())
	th := newWebTestHarness(hhRepo, h, sm)
	cookies, _ := th.seed(owner.ID.String())

	// Invalid percent-encoding makes url.ParseQuery (and so r.ParseForm)
	// return an error — the one ParseForm-failure path this handler has.
	rec := th.do(http.MethodPost, "/attach", cookies, "base_url=%zz")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if th.attach.ok {
		t.Error("ok = true, want false for a malformed form body")
	}
}

func TestFederationWebHandlers_Attach_CSRFMismatchIsForbidden(t *testing.T) {
	owner := newOwner()
	hhRepo := &stubHouseholdRepo{currentMember: owner}
	sm := newSettingsSessionManager()
	repo := newStubLinkRepo()
	h := adapter.NewFederationWebHandlers(mustLinkService(t, repo, &stubVerifier{}), sm, discardLogger())
	th := newWebTestHarness(hhRepo, h, sm)
	cookies, _ := th.seed(owner.ID.String())

	rec := th.do(http.MethodPost, "/attach", cookies, "csrf_token=wrong-token&base_url=https://nestorage.example.ts.net&api_key=key")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if th.attach.ok {
		t.Error("ok = true, want false for a CSRF mismatch")
	}
	if _, err := repo.GetByHousehold(context.Background(), owner.HouseholdID); err == nil {
		t.Error("a CSRF failure must not have stored a link")
	}
}

func TestFederationWebHandlers_Attach_MissingFields(t *testing.T) {
	owner := newOwner()
	hhRepo := &stubHouseholdRepo{currentMember: owner}
	sm := newSettingsSessionManager()
	verifier := &stubVerifier{}
	h := adapter.NewFederationWebHandlers(mustLinkService(t, newStubLinkRepo(), verifier), sm, discardLogger())
	th := newWebTestHarness(hhRepo, h, sm)
	cookies, csrfToken := th.seed(owner.ID.String())

	th.do(http.MethodPost, "/attach", cookies, "csrf_token="+csrfToken+"&base_url=&api_key=")
	if !th.attach.ok {
		t.Fatal("ok = false, want true (a soft failure the caller must recompose)")
	}
	if th.attach.status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", th.attach.status)
	}
	if !strings.Contains(th.attach.errMsg, "Enter both the Nestorage URL and its API key") {
		t.Errorf("errMsg = %q, want the blank-fields message", th.attach.errMsg)
	}
	if verifier.calls != 0 {
		t.Errorf("verifier.calls = %d, want 0 — blank fields must never reach Verify", verifier.calls)
	}
}

// ---------------------------------------------------------------------------
// Attach: the verification failure paths (AC 2) — an unreachable instance, a
// bad/rejected credential (including the 403 "wrong household scope" case
// NestorageClient.Verify folds into ErrInvalidAPIKey), and a household or
// instance already bound. Every one of these must store nothing.
// ---------------------------------------------------------------------------

func TestFederationWebHandlers_Attach_VerifyFailureStoresNothing(t *testing.T) {
	tests := []struct {
		name        string
		verifierErr error
		wantSubstr  string
	}{
		{
			name:        "invalid api key",
			verifierErr: domain.ErrInvalidAPIKey,
			wantSubstr:  "rejected that API key",
		},
		{
			// NestorageClient.Verify maps BOTH a 401 (wrong key) and a 403
			// (right key, wrong household scope — a "household mismatch")
			// to this same sentinel (see nestorage_client.go's Verify doc),
			// so this case doubles as the household-mismatch scenario.
			name:        "instance unreachable",
			verifierErr: domain.ErrInstanceUnreachable,
			wantSubstr:  "Could not reach",
		},
		{
			name:        "invalid base url",
			verifierErr: domain.ErrInvalidBaseURL,
			wantSubstr:  "Enter a valid Nestorage URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner := newOwner()
			hhRepo := &stubHouseholdRepo{currentMember: owner}
			sm := newSettingsSessionManager()
			repo := newStubLinkRepo()
			verifier := &stubVerifier{err: tt.verifierErr}
			h := adapter.NewFederationWebHandlers(mustLinkService(t, repo, verifier), sm, discardLogger())
			th := newWebTestHarness(hhRepo, h, sm)
			cookies, csrfToken := th.seed(owner.ID.String())

			const rawKey = "super-secret-nestorage-key"
			th.do(http.MethodPost, "/attach", cookies,
				"csrf_token="+csrfToken+"&base_url=https://nestorage.example.ts.net&api_key="+rawKey)

			if !th.attach.ok {
				t.Fatal("ok = false, want true (a soft failure the caller must recompose)")
			}
			if th.attach.status != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422", th.attach.status)
			}
			if !strings.Contains(th.attach.errMsg, tt.wantSubstr) {
				t.Errorf("errMsg = %q, want it to contain %q", th.attach.errMsg, tt.wantSubstr)
			}
			if strings.Contains(th.attach.errMsg, rawKey) {
				t.Error("errMsg contains the raw api key")
			}
			if _, err := repo.GetByHousehold(context.Background(), owner.HouseholdID); err == nil {
				t.Error("a failed verify must not have stored a link")
			}
		})
	}
}

func TestFederationWebHandlers_Attach_HouseholdAlreadyBound(t *testing.T) {
	owner := newOwner()
	hhRepo := &stubHouseholdRepo{currentMember: owner}
	sm := newSettingsSessionManager()
	repo := newStubLinkRepo()
	h := adapter.NewFederationWebHandlers(mustLinkService(t, repo, &stubVerifier{}), sm, discardLogger())
	th := newWebTestHarness(hhRepo, h, sm)
	cookies, csrfToken := th.seed(owner.ID.String())

	th.do(http.MethodPost, "/attach", cookies, "csrf_token="+csrfToken+"&base_url=https://one.example.ts.net&api_key=key-one")
	if !th.attach.ok || th.attach.status != http.StatusOK {
		t.Fatalf("first attach: ok=%v status=%d, want ok=true status=200", th.attach.ok, th.attach.status)
	}

	th.do(http.MethodPost, "/attach", cookies, "csrf_token="+csrfToken+"&base_url=https://two.example.ts.net&api_key=key-two")
	if th.attach.status != http.StatusUnprocessableEntity {
		t.Fatalf("second attach status = %d, want 422", th.attach.status)
	}
	if !strings.Contains(th.attach.errMsg, "already attached to a Nestorage instance") {
		t.Errorf("errMsg = %q, want the already-bound message", th.attach.errMsg)
	}
}

func TestFederationWebHandlers_Attach_InstanceBoundToAnotherHousehold(t *testing.T) {
	first := newOwner()
	sm := newSettingsSessionManager()
	repo := newStubLinkRepo()
	if err := repo.Create(context.Background(), &domain.InstanceLink{
		ID:          domain.NewInstanceLinkID(),
		HouseholdID: first.HouseholdID,
		BaseURL:     "https://shared.example.ts.net",
		APIKeyEnc:   []byte("encrypted-bytes"),
	}); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	second := newOwner()
	hhRepo := &stubHouseholdRepo{currentMember: second}
	h := adapter.NewFederationWebHandlers(mustLinkService(t, repo, &stubVerifier{}), sm, discardLogger())
	th := newWebTestHarness(hhRepo, h, sm)
	cookies, csrfToken := th.seed(second.ID.String())

	th.do(http.MethodPost, "/attach", cookies, "csrf_token="+csrfToken+"&base_url=https://shared.example.ts.net&api_key=key")
	if th.attach.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", th.attach.status)
	}
	if !strings.Contains(th.attach.errMsg, "already attached to a different household") {
		t.Errorf("errMsg = %q, want the instance-already-bound message", th.attach.errMsg)
	}
}

func TestFederationWebHandlers_Attach_UnexpectedRepositoryErrorLogsAndHidesDetail(t *testing.T) {
	owner := newOwner()
	hhRepo := &stubHouseholdRepo{currentMember: owner}
	sm := newSettingsSessionManager()
	repo := newStubLinkRepo()
	repo.createErr = errors.New("connection reset by peer")
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	h := adapter.NewFederationWebHandlers(mustLinkService(t, repo, &stubVerifier{}), sm, logger)
	th := newWebTestHarness(hhRepo, h, sm)
	cookies, csrfToken := th.seed(owner.ID.String())

	const rawKey = "super-secret-nestorage-key"
	th.do(http.MethodPost, "/attach", cookies, "csrf_token="+csrfToken+"&base_url=https://nestorage.example.ts.net&api_key="+rawKey)

	if th.attach.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", th.attach.status)
	}
	if th.attach.errMsg != "Something went wrong attaching that instance. Try again." {
		t.Errorf("errMsg = %q, want the generic fallback message", th.attach.errMsg)
	}
	if !strings.Contains(logBuf.String(), "connection reset by peer") {
		t.Error("the unexpected repository error was not logged")
	}
	if strings.Contains(logBuf.String(), rawKey) {
		t.Error("log output contains the raw api key")
	}
}

// ---------------------------------------------------------------------------
// Attach: the success path (AC 1/AC 6) — verified live, stored encrypted,
// never in plaintext anywhere observable.
// ---------------------------------------------------------------------------

func TestFederationWebHandlers_Attach_Success(t *testing.T) {
	owner := newOwner()
	hhRepo := &stubHouseholdRepo{currentMember: owner}
	sm := newSettingsSessionManager()
	repo := newStubLinkRepo()
	verifier := &stubVerifier{}
	h := adapter.NewFederationWebHandlers(mustLinkService(t, repo, verifier), sm, discardLogger())
	th := newWebTestHarness(hhRepo, h, sm)
	cookies, csrfToken := th.seed(owner.ID.String())

	const rawKey = "the-real-nestorage-key"
	th.do(http.MethodPost, "/attach", cookies,
		"csrf_token="+csrfToken+"&base_url=https://Nestorage.Example.ts.net/&api_key="+rawKey)

	if !th.attach.ok {
		t.Fatal("ok = false, want true on success")
	}
	if th.attach.status != http.StatusOK {
		t.Errorf("status = %d, want 200", th.attach.status)
	}
	if th.attach.errMsg != "" {
		t.Errorf("errMsg = %q, want empty on success", th.attach.errMsg)
	}
	if th.attach.member == nil || th.attach.member.ID != owner.ID {
		t.Error("member returned does not match the authenticated owner")
	}
	if verifier.calls != 1 {
		t.Fatalf("verifier.calls = %d, want 1", verifier.calls)
	}
	if verifier.seenKey != rawKey {
		t.Errorf("verifier saw key %q, want %q", verifier.seenKey, rawKey)
	}
	if verifier.seenHH != owner.HouseholdID {
		t.Errorf("verifier saw household %v, want %v", verifier.seenHH, owner.HouseholdID)
	}

	link, err := repo.GetByHousehold(context.Background(), owner.HouseholdID)
	if err != nil {
		t.Fatalf("GetByHousehold after attach: %v", err)
	}
	if link.BaseURL != "https://nestorage.example.ts.net" {
		t.Errorf("stored BaseURL = %q, want normalized", link.BaseURL)
	}
	if strings.Contains(string(link.APIKeyEnc), rawKey) {
		t.Error("stored APIKeyEnc contains the raw api key")
	}
}

// ---------------------------------------------------------------------------
// Detach: owner-only gate, CSRF, and the credential-clearing success path.
// ---------------------------------------------------------------------------

func TestFederationWebHandlers_Detach_Unauthenticated(t *testing.T) {
	sm := newSettingsSessionManager()
	h := adapter.NewFederationWebHandlers(mustLinkService(t, newStubLinkRepo(), &stubVerifier{}), sm, discardLogger())
	th := newWebTestHarness(&stubHouseholdRepo{}, h, sm)

	rec := th.do(http.MethodPost, "/detach", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if th.detach.ok {
		t.Error("ok = true, want false for an unauthenticated request")
	}
}

func TestFederationWebHandlers_Detach_ForbiddenForNonOwner(t *testing.T) {
	adult := newAdultInHousehold(household.NewHouseholdID())
	hhRepo := &stubHouseholdRepo{currentMember: adult}
	sm := newSettingsSessionManager()
	h := adapter.NewFederationWebHandlers(mustLinkService(t, newStubLinkRepo(), &stubVerifier{}), sm, discardLogger())
	th := newWebTestHarness(hhRepo, h, sm)
	cookies, csrfToken := th.seed(adult.ID.String())

	rec := th.do(http.MethodPost, "/detach", cookies, "csrf_token="+csrfToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if th.detach.ok {
		t.Error("ok = true, want false for a non-owner adult")
	}
}

func TestFederationWebHandlers_Detach_MalformedFormIsBadRequest(t *testing.T) {
	owner := newOwner()
	hhRepo := &stubHouseholdRepo{currentMember: owner}
	sm := newSettingsSessionManager()
	h := adapter.NewFederationWebHandlers(mustLinkService(t, newStubLinkRepo(), &stubVerifier{}), sm, discardLogger())
	th := newWebTestHarness(hhRepo, h, sm)
	cookies, _ := th.seed(owner.ID.String())

	rec := th.do(http.MethodPost, "/detach", cookies, "csrf_token=%zz")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if th.detach.ok {
		t.Error("ok = true, want false for a malformed form body")
	}
}

func TestFederationWebHandlers_Detach_CSRFMismatchIsForbidden(t *testing.T) {
	owner := newOwner()
	hhRepo := &stubHouseholdRepo{currentMember: owner}
	sm := newSettingsSessionManager()
	repo := newStubLinkRepo()
	h := adapter.NewFederationWebHandlers(mustLinkService(t, repo, &stubVerifier{}), sm, discardLogger())
	th := newWebTestHarness(hhRepo, h, sm)
	cookies, _ := th.seed(owner.ID.String())

	rec := th.do(http.MethodPost, "/detach", cookies, "csrf_token=wrong-token")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if th.detach.ok {
		t.Error("ok = true, want false for a CSRF mismatch")
	}
}

func TestFederationWebHandlers_Detach_ClearsStoredCredential(t *testing.T) {
	owner := newOwner()
	hhRepo := &stubHouseholdRepo{currentMember: owner}
	sm := newSettingsSessionManager()
	repo := newStubLinkRepo()
	h := adapter.NewFederationWebHandlers(mustLinkService(t, repo, &stubVerifier{}), sm, discardLogger())
	th := newWebTestHarness(hhRepo, h, sm)
	cookies, csrfToken := th.seed(owner.ID.String())

	th.do(http.MethodPost, "/attach", cookies, "csrf_token="+csrfToken+"&base_url=https://nestorage.example.ts.net&api_key=key")
	if !th.attach.ok || th.attach.status != http.StatusOK {
		t.Fatalf("attach precondition failed: ok=%v status=%d", th.attach.ok, th.attach.status)
	}

	th.do(http.MethodPost, "/detach", cookies, "csrf_token="+csrfToken)
	if !th.detach.ok {
		t.Fatal("ok = false, want true on a successful detach")
	}
	if th.detach.member == nil || th.detach.member.ID != owner.ID {
		t.Error("member returned does not match the authenticated owner")
	}
	if _, err := repo.GetByHousehold(context.Background(), owner.HouseholdID); !errors.Is(err, domain.ErrLinkNotFound) {
		t.Errorf("GetByHousehold after detach error = %v, want ErrLinkNotFound", err)
	}
}

func TestFederationWebHandlers_Detach_RepositoryErrorIsInternalServerError(t *testing.T) {
	owner := newOwner()
	hhRepo := &stubHouseholdRepo{currentMember: owner}
	sm := newSettingsSessionManager()
	repo := newStubLinkRepo()
	repo.deleteErr = errors.New("disk full")
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	h := adapter.NewFederationWebHandlers(mustLinkService(t, repo, &stubVerifier{}), sm, logger)
	th := newWebTestHarness(hhRepo, h, sm)
	cookies, csrfToken := th.seed(owner.ID.String())

	rec := th.do(http.MethodPost, "/detach", cookies, "csrf_token="+csrfToken)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if th.detach.ok {
		t.Error("ok = true, want false when the repository fails")
	}
	if !strings.Contains(logBuf.String(), "disk full") {
		t.Error("the detach repository error was not logged")
	}
}
