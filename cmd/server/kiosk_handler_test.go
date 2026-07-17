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

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	calendarapp "github.com/ericfisherdev/nestova/internal/calendar/app"
	calendardomain "github.com/ericfisherdev/nestova/internal/calendar/domain"
	deeplinkapp "github.com/ericfisherdev/nestova/internal/deeplink/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	kioskadapter "github.com/ericfisherdev/nestova/internal/kiosk/adapter"
	kioskapp "github.com/ericfisherdev/nestova/internal/kiosk/app"
	kioskdomain "github.com/ericfisherdev/nestova/internal/kiosk/domain"
	mealsapp "github.com/ericfisherdev/nestova/internal/meals/app"
	mediaapp "github.com/ericfisherdev/nestova/internal/media/app"
	subscriptionsdomain "github.com/ericfisherdev/nestova/internal/subscriptions/domain"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
	trackingapp "github.com/ericfisherdev/nestova/internal/tracking/app"
	trackingdomain "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// ---------------------------------------------------------------------------
// Fakes local to the kiosk test harness (the calendar unified view's narrow
// ports have no existing cmd/server fakes to reuse — /calendar isn't exercised
// at this layer elsewhere).
// ---------------------------------------------------------------------------

type fakeExternalEventLister struct{}

func (fakeExternalEventLister) ListByHouseholdRange(context.Context, household.HouseholdID, time.Time, time.Time) ([]*calendardomain.ExternalEvent, error) {
	return nil, nil
}

type fakeSubscriptionLister struct{}

func (fakeSubscriptionLister) ListActiveByHousehold(context.Context, household.HouseholdID) ([]*subscriptionsdomain.Subscription, error) {
	return nil, nil
}

// fakeKioskDeviceRepo is an in-memory domain.KioskDeviceRepository, mirroring
// internal/kiosk/app's own test fake (duplicated rather than imported: that
// one lives in an internal _test.go file and is not exported across packages).
type fakeKioskDeviceRepo struct {
	byID map[kioskdomain.KioskDeviceID]*kioskdomain.KioskDevice
}

func newFakeKioskDeviceRepo() *fakeKioskDeviceRepo {
	return &fakeKioskDeviceRepo{byID: make(map[kioskdomain.KioskDeviceID]*kioskdomain.KioskDevice)}
}

func (f *fakeKioskDeviceRepo) Create(_ context.Context, device *kioskdomain.KioskDevice) error {
	device.CreatedAt = time.Now()
	cp := *device
	f.byID[device.ID] = &cp
	return nil
}

func (f *fakeKioskDeviceRepo) GetByTokenHash(_ context.Context, tokenHash string) (*kioskdomain.KioskDevice, error) {
	for _, d := range f.byID {
		if d.TokenHash == tokenHash {
			cp := *d
			return &cp, nil
		}
	}
	return nil, kioskdomain.ErrKioskDeviceNotFound
}

func (f *fakeKioskDeviceRepo) Revoke(_ context.Context, householdID household.HouseholdID, id kioskdomain.KioskDeviceID, revokedAt time.Time) error {
	d, ok := f.byID[id]
	if !ok || d.HouseholdID != householdID || d.RevokedAt != nil {
		return kioskdomain.ErrKioskDeviceNotFound
	}
	d.RevokedAt = &revokedAt
	return nil
}

func (f *fakeKioskDeviceRepo) ListByHousehold(_ context.Context, householdID household.HouseholdID) ([]*kioskdomain.KioskDevice, error) {
	var out []*kioskdomain.KioskDevice
	for _, d := range f.byID {
		if d.HouseholdID == householdID {
			cp := *d
			out = append(out, &cp)
		}
	}
	return out, nil
}

// fakeActivationCodeRepo is an in-memory domain.ActivationCodeRepository,
// mirroring internal/kiosk/app's own test fake (duplicated for the same
// cross-package reason as fakeKioskDeviceRepo above). Redeem revokes/inserts
// directly against the paired fakeKioskDeviceRepo, modeling the real
// adapter's atomic contract closely enough for these HTTP-layer tests; true
// rollback-on-failure atomicity is covered by the gated adapter test.
type fakeActivationCodeRepo struct {
	byHash  map[string]*kioskdomain.ActivationCode
	devices *fakeKioskDeviceRepo
}

func newFakeActivationCodeRepo(devices *fakeKioskDeviceRepo) *fakeActivationCodeRepo {
	return &fakeActivationCodeRepo{byHash: make(map[string]*kioskdomain.ActivationCode), devices: devices}
}

func (f *fakeActivationCodeRepo) Create(_ context.Context, code *kioskdomain.ActivationCode) error {
	code.CreatedAt = time.Now()
	cp := *code
	f.byHash[code.CodeHash] = &cp
	return nil
}

func (f *fakeActivationCodeRepo) Redeem(_ context.Context, codeHash string, now time.Time, device *kioskdomain.KioskDevice) error {
	code, ok := f.byHash[codeHash]
	if !ok {
		return kioskdomain.ErrActivationCodeNotFound
	}
	if code.UsedAt != nil {
		return kioskdomain.ErrActivationCodeUsed
	}
	if !now.Before(code.ExpiresAt) {
		return kioskdomain.ErrActivationCodeExpired
	}
	usedAt := now
	code.UsedAt = &usedAt

	for _, d := range f.devices.byID {
		if d.HouseholdID == code.HouseholdID && d.RevokedAt == nil {
			revokedAt := now
			d.RevokedAt = &revokedAt
		}
	}
	device.HouseholdID = code.HouseholdID
	device.Name = code.Name
	return f.devices.Create(context.Background(), device)
}

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

// kioskTestPublicBaseURL is the PUBLIC_BASE_URL buildKioskTestHandler wires
// every test with (NES-129), so kiosk QR generation is exercised against a
// real configured origin rather than always falling back to whatever Host
// the test's httptest request happens to carry — the fallback path has its
// own dedicated coverage in internal/kiosk/adapter's white-box
// deepLinkURL tests.
const kioskTestPublicBaseURL = "https://kiosk.test"

// kioskFakes bundles the fakes a kiosk test seeds and asserts against.
type kioskFakes struct {
	devices  *fakeKioskDeviceRepo
	codes    *fakeActivationCodeRepo
	shopping *fakeShoppingRepo
}

// buildKioskTestHandler wires the full /kiosk/* and /settings route surface
// with in-memory fakes, mirroring runServer's composition in main.go closely
// enough to exercise the real middleware chain (session + member auth + kiosk
// device auth) and the real route gating — no database required. rewards is
// an optional (variadic, so every existing call site is unaffected) override
// for the reward repository; when omitted it defaults to an empty
// &configurableRewardRepo{}, matching every test that does not care about
// the kiosk's rewards section.
func buildKioskTestHandler(t *testing.T, member *household.Member, rewards ...tasksdomain.RewardRepository) (http.Handler, *scs.SessionManager, *kioskapp.KioskService, *kioskFakes) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	householdRepo := authedHouseholdRepo{member: member}

	var rewardRepo tasksdomain.RewardRepository = &configurableRewardRepo{}
	if len(rewards) > 0 {
		rewardRepo = rewards[0]
	}

	recurringRepo := fakeRecurringTaskRepo{}
	instanceRepo := &fakeTaskInstanceRepo{}

	unifiedCalendar, err := calendarapp.NewUnifiedCalendarService(
		fakeExternalEventLister{}, instanceRepo, recurringRepo, fakeSubscriptionLister{}, householdRepo, logger,
	)
	if err != nil {
		t.Fatalf("NewUnifiedCalendarService: %v", err)
	}

	recipeRepo := newMealsRecipeRepo()
	planRepo := newMealsPlanRepo()
	plannerSvc, err := mealsapp.NewPlannerService(planRepo, recipeRepo)
	if err != nil {
		t.Fatalf("NewPlannerService: %v", err)
	}

	shoppingRepo := &fakeShoppingRepo{}
	shoppingSvc, err := trackingapp.NewShoppingListService(shoppingRepo)
	if err != nil {
		t.Fatalf("NewShoppingListService: %v", err)
	}
	ingredientCatalog := newFakeIngredientCatalog()

	albumRepo := newFakeMediaAlbumRepo()
	photoRepo := newFakeMediaPhotoRepo()
	albumPhotoRepo := &fakeMediaAlbumPhotoRepo{}
	albumSvc, err := mediaapp.NewAlbumService(albumRepo, photoRepo, albumPhotoRepo)
	if err != nil {
		t.Fatalf("NewAlbumService: %v", err)
	}
	photoSvc, err := mediaapp.NewPhotoService(&fakeMediaStore{}, fakeMediaExif{}, photoRepo)
	if err != nil {
		t.Fatalf("NewPhotoService: %v", err)
	}

	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	kioskSvc, err := kioskapp.NewKioskService(devices, codes, nil)
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	settingsHandlers := kioskadapter.NewSettingsWebHandlers(kioskSvc, sm, logger)
	deepLinkSigner, err := deeplinkapp.NewSigner([]byte("kiosk-test-harness-deep-link-key"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	kioskHandlers := kioskadapter.NewKioskWebHandlers(
		kioskSvc, instanceRepo, recurringRepo, unifiedCalendar, plannerSvc, recipeRepo,
		shoppingSvc, ingredientCatalog, albumSvc, photoSvc, householdRepo, rewardRepo,
		// A real (non-empty) publicBaseURL exercises the absolute-URL contract
		// deep-link QR generation actually promises (NES-129): every generated
		// link must be reachable from a phone off the kiosk's own network
		// segment, not merely relative to whatever Host the test's httptest
		// request happens to carry.
		sm, logger, false, deepLinkSigner, kioskTestPublicBaseURL, nil,
	)

	// GET /login is registered so seedAuthedSession (used by the parent/child
	// role-gate tests) can mint a session cookie + CSRF token exactly as it
	// does against the real app; login itself is never exercised (the tests
	// stamp member_id into the session directly, bypassing real credentials).
	authHandlers := authadapter.NewHandlers(sm, authapp.New(testCredRepo{}), logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	registerSettingsPage(mux, logger, sm, householdRepo, settingsHandlers)
	registerKioskPages(mux, kioskHandlers)

	handler := sm.LoadAndSave(
		authadapter.Authenticate(sm, householdRepo)(
			kioskadapter.AuthenticateDevice(kioskSvc, logger)(mux),
		),
	)
	return handler, sm, kioskSvc, &kioskFakes{devices: devices, codes: codes, shopping: shoppingRepo}
}

// provisionDevice runs the full CreateActivationCode + Redeem round trip a
// real kiosk activation would perform, for tests that only need an
// already-provisioned device and its bearer token.
func provisionDevice(t *testing.T, kioskSvc *kioskapp.KioskService, householdID household.HouseholdID, name string) (*kioskdomain.KioskDevice, string) {
	t.Helper()
	_, rawCode, err := kioskSvc.CreateActivationCode(context.Background(), householdID, name)
	if err != nil {
		t.Fatalf("CreateActivationCode: %v", err)
	}
	device, rawToken, err := kioskSvc.Redeem(context.Background(), rawCode)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	return device, rawToken
}

// ---------------------------------------------------------------------------
// AC1: no identity → 401, never a peek at household data
// ---------------------------------------------------------------------------

func TestKioskChores_NoIdentityUnauthorized(t *testing.T) {
	handler, _, _, _ := buildKioskTestHandler(t, testMember())

	req := httptest.NewRequest(http.MethodGet, "/kiosk/chores", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /kiosk/chores with no identity: status = %d, want 401", rec.Code)
	}
}

func TestKioskShoppingInCart_NoIdentityUnauthorized(t *testing.T) {
	handler, _, _, _ := buildKioskTestHandler(t, testMember())

	req := httptest.NewRequest(http.MethodPost, "/kiosk/shopping/some-id/in-cart", strings.NewReader("csrf_token=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST in-cart with no identity: status = %d, want 401", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// AC2: parent can provision/revoke; child cannot
// ---------------------------------------------------------------------------

func TestSettingsGenerateActivationCode_ForbiddenForChild(t *testing.T) {
	child := adminTestChild()
	handler, sm, _, fakes := buildKioskTestHandler(t, child)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, child.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/settings/kiosk/generate", strings.NewReader("csrf_token="+csrfToken+"&name=Kitchen"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /settings/kiosk/generate as a child: status = %d, want 403", rec.Code)
	}
	if len(fakes.codes.byHash) != 0 {
		t.Errorf("a forbidden generate must not issue an activation code, got %d", len(fakes.codes.byHash))
	}
}

func TestSettingsGenerateActivationCode_AllowedForParentRevealsCodeOnce(t *testing.T) {
	adult := adminTestAdult()
	handler, sm, _, fakes := buildKioskTestHandler(t, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/settings/kiosk/generate", strings.NewReader("csrf_token="+csrfToken+"&name=Kitchen+wall+display"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/kiosk/generate as a parent: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "kiosk/activate?code=") {
		t.Errorf("response missing the one-time activation link: %s", rec.Body.String())
	}
	// The long-lived device token must never appear in this response — only
	// the short-lived, single-use activation code (MAJOR finding #1).
	if strings.Contains(rec.Body.String(), "token=") {
		t.Errorf("response must not embed a long-lived device token: %s", rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (this response reveals a live credential)", cc)
	}
	if len(fakes.codes.byHash) != 1 {
		t.Fatalf("issued %d activation codes, want 1", len(fakes.codes.byHash))
	}
	// No device is minted until the code is redeemed.
	if devices, _ := fakes.devices.ListByHousehold(context.Background(), adult.HouseholdID); len(devices) != 0 {
		t.Errorf("generating a code must not itself provision a device, got %d", len(devices))
	}

	// The reveal is one-time: a later GET /settings must not show it again.
	rawCode := extractInputValue(rec.Body.String(), "kiosk-code-value")
	if rawCode == "" {
		t.Fatal("could not extract the revealed code from the generate response")
	}

	followUp := httptest.NewRequest(http.MethodGet, "/settings", nil)
	followUp.Header.Set("Cookie", cookie)
	followUpRec := httptest.NewRecorder()
	handler.ServeHTTP(followUpRec, followUp)

	if followUpRec.Code != http.StatusOK {
		t.Fatalf("GET /settings after generate: status = %d, want 200", followUpRec.Code)
	}
	if strings.Contains(followUpRec.Body.String(), rawCode) {
		t.Errorf("a later GET /settings must not re-display the one-time activation code: %s", followUpRec.Body.String())
	}
	if strings.Contains(followUpRec.Body.String(), `id="kiosk-code-value"`) {
		t.Error("a later GET /settings must not render the reveal panel at all")
	}
}

// TestSettingsGenerateActivationCode_WhitespaceOnlyNameFallsBackToDefault is
// the regression test for MINOR finding #9 (round 2): a whitespace-only name
// field must trim to empty and fall back to the "Kiosk" default, not reach
// CreateActivationCode with a non-empty-but-blank-after-trim string that
// domain.ActivationCode.Validate then rejects as a 500.
func TestSettingsGenerateActivationCode_WhitespaceOnlyNameFallsBackToDefault(t *testing.T) {
	adult := adminTestAdult()
	handler, sm, _, fakes := buildKioskTestHandler(t, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/settings/kiosk/generate", strings.NewReader("csrf_token="+csrfToken+"&name=+++"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/kiosk/generate with a whitespace-only name: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var name string
	for _, code := range fakes.codes.byHash {
		name = code.Name
	}
	if name != "Kiosk" {
		t.Errorf("activation code name = %q, want the \"Kiosk\" default", name)
	}
}

// extractInputValue pulls the value="..." attribute out of the first element
// carrying id="elementID" in a rendered page body.
func extractInputValue(body, elementID string) string {
	idAttr := `id="` + elementID + `"`
	idx := strings.Index(body, idAttr)
	if idx < 0 {
		return ""
	}
	rest := body[idx:]
	valStart := strings.Index(rest, `value="`)
	if valStart < 0 {
		return ""
	}
	s := rest[valStart+len(`value="`):]
	end := strings.Index(s, `"`)
	if end < 0 {
		return ""
	}
	return s[:end]
}

// TestSettingsRevokeKioskDevice_RoleGateAndSuccess exercises both the child
// role gate and a parent's successful revoke, each against its own harness
// (authedHouseholdRepo resolves every session to the single member it was
// built with, so the two cases cannot share one handler instance).
func TestSettingsRevokeKioskDevice_RoleGateAndSuccess(t *testing.T) {
	t.Run("forbidden for child", func(t *testing.T) {
		child := adminTestChild()
		handler, sm, kioskSvc, _ := buildKioskTestHandler(t, child)
		device, _ := provisionDevice(t, kioskSvc, child.HouseholdID, "Kitchen")
		cookie, csrfToken := seedAuthedSession(t, handler, sm, child.ID.String())

		req := httptest.NewRequest(http.MethodPost, "/settings/kiosk/"+device.ID.String()+"/revoke", strings.NewReader("csrf_token="+csrfToken))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("revoke as a child: status = %d, want 403", rec.Code)
		}
		active, ok, err := kioskSvc.ActiveDevice(context.Background(), child.HouseholdID)
		if err != nil || !ok || active.ID != device.ID {
			t.Error("a forbidden revoke must not actually revoke the device")
		}
	})

	t.Run("succeeds for parent", func(t *testing.T) {
		adult := adminTestAdult()
		handler, sm, kioskSvc, _ := buildKioskTestHandler(t, adult)
		device, _ := provisionDevice(t, kioskSvc, adult.HouseholdID, "Kitchen")
		cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

		req := httptest.NewRequest(http.MethodPost, "/settings/kiosk/"+device.ID.String()+"/revoke", strings.NewReader("csrf_token="+csrfToken))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusSeeOther {
			t.Fatalf("revoke as a parent: status = %d, want 303; body: %s", rec.Code, rec.Body.String())
		}
		if _, ok, _ := kioskSvc.ActiveDevice(context.Background(), adult.HouseholdID); ok {
			t.Error("device should no longer be active after a parent's revoke")
		}
	})

	// MINOR finding #10 (round 2): a stale/unknown device id must map to 404,
	// mirroring tasksadapter.GamificationWebHandlers.ArchiveReward's
	// convention — not the generic 500 an unmapped domain error would produce.
	t.Run("unknown device id returns 404", func(t *testing.T) {
		adult := adminTestAdult()
		handler, sm, _, _ := buildKioskTestHandler(t, adult)
		cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

		unknownID := kioskdomain.NewKioskDeviceID()
		req := httptest.NewRequest(http.MethodPost, "/settings/kiosk/"+unknownID.String()+"/revoke", strings.NewReader("csrf_token="+csrfToken))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("revoke of an unknown device id: status = %d, want 404; body: %s", rec.Code, rec.Body.String())
		}
	})

	// Re-revoking an already-revoked device hits the same ErrKioskDeviceNotFound
	// path (Revoke's WHERE clause only matches a still-active row) and must
	// also map to 404, not a 500.
	t.Run("already revoked device returns 404", func(t *testing.T) {
		adult := adminTestAdult()
		handler, sm, kioskSvc, _ := buildKioskTestHandler(t, adult)
		device, _ := provisionDevice(t, kioskSvc, adult.HouseholdID, "Kitchen")
		if err := kioskSvc.Revoke(context.Background(), adult.HouseholdID, device.ID); err != nil {
			t.Fatalf("pre-revoke: %v", err)
		}
		cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

		req := httptest.NewRequest(http.MethodPost, "/settings/kiosk/"+device.ID.String()+"/revoke", strings.NewReader("csrf_token="+csrfToken))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("re-revoke of an already-revoked device: status = %d, want 404; body: %s", rec.Code, rec.Body.String())
		}
	})
}

// ---------------------------------------------------------------------------
// AC5: marking a shopping item in-cart works from the kiosk with device auth
// ---------------------------------------------------------------------------

func TestKioskMarkInCart_WithDeviceAuth(t *testing.T) {
	adult := adminTestAdult()
	handler, _, kioskSvc, fakes := buildKioskTestHandler(t, adult)
	_, rawToken := provisionDevice(t, kioskSvc, adult.HouseholdID, "Kitchen")

	item := &trackingdomain.ShoppingListItem{
		ID: trackingdomain.NewShoppingListItemID(), HouseholdID: adult.HouseholdID,
		Name: "Milk", Quantity: household.Quantity{Amount: 1, Unit: household.UnitCount},
		Source: trackingdomain.SourceManual, Status: trackingdomain.StatusNeeded,
	}
	if err := fakes.shopping.Add(context.Background(), item); err != nil {
		t.Fatalf("seed shopping item: %v", err)
	}

	// Load the kiosk shopping page first to mint a CSRF token bound to this
	// browser's (cookie-carrying) anonymous session, mirroring how a real
	// kiosk page load always precedes its own form submit.
	pageReq := httptest.NewRequest(http.MethodGet, "/kiosk/shopping", nil)
	pageReq.AddCookie(&http.Cookie{Name: kioskadapter.CookieName, Value: rawToken})
	pageRec := httptest.NewRecorder()
	handler.ServeHTTP(pageRec, pageReq)
	if pageRec.Code != http.StatusOK {
		t.Fatalf("GET /kiosk/shopping: status = %d, want 200; body: %s", pageRec.Code, pageRec.Body.String())
	}
	if !strings.Contains(pageRec.Body.String(), "Milk") {
		t.Fatalf("kiosk shopping page missing seeded item: %s", pageRec.Body.String())
	}
	csrfToken := extractCSRFToken(pageRec.Body.String())
	sessionCookie := extractCookie(pageRec.Result().Cookies(), "session")

	req := httptest.NewRequest(http.MethodPost, "/kiosk/shopping/"+item.ID.String()+"/in-cart", strings.NewReader("csrf_token="+csrfToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", sessionCookie+"; "+kioskadapter.CookieName+"="+rawToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST in-cart with device auth: status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if item.Status != trackingdomain.StatusInCart {
		t.Errorf("item status = %q, want %q", item.Status, trackingdomain.StatusInCart)
	}
}

// TestKioskMarkInCart_RejectsReplayAfterPurchased is the replay-after-purchase
// regression test: once an item is purchased, the kiosk's in-cart action must
// not move it backward.
func TestKioskMarkInCart_RejectsReplayAfterPurchased(t *testing.T) {
	adult := adminTestAdult()
	handler, _, kioskSvc, fakes := buildKioskTestHandler(t, adult)
	_, rawToken := provisionDevice(t, kioskSvc, adult.HouseholdID, "Kitchen")

	item := &trackingdomain.ShoppingListItem{
		ID: trackingdomain.NewShoppingListItemID(), HouseholdID: adult.HouseholdID,
		Name: "Milk", Quantity: household.Quantity{Amount: 1, Unit: household.UnitCount},
		Source: trackingdomain.SourceManual, Status: trackingdomain.StatusPurchased,
	}
	if err := fakes.shopping.Add(context.Background(), item); err != nil {
		t.Fatalf("seed shopping item: %v", err)
	}
	// A decoy needed item so the page renders at least one CSRF-carrying
	// form to extract from — the purchased item above renders in neither
	// the needed nor in-cart section, so on its own the page would have no
	// form at all.
	decoy := &trackingdomain.ShoppingListItem{
		ID: trackingdomain.NewShoppingListItemID(), HouseholdID: adult.HouseholdID,
		Name: "Eggs", Quantity: household.Quantity{Amount: 1, Unit: household.UnitCount},
		Source: trackingdomain.SourceManual, Status: trackingdomain.StatusNeeded,
	}
	if err := fakes.shopping.Add(context.Background(), decoy); err != nil {
		t.Fatalf("seed decoy shopping item: %v", err)
	}

	pageReq := httptest.NewRequest(http.MethodGet, "/kiosk/shopping", nil)
	pageReq.AddCookie(&http.Cookie{Name: kioskadapter.CookieName, Value: rawToken})
	pageRec := httptest.NewRecorder()
	handler.ServeHTTP(pageRec, pageReq)
	csrfToken := extractCSRFToken(pageRec.Body.String())
	sessionCookie := extractCookie(pageRec.Result().Cookies(), "session")

	req := httptest.NewRequest(http.MethodPost, "/kiosk/shopping/"+item.ID.String()+"/in-cart", strings.NewReader("csrf_token="+csrfToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", sessionCookie+"; "+kioskadapter.CookieName+"="+rawToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("POST in-cart replay on a purchased item: status = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
	if item.Status != trackingdomain.StatusPurchased {
		t.Errorf("item status = %q, must remain purchased (never moved backward)", item.Status)
	}
}

// ---------------------------------------------------------------------------
// AC6: a revoked device no longer grants access
// ---------------------------------------------------------------------------

func TestKioskChores_RevokedDeviceDenied(t *testing.T) {
	adult := adminTestAdult()
	handler, _, kioskSvc, _ := buildKioskTestHandler(t, adult)
	device, rawToken := provisionDevice(t, kioskSvc, adult.HouseholdID, "Kitchen")

	// Confirm the device works before revocation.
	before := httptest.NewRequest(http.MethodGet, "/kiosk/chores", nil)
	before.AddCookie(&http.Cookie{Name: kioskadapter.CookieName, Value: rawToken})
	beforeRec := httptest.NewRecorder()
	handler.ServeHTTP(beforeRec, before)
	if beforeRec.Code != http.StatusOK {
		t.Fatalf("GET /kiosk/chores before revoke: status = %d, want 200", beforeRec.Code)
	}

	if err := kioskSvc.Revoke(context.Background(), adult.HouseholdID, device.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	after := httptest.NewRequest(http.MethodGet, "/kiosk/chores", nil)
	after.AddCookie(&http.Cookie{Name: kioskadapter.CookieName, Value: rawToken})
	afterRec := httptest.NewRecorder()
	handler.ServeHTTP(afterRec, after)
	if afterRec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /kiosk/chores after revoke: status = %d, want 401", afterRec.Code)
	}
}

// ---------------------------------------------------------------------------
// NES-129: QR refresh self-poll
// ---------------------------------------------------------------------------

// TestKioskChores_SelfPollsForFreshQRCodes asserts the chores tab's content
// wrapper carries the htmx self-poll wiring (finding: a display left open
// past deeplinkapp.LinkTTL would otherwise keep showing QR codes that scan
// to an already-expired rescan page), and that its target,
// GET /kiosk/chores/content, returns just the content fragment (not the full
// kiosk shell) so the poll never duplicates <html>/<head>/the tab bar.
func TestKioskChores_SelfPollsForFreshQRCodes(t *testing.T) {
	adult := adminTestAdult()
	handler, _, kioskSvc, _ := buildKioskTestHandler(t, adult)
	_, rawToken := provisionDevice(t, kioskSvc, adult.HouseholdID, "Kitchen")

	req := httptest.NewRequest(http.MethodGet, "/kiosk/chores", nil)
	req.AddCookie(&http.Cookie{Name: kioskadapter.CookieName, Value: rawToken})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /kiosk/chores: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="kiosk-chores-content"`) {
		t.Errorf("response missing the self-poll wrapper id: %s", body)
	}
	if !strings.Contains(body, `hx-get="/kiosk/chores/content"`) {
		t.Errorf("response missing the poll target: %s", body)
	}
	// deeplinkapp.LinkTTL is 10 minutes; the poll interval is half of that.
	if !strings.Contains(body, `hx-trigger="every 300s"`) {
		t.Errorf("response missing the expected poll interval (every 300s): %s", body)
	}

	contentReq := httptest.NewRequest(http.MethodGet, "/kiosk/chores/content", nil)
	contentReq.AddCookie(&http.Cookie{Name: kioskadapter.CookieName, Value: rawToken})
	contentRec := httptest.NewRecorder()
	handler.ServeHTTP(contentRec, contentReq)
	if contentRec.Code != http.StatusOK {
		t.Fatalf("GET /kiosk/chores/content: status = %d, want 200", contentRec.Code)
	}
	contentBody := contentRec.Body.String()
	if strings.Contains(contentBody, "<html") {
		t.Errorf("the poll fragment must not include the full kiosk shell: %s", contentBody)
	}
	if !strings.Contains(contentBody, `id="kiosk-chores-content"`) {
		t.Errorf("poll fragment missing its own self-poll wrapper (must re-arm on every swap): %s", contentBody)
	}
}

// activeVsStorefrontRewardRepo is a RewardRepository fake whose
// ListActiveRewards and ListStorefrontRewards deliberately disagree — active
// returns a reward that is NOT in ListStorefrontRewards's result — so a test
// can prove which of the two the kiosk actually calls. Real
// ListStorefrontRewards implementations exclude an archived OR
// stock-exhausted reward entirely (see RewardRepository's doc comment); this
// fake models exactly that divergence without needing real stock-counting
// logic.
type activeVsStorefrontRewardRepo struct {
	configurableRewardRepo
	active     []*tasksdomain.Reward
	storefront []tasksdomain.StorefrontReward
}

func (r *activeVsStorefrontRewardRepo) ListActiveRewards(context.Context, household.HouseholdID) ([]*tasksdomain.Reward, error) {
	return r.active, nil
}

func (r *activeVsStorefrontRewardRepo) ListStorefrontRewards(context.Context, household.HouseholdID) ([]tasksdomain.StorefrontReward, error) {
	return r.storefront, nil
}

// Compile-time assertion.
var _ tasksdomain.RewardRepository = (*activeVsStorefrontRewardRepo)(nil)

// TestKioskChores_ExhaustedRewardGetsNoQRCard is the regression test for the
// ListActiveRewards → ListStorefrontRewards fix: an exhausted (or archived)
// reward must never get a card on the kiosk, since scanning its QR would be
// a dead end — nothing on the phone side could ever redeem it. The fake
// reward repo's ListActiveRewards would still return the exhausted reward
// (proving the OLD code path would have shown it), while
// ListStorefrontRewards — what the kiosk must actually call — returns none.
func TestKioskChores_ExhaustedRewardGetsNoQRCard(t *testing.T) {
	adult := adminTestAdult()
	exhausted := &tasksdomain.Reward{
		ID: tasksdomain.NewRewardID(), HouseholdID: adult.HouseholdID,
		Name: "Sold Out Prize", CostPoints: 10, Active: true,
	}
	rewards := &activeVsStorefrontRewardRepo{
		active:     []*tasksdomain.Reward{exhausted},
		storefront: nil, // ListStorefrontRewards excludes it (exhausted/archived)
	}
	handler, _, kioskSvc, _ := buildKioskTestHandler(t, adult, rewards)
	_, rawToken := provisionDevice(t, kioskSvc, adult.HouseholdID, "Kitchen")

	req := httptest.NewRequest(http.MethodGet, "/kiosk/chores", nil)
	req.AddCookie(&http.Cookie{Name: kioskadapter.CookieName, Value: rawToken})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /kiosk/chores: status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Sold Out Prize") {
		t.Errorf("kiosk rendered a card for an exhausted/archived reward: %s", rec.Body.String())
	}
}

// TestKioskChoresContent_NoIdentityUnauthorized mirrors
// TestKioskChores_NoIdentityUnauthorized for the poll-target endpoint: it
// must never become an unauthenticated back door into household data.
func TestKioskChoresContent_NoIdentityUnauthorized(t *testing.T) {
	handler, _, _, _ := buildKioskTestHandler(t, testMember())

	req := httptest.NewRequest(http.MethodGet, "/kiosk/chores/content", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /kiosk/chores/content with no identity: status = %d, want 401", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Activation round trip
// ---------------------------------------------------------------------------

// postActivate submits the activation form's POST using the session cookie
// and CSRF token minted by an initial GET — the only path that can ever
// redeem a code (see KioskWebHandlers.Activate: GET never redeems).
func postActivate(t *testing.T, handler http.Handler, rawCode string) *httptest.ResponseRecorder {
	t.Helper()
	getReq := httptest.NewRequest(http.MethodGet, "/kiosk/activate", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	csrfToken := extractCSRFToken(getRec.Body.String())
	sessionCookie := extractCookie(getRec.Result().Cookies(), "session")

	req := httptest.NewRequest(http.MethodPost, "/kiosk/activate", strings.NewReader("csrf_token="+csrfToken+"&code="+rawCode))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", sessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestKioskActivate_GetNeverRedeems is the regression test for MAJOR finding
// #1: a GET carrying ?code=... (the one-click link, or a prefetch/preview
// following it) must only render a pre-filled confirmation form — it must
// never set the device cookie or consume the single-use code.
func TestKioskActivate_GetNeverRedeems(t *testing.T) {
	adult := adminTestAdult()
	handler, _, kioskSvc, fakes := buildKioskTestHandler(t, adult)
	_, rawCode, err := kioskSvc.CreateActivationCode(context.Background(), adult.HouseholdID, "Kitchen")
	if err != nil {
		t.Fatalf("CreateActivationCode: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/kiosk/activate?code="+rawCode, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /kiosk/activate?code=...: status = %d, want 200 (a form, not a redirect); body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `value="`+rawCode+`"`) {
		t.Errorf("GET should pre-fill the code into the confirmation form: %s", rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == kioskadapter.CookieName {
			t.Errorf("GET must never set the kiosk device cookie, got %q", c.Value)
		}
	}
	for _, code := range fakes.codes.byHash {
		if code.UsedAt != nil {
			t.Error("GET must never mark the activation code used")
		}
	}
	// The code must still be genuinely redeemable afterward.
	postRec := postActivate(t, handler, rawCode)
	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST after a prior GET: status = %d, want 303; body: %s", postRec.Code, postRec.Body.String())
	}
}

func TestKioskActivate_PostRedeemsSetsCookieAndRedirects(t *testing.T) {
	adult := adminTestAdult()
	handler, _, kioskSvc, _ := buildKioskTestHandler(t, adult)
	_, rawCode, err := kioskSvc.CreateActivationCode(context.Background(), adult.HouseholdID, "Kitchen")
	if err != nil {
		t.Fatalf("CreateActivationCode: %v", err)
	}

	rec := postActivate(t, handler, rawCode)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /kiosk/activate: status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/kiosk/chores" {
		t.Errorf("Location = %q, want /kiosk/chores", loc)
	}
	var deviceToken string
	for _, c := range rec.Result().Cookies() {
		if c.Name == kioskadapter.CookieName {
			deviceToken = c.Value
		}
	}
	if deviceToken == "" {
		t.Fatal("activation did not set the kiosk device cookie")
	}
	if _, err := kioskSvc.Authenticate(context.Background(), deviceToken); err != nil {
		t.Errorf("the minted device token does not authenticate: %v", err)
	}
}

func TestKioskActivate_UnknownCodeShowsRetryForm(t *testing.T) {
	handler, _, _, _ := buildKioskTestHandler(t, testMember())

	rec := postActivate(t, handler, "NEVER-ISSUED")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /kiosk/activate with an unknown code: status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `name="code"`) {
		t.Errorf("failed activation should re-show the manual entry form: %s", rec.Body.String())
	}
}

func TestKioskActivate_NoCodeShowsEntryForm(t *testing.T) {
	handler, _, _, _ := buildKioskTestHandler(t, testMember())

	req := httptest.NewRequest(http.MethodGet, "/kiosk/activate", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /kiosk/activate with no code: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `name="code"`) {
		t.Errorf("bare GET /kiosk/activate should show the manual entry form: %s", rec.Body.String())
	}
}

// TestKioskActivate_CodeIsSingleUse is the HTTP-layer regression test for a
// second redemption attempt of the same code (a MAJOR finding's explicit
// "second redemption 401" requirement).
func TestKioskActivate_CodeIsSingleUse(t *testing.T) {
	adult := adminTestAdult()
	handler, _, kioskSvc, _ := buildKioskTestHandler(t, adult)
	_, rawCode, err := kioskSvc.CreateActivationCode(context.Background(), adult.HouseholdID, "Kitchen")
	if err != nil {
		t.Fatalf("CreateActivationCode: %v", err)
	}

	firstRec := postActivate(t, handler, rawCode)
	if firstRec.Code != http.StatusSeeOther {
		t.Fatalf("first redemption: status = %d, want 303", firstRec.Code)
	}

	secondRec := postActivate(t, handler, rawCode)
	if secondRec.Code != http.StatusUnauthorized {
		t.Fatalf("second redemption of the same code: status = %d, want 401", secondRec.Code)
	}
}

// TestKioskActivate_ExpiredCodeRejected is the HTTP-layer regression test for
// an expired code (a MAJOR finding's explicit "expired code 401" requirement).
func TestKioskActivate_ExpiredCodeRejected(t *testing.T) {
	adult := adminTestAdult()
	handler, _, kioskSvc, fakes := buildKioskTestHandler(t, adult)
	_, rawCode, err := kioskSvc.CreateActivationCode(context.Background(), adult.HouseholdID, "Kitchen")
	if err != nil {
		t.Fatalf("CreateActivationCode: %v", err)
	}
	// Force the code's expiry into the past.
	for _, code := range fakes.codes.byHash {
		code.ExpiresAt = time.Now().Add(-time.Minute)
	}

	rec := postActivate(t, handler, rawCode)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /kiosk/activate with an expired code: status = %d, want 401", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// extractCSRFToken pulls the csrf_token hidden-input value out of a rendered
// page body, mirroring seedAuthedSession's own inline extraction.
func extractCSRFToken(body string) string {
	tokenStart := strings.Index(body, `name="csrf_token"`)
	if tokenStart < 0 {
		return ""
	}
	valStart := strings.Index(body[tokenStart:], `value="`)
	if valStart < 0 {
		return ""
	}
	s := body[tokenStart+valStart+len(`value="`):]
	end := strings.Index(s, `"`)
	if end < 0 {
		return ""
	}
	return s[:end]
}

func extractCookie(cookies []*http.Cookie, name string) string {
	for _, c := range cookies {
		if c.Name == name {
			return c.Name + "=" + c.Value
		}
	}
	return ""
}
