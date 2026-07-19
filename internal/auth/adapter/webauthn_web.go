package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// sessionKeyWebAuthnRegChallenge stores the pending registration
// webauthn.SessionData (NES-136) between RegisterBegin and RegisterFinish,
// on the current AUTHENTICATED member's own session — unlike NES-135's
// pre-auth pending-MFA key, registration only ever happens after login
// (RequireStepUp, wired in cmd/server/home.go, gates the route). Cleared
// after exactly one RegisterFinish attempt, win or lose (see RegisterFinish),
// so a challenge is never usable twice — the NES-136 AC that "challenges are
// single-use and expire; replayed registration responses fail".
const sessionKeyWebAuthnRegChallenge = "webauthn_reg_challenge"

// WebAuthnRegChallengeSessionKeyForTests exposes sessionKeyWebAuthnRegChallenge
// to tests outside this package (cmd/server's seedWebAuthnChallenge, which
// injects a pending challenge directly into the session store to drive
// RegisterFinish against a fixed W3C spec test vector's challenge, since a
// real BeginRegistration's random challenge could never match one). Defined
// as an alias of the unexported constant (not a separately maintained
// literal), so a rename of either the key's VALUE or its Go identifier is
// caught here — the former propagates automatically, the latter fails to
// compile — rather than silently drifting out of sync with an external
// test's own copy of the string.
const WebAuthnRegChallengeSessionKeyForTests = sessionKeyWebAuthnRegChallenge

// webauthnRegistrationErrorMessage is the generic, non-enumerating message
// shown for any failed registration attempt, mirroring genericMFAError's
// (mfa_web.go) convention.
const webauthnRegistrationErrorMessage = "Registration could not be completed. Please try again."

// webauthnDisplayDateLayout is the human-readable date layout shown for a
// device's registered/last-used timestamps, matching
// kioskadapter.SettingsWebHandlers's own settingsDisplayDateLayout value
// (a different package, so not directly reusable).
const webauthnDisplayDateLayout = "Jan 2, 2006 3:04 PM"

// WebAuthnWebHandlers serves the auth context's per-member "Your devices"
// section of the shared /settings page (NES-136): the registered-passkey
// list, the begin/finish JSON registration ceremony, and rename/revoke.
// Like MFAWebHandlers, it never writes an HTTP response for a mutation's
// SUCCESS path that needs the OTHER sections recomposed — the composition
// root (cmd/server/home.go's registerSettingsPage) does. Unlike
// MFAWebHandlers, the registration ceremony itself is JS-driven (WebAuthn
// requires navigator.credentials.create(), which no plain HTML form can
// invoke), so RegisterBegin/RegisterFinish are pure JSON endpoints rather
// than composePage-style full-page mutations.
type WebAuthnWebHandlers struct {
	webauthn *authapp.WebAuthnService
	sm       *scs.SessionManager
	logger   *slog.Logger
}

// NewWebAuthnWebHandlers constructs WebAuthnWebHandlers with all required
// dependencies. It panics when any dependency is nil so misconfigured
// composition roots are caught at startup rather than at the first HTTP
// request.
func NewWebAuthnWebHandlers(webauthnService *authapp.WebAuthnService, sm *scs.SessionManager, logger *slog.Logger) *WebAuthnWebHandlers {
	if webauthnService == nil {
		panic("auth/adapter: NewWebAuthnWebHandlers requires a non-nil WebAuthnService")
	}
	if sm == nil {
		panic("auth/adapter: NewWebAuthnWebHandlers requires a non-nil session manager")
	}
	if logger == nil {
		panic("auth/adapter: NewWebAuthnWebHandlers requires a non-nil logger")
	}
	return &WebAuthnWebHandlers{webauthn: webauthnService, sm: sm, logger: logger}
}

// SectionView builds the "Your devices" section's view model for member,
// rendered for every member regardless of role.
func (h *WebAuthnWebHandlers) SectionView(ctx context.Context, member *household.Member) (components.WebAuthnSettingsView, error) {
	creds, err := h.webauthn.ListDevices(ctx, member.ID)
	if err != nil {
		return components.WebAuthnSettingsView{}, err
	}
	view := components.WebAuthnSettingsView{
		CSRFToken: GetCSRFToken(ctx, h.sm),
		BeginURL:  "/settings/webauthn/register/begin",
		FinishURL: "/settings/webauthn/register/finish",
	}
	view.Devices = make([]components.WebAuthnDeviceView, 0, len(creds))
	for _, c := range creds {
		view.Devices = append(view.Devices, toWebAuthnDeviceView(c))
	}
	return view, nil
}

// RegisterBegin handles POST /settings/webauthn/register/begin: verifies
// CSRF (via the X-CSRF-Token header — this is a JSON endpoint with no form
// field to carry it in, see VerifyCSRF's own doc), starts a new
// registration ceremony, stores the pending challenge in the member's own
// session, and writes the credential creation options as JSON for the
// client's navigator.credentials.create() call. This route is
// step-up-gated (cmd/server/home.go's registerSettingsPage) — a stale
// session never reaches this handler at all (NES-136 AC: "registration
// without fresh step-up is rejected").
func (h *WebAuthnWebHandlers) RegisterBegin(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireMember(w, r)
	if !ok {
		return
	}
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	creation, session, err := h.webauthn.BeginRegistration(r.Context(), member.ID, member.DisplayName)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "webauthn: begin registration", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Stored by value (not *webauthn.SessionData): scs serializes session
	// data via gob, which requires the CONCRETE type stored through Put's
	// interface{} parameter to be registered up front (see session.go's
	// init(), gob.Register(webauthn.SessionData{}) — mirroring
	// time.Time's own registration for NES-135's mfa_verified_at).
	h.sm.Put(r.Context(), sessionKeyWebAuthnRegChallenge, *session)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(creation); err != nil {
		h.logger.ErrorContext(r.Context(), "webauthn: encode creation options", "error", err)
	}
}

// webauthnFinishPayload is the JSON body POST /settings/webauthn/register/finish
// expects: the browser's own PublicKeyCredential.toJSON() fields (id, rawId,
// type, response — parsed separately by protocol.ParseCredentialCreationResponseBody,
// which ignores unknown fields, including Nickname below) PLUS the
// member-chosen nickname, all in one JSON object — keeping the nickname out
// of the URL entirely (unlike an earlier version of this code, which used a
// query parameter) without needing a second HTTP header whose value would
// have to survive round-tripping arbitrary member-supplied text (a nickname
// may contain non-ASCII characters a raw header value cannot safely carry).
type webauthnFinishPayload struct {
	Nickname string `json:"nickname"`
}

// RegisterFinish handles POST /settings/webauthn/register/finish: parses
// the browser's attestation response body, verifies it against the pending
// challenge, and persists the new credential under the request body's
// nickname field (see webauthnFinishPayload).
//
// The pending challenge is popped (read-and-remove) from the session
// BEFORE verification is attempted, unconditionally — a challenge is
// single-use whether verification succeeds OR fails, so a captured/replayed
// POST of the same response can never be resubmitted against a still-live
// challenge.
//
// CSRF is verified via the X-CSRF-Token header (see VerifyCSRF's own doc) —
// the request body is JSON, not a form-urlencoded body, so there is no form
// field to carry it in.
//
// On success this ALSO stamps sessionKeyMFAVerifiedAt (NES-137) — the same
// key finishLogin/LoginMFAHandlers.Verify stamp on a completed TOTP/
// recovery-code/passkey step-up: a freshly registered passkey is itself a
// UV-gated proof of possession, at least as strong as a TOTP code, so it
// satisfies RequireStepUp's freshness window for the rest of THIS session
// exactly like completing an explicit step-up prompt would. Without this,
// a member with no OTHER second factor who registers their very FIRST
// passkey would immediately trip RequireStepUp's OWN gate on their very
// next step-up-gated request (RequireStepUp now treats "has a registered
// passkey" as something to step up FROM — see session.go's
// webAuthnDeviceLister) — including, self-referentially, a second call to
// this same route.
func (h *WebAuthnWebHandlers) RegisterFinish(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireMember(w, r)
	if !ok {
		return
	}
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	sessionVal := h.sm.Pop(r.Context(), sessionKeyWebAuthnRegChallenge)
	session, ok := sessionVal.(webauthn.SessionData)
	if !ok {
		// No pending challenge (never started, already consumed by a prior
		// finish attempt, or the session predates this ceremony) — reported
		// identically to any other verification failure, no oracle.
		http.Error(w, webauthnRegistrationErrorMessage, http.StatusBadRequest)
		return
	}

	// Buffered so the same bytes can be read twice: once (below) for the
	// nickname field this handler owns, and once by
	// ParseCredentialCreationResponseBody for the credential fields it
	// owns — protocol.CredentialCreationResponse's JSON unmarshal silently
	// ignores the "nickname" key it does not know about, so both reads see
	// exactly the same wire payload with no risk of the two disagreeing.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var payload webauthnFinishPayload
	_ = json.Unmarshal(body, &payload) // best-effort; a bad/missing nickname just falls back to the service's own default

	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(body))
	if err != nil {
		http.Error(w, webauthnRegistrationErrorMessage, http.StatusBadRequest)
		return
	}

	if err := h.webauthn.FinishRegistration(r.Context(), member.ID, member.HouseholdID, member.DisplayName, payload.Nickname, session, parsed); err != nil {
		if errors.Is(err, authdomain.ErrWebAuthnVerificationFailed) {
			http.Error(w, webauthnRegistrationErrorMessage, http.StatusUnauthorized)
			return
		}
		h.logger.ErrorContext(r.Context(), "webauthn: finish registration", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	h.sm.Put(r.Context(), sessionKeyMFAVerifiedAt, time.Now())

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// Rename handles POST /settings/webauthn/{id}/rename: verifies CSRF and
// updates the credential's nickname. ok=false means a response was already
// written directly; ok=true means the caller should redirect to the
// settings page (mirroring kioskadapter.SettingsWebHandlers.RevokeKioskToken
// — there is no reveal to compose here either).
func (h *WebAuthnWebHandlers) Rename(w http.ResponseWriter, r *http.Request) (member *household.Member, ok bool) {
	member, ok = h.requireMember(w, r)
	if !ok {
		return nil, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, false
	}
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, false
	}
	id, err := authdomain.ParseWebAuthnCredentialID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid credential id", http.StatusBadRequest)
		return nil, false
	}
	if err := h.webauthn.Rename(r.Context(), member.HouseholdID, member.ID, id, r.FormValue("nickname")); err != nil {
		if errors.Is(err, authdomain.ErrWebAuthnCredentialNotFound) {
			http.Error(w, "credential not found", http.StatusNotFound)
			return nil, false
		}
		h.logger.ErrorContext(r.Context(), "webauthn: rename credential", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, false
	}
	return member, true
}

// Revoke handles POST /settings/webauthn/{id}/revoke: verifies CSRF and
// removes the credential immediately (NES-136 AC). Same ok contract as
// Rename.
func (h *WebAuthnWebHandlers) Revoke(w http.ResponseWriter, r *http.Request) (member *household.Member, ok bool) {
	member, ok = h.requireMember(w, r)
	if !ok {
		return nil, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, false
	}
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, false
	}
	id, err := authdomain.ParseWebAuthnCredentialID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid credential id", http.StatusBadRequest)
		return nil, false
	}
	if err := h.webauthn.Revoke(r.Context(), member.HouseholdID, member.ID, id); err != nil {
		if errors.Is(err, authdomain.ErrWebAuthnCredentialNotFound) {
			http.Error(w, "credential not found", http.StatusNotFound)
			return nil, false
		}
		h.logger.ErrorContext(r.Context(), "webauthn: revoke credential", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, false
	}
	return member, true
}

// requireMember resolves the current member — any role, unlike
// kioskadapter.SettingsWebHandlers.requireParent — since passkey
// registration is a per-member self-service concern (mirrors
// MFAWebHandlers.requireMember).
func (h *WebAuthnWebHandlers) requireMember(w http.ResponseWriter, r *http.Request) (*household.Member, bool) {
	member, ok := CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	return member, true
}

// toWebAuthnDeviceView maps a domain WebAuthnCredential to its display view
// model.
func toWebAuthnDeviceView(c authdomain.WebAuthnCredential) components.WebAuthnDeviceView {
	view := components.WebAuthnDeviceView{
		ID:             c.ID.String(),
		Nickname:       c.Nickname,
		CreatedAtLabel: c.CreatedAt.Format(webauthnDisplayDateLayout),
	}
	if c.LastUsedAt != nil {
		view.LastUsedAtLabel = c.LastUsedAt.Format(webauthnDisplayDateLayout)
	}
	return view
}
