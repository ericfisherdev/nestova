package adapter

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
)

// sessionKeyWebAuthnLoginChallenge stores the pending USERNAMELESS login
// webauthn.SessionData (NES-137) on the anonymous pre-auth session — the
// same session sm.LoadAndSave already wraps the whole mux with, alive
// before any login (mirroring sessionKeyMFAPendingMemberID's own pre-auth
// storage, http.go). Distinct from sessionKeyWebAuthnRegChallenge
// (registration, always post-auth) and sessionKeyWebAuthnStepUpChallenge
// (step-up, always post-auth, login_mfa.go): a stale challenge from one
// ceremony must never accidentally satisfy a different one. Cleared after
// exactly one Finish attempt, win or lose, so a challenge is never usable
// twice.
const sessionKeyWebAuthnLoginChallenge = "webauthn_login_challenge"

// webauthnLoginErrorMessage is the generic, non-enumerating message shown
// for any failed login ceremony attempt — a wrong RP ID/origin, an
// expired/replayed challenge, and an unrecognized passkey are all reported
// identically (see WebAuthnService.FinishLogin's own no-oracle doc).
const webauthnLoginErrorMessage = "Passkey sign-in could not be completed. Please try again or use your password."

// LoginPasskeyHandlers serves the pre-auth, usernameless "Sign in with
// passkey" ceremony (NES-137): GET /login/passkey/begin returns assertion
// options for the browser's navigator.credentials.get() with an EMPTY
// allowCredentials (any of the browser's discoverable credentials for this
// Relying Party may be offered — the server does not yet know who is
// signing in), and POST /login/passkey/finish verifies the browser's
// response, resolves which member it belongs to from the assertion's own
// reported user handle, and promotes the session exactly like a completed
// password+MFA login (finishLogin, verified=true — a user-verified passkey
// assertion counts as both factors in one gesture, skipping NES-135's
// pending-MFA hand-off entirely).
//
// Distinct from WebAuthnWebHandlers (NES-136, settings-page registration/
// device management) and from LoginMFAHandlers' step-up passkey methods
// (login_mfa.go): this type ONLY drives the pre-auth login ceremony.
type LoginPasskeyHandlers struct {
	sm       *scs.SessionManager
	webauthn *authapp.WebAuthnService
	logger   *slog.Logger
}

// NewLoginPasskeyHandlers constructs LoginPasskeyHandlers with all required
// dependencies; it panics when any is nil — mirroring
// NewWebAuthnWebHandlers, this type is only ever constructed at all when
// the composition root has already decided WebAuthn is wired (see
// cmd/server/main.go's Server.PublicBaseURL guard), so webauthnService is
// never optional here.
func NewLoginPasskeyHandlers(sm *scs.SessionManager, webauthnService *authapp.WebAuthnService, logger *slog.Logger) *LoginPasskeyHandlers {
	if sm == nil {
		panic("auth/adapter: NewLoginPasskeyHandlers requires a non-nil session manager")
	}
	if webauthnService == nil {
		panic("auth/adapter: NewLoginPasskeyHandlers requires a non-nil WebAuthnService")
	}
	if logger == nil {
		panic("auth/adapter: NewLoginPasskeyHandlers requires a non-nil logger")
	}
	return &LoginPasskeyHandlers{sm: sm, webauthn: webauthnService, logger: logger}
}

// Begin handles GET /login/passkey/begin: starts a new usernameless login
// ceremony and stores the pending challenge on the pre-auth session. Not
// CSRF-protected — a GET that stores only a random challenge on the
// requester's OWN session changes nothing an attacker could exploit
// cross-site (mirroring RequireMember's own GET routes' convention of
// leaving safe/idempotent requests unchecked).
func (h *LoginPasskeyHandlers) Begin(w http.ResponseWriter, r *http.Request) {
	assertion, session, err := h.webauthn.BeginLogin(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "webauthn: begin login", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Stored by value, not *webauthn.SessionData: scs serializes session
	// data via gob, which requires the CONCRETE type stored through Put's
	// interface{} parameter to be registered up front — see session.go's
	// init(), gob.Register(webauthn.SessionData{}), already registered for
	// sessionKeyWebAuthnRegChallenge (NES-136) and reused here.
	h.sm.Put(r.Context(), sessionKeyWebAuthnLoginChallenge, *session)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(assertion); err != nil {
		h.logger.ErrorContext(r.Context(), "webauthn: encode assertion options", "error", err)
	}
}

// Finish handles POST /login/passkey/finish: verifies the browser's
// assertion response against the pending challenge, resolves and promotes
// the member's session on success, and reports a server-computed, sanitized
// redirect target in the JSON response — the client must navigate to THAT
// value, not to its own copy of the `next` query parameter, since a
// client-supplied redirect target is exactly the open-redirect risk
// sanitizeNext exists to close (see LoginPage's own `next` handling; unlike
// a form POST, this JSON endpoint has no server-rendered page to embed an
// already-sanitized hidden field into, so the sanitized value travels back
// in the response instead).
//
// The pending challenge is popped (read-and-remove) from the session BEFORE
// verification is attempted, unconditionally — mirroring
// WebAuthnWebHandlers.RegisterFinish's own single-use-regardless-of-outcome
// contract.
func (h *LoginPasskeyHandlers) Finish(w http.ResponseWriter, r *http.Request) {
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	sessionVal := h.sm.Pop(r.Context(), sessionKeyWebAuthnLoginChallenge)
	session, ok := sessionVal.(webauthn.SessionData)
	if !ok {
		// No pending challenge (never started, already consumed by a prior
		// finish attempt, or the session predates this ceremony) — reported
		// identically to any other verification failure, no oracle.
		http.Error(w, webauthnLoginErrorMessage, http.StatusBadRequest)
		return
	}

	parsed, err := protocol.ParseCredentialRequestResponseBody(r.Body)
	if err != nil {
		http.Error(w, webauthnLoginErrorMessage, http.StatusBadRequest)
		return
	}

	memberID, err := h.webauthn.FinishLogin(r.Context(), session, parsed)
	if err != nil {
		if errors.Is(err, authdomain.ErrWebAuthnVerificationFailed) {
			http.Error(w, webauthnLoginErrorMessage, http.StatusUnauthorized)
			return
		}
		h.logger.ErrorContext(r.Context(), "webauthn: finish login", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := finishLogin(r.Context(), h.sm, memberID, true); err != nil {
		h.logger.ErrorContext(r.Context(), "renew session token", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	redirect := sanitizeNext(r.URL.Query().Get("next"))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"redirect": redirect})
}
