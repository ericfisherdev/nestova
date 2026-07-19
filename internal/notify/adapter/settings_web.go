package adapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/notify/app"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// clockTimeLayout is the <input type="time"> wire format ("HH:MM"), used
// both to render QuietHoursSettingsView's Start/EndValue and to parse the
// submitted quiet-hours form fields.
const clockTimeLayout = "15:04"

// NotifyWebHandlers serves the notify context's two /settings page
// sections (NES-139): the SMS notification section (phone entry, opt-in
// consent, per-event-type preferences — every member, any role) and the
// owner-only quiet-hours section. Like kioskadapter.SettingsWebHandlers
// and authadapter.MFAWebHandlers, it never writes an HTTP response for a
// mutation's success path that needs the OTHER sections recomposed — the
// composition root (cmd/server/home.go's registerSettingsPage) does,
// after calling the SectionView methods here.
type NotifyWebHandlers struct {
	settings *app.SettingsService
	sm       *scs.SessionManager
	logger   *slog.Logger
}

// NewNotifyWebHandlers constructs NotifyWebHandlers with all required
// dependencies. It panics when any dependency is nil so misconfigured
// composition roots are caught at startup rather than at the first HTTP
// request.
func NewNotifyWebHandlers(settings *app.SettingsService, sm *scs.SessionManager, logger *slog.Logger) *NotifyWebHandlers {
	if settings == nil {
		panic("notify/adapter: NewNotifyWebHandlers requires a non-nil SettingsService")
	}
	if sm == nil {
		panic("notify/adapter: NewNotifyWebHandlers requires a non-nil session manager")
	}
	if logger == nil {
		panic("notify/adapter: NewNotifyWebHandlers requires a non-nil logger")
	}
	return &NotifyWebHandlers{settings: settings, sm: sm, logger: logger}
}

// SMSSectionView builds the SMS notification section's view model for
// member — rendered for every member regardless of role. errMsg re-shows
// an inline failure message from a mutation on this same response.
func (h *NotifyWebHandlers) SMSSectionView(ctx context.Context, member *household.Member, errMsg string) (components.NotifySettingsView, error) {
	contact, err := h.settings.GetContact(ctx, member.ID)
	if err != nil {
		return components.NotifySettingsView{}, err
	}
	prefs, err := h.settings.ListPreferences(ctx, member.ID)
	if err != nil {
		return components.NotifySettingsView{}, err
	}
	byEventType := make(map[domain.EventType]domain.Channel, len(prefs))
	for _, p := range prefs {
		byEventType[p.EventType] = p.Channel
	}

	allTypes := domain.AllEventTypes()
	rows := make([]components.NotifyPreferenceRow, 0, len(allTypes))
	for _, et := range allTypes {
		channel := domain.ChannelInApp
		if c, ok := byEventType[et]; ok {
			channel = c
		}
		rows = append(rows, components.NotifyPreferenceRow{
			EventType: et.String(),
			Label:     et.Label(),
			Channel:   channel.String(),
		})
	}

	phone := ""
	if contact.Phone != nil {
		phone = contact.Phone.String()
	}
	return components.NotifySettingsView{
		Phone:       phone,
		OptedIn:     contact.SMSOptedIn,
		Preferences: rows,
		CSRFToken:   authadapter.GetCSRFToken(ctx, h.sm),
		Error:       errMsg,
	}, nil
}

// QuietHoursSectionView builds the quiet-hours section's view model for
// member, reporting show=false when the section must not be rendered at
// all — quiet hours are owner-only (NES-139), a stricter gate than the
// kiosk section's parent (owner-or-adult) one, since they affect every
// member's SMS delivery timing household-wide, not just the acting
// member's own settings.
func (h *NotifyWebHandlers) QuietHoursSectionView(ctx context.Context, member *household.Member, errMsg string) (view components.QuietHoursSettingsView, show bool, err error) {
	if member.Role != household.RoleOwner {
		return components.QuietHoursSettingsView{}, false, nil
	}
	start, end, err := h.settings.QuietHours(ctx, member.HouseholdID)
	if err != nil {
		return components.QuietHoursSettingsView{}, false, err
	}
	return components.QuietHoursSettingsView{
		Enabled:    start != nil && end != nil,
		StartValue: formatClockTime(start),
		EndValue:   formatClockTime(end),
		CSRFToken:  authadapter.GetCSRFToken(ctx, h.sm),
		Error:      errMsg,
	}, true, nil
}

// UpdatePhone handles the mutation behind POST /settings/notify/phone.
// Same ok/status contract as authadapter.MFAWebHandlers.Confirm: ok=false
// means a hard failure was already written directly; ok=true means the
// caller must compose the full page at status with errMsg embedded.
func (h *NotifyWebHandlers) UpdatePhone(w http.ResponseWriter, r *http.Request) (member *household.Member, errMsg string, status int, ok bool) {
	member, ok = h.requireMember(w, r)
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

	if err := h.settings.UpdatePhone(r.Context(), member.ID, r.FormValue("phone")); err != nil {
		if errors.Is(err, domain.ErrInvalidPhoneFormat) {
			return member, "Enter a valid phone number, e.g. +15551234567.", http.StatusBadRequest, true
		}
		h.logger.ErrorContext(r.Context(), "notify settings: update phone", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, "", 0, false
	}
	return member, "", http.StatusOK, true
}

// UpdateOptIn handles the mutation behind POST /settings/notify/opt-in.
// Same ok/status contract as UpdatePhone. An HTML checkbox submits its
// field only when checked, so the field's mere presence (any value) means
// opted in; its absence means opted out.
func (h *NotifyWebHandlers) UpdateOptIn(w http.ResponseWriter, r *http.Request) (member *household.Member, errMsg string, status int, ok bool) {
	member, ok = h.requireMember(w, r)
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

	_, present := r.Form["opted_in"]
	if err := h.settings.SetOptIn(r.Context(), member.ID, present); err != nil {
		if errors.Is(err, domain.ErrPhoneRequiredForOptIn) {
			return member, "Add a phone number before turning on text messages.", http.StatusBadRequest, true
		}
		h.logger.ErrorContext(r.Context(), "notify settings: update opt-in", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, "", 0, false
	}
	return member, "", http.StatusOK, true
}

// UpdatePreferences handles the mutation behind
// POST /settings/notify/preferences: every domain.AllEventTypes() row
// submitted in the form is parsed, then upserted in ONE
// SettingsService.SetPreferences call (CodeRabbit round 2, trivial
// finding #6) — resolving SMS-readiness once per request rather than once
// per sms row, and rejecting the whole submission together rather than
// partially applying rows processed before an invalid one. Same
// ok/status contract as UpdatePhone.
func (h *NotifyWebHandlers) UpdatePreferences(w http.ResponseWriter, r *http.Request) (member *household.Member, errMsg string, status int, ok bool) {
	member, ok = h.requireMember(w, r)
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

	updates := make(map[domain.EventType]domain.Channel)
	for _, et := range domain.AllEventTypes() {
		raw := r.FormValue("pref_" + et.String())
		if raw == "" {
			continue
		}
		channel, err := domain.ParseChannel(raw)
		if err != nil {
			return member, "That's not a valid notification channel.", http.StatusBadRequest, true
		}
		updates[et] = channel
	}

	if err := h.settings.SetPreferences(r.Context(), member.HouseholdID, member.ID, updates); err != nil {
		if errors.Is(err, domain.ErrMemberNotSMSReady) {
			return member, "Opt in to text messages with a verified phone number before choosing SMS.", http.StatusBadRequest, true
		}
		if errors.Is(err, domain.ErrChannelNotDeliverable) {
			return member, "That notification channel isn't available yet.", http.StatusBadRequest, true
		}
		h.logger.ErrorContext(r.Context(), "notify settings: update preferences", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, "", 0, false
	}
	return member, "", http.StatusOK, true
}

// UpdateQuietHours handles the mutation behind
// POST /settings/notify/quiet-hours (owner-only). Same ok/status contract
// as UpdatePhone, plus a 403 when the acting member is not the household
// owner.
func (h *NotifyWebHandlers) UpdateQuietHours(w http.ResponseWriter, r *http.Request) (member *household.Member, errMsg string, status int, ok bool) {
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

	_, enabled := r.Form["quiet_enabled"]
	if !enabled {
		if err := h.settings.SetQuietHours(r.Context(), member.HouseholdID, nil, nil); err != nil {
			h.logger.ErrorContext(r.Context(), "notify settings: disable quiet hours", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return nil, "", 0, false
		}
		return member, "", http.StatusOK, true
	}

	startRaw, endRaw := r.FormValue("quiet_start"), r.FormValue("quiet_end")
	if startRaw == "" || endRaw == "" {
		return member, "Enter both a start and end time, or turn quiet hours off.", http.StatusBadRequest, true
	}
	start, err := parseClockTime(startRaw)
	if err != nil {
		return member, "Enter a valid start time.", http.StatusBadRequest, true
	}
	end, err := parseClockTime(endRaw)
	if err != nil {
		return member, "Enter a valid end time.", http.StatusBadRequest, true
	}
	if err := h.settings.SetQuietHours(r.Context(), member.HouseholdID, &start, &end); err != nil {
		h.logger.ErrorContext(r.Context(), "notify settings: set quiet hours", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, "", 0, false
	}
	return member, "", http.StatusOK, true
}

// requireMember resolves the current member — any role, since the SMS
// section is a per-member self-service concern (mirrors
// authadapter.MFAWebHandlers.requireMember).
func (h *NotifyWebHandlers) requireMember(w http.ResponseWriter, r *http.Request) (*household.Member, bool) {
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	return member, true
}

// requireOwner resolves the current member and enforces the owner-only
// role gate quiet-hours mutations share (mirrors
// kioskadapter.SettingsWebHandlers.requireParent, but stricter: owner
// only, not owner-or-adult).
func (h *NotifyWebHandlers) requireOwner(w http.ResponseWriter, r *http.Request) (*household.Member, bool) {
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

// formatClockTime formats d as an "HH:MM" <input type="time"> value, or
// "" when d is nil.
func formatClockTime(d *time.Duration) string {
	if d == nil {
		return ""
	}
	h := *d / time.Hour
	m := (*d % time.Hour) / time.Minute
	return fmt.Sprintf("%02d:%02d", h, m)
}

// parseClockTime parses an "HH:MM" <input type="time"> value into a
// duration since midnight.
func parseClockTime(s string) (time.Duration, error) {
	t, err := time.Parse(clockTimeLayout, s)
	if err != nil {
		return 0, fmt.Errorf("parse clock time %q: %w", s, err)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}
