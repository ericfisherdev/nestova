package adapter

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	"github.com/ericfisherdev/nestova/internal/calendar/app"
	"github.com/ericfisherdev/nestova/internal/platform/render"
)

// postConnectRedirect is where the member lands after the OAuth round trip. It
// points at the dashboard for now; the calendar UI (NES-70) repoints it at the
// calendar page.
const postConnectRedirect = "/"

// WebHandlers holds the HTTP handlers for the Google account connect flow. All
// dependencies are injected so the type is testable with fakes.
type WebHandlers struct {
	accounts *app.AccountService
	sm       *scs.SessionManager
	logger   *slog.Logger
}

// NewWebHandlers constructs a WebHandlers, panicking on a nil dependency so a
// misconfigured composition root fails at startup, not at the first request.
func NewWebHandlers(accounts *app.AccountService, sm *scs.SessionManager, logger *slog.Logger) *WebHandlers {
	if accounts == nil {
		panic("adapter: NewWebHandlers requires a non-nil AccountService")
	}
	if sm == nil {
		panic("adapter: NewWebHandlers requires a non-nil session manager")
	}
	if logger == nil {
		panic("adapter: NewWebHandlers requires a non-nil logger")
	}
	return &WebHandlers{accounts: accounts, sm: sm, logger: logger}
}

// Connect starts the Google OAuth flow by redirecting the member to Google's
// consent screen with a signed state. It is a CSRF-verified POST (the button
// action); the signed state additionally protects the OAuth round trip.
func (h *WebHandlers) Connect(w http.ResponseWriter, r *http.Request) {
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !authadapter.VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	authURL := h.accounts.AuthURL(member.ID, time.Now())
	// HTMX requests get an HX-Redirect (client-side full redirect to Google);
	// full navigations get a 303 to the same URL.
	if render.IsHTMX(r) {
		w.Header().Set("HX-Redirect", authURL)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

// Callback completes the OAuth flow: it verifies the signed state binds the
// current member, exchanges the authorization code, stores the encrypted tokens,
// and redirects back. Tokens/secrets are never logged (member/account ids only).
func (h *WebHandlers) Callback(w http.ResponseWriter, r *http.Request) {
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
		// The member declined consent or Google reported an error.
		h.logger.WarnContext(r.Context(), "calendar oauth callback returned an error",
			"member_id", member.ID.String(), "reason", oauthErr)
		http.Redirect(w, r, postConnectRedirect+"?connect=denied", http.StatusSeeOther)
		return
	}

	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}

	stateMember, err := h.accounts.VerifyState(state, time.Now())
	if err != nil || stateMember != member.ID {
		// Coarse failure: a bad signature, expiry, or a state minted for another
		// member all reject the callback.
		h.logger.WarnContext(r.Context(), "calendar oauth state verification failed",
			"member_id", member.ID.String())
		http.Error(w, "invalid oauth state", http.StatusForbidden)
		return
	}

	if _, err := h.accounts.Connect(r.Context(), member.ID, member.HouseholdID, code); err != nil {
		h.logger.ErrorContext(r.Context(), "calendar oauth connect failed",
			"member_id", member.ID.String(), "error", err)
		http.Redirect(w, r, postConnectRedirect+"?connect=error", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, postConnectRedirect+"?connect=ok", http.StatusSeeOther)
}
