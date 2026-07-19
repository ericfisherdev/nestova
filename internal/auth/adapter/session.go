package adapter

import (
	"context"
	"encoding/gob"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgxpool"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver/middleware"
)

// stepUpFreshnessWindow bounds how recently a session must have completed
// login MFA verification for RequireStepUp (NES-135) to consider it fresh
// enough to gate a security-sensitive action without demanding it again.
const stepUpFreshnessWindow = 15 * time.Minute

func init() {
	// scs serializes session data via gob, which requires every concrete
	// type assigned through an interface{} (sm.Put's value parameter) to be
	// registered up front (see the scs README's "Storing custom types"
	// note). sessionKeyMFAVerifiedAt (NES-135, http.go) is the first
	// non-string value this codebase stores in the session — every prior
	// sm.Put call stores a plain string (e.g. sessionKeyMemberID's
	// memberID.String()), which gob handles natively without registration.
	gob.Register(time.Time{})
	// webauthn.SessionData (NES-136, webauthn_web.go's
	// sessionKeyWebAuthnRegChallenge) is the second non-string value stored
	// in the session, for the same reason.
	gob.Register(webauthn.SessionData{})
}

// sessionContextKey is the unexported type for context keys in this package.
type sessionContextKey int

// currentMemberKey stores the authenticated Member in the request context.
const currentMemberKey sessionContextKey = iota

// NewSessionManager constructs an scs.SessionManager backed by Postgres using
// the pgxpool shared with the rest of the application. Cookie settings are
// derived from cfg: Secure follows the resolved SESSION_COOKIE_SECURE policy
// (auto → prod-only, or forced true/false), Lifetime from SESSION_LIFETIME.
func NewSessionManager(pool *pgxpool.Pool, cfg config.SessionConfig) *scs.SessionManager {
	sm := scs.New()
	sm.Store = pgxstore.New(pool)
	sm.Lifetime = cfg.Lifetime
	// Expire idle sessions at half the absolute lifetime: active users are kept
	// signed in (each request refreshes idle time) while abandoned sessions are
	// reclaimed well before the hard Lifetime cap.
	sm.IdleTimeout = cfg.Lifetime / 2
	sm.Cookie.HttpOnly = true
	sm.Cookie.SameSite = http.SameSiteLaxMode
	sm.Cookie.Secure = cfg.Secure
	sm.Cookie.Path = "/"
	sm.Cookie.Persist = true
	return sm
}

// lookupMember resolves the session's member_id to a Member. When the id is
// malformed or the member no longer exists, the stale session key is removed so
// subsequent requests do not repeat the failed lookup, and it reports ok=false
// (the request proceeds anonymously).
func lookupMember(ctx context.Context, sm *scs.SessionManager, members household.HouseholdRepository, memberIDStr string) (*household.Member, bool) {
	memberID, err := household.ParseMemberID(memberIDStr)
	if err != nil {
		// A malformed id can never become valid, so clear it.
		sm.Remove(ctx, "member_id")
		return nil, false
	}
	member, err := members.GetMember(ctx, memberID)
	if err != nil {
		// Only clear the key when the member is genuinely gone; a transient
		// error (DB/network) must not log the user out — proceed anonymous for
		// just this request and keep the session for a later retry.
		if errors.Is(err, household.ErrMemberNotFound) {
			sm.Remove(ctx, "member_id")
		}
		return nil, false
	}
	return member, true
}

// CurrentMember returns the Member stored in ctx by the Authenticate
// middleware, and false when no authenticated member is present.
func CurrentMember(ctx context.Context) (*household.Member, bool) {
	m, ok := ctx.Value(currentMemberKey).(*household.Member)
	return m, ok
}

// Authenticate is a middleware that loads the authenticated Member into the
// request context when the session contains a valid "member_id". Requests
// without a session (or with an unknown member_id) proceed unchanged so that
// public routes keep working; RequireMember enforces authentication for
// protected routes.
func Authenticate(sm *scs.SessionManager, members household.HouseholdRepository) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			memberIDStr := sm.GetString(r.Context(), "member_id")
			if memberIDStr != "" {
				if member, ok := lookupMember(r.Context(), sm, members, memberIDStr); ok {
					r = r.WithContext(context.WithValue(r.Context(), currentMemberKey, member))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireMember enforces that a Member is present in the context (injected by
// Authenticate). For HTMX partial requests it writes 401 Unauthorized; for
// full navigations it redirects to /login with the original path in the `next`
// query parameter for post-login redirect. The session manager parameter is
// accepted for signature consistency with other auth middleware but is not used
// directly; the member check is performed against the request context.
func RequireMember(_ *scs.SessionManager) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, ok := CurrentMember(r.Context())
			if !ok {
				if r.Header.Get("HX-Request") == "true" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				target := "/login?next=" + escapePath(r.URL.RequestURI())
				http.Redirect(w, r, target, http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// escapePath percent-encodes a path+query string for safe use as a query
// parameter value. It uses url.QueryEscape so that the `next` value can be
// round-tripped through the login form hidden field.
func escapePath(path string) string {
	return url.QueryEscape(path)
}

// MFAVerifiedAt returns the timestamp the current session last completed
// login MFA verification (see sessionKeyMFAVerifiedAt's doc in http.go),
// and false when the session carries no such stamp at all — either because
// it predates NES-135, or because finishLogin deliberately left it unset
// (a remembered-device login that skipped the prompt).
func MFAVerifiedAt(ctx context.Context, sm *scs.SessionManager) (time.Time, bool) {
	t := sm.GetTime(ctx, sessionKeyMFAVerifiedAt)
	return t, !t.IsZero()
}

// mfaStatusChecker is the narrow read port RequireStepUp depends on (ISP):
// only the enrollment-status lookup authapp.MFAService exposes, needed to
// decide whether a member has anything to step up FROM at all. Satisfied
// by *authapp.MFAService (a superset); defined locally rather than
// importing authapp's own interface to avoid a package-level dependency on
// authapp just for this one method shape.
type mfaStatusChecker interface {
	Status(ctx context.Context, memberID household.MemberID) (*authdomain.MFAEnrollment, error)
}

// RequireStepUp is a middleware enforcing that the current session's login
// MFA verification is fresh (within stepUpFreshnessWindow) before allowing
// a security-sensitive action to proceed — e.g. provisioning a kiosk device
// token (cmd/server/home.go's registerSettingsPage). It must run AFTER
// RequireMember (it assumes an authenticated Member is already present in
// the context) and mirrors RequireMember's own HX-Request-vs-redirect
// branching for the "needs step-up" outcome.
//
// A member with NO confirmed MFA enrollment always passes through
// unconditionally — there is nothing to step up FROM (the same reasoning
// finishLogin's own doc applies to a password-only member); without this
// check, EVERY member's session would eventually go "stale" past
// stepUpFreshnessWindow and get stuck unable to satisfy a step-up prompt
// they have no second factor to complete.
//
// landingPath is where the member lands after completing (or already
// satisfying) the step-up: RequireStepUp never attempts to replay the
// original request, which may have been a POST mutation a GET redirect
// cannot resubmit — the caller supplies a safe page its own route group can
// be re-entered from (e.g. "/settings").
func RequireStepUp(sm *scs.SessionManager, mfa mfaStatusChecker, landingPath string, logger *slog.Logger) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			member, ok := CurrentMember(r.Context())
			if !ok {
				// RequireMember (which must run before this) should already
				// have handled the unauthenticated case — defensive fallback
				// only.
				http.Redirect(w, r, "/login?next="+escapePath(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}

			enrollment, err := mfa.Status(r.Context(), member.ID)
			if err != nil {
				logger.ErrorContext(r.Context(), "step-up: mfa status", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			if !enrollment.Confirmed() {
				next.ServeHTTP(w, r)
				return
			}
			if verifiedAt, ok := MFAVerifiedAt(r.Context(), sm); ok && time.Since(verifiedAt) <= stepUpFreshnessWindow {
				next.ServeHTTP(w, r)
				return
			}

			if r.Header.Get("HX-Request") == "true" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// Re-prompt the second factor directly — the member is already
			// authenticated, so there is no need to re-enter their
			// password, mirroring Handlers.Login's own pending-MFA handoff.
			sm.Put(r.Context(), sessionKeyMFAPendingMemberID, member.ID.String())
			http.Redirect(w, r, "/login/mfa?next="+url.QueryEscape(landingPath), http.StatusSeeOther)
		})
	}
}
