package adapter

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/alexedwards/scs/v2"

	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/web/components"
)

const (
	// sessionKeyMemberID is the session key storing the authenticated MemberID.
	sessionKeyMemberID = "member_id"
	// sessionKeyCSRF is the session key storing the per-session CSRF token.
	sessionKeyCSRF = "csrf_token"
	// csrfTokenLen is the length of the generated CSRF token in bytes (produces
	// a 64-character hex string).
	csrfTokenLen = 32
)

// Handlers holds the HTTP handler methods for the auth context (login page,
// login form submission, logout).
type Handlers struct {
	sm     *scs.SessionManager
	authn  *authapp.Authenticator
	logger *slog.Logger
}

// NewHandlers constructs auth Handlers. All three dependencies are required.
func NewHandlers(sm *scs.SessionManager, authn *authapp.Authenticator, logger *slog.Logger) *Handlers {
	if sm == nil {
		panic("adapter: NewHandlers requires a non-nil session manager")
	}
	if authn == nil {
		panic("adapter: NewHandlers requires a non-nil Authenticator")
	}
	if logger == nil {
		panic("adapter: NewHandlers requires a non-nil logger")
	}
	return &Handlers{sm: sm, authn: authn, logger: logger}
}

// GetCSRFToken returns the per-session CSRF token stored in the session,
// generating and storing a new 32-byte random hex token when none exists yet.
// Exported so callers (e.g. handlers that build forms outside this package)
// can embed the token in templates.
func GetCSRFToken(ctx context.Context, sm *scs.SessionManager) string {
	token := sm.GetString(ctx, sessionKeyCSRF)
	if token != "" {
		return token
	}
	b := make([]byte, csrfTokenLen)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is not recoverable; return a placeholder so the
		// form does not break, but the CSRF check will always fail, which is
		// the safe outcome.
		return ""
	}
	token = hex.EncodeToString(b)
	sm.Put(ctx, sessionKeyCSRF, token)
	return token
}

// LoginPage handles GET /login — it renders the login form with a CSRF token
// and an optional `next` redirect parameter. The login page uses a bare HTML
// wrapper (not the full app shell) because the sidebar requires an
// authenticated member.
func (h *Handlers) LoginPage(w http.ResponseWriter, r *http.Request) {
	token := GetCSRFToken(r.Context(), h.sm)
	next := r.URL.Query().Get("next")
	h.renderLoginPage(w, r, http.StatusOK, components.LoginForm{
		CSRFToken: token,
		Next:      next,
	})
}

// Login handles POST /login — it verifies the CSRF token, reads credentials,
// authenticates via the Authenticator, and on success issues a renewed session
// and redirects to the `next` URL. On failure it re-renders the login page
// with a generic error message (no enumeration).
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if !h.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	next := sanitizeNext(r.FormValue("next"))

	memberID, err := h.authn.Login(r.Context(), email, password)
	if err != nil {
		if errors.Is(err, authdomain.ErrInvalidCredentials) {
			token := GetCSRFToken(r.Context(), h.sm)
			h.renderLoginPage(w, r, http.StatusUnauthorized, components.LoginForm{
				CSRFToken: token,
				Next:      next,
				Email:     email,
				Error:     "Invalid email or password.",
			})
			return
		}
		h.logger.ErrorContext(r.Context(), "login internal error", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Renew the session token on privilege escalation to prevent session fixation.
	if err := h.sm.RenewToken(r.Context()); err != nil {
		h.logger.ErrorContext(r.Context(), "renew session token", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	h.sm.Put(r.Context(), sessionKeyMemberID, memberID.String())
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// Logout handles POST /logout — it verifies the CSRF token, destroys the
// session entirely, and redirects to /login.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !h.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if err := h.sm.Destroy(r.Context()); err != nil {
		h.logger.ErrorContext(r.Context(), "destroy session", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// renderLoginPage renders the login page component directly (without the full
// app shell) at the given status code.
func (h *Handlers) renderLoginPage(w http.ResponseWriter, r *http.Request, status int, form components.LoginForm) {
	if err := render.Render(r.Context(), w, status, components.LoginPage(form)); err != nil {
		h.logger.ErrorContext(r.Context(), "render login page", "error", err)
	}
}

// checkCSRF performs a constant-time comparison of the form's csrf_token field
// against the value stored in the session. Returns false when either value is
// absent.
func (h *Handlers) checkCSRF(r *http.Request) bool {
	sessionToken := h.sm.GetString(r.Context(), sessionKeyCSRF)
	formToken := r.FormValue("csrf_token")
	if sessionToken == "" || formToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sessionToken), []byte(formToken)) == 1
}

// sanitizeNext ensures the post-login redirect target is a safe same-origin path
// to prevent open-redirect attacks. It parses next, rejects anything absolute or
// host-bearing (including protocol-relative // and percent-encoded variants), and
// path-normalizes to collapse traversal sequences (e.g. "/foo/..//evil.com" that
// a browser would normalize to the protocol-relative "//evil.com"). Anything
// suspicious falls back to the dashboard root "/".
func sanitizeNext(next string) string {
	if next == "" {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") {
		return "/"
	}
	cleaned := path.Clean(u.Path)
	if !strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "//") {
		return "/"
	}
	if u.RawQuery != "" {
		cleaned += "?" + u.RawQuery
	}
	return cleaned
}
