package adapter

import (
	"context"
	"net/http"
	"net/url"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver/middleware"
)

// sessionContextKey is the unexported type for context keys in this package.
type sessionContextKey int

// currentMemberKey stores the authenticated Member in the request context.
const currentMemberKey sessionContextKey = iota

// NewSessionManager constructs an scs.SessionManager backed by Postgres using
// the pgxpool shared with the rest of the application. Cookie settings are
// derived from cfg: Secure is set in production only, Lifetime from
// SESSION_LIFETIME.
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
				memberID, err := household.ParseMemberID(memberIDStr)
				if err == nil {
					member, err := members.GetMember(r.Context(), memberID)
					if err == nil {
						ctx := context.WithValue(r.Context(), currentMemberKey, member)
						r = r.WithContext(ctx)
					}
					// If GetMember fails (e.g. member deleted) the session key is
					// stale; proceed as anonymous rather than returning an error so
					// the user can still access the login page.
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
