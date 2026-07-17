package adapter

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/kiosk/app"
	"github.com/ericfisherdev/nestova/internal/kiosk/domain"
	"github.com/ericfisherdev/nestova/internal/platform/httpserver/middleware"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/web/components"
)

// settingsPath is the canonical page path the settings routes redirect back to
// after a mutation that has nothing left to reveal.
const settingsPath = "/settings"

// settingsDisplayDateLayout is the human-readable date layout shown for
// provisioning/revocation timestamps.
const settingsDisplayDateLayout = "Jan 2, 2006 3:04 PM"

// SettingsLayoutFunc wraps page content in the app shell; home.go provides it,
// mirroring every other bounded context's adapter.
type SettingsLayoutFunc func(member *household.Member) func(templ.Component) templ.Component

// SettingsWebHandlers serves the parent-only /settings page's kiosk device
// section: generating and revoking the household's kiosk token.
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

// Page handles GET /settings. Parent-only (owner/adult); a child member
// receives 403, mirroring the reward admin and trade history pages' gate.
func (h *SettingsWebHandlers) Page(layoutFn SettingsLayoutFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := h.requireParent(w, r)
		if !ok {
			return
		}
		view, err := h.buildView(r, member, nil)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "settings: build view", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if err := render.Page(r.Context(), w, r, layoutFn(member), components.SettingsPage(view)); err != nil {
			h.logger.ErrorContext(r.Context(), "settings: render page", "error", err)
		}
	}
}

// GenerateActivationCode handles POST /settings/kiosk/generate. It issues a
// short-lived, single-use activation code and renders the settings page
// directly — not a redirect — because the raw code exists only in this one
// response; a redirect would lose it. The settings page never displays the
// long-lived kiosk_device bearer token itself: that is generated only when
// the kiosk device redeems this code at /kiosk/activate.
func (h *SettingsWebHandlers) GenerateActivationCode(layoutFn SettingsLayoutFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := h.requireParent(w, r)
		if !ok {
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !authadapter.VerifyCSRF(r, h.sm) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
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
			return
		}

		reveal := &components.KioskActivationReveal{
			Code:             rawCode,
			ActivationURL:    activationURL(r, rawCode),
			ExpiresInMinutes: int(domain.ActivationCodeTTL.Minutes()),
		}
		view, err := h.buildView(r, member, reveal)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "settings: build view after code generation", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		h.logger.InfoContext(r.Context(), "kiosk activation code generated", "code_id", code.ID.String())
		// This response reveals a live credential (even though it is
		// short-lived and single-use): it must never be cached by a shared
		// proxy or stored in the browser's disk cache.
		w.Header().Set("Cache-Control", "no-store")
		if err := render.Render(r.Context(), w, http.StatusOK, layoutFn(member)(components.SettingsPage(view))); err != nil {
			h.logger.ErrorContext(r.Context(), "settings: render after code generation", "error", err)
		}
	}
}

// RevokeKioskToken handles POST /settings/kiosk/{id}/revoke.
//
// Error mapping (mirrors tasksadapter.GamificationWebHandlers.ArchiveReward's
// convention for a stale parent admin action):
//   - bad CSRF                   → 403
//   - not a parent (owner/adult) → 403
//   - malformed device id        → 400
//   - ErrKioskDeviceNotFound     → 404
//   - other                      → 500
func (h *SettingsWebHandlers) RevokeKioskToken(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireParent(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !authadapter.VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	id, err := domain.ParseKioskDeviceID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid kiosk device id", http.StatusBadRequest)
		return
	}
	if err := h.kiosk.Revoke(r.Context(), member.HouseholdID, id); err != nil {
		if errors.Is(err, domain.ErrKioskDeviceNotFound) {
			http.Error(w, "kiosk device not found", http.StatusNotFound)
			return
		}
		h.logger.ErrorContext(r.Context(), "settings: revoke kiosk device", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if render.IsHTMX(r) {
		w.Header().Set("HX-Redirect", settingsPath)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, settingsPath, http.StatusSeeOther)
}

// requireParent resolves the current member and enforces the parent-only role
// gate shared by every action on this page, writing the appropriate error
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

// buildView assembles the SettingsView for member's household. reveal is
// non-nil only immediately after a successful GenerateActivationCode.
func (h *SettingsWebHandlers) buildView(r *http.Request, member *household.Member, reveal *components.KioskActivationReveal) (components.SettingsView, error) {
	devices, err := h.kiosk.ListByHousehold(r.Context(), member.HouseholdID)
	if err != nil {
		return components.SettingsView{}, err
	}
	views := make([]components.KioskDeviceView, 0, len(devices))
	for _, d := range devices {
		views = append(views, toKioskDeviceView(d))
	}
	return components.SettingsView{
		Devices:   views,
		NewToken:  reveal,
		CSRFToken: authadapter.GetCSRFToken(r.Context(), h.sm),
	}, nil
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
