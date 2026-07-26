package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	federationadapter "github.com/ericfisherdev/nestova/internal/federation/adapter"
	federationapp "github.com/ericfisherdev/nestova/internal/federation/app"
	federationdomain "github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	kioskadapter "github.com/ericfisherdev/nestova/internal/kiosk/adapter"
	kioskapp "github.com/ericfisherdev/nestova/internal/kiosk/app"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	"github.com/ericfisherdev/nestova/internal/platform/crypto/cryptotest"
	"github.com/ericfisherdev/nestova/internal/platform/totp"
)

// ---------------------------------------------------------------------------
// Test harness for the federation instance-link settings section (NSTR-106).
// Deliberately separate from home_test.go's inert fakeInstanceLinkRepo/
// fakeInstanceVerifier, which many OTHER tests share and rely on staying
// inert (a no-op federation section that never actually binds anything).
// These fakes track real state so this file can exercise an actual
// attach/conflict/detach round trip over HTTP, mirroring
// notify_settings_handler_test.go's own statefulContactDirectory rationale.
// ---------------------------------------------------------------------------

// statefulInstanceLinkRepo is an in-memory federationdomain.
// InstanceLinkRepository keyed by household id, with a secondary index by
// base url — mirroring the two unique constraints migration 00038 enforces
// at the storage layer.
type statefulInstanceLinkRepo struct {
	byHousehold map[household.HouseholdID]*federationdomain.InstanceLink
	byBaseURL   map[string]*federationdomain.InstanceLink
}

func newStatefulInstanceLinkRepo() *statefulInstanceLinkRepo {
	return &statefulInstanceLinkRepo{
		byHousehold: make(map[household.HouseholdID]*federationdomain.InstanceLink),
		byBaseURL:   make(map[string]*federationdomain.InstanceLink),
	}
}

func (r *statefulInstanceLinkRepo) Create(_ context.Context, link *federationdomain.InstanceLink) error {
	if _, ok := r.byHousehold[link.HouseholdID]; ok {
		return federationdomain.ErrHouseholdAlreadyBound
	}
	if _, ok := r.byBaseURL[link.BaseURL]; ok {
		return federationdomain.ErrInstanceAlreadyBound
	}
	r.byHousehold[link.HouseholdID] = link
	r.byBaseURL[link.BaseURL] = link
	return nil
}

func (r *statefulInstanceLinkRepo) GetByHousehold(_ context.Context, id household.HouseholdID) (*federationdomain.InstanceLink, error) {
	if link, ok := r.byHousehold[id]; ok {
		return link, nil
	}
	return nil, federationdomain.ErrLinkNotFound
}

func (r *statefulInstanceLinkRepo) GetByBaseURL(_ context.Context, baseURL string) (*federationdomain.InstanceLink, error) {
	if link, ok := r.byBaseURL[baseURL]; ok {
		return link, nil
	}
	return nil, federationdomain.ErrLinkNotFound
}

func (r *statefulInstanceLinkRepo) DeleteByHousehold(_ context.Context, id household.HouseholdID) error {
	if link, ok := r.byHousehold[id]; ok {
		delete(r.byHousehold, id)
		delete(r.byBaseURL, link.BaseURL)
	}
	return nil
}

var _ federationdomain.InstanceLinkRepository = (*statefulInstanceLinkRepo)(nil)

// scriptedInstanceVerifier returns err (nil for success) from every Verify
// call and records the last presented key, so a test can assert Attach
// never persists the raw key anywhere it can be observed.
type scriptedInstanceVerifier struct {
	err     error
	calls   int
	seenKey string
}

func (v *scriptedInstanceVerifier) Verify(_ context.Context, _, apiKey string, _ household.HouseholdID) error {
	v.calls++
	v.seenKey = apiKey
	return v.err
}

// buildFederationSettingsTestHandler mirrors buildWebAuthnSettingsTestHandler's
// construction, but wires a REAL LinkService backed by
// statefulInstanceLinkRepo and verifier (unlike buildSettingsTestHandler,
// which always wires the no-op newTestFederationWebHandlers), returning the
// repo alongside the handler for direct seeding/assertions.
func buildFederationSettingsTestHandler(t *testing.T, hhRepo *multiMemberHouseholdRepo, verifier *scriptedInstanceVerifier) (http.Handler, *scs.SessionManager, *statefulInstanceLinkRepo) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	credRepo := newFakeMemberCredRepo()

	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	kioskSvc, err := kioskapp.NewKioskService(devices, codes, nil)
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	settingsHandlers := kioskadapter.NewSettingsWebHandlers(kioskSvc, sm, logger)

	mfaCipher, err := crypto.NewCipher([]byte("federation-test-harness-mfa-ciph"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	mfaService, err := authapp.NewMFAService(newFakeMFARepo(), mfaCipher, totp.NewProvider(), credRepo, hhRepo, cryptotest.Hasher(), logger)
	if err != nil {
		t.Fatalf("NewMFAService: %v", err)
	}
	mfaHandlers := authadapter.NewMFAWebHandlers(mfaService, hhRepo, sm, logger)

	fedCipher, err := crypto.NewCipher([]byte("federation-test-harness-lnk-ciph"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	repo := newStatefulInstanceLinkRepo()
	linkService, err := federationapp.NewLinkService(repo, fedCipher, verifier, logger)
	if err != nil {
		t.Fatalf("NewLinkService: %v", err)
	}
	federationHandlers := federationadapter.NewFederationWebHandlers(linkService, sm, logger)

	authHandlers := authadapter.NewHandlers(sm, authapp.New(credRepo, cryptotest.Hasher()), nil, nil, nil, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	registerSettingsPage(mux, logger, sm, hhRepo, settingsHandlers, mfaHandlers, mfaService, nil, nil, newTestNotifyWebHandlers(hhRepo, sm, logger), federationHandlers)

	handler := sm.LoadAndSave(authadapter.Authenticate(sm, hhRepo)(mux))
	return handler, sm, repo
}

// ---------------------------------------------------------------------------
// AC 7: only an administrator (RoleOwner) of the household may attach or
// detach.
// ---------------------------------------------------------------------------

func TestFederationSettings_SectionHiddenFromNonOwner(t *testing.T) {
	owner := settingsTestOwner()
	adult := settingsTestAdultInHousehold(owner.HouseholdID)
	hhRepo := newMultiMemberHouseholdRepo(owner, adult)
	handler, sm, _ := buildFederationSettingsTestHandler(t, hhRepo, &scriptedInstanceVerifier{})
	cookie, _ := seedAuthedSession(t, handler, sm, adult.ID.String())

	rec := doForm(t, handler, http.MethodGet, "/settings", cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings as a non-owner adult: status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Nestorage") {
		t.Error("a non-owner adult must not see the federation section")
	}
}

func TestFederationSettings_SectionVisibleToOwner(t *testing.T) {
	owner := settingsTestOwner()
	hhRepo := newMultiMemberHouseholdRepo(owner)
	handler, sm, _ := buildFederationSettingsTestHandler(t, hhRepo, &scriptedInstanceVerifier{})
	cookie, _ := seedAuthedSession(t, handler, sm, owner.ID.String())

	rec := doForm(t, handler, http.MethodGet, "/settings", cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings as the owner: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Nestorage") {
		t.Error("the owner must see the federation section")
	}
	if !strings.Contains(rec.Body.String(), "Attach instance") {
		t.Error("an unattached owner must see the attach form")
	}
}

func TestFederationSettings_Attach_ForbiddenForNonOwner(t *testing.T) {
	owner := settingsTestOwner()
	adult := settingsTestAdultInHousehold(owner.HouseholdID)
	hhRepo := newMultiMemberHouseholdRepo(owner, adult)
	handler, sm, repo := buildFederationSettingsTestHandler(t, hhRepo, &scriptedInstanceVerifier{})
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/federation/attach", cookie,
		"csrf_token="+csrfToken+"&base_url=https://nestorage.example.ts.net&api_key=some-key")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /settings/federation/attach as a non-owner adult: status = %d, want 403", rec.Code)
	}
	if _, err := repo.GetByHousehold(context.Background(), owner.HouseholdID); err == nil {
		t.Error("a forbidden attach attempt must not have stored a link")
	}
}

func TestFederationSettings_Detach_ForbiddenForNonOwner(t *testing.T) {
	owner := settingsTestOwner()
	adult := settingsTestAdultInHousehold(owner.HouseholdID)
	hhRepo := newMultiMemberHouseholdRepo(owner, adult)
	handler, sm, _ := buildFederationSettingsTestHandler(t, hhRepo, &scriptedInstanceVerifier{})
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/federation/detach", cookie, "csrf_token="+csrfToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /settings/federation/detach as a non-owner adult: status = %d, want 403", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// AC 1 + AC 6: verified before storing, and success leads into
// reconciliation without provisioning or reconciling anything itself.
// ---------------------------------------------------------------------------

func TestFederationSettings_Attach_Success_RedirectsToReconcile(t *testing.T) {
	owner := settingsTestOwner()
	hhRepo := newMultiMemberHouseholdRepo(owner)
	verifier := &scriptedInstanceVerifier{}
	handler, sm, repo := buildFederationSettingsTestHandler(t, hhRepo, verifier)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, owner.ID.String())

	req := doForm(t, handler, http.MethodPost, "/settings/federation/attach", cookie,
		"csrf_token="+csrfToken+"&base_url=https://Nestorage.Example.ts.net/&api_key=the-real-key")
	if req.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings/federation/attach (valid): status = %d, want 303; body: %s", req.Code, req.Body.String())
	}
	if loc := req.Header().Get("Location"); loc != "/settings/federation/reconcile" {
		t.Errorf("Location = %q, want /settings/federation/reconcile", loc)
	}
	if verifier.seenKey != "the-real-key" {
		t.Errorf("verifier saw key %q, want %q", verifier.seenKey, "the-real-key")
	}

	link, err := repo.GetByHousehold(context.Background(), owner.HouseholdID)
	if err != nil {
		t.Fatalf("GetByHousehold after attach: %v", err)
	}
	if link.BaseURL != "https://nestorage.example.ts.net" {
		t.Errorf("stored BaseURL = %q, want normalized", link.BaseURL)
	}
	if strings.Contains(string(link.APIKeyEnc), "the-real-key") {
		t.Error("stored APIKeyEnc contains the raw api key")
	}
}

func TestFederationSettings_Attach_WrongKey_ShowsInlineErrorAndStoresNothing(t *testing.T) {
	owner := settingsTestOwner()
	hhRepo := newMultiMemberHouseholdRepo(owner)
	verifier := &scriptedInstanceVerifier{err: federationdomain.ErrInvalidAPIKey}
	handler, sm, repo := buildFederationSettingsTestHandler(t, hhRepo, verifier)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, owner.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/federation/attach", cookie,
		"csrf_token="+csrfToken+"&base_url=https://nestorage.example.ts.net&api_key=wrong-key")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /settings/federation/attach (wrong key): status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rejected that API key") {
		t.Errorf("response missing the wrong-key inline message: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "wrong-key") {
		t.Error("response body echoes the rejected api key")
	}
	if _, err := repo.GetByHousehold(context.Background(), owner.HouseholdID); err == nil {
		t.Error("a failed verify must not have stored a link")
	}
}

func TestFederationSettings_Attach_Unreachable_ShowsInlineError(t *testing.T) {
	owner := settingsTestOwner()
	hhRepo := newMultiMemberHouseholdRepo(owner)
	verifier := &scriptedInstanceVerifier{err: federationdomain.ErrInstanceUnreachable}
	handler, sm, repo := buildFederationSettingsTestHandler(t, hhRepo, verifier)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, owner.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/federation/attach", cookie,
		"csrf_token="+csrfToken+"&base_url=https://nestorage.example.ts.net&api_key=some-key")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /settings/federation/attach (unreachable): status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Could not reach") {
		t.Errorf("response missing the unreachable inline message: %s", rec.Body.String())
	}
	if _, err := repo.GetByHousehold(context.Background(), owner.HouseholdID); err == nil {
		t.Error("an unreachable instance must not have stored a link")
	}
}

func TestFederationSettings_Attach_MissingFields_ShowsInlineError(t *testing.T) {
	owner := settingsTestOwner()
	hhRepo := newMultiMemberHouseholdRepo(owner)
	verifier := &scriptedInstanceVerifier{}
	handler, sm, _ := buildFederationSettingsTestHandler(t, hhRepo, verifier)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, owner.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/federation/attach", cookie, "csrf_token="+csrfToken+"&base_url=&api_key=")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /settings/federation/attach (blank fields): status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Enter both the Nestorage URL and its API key") {
		t.Errorf("response missing the blank-fields inline message: %s", rec.Body.String())
	}
	if verifier.calls != 0 {
		t.Errorf("verifier.calls = %d, want 0 — blank fields must never reach Verify", verifier.calls)
	}
}

func TestFederationSettings_Attach_CSRFFailure(t *testing.T) {
	owner := settingsTestOwner()
	hhRepo := newMultiMemberHouseholdRepo(owner)
	handler, sm, repo := buildFederationSettingsTestHandler(t, hhRepo, &scriptedInstanceVerifier{})
	cookie, _ := seedAuthedSession(t, handler, sm, owner.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/federation/attach", cookie,
		"csrf_token=wrong-token&base_url=https://nestorage.example.ts.net&api_key=some-key")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /settings/federation/attach (bad CSRF): status = %d, want 403", rec.Code)
	}
	if _, err := repo.GetByHousehold(context.Background(), owner.HouseholdID); err == nil {
		t.Error("a CSRF failure must not have stored a link")
	}
}

// ---------------------------------------------------------------------------
// AC 4: the binding records which household is attached to which instance,
// and a second or conflicting binding is refused.
// ---------------------------------------------------------------------------

func TestFederationSettings_Attach_SecondBindingForHouseholdRefused(t *testing.T) {
	owner := settingsTestOwner()
	hhRepo := newMultiMemberHouseholdRepo(owner)
	verifier := &scriptedInstanceVerifier{}
	handler, sm, _ := buildFederationSettingsTestHandler(t, hhRepo, verifier)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, owner.ID.String())

	first := doForm(t, handler, http.MethodPost, "/settings/federation/attach", cookie,
		"csrf_token="+csrfToken+"&base_url=https://one.example.ts.net&api_key=key-one")
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first attach: status = %d, want 303; body: %s", first.Code, first.Body.String())
	}

	second := doForm(t, handler, http.MethodPost, "/settings/federation/attach", cookie,
		"csrf_token="+csrfToken+"&base_url=https://two.example.ts.net&api_key=key-two")
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("second attach: status = %d, want 422; body: %s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "already attached to a Nestorage instance") {
		t.Errorf("response missing the already-bound inline message: %s", second.Body.String())
	}
}

// ---------------------------------------------------------------------------
// AC 5: the connection state is visible, and detaching clears the stored
// credential.
// ---------------------------------------------------------------------------

func TestFederationSettings_Detach_ClearsCredential(t *testing.T) {
	owner := settingsTestOwner()
	hhRepo := newMultiMemberHouseholdRepo(owner)
	handler, sm, repo := buildFederationSettingsTestHandler(t, hhRepo, &scriptedInstanceVerifier{})
	cookie, csrfToken := seedAuthedSession(t, handler, sm, owner.ID.String())

	// A host that shares no substring with the attach form's own
	// placeholder text ("nestorage.example.ts.net"), so "still present
	// after detach" cannot be a false positive from the empty form itself.
	const attachedHost = "mybins.example.ts.net"
	doForm(t, handler, http.MethodPost, "/settings/federation/attach", cookie,
		"csrf_token="+csrfToken+"&base_url=https://"+attachedHost+"&api_key=some-key")

	statusRec := doForm(t, handler, http.MethodGet, "/settings", cookie, "")
	if !strings.Contains(statusRec.Body.String(), attachedHost) {
		t.Fatalf("attached settings page missing the instance host: %s", statusRec.Body.String())
	}

	detachRec := doForm(t, handler, http.MethodPost, "/settings/federation/detach", cookie, "csrf_token="+csrfToken)
	if detachRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings/federation/detach: status = %d, want 303; body: %s", detachRec.Code, detachRec.Body.String())
	}
	if _, err := repo.GetByHousehold(context.Background(), owner.HouseholdID); err == nil {
		t.Error("Detach must have cleared the stored link")
	}

	afterRec := doForm(t, handler, http.MethodGet, "/settings", cookie, "")
	if strings.Contains(afterRec.Body.String(), attachedHost) {
		t.Error("settings page still shows the instance host after detach")
	}
	if !strings.Contains(afterRec.Body.String(), "Attach instance") {
		t.Error("settings page must show the attach form again after detach")
	}
}
