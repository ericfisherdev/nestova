package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
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

// sessionKeyWebAuthnStepUpChallenge stores the pending TARGETED step-up
// webauthn.SessionData (NES-137) on the current AUTHENTICATED member's own
// session — unlike sessionKeyWebAuthnLoginChallenge (always pre-auth), a
// step-up ceremony only ever runs for a member who is already
// authenticated (RequireStepUp, session.go). Distinct from BOTH
// sessionKeyWebAuthnLoginChallenge and sessionKeyWebAuthnRegChallenge
// (webauthn_web.go) so a stale challenge from one ceremony can never
// accidentally satisfy a different one. Cleared after exactly one
// PasskeyFinish attempt, win or lose.
const sessionKeyWebAuthnStepUpChallenge = "webauthn_stepup_challenge"

// LoginMFAHandlers serves the pre-auth login MFA step (NES-135) — TOTP,
// recovery code, or (NES-137) a passkey assertion: GET /login/mfa renders
// the code-entry form for the member whose password already verified
// (Handlers.Login) or whose session went stale (RequireStepUp,
// session.go), POST /login/mfa verifies a submitted TOTP or recovery code
// and promotes the session on success, and PasskeyBegin/PasskeyFinish drive
// the SAME pending-member hand-off via a passkey assertion instead — a
// user-verified assertion satisfies step-up exactly like a correct TOTP
// code does (finishLogin, verified=true).
type LoginMFAHandlers struct {
	sm       *scs.SessionManager
	mfa      *authapp.MFAService
	remember *authapp.RememberDeviceSigner
	// webauthn is OPTIONAL (nil allowed) — mirroring Handlers' own mfa/
	// remember exception (see NewHandlers' doc): a deployment with no
	// Server.PublicBaseURL configured never wires WebAuthn at all
	// (cmd/server/main.go), so the "use your passkey" option on
	// /login/mfa, and PasskeyBegin/PasskeyFinish, are simply unavailable —
	// Page never offers it and the two methods 500 defensively if somehow
	// reached (the composition root never registers their routes when
	// webauthn is nil, mirroring registerSettingsPage's own
	// webauthnHandlers-nil convention).
	webauthn *authapp.WebAuthnService
	notify   notifydomain.Enqueuer
	limiter  *loginAttemptLimiter
	secure   bool
	logger   *slog.Logger
}

// NewLoginMFAHandlers constructs LoginMFAHandlers. sm, mfa, remember,
// notify, and logger are required and panic when nil; webauthn is the
// deliberate exception (see its own field doc). secure mirrors the session
// cookie's SESSION_COOKIE_SECURE policy (cfg.Session.Secure), applied to
// the remember-device cookie too.
func NewLoginMFAHandlers(sm *scs.SessionManager, mfa *authapp.MFAService, remember *authapp.RememberDeviceSigner, webauthnService *authapp.WebAuthnService, notify notifydomain.Enqueuer, secure bool, logger *slog.Logger) *LoginMFAHandlers {
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
		sm: sm, mfa: mfa, remember: remember, webauthn: webauthnService, notify: notify,
		limiter: newLoginAttemptLimiter(), secure: secure, logger: logger,
	}
}

// Page handles GET /login/mfa: it renders the code-entry form for the
// member left pending by Handlers.Login or RequireStepUp. A request with no
// pending state (e.g. a direct visit, or a page reload after the flow
// already completed) redirects to /login — there is nothing to verify.
//
// When h.webauthn is wired AND memberID has at least one registered
// passkey, the form additionally offers "use your passkey" (NES-137) —
// checked here, not left to the client to assume, so a member with no
// passkeys never sees an option that could only fail.
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
		CSRFToken:  GetCSRFToken(r.Context(), h.sm),
		Next:       next,
		Error:      errMsg,
		HasPasskey: h.hasPasskey(r, memberID),
	})
}

// hasPasskey reports whether memberID has at least one registered passkey,
// false whenever h.webauthn is nil (WebAuthn not wired at all) or the
// lookup itself fails — a lookup failure hides the option rather than
// erroring the whole page, mirroring how a soft display concern degrades
// elsewhere in this codebase (e.g. MFAWebHandlers.SectionView's own
// failure handling one layer up).
func (h *LoginMFAHandlers) hasPasskey(r *http.Request, memberID household.MemberID) bool {
	if h.webauthn == nil {
		return false
	}
	creds, err := h.webauthn.ListDevices(r.Context(), memberID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "login mfa: list passkeys", "error", err)
		return false
	}
	return len(creds) > 0
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
			CSRFToken:  GetCSRFToken(r.Context(), h.sm),
			Next:       next,
			Error:      loginMFALockedMessage,
			HasPasskey: h.hasPasskey(r, memberID),
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
			CSRFToken:  GetCSRFToken(r.Context(), h.sm),
			Next:       next,
			Error:      loginMFAErrorMessage,
			HasPasskey: h.hasPasskey(r, memberID),
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

// PasskeyBegin handles GET /login/mfa/passkey/begin: starts a TARGETED
// step-up assertion ceremony for the member Handlers.Login or RequireStepUp
// left pending (unlike LoginPasskeyHandlers.Begin's usernameless ceremony,
// the member is already known here), storing the pending challenge on
// their session. Not CSRF-protected, mirroring LoginPasskeyHandlers.Begin's
// own reasoning (a GET that only stores a challenge on the requester's own
// session). 404s (via http.NotFound, since this route is simply never
// registered when h.webauthn is nil — see the composition root) never
// reaches this method in that configuration, but the defensive check stays
// for direct callers (e.g. tests constructing LoginMFAHandlers with a nil
// webauthn service).
func (h *LoginMFAHandlers) PasskeyBegin(w http.ResponseWriter, r *http.Request) {
	memberID, ok := h.pendingMember(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.webauthn == nil {
		http.NotFound(w, r)
		return
	}

	assertion, session, err := h.webauthn.BeginStepUp(r.Context(), memberID)
	if err != nil {
		if errors.Is(err, authdomain.ErrWebAuthnVerificationFailed) {
			// No registered passkey to assert with — Page's own hasPasskey
			// check should have hidden this option already; report the same
			// generic error a client-side race (revoked the ONLY passkey in
			// another tab, then clicked "use your passkey" here) would need
			// to see anyway.
			http.Error(w, webauthnLoginErrorMessage, http.StatusBadRequest)
			return
		}
		h.logger.ErrorContext(r.Context(), "webauthn: begin step-up", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	h.sm.Put(r.Context(), sessionKeyWebAuthnStepUpChallenge, *session)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(assertion); err != nil {
		h.logger.ErrorContext(r.Context(), "webauthn: encode step-up assertion options", "error", err)
	}
}

// PasskeyFinish handles POST /login/mfa/passkey/finish: verifies the
// browser's assertion response against the pending step-up challenge for
// the member left pending, and — on success — promotes the session exactly
// like a correct TOTP code does (finishLogin, verified=true), reporting a
// server-computed, sanitized redirect target in the JSON response
// (mirroring LoginPasskeyHandlers.Finish's own reasoning). Unlike Verify,
// this never sets the remember-device cookie — "remember this device"
// exists to skip a FUTURE login's MFA prompt entirely, a concept specific
// to Handlers.Login's own hand-off, not to re-proving freshness mid-session.
func (h *LoginMFAHandlers) PasskeyFinish(w http.ResponseWriter, r *http.Request) {
	// A real assertion response is at most a few KB; bounding the body
	// BEFORE anything reads it caps an attacker-supplied body to a fixed,
	// small cost instead of an unbounded one (see
	// maxWebAuthnResponseBodyBytes's own doc, webauthn_web.go).
	r.Body = http.MaxBytesReader(w, r.Body, maxWebAuthnResponseBodyBytes)

	memberID, ok := h.pendingMember(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.webauthn == nil {
		http.NotFound(w, r)
		return
	}
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	sessionVal := h.sm.Pop(r.Context(), sessionKeyWebAuthnStepUpChallenge)
	session, ok := sessionVal.(webauthn.SessionData)
	if !ok {
		http.Error(w, webauthnLoginErrorMessage, http.StatusBadRequest)
		return
	}

	parsed, err := protocol.ParseCredentialRequestResponseBody(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, webauthnLoginErrorMessage, http.StatusBadRequest)
		return
	}

	if err := h.webauthn.VerifyStepUp(r.Context(), memberID, session, parsed); err != nil {
		if errors.Is(err, authdomain.ErrWebAuthnVerificationFailed) {
			http.Error(w, webauthnLoginErrorMessage, http.StatusUnauthorized)
			return
		}
		h.logger.ErrorContext(r.Context(), "webauthn: verify step-up", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := finishLogin(r.Context(), h.sm, memberID, true); err != nil {
		h.logger.ErrorContext(r.Context(), "renew session token", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	next := sanitizeNext(r.URL.Query().Get("next"))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"redirect": next})
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
