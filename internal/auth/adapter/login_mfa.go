package adapter

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"

	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/web/components"
)

// RememberDeviceCookieName is the "remember this device" cookie set on a
// successful login-MFA verification when the member opts in (NES-135). It
// is read back by Handlers.hasRememberedDevice on a SUBSEQUENT login to
// skip the MFA prompt for up to authapp.RememberDeviceTTL.
const RememberDeviceCookieName = "nestova_remember"

// loginMFAErrorMessage is the generic, non-enumerating message shown for
// any wrong login-MFA credential, mirroring genericMFAError's settings-page
// convention (mfa_web.go): it must not distinguish a wrong TOTP code from a
// wrong recovery code from a replayed code.
const loginMFAErrorMessage = "That code could not be verified. Please try again."

// loginMFALockedMessage is shown while a member is in the attempt
// limiter's backoff window (NES-135's NES-86 gap closure).
const loginMFALockedMessage = "Too many incorrect codes. Please wait a few minutes and try again."

// loginMFALockoutNotificationTitle/Body are the outbox notification raised
// on the (threshold+1)th consecutive wrong login MFA code — the member is
// told through the SAME channel every other Nestova notification uses, not
// a special-cased email/SMS side channel.
const (
	loginMFALockoutNotificationTitle = "Sign-in verification temporarily locked"
	loginMFALockoutNotificationBody  = "Too many incorrect two-factor codes were entered while signing in to your account. Verification is temporarily locked; try again in a few minutes."
)

// LoginMFAHandlers serves the pre-auth login MFA step (NES-135): GET
// /login/mfa renders the code-entry form for the member whose password
// already verified (Handlers.Login) or whose session went stale
// (RequireStepUp, session.go), and POST /login/mfa verifies the submitted
// TOTP or recovery code and promotes the session on success.
type LoginMFAHandlers struct {
	sm       *scs.SessionManager
	mfa      *authapp.MFAService
	remember *authapp.RememberDeviceSigner
	notify   notifydomain.Enqueuer
	limiter  *loginAttemptLimiter
	secure   bool
	logger   *slog.Logger
}

// NewLoginMFAHandlers constructs LoginMFAHandlers with all required
// dependencies; it panics when any is nil. secure mirrors the session
// cookie's SESSION_COOKIE_SECURE policy (cfg.Session.Secure), applied to
// the remember-device cookie too.
func NewLoginMFAHandlers(sm *scs.SessionManager, mfa *authapp.MFAService, remember *authapp.RememberDeviceSigner, notify notifydomain.Enqueuer, secure bool, logger *slog.Logger) *LoginMFAHandlers {
	if sm == nil {
		panic("auth/adapter: NewLoginMFAHandlers requires a non-nil session manager")
	}
	if mfa == nil {
		panic("auth/adapter: NewLoginMFAHandlers requires a non-nil MFAService")
	}
	if remember == nil {
		panic("auth/adapter: NewLoginMFAHandlers requires a non-nil RememberDeviceSigner")
	}
	if notify == nil {
		panic("auth/adapter: NewLoginMFAHandlers requires a non-nil notify Enqueuer")
	}
	if logger == nil {
		panic("auth/adapter: NewLoginMFAHandlers requires a non-nil logger")
	}
	return &LoginMFAHandlers{
		sm: sm, mfa: mfa, remember: remember, notify: notify,
		limiter: newLoginAttemptLimiter(), secure: secure, logger: logger,
	}
}

// Page handles GET /login/mfa: it renders the code-entry form for the
// member left pending by Handlers.Login or RequireStepUp. A request with no
// pending state (e.g. a direct visit, or a page reload after the flow
// already completed) redirects to /login — there is nothing to verify.
func (h *LoginMFAHandlers) Page(w http.ResponseWriter, r *http.Request) {
	memberID, ok := h.pendingMember(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	next := sanitizeNext(r.URL.Query().Get("next"))
	errMsg := ""
	if h.limiter.locked(memberID.String(), time.Now()) {
		errMsg = loginMFALockedMessage
	}
	h.render(w, r, http.StatusOK, components.LoginMFAForm{
		CSRFToken: GetCSRFToken(r.Context(), h.sm),
		Next:      next,
		Error:     errMsg,
	})
}

// Verify handles POST /login/mfa: it verifies CSRF, resolves the pending
// member, and — unless the member is currently in the attempt limiter's
// backoff window — verifies the submitted TOTP or recovery code. On success
// it promotes the session (finishLogin, verified=true) and, when the
// member checked "remember this device", sets the remember-device cookie.
// On a wrong code it re-renders the form with a generic error; the
// (threshold+1)th consecutive wrong code additionally enqueues a
// notification to the member via the outbox (NES-86 gap closure).
func (h *LoginMFAHandlers) Verify(w http.ResponseWriter, r *http.Request) {
	memberID, ok := h.pendingMember(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	next := sanitizeNext(r.FormValue("next"))
	memberKey := memberID.String()
	now := time.Now()

	if h.limiter.locked(memberKey, now) {
		h.render(w, r, http.StatusTooManyRequests, components.LoginMFAForm{
			CSRFToken: GetCSRFToken(r.Context(), h.sm),
			Next:      next,
			Error:     loginMFALockedMessage,
		})
		return
	}

	totpCode := r.FormValue("code")
	recoveryCode := r.FormValue("recovery_code")
	verifyErr := h.mfa.VerifyLoginCode(r.Context(), memberID, totpCode, recoveryCode)
	if verifyErr != nil {
		if !isExpectedMFAError(verifyErr) {
			h.logger.ErrorContext(r.Context(), "login mfa verify", "error", verifyErr)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if h.limiter.recordFailure(memberKey, now) {
			h.notifyLockout(r.Context(), memberID)
		}
		h.render(w, r, http.StatusUnauthorized, components.LoginMFAForm{
			CSRFToken: GetCSRFToken(r.Context(), h.sm),
			Next:      next,
			Error:     loginMFAErrorMessage,
		})
		return
	}

	h.limiter.recordSuccess(memberKey)
	if err := finishLogin(r.Context(), h.sm, memberID, true); err != nil {
		h.logger.ErrorContext(r.Context(), "renew session token", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if r.FormValue("remember_device") != "" {
		h.setRememberDeviceCookie(w, memberID, now)
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// pendingMember resolves the memberID Handlers.Login (or RequireStepUp)
// left pending in the session, clearing a malformed value defensively (it
// can never become valid).
func (h *LoginMFAHandlers) pendingMember(r *http.Request) (household.MemberID, bool) {
	idStr := h.sm.GetString(r.Context(), sessionKeyMFAPendingMemberID)
	if idStr == "" {
		return household.MemberID{}, false
	}
	memberID, err := household.ParseMemberID(idStr)
	if err != nil {
		h.sm.Remove(r.Context(), sessionKeyMFAPendingMemberID)
		return household.MemberID{}, false
	}
	return memberID, true
}

// notifyLockout enqueues the lockout notification for memberID, resolving
// its household via the SAME MFAService.Status call the rest of this file
// already depends on (no new dependency needed). A failure to resolve the
// enrollment (e.g. it was disenrolled mid-flow — unlikely, but not
// impossible) or to enqueue is logged and otherwise swallowed: a lockout
// notification is a best-effort courtesy, not something that should turn an
// already-failed verification into a 500.
func (h *LoginMFAHandlers) notifyLockout(ctx context.Context, memberID household.MemberID) {
	enrollment, err := h.mfa.Status(ctx, memberID)
	if err != nil || !enrollment.Confirmed() {
		if err != nil {
			h.logger.ErrorContext(ctx, "login mfa lockout: resolve household", "error", err)
		}
		return
	}
	n := &notifydomain.Notification{
		ID:           notifydomain.NewNotificationID(),
		HouseholdID:  enrollment.HouseholdID,
		MemberID:     &memberID,
		Channel:      notifydomain.ChannelInApp,
		Title:        loginMFALockoutNotificationTitle,
		Body:         loginMFALockoutNotificationBody,
		ScheduledFor: time.Now(),
		Status:       notifydomain.StatusPending,
	}
	if err := h.notify.Enqueue(ctx, n); err != nil {
		h.logger.ErrorContext(ctx, "login mfa lockout: enqueue notification", "error", err)
	}
}

// setRememberDeviceCookie signs and sets the "remember this device" cookie
// for memberID, valid for authapp.RememberDeviceTTL.
func (h *LoginMFAHandlers) setRememberDeviceCookie(w http.ResponseWriter, memberID household.MemberID, now time.Time) {
	token := h.remember.Sign(memberID, now)
	http.SetCookie(w, &http.Cookie{
		Name:     RememberDeviceCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(authapp.RememberDeviceTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// render renders the login MFA page component directly (without the full
// app shell — mirroring Handlers.renderLoginPage, since a pre-login page
// has no authenticated member to build a sidebar for).
func (h *LoginMFAHandlers) render(w http.ResponseWriter, r *http.Request, status int, form components.LoginMFAForm) {
	if err := render.Render(r.Context(), w, status, components.LoginMFAPage(form)); err != nil {
		h.logger.ErrorContext(r.Context(), "render login mfa page", "error", err)
	}
}
