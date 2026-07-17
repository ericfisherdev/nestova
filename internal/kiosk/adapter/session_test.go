package adapter_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/kiosk/adapter"
	"github.com/ericfisherdev/nestova/internal/kiosk/domain"
)

// testLogger discards output; the session tests care about status codes and
// context state, not log formatting.
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeAuthenticator is a minimal stand-in for app.KioskService's Authenticate
// method — Go's structural typing lets it satisfy adapter's unexported
// kioskAuthenticator port without importing the app package here. err, when
// set, is returned for EVERY call regardless of byToken, letting a test
// simulate an infrastructure failure (as opposed to a legitimate "unknown
// token" domain error).
type fakeAuthenticator struct {
	byToken map[string]*domain.KioskDevice
	err     error
}

func (f *fakeAuthenticator) Authenticate(_ context.Context, rawToken string) (*domain.KioskDevice, error) {
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.byToken[rawToken]
	if !ok {
		return nil, domain.ErrKioskDeviceNotFound
	}
	return d, nil
}

func passthroughHandler(hit *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*hit = true
		w.WriteHeader(http.StatusOK)
	})
}

func TestAuthenticateDevice_NoCookieProceedsAnonymously(t *testing.T) {
	auth := &fakeAuthenticator{byToken: map[string]*domain.KioskDevice{}}
	var reachedNext bool
	var sawDevice bool
	handler := adapter.AuthenticateDevice(auth, testLogger())(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reachedNext = true
		_, sawDevice = adapter.CurrentDevice(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/kiosk/chores", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !reachedNext {
		t.Fatal("AuthenticateDevice must always call next, even with no cookie")
	}
	if sawDevice {
		t.Error("no cookie should mean no device in context")
	}
}

func TestAuthenticateDevice_ValidCookieLoadsDevice(t *testing.T) {
	device := &domain.KioskDevice{ID: domain.NewKioskDeviceID(), HouseholdID: household.NewHouseholdID(), Name: "Kitchen"}
	auth := &fakeAuthenticator{byToken: map[string]*domain.KioskDevice{"valid-token": device}}

	var gotDevice *domain.KioskDevice
	var ok bool
	handler := adapter.AuthenticateDevice(auth, testLogger())(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotDevice, ok = adapter.CurrentDevice(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/kiosk/chores", nil)
	req.AddCookie(&http.Cookie{Name: adapter.CookieName, Value: "valid-token"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !ok {
		t.Fatal("valid cookie should load a device into context")
	}
	if gotDevice.ID != device.ID {
		t.Errorf("CurrentDevice returned %s, want %s", gotDevice.ID, device.ID)
	}
}

func TestAuthenticateDevice_UnknownOrRevokedCookieProceedsAnonymously(t *testing.T) {
	auth := &fakeAuthenticator{byToken: map[string]*domain.KioskDevice{}}
	var sawDevice bool
	handler := adapter.AuthenticateDevice(auth, testLogger())(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, sawDevice = adapter.CurrentDevice(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/kiosk/chores", nil)
	req.AddCookie(&http.Cookie{Name: adapter.CookieName, Value: "revoked-or-unknown"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if sawDevice {
		t.Error("an unresolvable token must not populate a device in context")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (AuthenticateDevice never rejects on its own)", rec.Code)
	}
}

// TestAuthenticateDevice_RevokedDeviceProceedsAnonymously exercises the
// domain.ErrKioskDeviceRevoked sentinel specifically (the test above only
// ever produces domain.ErrKioskDeviceNotFound via its empty byToken map), so
// a revoked-but-otherwise-known device's token is confirmed to take the same
// "proceed anonymously" path as an unknown one — not the infra failure 500 path.
func TestAuthenticateDevice_RevokedDeviceProceedsAnonymously(t *testing.T) {
	auth := &fakeAuthenticator{err: domain.ErrKioskDeviceRevoked}
	var reachedNext bool
	var sawDevice bool
	handler := adapter.AuthenticateDevice(auth, testLogger())(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reachedNext = true
		_, sawDevice = adapter.CurrentDevice(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/kiosk/chores", nil)
	req.AddCookie(&http.Cookie{Name: adapter.CookieName, Value: "revoked-token"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !reachedNext {
		t.Fatal("a revoked device's token must still proceed to next anonymously, not be rejected here")
	}
	if sawDevice {
		t.Error("a revoked device must not populate a device in context")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestAuthenticateDevice_InfraFailureReturns500NotAnonymous(t *testing.T) {
	// A database outage (or any error other than "unknown token"/"revoked")
	// must NOT be silently treated as "no device": that would let
	// RequireKioskOrMember mask a real infrastructure failure behind a
	// misleading 401, hiding an outage from monitoring.
	auth := &fakeAuthenticator{err: errors.New("kiosk: database unreachable")}
	var reachedNext bool
	handler := adapter.AuthenticateDevice(auth, testLogger())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reachedNext = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/kiosk/chores", nil)
	req.AddCookie(&http.Cookie{Name: adapter.CookieName, Value: "some-token"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if reachedNext {
		t.Fatal("an infrastructure failure must not reach the next handler")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestRequireKioskOrMember_RejectsWithNeitherIdentity(t *testing.T) {
	var hit bool
	handler := adapter.RequireKioskOrMember()(passthroughHandler(&hit))

	req := httptest.NewRequest(http.MethodGet, "/kiosk/chores", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if hit {
		t.Fatal("the next handler must not run without a kiosk device or member identity")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	// The kiosk has no login page: it must never redirect to /login, unlike
	// authadapter.RequireMember.
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("must not redirect (Location=%q); a public kiosk must never surface a login prompt", loc)
	}
}

func TestRequireKioskOrMember_AllowsAuthenticatedDevice(t *testing.T) {
	device := &domain.KioskDevice{ID: domain.NewKioskDeviceID(), HouseholdID: household.NewHouseholdID()}
	auth := &fakeAuthenticator{byToken: map[string]*domain.KioskDevice{"valid-token": device}}

	var hit bool
	var resolvedHousehold household.HouseholdID
	var resolvedOK bool
	chain := adapter.AuthenticateDevice(auth, testLogger())(adapter.RequireKioskOrMember()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hit = true
		resolvedHousehold, resolvedOK = adapter.CurrentHouseholdID(r.Context())
	})))

	req := httptest.NewRequest(http.MethodGet, "/kiosk/chores", nil)
	req.AddCookie(&http.Cookie{Name: adapter.CookieName, Value: "valid-token"})
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	if !hit {
		t.Fatal("a valid device cookie should reach the protected handler")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !resolvedOK || resolvedHousehold != device.HouseholdID {
		t.Errorf("CurrentHouseholdID = (%v, %v), want (%v, true)", resolvedHousehold, resolvedOK, device.HouseholdID)
	}
}

func TestCurrentHouseholdID_NeitherIdentityPresent(t *testing.T) {
	if _, ok := adapter.CurrentHouseholdID(context.Background()); ok {
		t.Error("CurrentHouseholdID should report false with no identity in context")
	}
}

func TestSetAndClearCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	adapter.SetCookie(rec, "raw-token", true)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("SetCookie wrote %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != adapter.CookieName || c.Value != "raw-token" {
		t.Errorf("cookie = %+v, want name %q value %q", c, adapter.CookieName, "raw-token")
	}
	if !c.HttpOnly {
		t.Error("kiosk cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Error("kiosk cookie must be Secure when secure=true was requested")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}

	rec2 := httptest.NewRecorder()
	adapter.ClearCookie(rec2, true)
	cleared := rec2.Result().Cookies()[0]
	if cleared.MaxAge >= 0 {
		t.Errorf("ClearCookie MaxAge = %d, want negative (delete)", cleared.MaxAge)
	}
}

// Ensure ErrNoHousehold remains a genuine sentinel usable with errors.Is
// (guards against an accidental refactor turning it into a non-comparable
// wrapped error).
func TestErrNoHouseholdIsComparable(t *testing.T) {
	if !errors.Is(adapter.ErrNoHousehold, adapter.ErrNoHousehold) {
		t.Fatal("ErrNoHousehold must satisfy errors.Is against itself")
	}
}
