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
	"time"

	"github.com/alexedwards/scs/v2"

	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/web/components"
)

const (
	// sessionKeyMemberID is the session key storing the authenticated MemberID.
	sessionKeyMemberID = "member_id"
	// sessionKeyCSRF is the session key storing the per-session CSRF token.
	sessionKeyCSRF = "csrf_token"
	// sessionKeyMFAPendingMemberID stores the MemberID awaiting a login MFA
	// step (NES-135): set by Login when a confirmed enrollment requires a
	// second factor and no valid remembered-device cookie was presented, and
	// by RequireStepUp (session.go) when a stale session needs to re-prove
	// freshness. Cleared by finishLogin on success. It lives on the SAME
	// (possibly anonymous, pre-login) session sm.LoadAndSave already wraps
	// the whole mux with (cmd/server/main.go) — never a separate store.
	sessionKeyMFAPendingMemberID = "mfa_pending_member_id"
	// sessionKeyMFAVerifiedAt stores WHEN the current session last completed
	// a login MFA verification — or, for a password-only member (nothing to
	// step up from), when the session was established at all. See
	// finishLogin's doc for exactly when it is (and is not) stamped, and
	// RequireStepUp (session.go) for how it is consumed.
	sessionKeyMFAVerifiedAt = "mfa_verified_at"
	// csrfTokenLen is the length of the generated CSRF token in bytes (produces
	// a 64-character hex string).
	csrfTokenLen = 32
)

// Handlers holds the HTTP handler methods for the auth context (login page,
// login form submission, logout).
type Handlers struct {
	sm       *scs.SessionManager
	authn    *authapp.Authenticator
	mfa      *authapp.MFAService
	remember *authapp.RememberDeviceSigner
	logger   *slog.Logger
}

// NewHandlers constructs auth Handlers. sm, authn, and logger are required
// and panic when nil, matching this codebase's usual "every dependency is
// required, fail fast at construction" DI convention.
//
// mfa and remember (NES-135) are a DELIBERATE exception to that convention:
// they MAY be nil. A nil mfa disables login MFA gating entirely, so Login
// behaves exactly as it did before NES-135. This exists purely to bound the
// blast radius of adding these two params: Handlers is constructed in ~20
// otherwise-unrelated cmd/server test files that have no need to wire the
// MFA context, and making mfa/remember required would force every one of
// them to build a full MFAService (repo/cipher/totp/cred/household fakes)
// just to compile. The real server composition root (cmd/server/main.go)
// always supplies both together — nil is a test-harness accommodation, not
// a supported production configuration.
func NewHandlers(sm *scs.SessionManager, authn *authapp.Authenticator, mfa *authapp.MFAService, remember *authapp.RememberDeviceSigner, logger *slog.Logger) *Handlers {
	if sm == nil {
		panic("adapter: NewHandlers requires a non-nil session manager")
	}
	if authn == nil {
		panic("adapter: NewHandlers requires a non-nil Authenticator")
	}
	if logger == nil {
		panic("adapter: NewHandlers requires a non-nil logger")
	}
	return &Handlers{sm: sm, authn: authn, mfa: mfa, remember: remember, logger: logger}
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
// authenticates via the Authenticator, and on success either promotes the
// session directly or (NES-135) hands off to the login MFA step. On a
// password failure it re-renders the login page with a generic error
// message (no enumeration).
//
// NES-135 MFA gate: a member with a CONFIRMED enrollment must complete a
// second factor before the session is promoted, UNLESS a valid
// remembered-device cookie naming THIS member is presented — in which case
// the prompt is skipped, but the session is NOT marked as freshly
// MFA-verified (see finishLogin's doc), so RequireStepUp still demands the
// prompt again for a security-sensitive action. A member with no confirmed
// enrollment (or when h.mfa is nil — see NewHandlers's doc) logs in
// unchanged.
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

	if h.mfa != nil {
		confirmed, err := h.hasConfirmedMFA(r.Context(), memberID)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "mfa status check", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if confirmed {
			if h.hasRememberedDevice(r, memberID) {
				if err := finishLogin(r.Context(), h.sm, memberID, false); err != nil {
					h.logger.ErrorContext(r.Context(), "renew session token", "error", err)
					http.Error(w, "internal server error", http.StatusInternalServerError)
					return
				}
				http.Redirect(w, r, next, http.StatusSeeOther)
				return
			}
			// Renew the session token before parking the pending-MFA state:
			// password verification is itself a privilege escalation (from
			// fully anonymous to "password proven"), so it gets the same
			// session-fixation defense as a completed login.
			if err := h.sm.RenewToken(r.Context()); err != nil {
				h.logger.ErrorContext(r.Context(), "renew session token", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			h.sm.Put(r.Context(), sessionKeyMFAPendingMemberID, memberID.String())
			http.Redirect(w, r, "/login/mfa?next="+url.QueryEscape(next), http.StatusSeeOther)
			return
		}
	}

	if err := finishLogin(r.Context(), h.sm, memberID, true); err != nil {
		h.logger.ErrorContext(r.Context(), "renew session token", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// hasConfirmedMFA reports whether memberID has a CONFIRMED MFA enrollment.
func (h *Handlers) hasConfirmedMFA(ctx context.Context, memberID household.MemberID) (bool, error) {
	enrollment, err := h.mfa.Status(ctx, memberID)
	if err != nil {
		return false, err
	}
	return enrollment.Confirmed(), nil
}

// hasRememberedDevice reports whether r carries a valid, unexpired
// remember-device cookie naming memberID specifically — a cookie belonging
// to a DIFFERENT member (e.g. a shared household device where someone else
// last checked "remember this device") must not skip THIS member's MFA
// step.
func (h *Handlers) hasRememberedDevice(r *http.Request, memberID household.MemberID) bool {
	if h.remember == nil {
		return false
	}
	cookie, err := r.Cookie(RememberDeviceCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	rememberedID, err := h.remember.Verify(cookie.Value, time.Now())
	return err == nil && rememberedID == memberID
}

// finishLogin promotes the session to authenticated by memberID: renews the
// session token (session-fixation defense on this privilege escalation),
// stores member_id, and clears any pending login-MFA state. It is a
// package-level function (not a Handlers method) so both Handlers.Login and
// LoginMFAHandlers.Verify can share it without either type needing a
// reference to the other.
//
// When verified is true, it also stamps mfa_verified_at = now, marking the
// session fresh for RequireStepUp — true for a password-only member
// (nothing to step up FROM, so every future action is already at that
// member's security ceiling) and for a member who just completed the login
// MFA step THIS session. When verified is false (a member admitted via a
// remembered-device cookie, who skipped the login prompt), any existing
// mfa_verified_at is cleared instead, so RequireStepUp still demands the
// prompt when a step-up-gated action is reached — the NES-135 acceptance
// criterion that a remembered device "still gets prompted for step-up
// actions".
func finishLogin(ctx context.Context, sm *scs.SessionManager, memberID household.MemberID, verified bool) error {
	if err := sm.RenewToken(ctx); err != nil {
		return err
	}
	sm.Put(ctx, sessionKeyMemberID, memberID.String())
	if verified {
		sm.Put(ctx, sessionKeyMFAVerifiedAt, time.Now())
	} else {
		sm.Remove(ctx, sessionKeyMFAVerifiedAt)
	}
	sm.Remove(ctx, sessionKeyMFAPendingMemberID)
	return nil
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

// VerifyCSRF performs a constant-time comparison of the form's csrf_token field
// against the value stored in the session. It returns false when either value is
// absent. The caller must have already parsed the form (e.g. via r.ParseForm())
// before calling this function. Exported so handlers outside this package (e.g.
// OnboardingHandlers, member handlers) can reuse the same CSRF check without
// duplicating the logic.
func VerifyCSRF(r *http.Request, sm *scs.SessionManager) bool {
	sessionToken := sm.GetString(r.Context(), sessionKeyCSRF)
	formToken := r.FormValue("csrf_token")
	if sessionToken == "" || formToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sessionToken), []byte(formToken)) == 1
}

// checkCSRF is the unexported per-receiver helper that delegates to VerifyCSRF
// using the Handlers session manager.
func (h *Handlers) checkCSRF(r *http.Request) bool {
	return VerifyCSRF(r, h.sm)
}

// sanitizeNext ensures the post-login redirect target is a safe same-origin path
// to prevent open-redirect attacks. It parses next, rejects anything absolute or
// host-bearing (including protocol-relative // and percent-encoded variants),
// rejects any backslash (literal or percent-encoded — see below), and
// path-normalizes to collapse traversal sequences (e.g. "/foo/..//evil.com" that
// a browser would normalize to the protocol-relative "//evil.com"). Anything
// suspicious falls back to the dashboard root "/".
//
// The backslash check runs BEFORE path.Clean and rejects outright rather than
// stripping: path.Clean only treats '/' as a path separator, so a backslash
// (e.g. "/\evil.example/steal") survives it completely unchanged — but
// browsers normalize '\' to '/' when resolving a URL, so that exact string
// would still be handed to http.Redirect verbatim and then be followed by the
// browser as the protocol-relative "//evil.example/steal", an off-origin
// redirect this function's other checks exist specifically to prevent.
// url.Parse already percent-decodes the path into u.Path, so checking u.Path
// (rather than the raw next string) catches both a literal backslash and its
// percent-encoded form ("%5C") with the same check.
func sanitizeNext(next string) string {
	if next == "" {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") || strings.ContainsRune(u.Path, '\\') {
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
