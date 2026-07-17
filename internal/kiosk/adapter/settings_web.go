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
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/kiosk/app"
	"github.com/ericfisherdev/nestova/internal/kiosk/domain"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver/middleware"
	"github.com/ericfisherdev/nestova/web/components"
)

// settingsDisplayDateLayout is the human-readable date layout shown for
// provisioning/revocation timestamps.
const settingsDisplayDateLayout = "Jan 2, 2006 3:04 PM"

// SettingsWebHandlers serves the kiosk device section of the shared
// /settings page: generating and revoking the household's kiosk token.
// NES-134 moved that page's overall composition (which also includes the
// auth context's per-member MFA section, rendered for every member) to the
// composition root (cmd/server/home.go's registerSettingsPage) — this type
// owns only the kiosk-specific reads and mutations, never writing an HTTP
// response for a mutation's SUCCESS path itself (the composition root does,
// after composing both sections); it still writes error responses directly
// for failures that never need the other section (auth/role/CSRF/not-found).
type SettingsWebHandlers struct {
	kiosk  *app.KioskService
	sm     *scs.SessionManager
	logger *slog.Logger
}

// NewSettingsWebHandlers constructs SettingsWebHandlers with all required
// dependencies. It panics when any dependency is nil so misconfigured
// composition roots are caught at startup rather than at the first HTTP
// request.
func NewSettingsWebHandlers(kiosk *app.KioskService, sm *scs.SessionManager, logger *slog.Logger) *SettingsWebHandlers {
	if kiosk == nil {
		panic("kiosk/adapter: NewSettingsWebHandlers requires a non-nil KioskService")
	}
	if sm == nil {
		panic("kiosk/adapter: NewSettingsWebHandlers requires a non-nil session manager")
	}
	if logger == nil {
		panic("kiosk/adapter: NewSettingsWebHandlers requires a non-nil logger")
	}
	return &SettingsWebHandlers{kiosk: kiosk, sm: sm, logger: logger}
}

// SectionView builds the kiosk section's view model for member, reporting
// show=false when the section must not be rendered at all — a non-parent
// (child) member never sees the kiosk section, matching NES-128's original
// parent-only gate, now applied at section level rather than blocking the
// whole /settings page (NES-134 opens the page itself to every member for
// their own MFA section). reveal is the one-time activation-code reveal to
// embed, non-nil only in the same response as a successful
// CreateActivationCode call.
func (h *SettingsWebHandlers) SectionView(ctx context.Context, member *household.Member, reveal *components.KioskActivationReveal) (view components.KioskSettingsView, show bool, err error) {
	if !member.Role.IsParent() {
		return components.KioskSettingsView{}, false, nil
	}
	devices, err := h.kiosk.ListByHousehold(ctx, member.HouseholdID)
	if err != nil {
		return components.KioskSettingsView{}, false, err
	}
	views := make([]components.KioskDeviceView, 0, len(devices))
	for _, d := range devices {
		views = append(views, toKioskDeviceView(d))
	}
	return components.KioskSettingsView{Devices: views, NewToken: reveal}, true, nil
}

// CreateActivationCode handles the mutation behind POST
// /settings/kiosk/generate: it verifies CSRF and the parent-only gate, then
// issues a new short-lived, single-use activation code. On any failure it
// writes the appropriate error response directly and returns ok=false (the
// caller does nothing further). On success it returns the authenticated
// member and the one-time reveal; the caller (the composition root) is
// responsible for composing and rendering the full settings page — the raw
// code exists only in that one response, so a redirect would lose it. The
// settings page never displays the long-lived kiosk_device bearer token
// itself: that is generated only when the kiosk device redeems this code at
// /kiosk/activate.
func (h *SettingsWebHandlers) CreateActivationCode(w http.ResponseWriter, r *http.Request) (member *household.Member, reveal *components.KioskActivationReveal, ok bool) {
	member, ok = h.requireParent(w, r)
	if !ok {
		return nil, nil, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, nil, false
	}
	if !authadapter.VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, nil, false
	}

	// Trim before the empty check: a whitespace-only submission (e.g. a
	// stray space) must fall back to the default the same as a genuinely
	// empty field, not reach CreateActivationCode with a non-empty string
	// that Validate then rejects as blank after its own trim — turning a
	// harmless input into a 500.
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "Kiosk"
	}
	code, rawCode, err := h.kiosk.CreateActivationCode(r.Context(), member.HouseholdID, name)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "settings: create activation code", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, nil, false
	}

	h.logger.InfoContext(r.Context(), "kiosk activation code generated", "code_id", code.ID.String())
	reveal = &components.KioskActivationReveal{
		Code:             rawCode,
		ActivationURL:    activationURL(r, rawCode),
		ExpiresInMinutes: int(domain.ActivationCodeTTL.Minutes()),
	}
	return member, reveal, true
}

// RevokeKioskToken handles the mutation behind POST
// /settings/kiosk/{id}/revoke: it verifies CSRF and the parent-only gate,
// then revokes the device. On any failure it writes the appropriate error
// response directly and returns ok=false. On success it returns the
// authenticated member; the caller redirects back to the settings page (no
// reveal to compose here, unlike CreateActivationCode).
//
// Error mapping (mirrors tasksadapter.GamificationWebHandlers.ArchiveReward's
// convention for a stale parent admin action):
//   - bad CSRF                   → 403
//   - not a parent (owner/adult) → 403
//   - malformed device id        → 400
//   - ErrKioskDeviceNotFound     → 404
//   - other                      → 500
func (h *SettingsWebHandlers) RevokeKioskToken(w http.ResponseWriter, r *http.Request) (member *household.Member, ok bool) {
	member, ok = h.requireParent(w, r)
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
	id, err := domain.ParseKioskDeviceID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid kiosk device id", http.StatusBadRequest)
		return nil, false
	}
	if err := h.kiosk.Revoke(r.Context(), member.HouseholdID, id); err != nil {
		if errors.Is(err, domain.ErrKioskDeviceNotFound) {
			http.Error(w, "kiosk device not found", http.StatusNotFound)
			return nil, false
		}
		h.logger.ErrorContext(r.Context(), "settings: revoke kiosk device", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, false
	}
	return member, true
}

// requireParent resolves the current member and enforces the parent-only role
// gate shared by every kiosk-section mutation, writing the appropriate error
// response and returning ok=false when either check fails.
func (h *SettingsWebHandlers) requireParent(w http.ResponseWriter, r *http.Request) (*household.Member, bool) {
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	if !member.Role.IsParent() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}
	return member, true
}

func toKioskDeviceView(d *domain.KioskDevice) components.KioskDeviceView {
	view := components.KioskDeviceView{
		ID:             d.ID.String(),
		Name:           d.Name,
		CreatedAtLabel: d.CreatedAt.Format(settingsDisplayDateLayout),
		Active:         d.Active(),
	}
	if d.RevokedAt != nil {
		view.RevokedAtLabel = d.RevokedAt.Format(settingsDisplayDateLayout)
	}
	return view
}

// activationURL builds the absolute link a parent opens from the kiosk
// device's own browser. Opening it (a GET) only lands on a confirmation form
// pre-filled with the code — it never redeems by itself (see
// KioskWebHandlers.Activate); the device still presses Activate to complete
// provisioning via a CSRF-checked POST. It uses the effective scheme resolved
// by the ForwardedHeaders middleware (https in production, honoring a trusted
// reverse proxy) and the request's Host, so the link is directly usable
// regardless of how the server is fronted. The code embedded in this URL is
// short-lived and single-use (see domain.ActivationCodeTTL): it is worthless
// as soon as it is redeemed or expires, so its appearing in browser
// history/access logs/Referer headers is an accepted, bounded exposure —
// unlike the long-lived device token this flow replaces, which the settings
// page never displays.
func activationURL(r *http.Request, rawCode string) string {
	scheme := middleware.RequestScheme(r.Context())
	if scheme == "" {
		// ForwardedHeaders did not run ahead of this handler (e.g. a unit test
		// exercising the handler directly rather than through httpserver.New,
		// which always installs it) — fall back to the on-wire TLS state.
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host + "/kiosk/activate?code=" + url.QueryEscape(rawCode)
}
