package adapter

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	"github.com/ericfisherdev/nestova/internal/federation/app"
	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// settingsDisplayDateLayout is the human-readable date layout shown for the
// attach timestamp, mirroring kioskadapter.SettingsWebHandlers' own
// constant of the same name (a distinct package, so no collision).
const settingsDisplayDateLayout = "Jan 2, 2006 3:04 PM"

// FederationWebHandlers serves the federation instance-link section of the
// shared /settings page (NSTR-106): attaching and detaching the
// household's single Nestorage instance. Owner-only — attaching widens a
// stored credential's reach to member personal data, the same owner-grade
// sensitivity kiosk token provisioning has (NES-135), which is why this
// plan maps "Administrator" to RoleOwner rather than the kiosk section's
// looser owner-or-adult gate. Like every other /settings section handler
// (kioskadapter.SettingsWebHandlers, notifyadapter.NotifyWebHandlers), it
// never writes an HTTP response for a mutation's success path that needs
// the OTHER sections recomposed — the composition root
// (cmd/server/home.go's registerSettingsPage) does. Attach's own success is
// the one exception: it redirects straight to the NSTR-109 reconciliation
// screen instead, since there is nothing from THIS section left to compose.
type FederationWebHandlers struct {
	link   *app.LinkService
	sm     *scs.SessionManager
	logger *slog.Logger
}

// NewFederationWebHandlers constructs FederationWebHandlers with all
// required dependencies. It panics when any dependency is nil so
// misconfigured composition roots are caught at startup rather than at the
// first HTTP request.
func NewFederationWebHandlers(link *app.LinkService, sm *scs.SessionManager, logger *slog.Logger) *FederationWebHandlers {
	if link == nil {
		panic("federation/adapter: NewFederationWebHandlers requires a non-nil LinkService")
	}
	if sm == nil {
		panic("federation/adapter: NewFederationWebHandlers requires a non-nil session manager")
	}
	if logger == nil {
		panic("federation/adapter: NewFederationWebHandlers requires a non-nil logger")
	}
	return &FederationWebHandlers{link: link, sm: sm, logger: logger}
}

// SectionView builds the federation section's view model for member,
// reporting show=false unless member is the household owner. errMsg
// re-shows an inline failure message from an Attach mutation on this same
// response. The stored api key is never included in the view, in either
// state.
func (h *FederationWebHandlers) SectionView(ctx context.Context, member *household.Member, errMsg string) (view components.FederationSettingsView, show bool, err error) {
	if member.Role != household.RoleOwner {
		return components.FederationSettingsView{}, false, nil
	}
	link, err := h.link.Status(ctx, member.HouseholdID)
	if err != nil {
		if !errors.Is(err, domain.ErrLinkNotFound) {
			return components.FederationSettingsView{}, false, err
		}
		return components.FederationSettingsView{
			CSRFToken:    authadapter.GetCSRFToken(ctx, h.sm),
			ErrorMessage: errMsg,
		}, true, nil
	}
	return components.FederationSettingsView{
		Attached:          true,
		InstanceHost:      instanceHost(link.BaseURL),
		AttachedAtDisplay: link.AttachedAt.Format(settingsDisplayDateLayout),
		CSRFToken:         authadapter.GetCSRFToken(ctx, h.sm),
		ErrorMessage:      errMsg,
	}, true, nil
}

// Attach handles the mutation behind POST /settings/federation/attach
// (owner-only; requireStepUp-gated at the composition root, since storing a
// long-lived credential is exactly the action NES-135 added step-up for).
// ok=false means a hard failure (unauthenticated, not-owner, bad CSRF, or a
// malformed form) was already written directly. ok=true with a non-empty
// errMsg means the caller must recompose the settings page with that
// inline error and nothing stored. ok=true with an empty errMsg means
// Attach succeeded — the caller redirects to
// /settings/federation/reconcile (NSTR-109) rather than recomposing
// /settings itself, since attach never provisions or reconciles anything
// on its own.
func (h *FederationWebHandlers) Attach(w http.ResponseWriter, r *http.Request) (member *household.Member, errMsg string, status int, ok bool) {
	member, ok = h.requireOwner(w, r)
	if !ok {
		return nil, "", 0, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, "", 0, false
	}
	if !authadapter.VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, "", 0, false
	}

	baseURL := strings.TrimSpace(r.FormValue("base_url"))
	apiKey := strings.TrimSpace(r.FormValue("api_key"))
	if baseURL == "" || apiKey == "" {
		return member, "Enter both the Nestorage URL and its API key.", http.StatusBadRequest, true
	}

	if _, err := h.link.Attach(r.Context(), member.HouseholdID, member.ID, baseURL, apiKey); err != nil {
		return member, h.attachErrorMessage(r.Context(), err), http.StatusUnprocessableEntity, true
	}
	return member, "", http.StatusOK, true
}

// attachErrorMessage maps an Attach failure to the inline message shown on
// the settings page, logging only the ones that are not themselves
// user-actionable. The presented api key is never included, in any branch.
func (h *FederationWebHandlers) attachErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, domain.ErrInvalidBaseURL):
		return "Enter a valid Nestorage URL (starting with http:// or https://)."
	case errors.Is(err, domain.ErrHouseholdAlreadyBound):
		return "This household is already attached to a Nestorage instance. Detach it first to attach a different one."
	case errors.Is(err, domain.ErrInstanceAlreadyBound):
		return "That Nestorage instance is already attached to a different household."
	case errors.Is(err, domain.ErrInvalidAPIKey):
		return "Nestorage rejected that API key. Check the key and try again."
	case errors.Is(err, domain.ErrInstanceUnreachable):
		return "Could not reach that Nestorage instance. Check the URL and try again."
	default:
		h.logger.ErrorContext(ctx, "settings: attach federation instance", "error", err)
		return "Something went wrong attaching that instance. Try again."
	}
}

// Detach handles the mutation behind POST /settings/federation/detach
// (owner-only). Same ok contract as
// kioskadapter.SettingsWebHandlers.RevokeKioskToken: ok=false means a hard
// failure was already written directly; ok=true means the caller redirects
// back to /settings. Detaching clears the stored credential.
func (h *FederationWebHandlers) Detach(w http.ResponseWriter, r *http.Request) (member *household.Member, ok bool) {
	member, ok = h.requireOwner(w, r)
	if !ok {
		return nil, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, false
	}
	if !authadapter.VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, false
	}
	if err := h.link.Detach(r.Context(), member.HouseholdID); err != nil {
		h.logger.ErrorContext(r.Context(), "settings: detach federation instance", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, false
	}
	return member, true
}

// requireOwner resolves the current member and enforces the owner-only
// role gate every federation-section mutation shares — mirrors
// notifyadapter.NotifyWebHandlers.requireOwner's identical quiet-hours
// gate, both stricter than the kiosk section's parent (owner-or-adult) one:
// attaching or detaching an external instance is owner-grade, per this
// plan's "Administrator maps to RoleOwner" decision.
func (h *FederationWebHandlers) requireOwner(w http.ResponseWriter, r *http.Request) (*household.Member, bool) {
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	if member.Role != household.RoleOwner {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}
	return member, true
}

// instanceHost extracts the host portion of an already-normalized base url
// for display, falling back to the full value on the (practically
// unreachable, since the stored value always passed NormalizeBaseURL
// first) chance it fails to parse.
func instanceHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	return u.Host
}
