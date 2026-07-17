package adapter

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/kiosk/domain"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver/middleware"
)

// CookieName is the kiosk device's bearer-token cookie. Unlike the member
// session cookie (server-side, scs-backed), this cookie carries the raw token
// directly: the kiosk device is authenticated by hashing the presented token
// and looking it up on every request (see app.KioskService.Authenticate),
// with no server-side session state of its own.
const CookieName = "kiosk_token"

// cookieMaxAge is how long the kiosk cookie persists on the device. It is
// deliberately long-lived (about ten years): the actual access control is the
// server-side revocation (kiosk_device.revoked_at), not the cookie's own
// expiry, so there is no security benefit to a short client-side lifetime —
// only the operational cost of re-provisioning a wall-mounted device that is
// never expected to log out.
const cookieMaxAge = 10 * 365 * 24 * time.Hour

// kioskContextKey is the unexported type for context keys in this package.
type kioskContextKey int

// currentDeviceKey stores the authenticated KioskDevice in the request context.
const currentDeviceKey kioskContextKey = iota

// kioskAuthenticator is the narrow read port AuthenticateDevice depends on
// (ISP): only the token-to-device resolution app.KioskService exposes.
type kioskAuthenticator interface {
	Authenticate(ctx context.Context, rawToken string) (*domain.KioskDevice, error)
}

// SetCookie writes the kiosk device's bearer token cookie on w. secure mirrors
// the session cookie's SESSION_COOKIE_SECURE policy (cfg.Session.Secure) so
// both cookies share one deployment posture.
func SetCookie(w http.ResponseWriter, rawToken string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    rawToken,
		Path:     "/",
		MaxAge:   int(cookieMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie removes the kiosk device's bearer token cookie from the browser
// that presents it (used when its device is revoked from the client's own
// request, and by tests that need a clean slate).
func ClearCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// CurrentDevice returns the KioskDevice stored in ctx by AuthenticateDevice,
// and false when no authenticated device is present.
func CurrentDevice(ctx context.Context) (*domain.KioskDevice, bool) {
	d, ok := ctx.Value(currentDeviceKey).(*domain.KioskDevice)
	return d, ok
}

// AuthenticateDevice is a middleware that loads the authenticated KioskDevice
// into the request context when the kiosk cookie carries a token that
// resolves to an active device. A missing cookie, an unknown token
// (domain.ErrKioskDeviceNotFound), or a revoked device
// (domain.ErrKioskDeviceRevoked) all proceed unchanged (no device in
// context) rather than rejecting the request outright — mirroring
// authadapter.Authenticate, this middleware only loads identity;
// RequireKioskOrMember enforces it.
//
// Any OTHER error from Authenticate (e.g. the database is unreachable) is
// NOT treated the same as "this device was never registered": silently
// proceeding anonymously would let RequireKioskOrMember mask a real
// infrastructure failure behind a misleading 401 unauthorized, hiding an
// outage from monitoring. Such an error is logged and answered with 500
// instead, and the request never reaches next.
func AuthenticateDevice(kiosk kioskAuthenticator, logger *slog.Logger) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(CookieName)
			if err != nil || cookie.Value == "" {
				next.ServeHTTP(w, r)
				return
			}
			device, authErr := kiosk.Authenticate(r.Context(), cookie.Value)
			switch {
			case authErr == nil:
				r = r.WithContext(context.WithValue(r.Context(), currentDeviceKey, device))
			case errors.Is(authErr, domain.ErrKioskDeviceNotFound), errors.Is(authErr, domain.ErrKioskDeviceRevoked):
				// Expected, legitimate "not authenticated" outcomes.
			default:
				logger.ErrorContext(r.Context(), "kiosk: authenticate device", "error", authErr)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireKioskOrMember enforces that either an authenticated KioskDevice or an
// authenticated Member is present in the context (injected by
// AuthenticateDevice and authadapter.Authenticate respectively). It always
// responds 401 Unauthorized when neither is present — unlike
// authadapter.RequireMember, it never redirects to /login: the kiosk shell has
// no login page of its own, and a public wall-mounted display must never
// surface a member-login prompt where any LAN guest standing in front of it
// could see or use it.
func RequireKioskOrMember() middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := CurrentDevice(r.Context()); ok {
				next.ServeHTTP(w, r)
				return
			}
			if _, ok := authadapter.CurrentMember(r.Context()); ok {
				next.ServeHTTP(w, r)
				return
			}
			// A bodyless 401: the kiosk gate never renders content or a
			// login affordance to an unidentified LAN client.
			w.WriteHeader(http.StatusUnauthorized)
		})
	}
}

// CurrentHouseholdID resolves the acting household for a /kiosk/* request from
// whichever identity RequireKioskOrMember admitted: the kiosk device (checked
// first, the expected identity on the wall-mounted display) or, when a parent
// is instead previewing the kiosk view from their own logged-in browser, the
// current member. Returns ok=false when neither identity is present (should
// not happen behind RequireKioskOrMember, but callers must not assume it).
func CurrentHouseholdID(ctx context.Context) (household.HouseholdID, bool) {
	if device, ok := CurrentDevice(ctx); ok {
		return device.HouseholdID, true
	}
	if member, ok := authadapter.CurrentMember(ctx); ok {
		return member.HouseholdID, true
	}
	return household.HouseholdID{}, false
}

// ErrNoHousehold is returned by handlers that call CurrentHouseholdID behind
// RequireKioskOrMember but find neither identity present — a defensive,
// should-never-happen guard against a misconfigured route.
var ErrNoHousehold = errors.New("kiosk: no device or member identity in context")
