package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// quietHoursStore is the narrow read+write port SettingsService needs onto
// a household's quiet hours (ISP): domain.QuietHoursReader for display,
// plus domain.QuietHoursWriter for the owner-only mutation.
// adapter.QuietHoursRepository satisfies this structurally.
type quietHoursStore interface {
	domain.QuietHoursReader
	domain.QuietHoursWriter
}

// SettingsService implements the member-facing business rules behind the
// settings page's SMS notification section (NES-139): phone number entry
// and opt-in consent, per-event-type channel preferences, and
// (owner-only) household quiet hours. It is the ONE place these rules
// live — the web handler (internal/notify/adapter's NotifyWebHandlers)
// stays a thin HTTP layer, mirroring authapp.MFAService /
// internal/auth/adapter's MFAWebHandlers split.
type SettingsService struct {
	contacts    domain.ContactDirectory
	preferences domain.PreferenceRepository
	households  quietHoursStore
	// deliverable is the set of channels this deployment has a wired
	// Sender for, built by the composition root from config. It is
	// narrower than domain.Channel.Valid(), which also accepts push (a
	// valid domain.Channel value reserved for a future ticket's own
	// Sender). See domain.ErrChannelNotDeliverable's own doc for why
	// accepting an undeliverable channel is a silent-notification-loss
	// risk, not just a cosmetic UI mismatch.
	deliverable map[domain.Channel]bool
}

// NewSettingsService constructs a SettingsService with injected
// dependencies. Panics when any is nil. deliverable lists the optional
// channels (SMS, email) this deployment has a wired Sender for; in-app is
// always deliverable and need not be listed.
func NewSettingsService(
	contacts domain.ContactDirectory,
	preferences domain.PreferenceRepository,
	households quietHoursStore,
	deliverable []domain.Channel,
) *SettingsService {
	if contacts == nil {
		panic("app: NewSettingsService requires a non-nil ContactDirectory")
	}
	if preferences == nil {
		panic("app: NewSettingsService requires a non-nil PreferenceRepository")
	}
	if households == nil {
		panic("app: NewSettingsService requires a non-nil household quiet-hours store")
	}
	channels := map[domain.Channel]bool{domain.ChannelInApp: true}
	for _, channel := range deliverable {
		channels[channel] = true
	}
	return &SettingsService{contacts: contacts, preferences: preferences, households: households, deliverable: channels}
}

// ChannelDeliverable reports whether channel has a wired Sender in this
// deployment, i.e. whether a member may be offered and select it.
func (s *SettingsService) ChannelDeliverable(channel domain.Channel) bool {
	return s.deliverable[channel]
}

// GetContact returns memberID's current SMS contact details.
func (s *SettingsService) GetContact(ctx context.Context, memberID household.MemberID) (*domain.MemberContact, error) {
	return s.contacts.GetContact(ctx, memberID)
}

// UpdatePhone validates and sets memberID's phone number. A blank (after
// trimming) rawPhone clears it. Returns domain.ErrInvalidPhoneFormat
// (wrapping the underlying ParseE164Phone failure) when rawPhone is
// non-blank but not valid E.164, and domain.ErrChannelNotDeliverable when
// rawPhone is non-blank but SMS has no wired sender (clearing a number is
// always allowed).
//
// Per ContactDirectory.SetPhone's own contract, clearing or CHANGING the
// number always resets opt-in — a member who removes their number, or
// enters a different one, must give fresh consent for it (docs/aws-sms.md's
// production consent gate).
func (s *SettingsService) UpdatePhone(ctx context.Context, memberID household.MemberID, rawPhone string) error {
	rawPhone = strings.TrimSpace(rawPhone)
	if rawPhone == "" {
		return s.contacts.SetPhone(ctx, memberID, nil)
	}
	if !s.deliverable[domain.ChannelSMS] {
		return domain.ErrChannelNotDeliverable
	}
	phone, err := domain.ParseE164Phone(rawPhone)
	if err != nil {
		return fmt.Errorf("%w: %w", domain.ErrInvalidPhoneFormat, err)
	}
	return s.contacts.SetPhone(ctx, memberID, &phone)
}

// SetOptIn sets memberID's SMS opt-in state. Setting true requires a
// phone number currently on file (domain.ErrPhoneRequiredForOptIn
// otherwise) — this check, enforced inside ContactDirectory.SetOptedIn,
// IS docs/aws-sms.md's production consent gate: the timestamp it records
// alongside the flag is the express-written-consent record itself.
// Setting true returns domain.ErrChannelNotDeliverable when SMS has no
// wired sender; withdrawing consent is always allowed.
func (s *SettingsService) SetOptIn(ctx context.Context, memberID household.MemberID, optIn bool) error {
	if optIn && !s.deliverable[domain.ChannelSMS] {
		return domain.ErrChannelNotDeliverable
	}
	return s.contacts.SetOptedIn(ctx, memberID, optIn)
}

// ListPreferences returns every explicit preference memberID has set.
// The caller (the settings section view builder) merges this sparse list
// against domain.AllEventTypes to render a complete, defaulted table —
// that is a presentation concern, not a business rule this service owns.
func (s *SettingsService) ListPreferences(ctx context.Context, memberID household.MemberID) ([]domain.MemberPreference, error) {
	return s.preferences.ListForMember(ctx, memberID)
}

// SetPreference validates and persists memberID's channel preference for
// eventType. It is a thin single-entry wrapper over SetPreferences (see
// that method's own doc for the SMS-readiness guard both share).
func (s *SettingsService) SetPreference(
	ctx context.Context,
	householdID household.HouseholdID,
	memberID household.MemberID,
	eventType domain.EventType,
	channel domain.Channel,
) error {
	return s.SetPreferences(ctx, householdID, memberID, map[domain.EventType]domain.Channel{eventType: channel})
}

// SetPreferences validates and persists a batch of memberID's channel
// preferences in one call, resolving SMS-readiness AT MOST ONCE regardless
// of how many entries in updates choose ChannelSMS (CodeRabbit round 2,
// trivial finding #6) — the web handler submitting all
// domain.AllEventTypes() rows from one settings-page save would otherwise
// trigger one contacts.GetContact call per sms row via repeated
// SetPreference calls.
//
// Every channel is format-validated, and SMS-readiness (when needed) is
// resolved, BEFORE anything is persisted: an invalid channel or a
// not-currently-SMS-ready member with at least one sms row rejects the
// WHOLE batch with nothing written, rather than partially applying rows
// that happened to be processed first — a stricter, more predictable
// contract than looping single SetPreference calls would have given
// (map iteration order is not defined in Go).
//
// Setting channel=ChannelSMS for ANY entry REQUIRES the member to
// currently be SMS-ready (domain.ErrMemberNotSMSReady otherwise) — the
// member_notification_pref.channel CHECK constraint cannot
// cross-reference the member table's own opt-in state, so this guard is
// the only place that rule is enforced (NES-139 plan step 8: "reject
// channel=sms preference writes when the member has no sms_opted_in_at").
//
// Every channel is also checked against the deliverable set injected at
// construction (domain.ErrChannelNotDeliverable otherwise):
// domain.Channel.Valid() alone accepts push too, since it remains a valid
// domain value for a future ticket's own Sender, and SMS or email may have
// no wired Sender in this deployment (NES-206). Persisting a preference
// for a channel nothing can send through would make the dispatcher fall
// back to in-app on every notification, or for push fail with no fallback
// at all (Dispatcher.fallbackToInApp covers SMS and, since NES-141,
// email — not push). Rejecting it here, at the write boundary, is the
// intentional defense-in-depth for a crafted POST that bypasses the
// settings UI's own <select> options — CodeRabbit PR #109 round 3.
func (s *SettingsService) SetPreferences(
	ctx context.Context,
	householdID household.HouseholdID,
	memberID household.MemberID,
	updates map[domain.EventType]domain.Channel,
) error {
	needsSMSCheck := false
	for _, channel := range updates {
		if !channel.Valid() {
			return fmt.Errorf("notify: invalid channel %q", channel)
		}
		if !s.deliverable[channel] {
			return domain.ErrChannelNotDeliverable
		}
		if channel == domain.ChannelSMS {
			needsSMSCheck = true
		}
	}

	if needsSMSCheck {
		contact, err := s.contacts.GetContact(ctx, memberID)
		if err != nil {
			return err
		}
		if !contact.ReadyForSMS() {
			return domain.ErrMemberNotSMSReady
		}
	}

	for eventType, channel := range updates {
		if err := s.preferences.Set(ctx, domain.MemberPreference{
			HouseholdID: householdID,
			MemberID:    memberID,
			EventType:   eventType,
			Channel:     channel,
		}); err != nil {
			return err
		}
	}
	return nil
}

// QuietHours returns householdID's current quiet-hours window — both nil
// when disabled.
func (s *SettingsService) QuietHours(ctx context.Context, householdID household.HouseholdID) (start, end *time.Duration, err error) {
	qh, err := s.households.GetQuietHours(ctx, householdID)
	if err != nil {
		return nil, nil, err
	}
	return qh.Start, qh.End, nil
}

// SetQuietHours updates householdID's quiet-hours window. Passing nil for
// both start and end disables quiet hours. The caller (the web handler)
// is responsible for the both-or-neither form-input validation — this
// method accepts any combination, including exactly one of the two set,
// since a future non-HTTP caller may have a legitimate reason to.
func (s *SettingsService) SetQuietHours(ctx context.Context, householdID household.HouseholdID, start, end *time.Duration) error {
	return s.households.SetQuietHours(ctx, householdID, start, end)
}
