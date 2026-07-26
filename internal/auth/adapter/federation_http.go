package adapter

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/alexedwards/scs/v2"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
)

// federationAuthorizeResponseType is the only response_type GET
// /federation/authorize accepts. This is NOT full OAuth token issuance (no
// PKCE, no access_token — see docs/federation.md, NSTR-110's own doc); the
// single query parameter it borrows from RFC 6749 section 4.1.1's
// authorization code grant just keeps the request shape familiar.
const federationAuthorizeResponseType = "code"

// FederationHandlers serves Nestova's identity-provider side of the
// federation authorize/token hand-off to Nestorage: GET /federation/authorize
// (NSTR-105, this file) and POST /federation/token (NSTR-110). It holds the
// registered client config, the session manager, the authorization-code
// repository, and a logger; NSTR-110 extends this same struct with its own
// member-lookup/email-resolution read ports and a loginAttemptLimiter
// instance for the token exchange — deliberately not pre-added here, since
// this ticket has no use for them.
type FederationHandlers struct {
	cfg    config.FederationConfig
	sm     *scs.SessionManager
	codes  authdomain.AuthorizationCodeRepository
	logger *slog.Logger
}

// NewFederationHandlers constructs FederationHandlers. Every dependency is
// required and panics when nil, matching this codebase's usual "fail fast
// at construction" DI convention. The composition root only constructs this
// type at all when cfg.Enabled() is true (cmd/server/main.go) — an
// unconfigured install never reaches here and registers no federation route
// (cmd/server/home.go).
func NewFederationHandlers(cfg config.FederationConfig, sm *scs.SessionManager, codes authdomain.AuthorizationCodeRepository, logger *slog.Logger) *FederationHandlers {
	if sm == nil {
		panic("auth/adapter: NewFederationHandlers requires a non-nil session manager")
	}
	if codes == nil {
		panic("auth/adapter: NewFederationHandlers requires a non-nil AuthorizationCodeRepository")
	}
	if logger == nil {
		panic("auth/adapter: NewFederationHandlers requires a non-nil logger")
	}
	return &FederationHandlers{cfg: cfg, sm: sm, codes: codes, logger: logger}
}

// Authorize handles GET /federation/authorize, the provider side of the
// authorization-code redirect (NSTR-105): Nestorage sends a person here with
// response_type=code, its registered client_id, redirect_uri, and an opaque
// state; Nestova authenticates them — reusing its OWN login page, MFA step,
// and passkey ceremony completely unchanged — and redirects back to
// redirect_uri with a short-lived, single-use code plus the state echoed
// verbatim. Redeeming that code for a member identity document is NSTR-110's
// POST /federation/token; nothing here returns identity data.
//
// Registered WITHOUT the requireMember middleware (cmd/server/home.go) so
// client/redirect validation always runs first: an unregistered client_id or
// redirect_uri is refused with a plain 400 before authentication is ever
// attempted (RFC 6749 section 4.1.2.1 — never redirect to an unvalidated
// target). Only once both are proven registered, and the request shape
// itself checks out, does Authorize consult CurrentMember: an
// unauthenticated request is sent through the ordinary /login flow with
// `next` set to this request's own URI, so the person lands straight back
// here once they finish (including MFA/passkeys) — mirroring
// RequireMember's own redirect shape exactly.
func (h *FederationHandlers) Authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")

	// Client and redirect are validated FIRST, before anything else —
	// including before the request shape (response_type/state) is even
	// looked at — so a request naming an unregistered client or redirect
	// can never be used to bounce a browser toward an attacker-controlled
	// origin, regardless of what else is wrong with it.
	if !clientIDsMatch(clientID, h.cfg.ClientID) || redirectURI != h.cfg.RedirectURL {
		http.Error(w, "unknown client or redirect", http.StatusBadRequest)
		return
	}

	responseType := q.Get("response_type")
	state := q.Get("state")
	if responseType != federationAuthorizeResponseType {
		h.redirectWithError(w, r, redirectURI, "unsupported_response_type")
		return
	}
	if state == "" {
		h.redirectWithError(w, r, redirectURI, "invalid_request")
		return
	}

	member, ok := CurrentMember(r.Context())
	if !ok {
		http.Redirect(w, r, "/login?next="+escapePath(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}

	rawCode, err := authdomain.GenerateAuthorizationCode()
	if err != nil {
		h.logger.ErrorContext(r.Context(), "federation: generate authorization code", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	code := &authdomain.AuthorizationCode{
		ID:          authdomain.NewAuthorizationCodeID(),
		MemberID:    member.ID,
		ClientID:    clientID,
		RedirectURI: redirectURI,
		CodeHash:    authdomain.HashAuthorizationCode(rawCode),
		ExpiresAt:   time.Now().Add(authdomain.AuthorizationCodeTTL),
	}
	if err := h.codes.Create(r.Context(), code); err != nil {
		h.logger.ErrorContext(r.Context(), "federation: create authorization code", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	dest, err := url.Parse(redirectURI)
	if err != nil {
		// redirectURI was already checked for exact equality against the
		// registered RedirectURL above, and RedirectURL itself is validated
		// as an absolute http(s) URL at startup (config.Load) — this branch
		// is defensive, not reachable in practice.
		h.logger.ErrorContext(r.Context(), "federation: parse registered redirect url", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	dq := dest.Query()
	dq.Set("code", rawCode)
	dq.Set("state", state)
	dest.RawQuery = dq.Encode()
	http.Redirect(w, r, dest.String(), http.StatusSeeOther)
}

// redirectWithError sends a 303 back to redirectURI carrying an RFC 6749
// error query parameter. Only used once client_id/redirect_uri are ALREADY
// proven registered, so the client — not this page — owns rendering the
// failure.
func (h *FederationHandlers) redirectWithError(w http.ResponseWriter, r *http.Request, redirectURI, errorCode string) {
	dest, err := url.Parse(redirectURI)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "federation: parse registered redirect url", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	dq := dest.Query()
	dq.Set("error", errorCode)
	dest.RawQuery = dq.Encode()
	http.Redirect(w, r, dest.String(), http.StatusSeeOther)
}

// clientIDsMatch performs a constant-time comparison of the presented
// client_id against the registered one — even though the client id is
// PUBLIC (it appears openly in the browser redirect), comparing it the same
// constant-time way VerifyCSRF compares a real secret costs nothing and
// avoids relying on "this value isn't sensitive" reasoning holding forever.
func clientIDsMatch(presented, registered string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(registered)) == 1
}
