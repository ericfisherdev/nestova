package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/notify/app"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// ----------------------------------------------------------------------------
// Additional fake behavior needed only here: fakeContactDirectory /
// fakePreferenceRepo need to record what they were called with, which the
// routing tests above did not require. contactSpy/preferenceSpy wrap the
// shared fakes rather than duplicating them.
// ----------------------------------------------------------------------------

type contactSpy struct {
	fakeContactDirectory
	setPhoneCalls   []*domain.E164Phone
	setOptedInCalls []bool
	setPhoneErr     error
	setOptedInErr   error
}

func (s *contactSpy) SetPhone(_ context.Context, _ household.MemberID, phone *domain.E164Phone) error {
	s.setPhoneCalls = append(s.setPhoneCalls, phone)
	return s.setPhoneErr
}

func (s *contactSpy) SetOptedIn(_ context.Context, _ household.MemberID, optIn bool) error {
	s.setOptedInCalls = append(s.setOptedInCalls, optIn)
	return s.setOptedInErr
}

func newSettingsService(contacts domain.ContactDirectory, prefs domain.PreferenceRepository, households *fakeHouseholdReader) *app.SettingsService {
	return app.NewSettingsService(contacts, prefs, households)
}

// ----------------------------------------------------------------------------
// UpdatePhone
// ----------------------------------------------------------------------------

func TestSettingsService_UpdatePhone_Valid(t *testing.T) {
	contacts := &contactSpy{}
	svc := newSettingsService(contacts, &fakePreferenceRepo{}, &fakeHouseholdReader{})

	if err := svc.UpdatePhone(context.Background(), household.NewMemberID(), "+15551234567"); err != nil {
		t.Fatalf("UpdatePhone: %v", err)
	}
	if len(contacts.setPhoneCalls) != 1 {
		t.Fatalf("SetPhone calls = %d, want 1", len(contacts.setPhoneCalls))
	}
	got := contacts.setPhoneCalls[0]
	if got == nil || got.String() != "+15551234567" {
		t.Errorf("SetPhone called with %v, want +15551234567", got)
	}
}

func TestSettingsService_UpdatePhone_BlankClears(t *testing.T) {
	contacts := &contactSpy{}
	svc := newSettingsService(contacts, &fakePreferenceRepo{}, &fakeHouseholdReader{})

	if err := svc.UpdatePhone(context.Background(), household.NewMemberID(), "   "); err != nil {
		t.Fatalf("UpdatePhone: %v", err)
	}
	if len(contacts.setPhoneCalls) != 1 || contacts.setPhoneCalls[0] != nil {
		t.Errorf("SetPhone calls = %v, want a single nil call (clear)", contacts.setPhoneCalls)
	}
}

func TestSettingsService_UpdatePhone_InvalidFormat(t *testing.T) {
	contacts := &contactSpy{}
	svc := newSettingsService(contacts, &fakePreferenceRepo{}, &fakeHouseholdReader{})

	err := svc.UpdatePhone(context.Background(), household.NewMemberID(), "not-a-phone-number")
	if !errors.Is(err, domain.ErrInvalidPhoneFormat) {
		t.Fatalf("UpdatePhone error = %v, want ErrInvalidPhoneFormat", err)
	}
	if len(contacts.setPhoneCalls) != 0 {
		t.Error("SetPhone must not be called for an invalid number")
	}
}

// ----------------------------------------------------------------------------
// SetOptIn
// ----------------------------------------------------------------------------

func TestSettingsService_SetOptIn_PassesThrough(t *testing.T) {
	contacts := &contactSpy{}
	svc := newSettingsService(contacts, &fakePreferenceRepo{}, &fakeHouseholdReader{})

	if err := svc.SetOptIn(context.Background(), household.NewMemberID(), true); err != nil {
		t.Fatalf("SetOptIn: %v", err)
	}
	if len(contacts.setOptedInCalls) != 1 || !contacts.setOptedInCalls[0] {
		t.Errorf("SetOptedIn calls = %v, want [true]", contacts.setOptedInCalls)
	}
}

func TestSettingsService_SetOptIn_PropagatesPhoneRequiredError(t *testing.T) {
	contacts := &contactSpy{setOptedInErr: domain.ErrPhoneRequiredForOptIn}
	svc := newSettingsService(contacts, &fakePreferenceRepo{}, &fakeHouseholdReader{})

	err := svc.SetOptIn(context.Background(), household.NewMemberID(), true)
	if !errors.Is(err, domain.ErrPhoneRequiredForOptIn) {
		t.Fatalf("SetOptIn error = %v, want ErrPhoneRequiredForOptIn", err)
	}
}

// ----------------------------------------------------------------------------
// SetPreference
// ----------------------------------------------------------------------------

func TestSettingsService_SetPreference_NonSMSChannel_NoContactCheck(t *testing.T) {
	memberID := household.NewMemberID()
	prefs := &fakePreferenceRepo{}
	// A contact directory that would ERROR if consulted — proves the
	// in-app path never calls it.
	contacts := &fakeContactDirectory{getErr: errors.New("must not be called")}
	svc := newSettingsService(contacts, prefs, &fakeHouseholdReader{})

	err := svc.SetPreference(context.Background(), household.NewHouseholdID(), memberID, domain.EventTypeClaimExpiring, domain.ChannelInApp)
	if err != nil {
		t.Fatalf("SetPreference: %v", err)
	}
	got, err := prefs.Get(context.Background(), memberID, domain.EventTypeClaimExpiring)
	if err != nil || got != domain.ChannelInApp {
		t.Errorf("stored preference = (%v, %v), want (ChannelInApp, nil)", got, err)
	}
}

func TestSettingsService_SetPreference_SMSChannel_MemberReady_Succeeds(t *testing.T) {
	memberID := household.NewMemberID()
	prefs := &fakePreferenceRepo{}
	contacts := &fakeContactDirectory{contact: readySMSContact(memberID)}
	svc := newSettingsService(contacts, prefs, &fakeHouseholdReader{})

	if err := svc.SetPreference(context.Background(), household.NewHouseholdID(), memberID, domain.EventTypeClaimExpiring, domain.ChannelSMS); err != nil {
		t.Fatalf("SetPreference: %v", err)
	}
	got, err := prefs.Get(context.Background(), memberID, domain.EventTypeClaimExpiring)
	if err != nil || got != domain.ChannelSMS {
		t.Errorf("stored preference = (%v, %v), want (ChannelSMS, nil)", got, err)
	}
}

func TestSettingsService_SetPreference_SMSChannel_MemberNotReady_Rejected(t *testing.T) {
	memberID := household.NewMemberID()
	prefs := &fakePreferenceRepo{}
	contacts := &fakeContactDirectory{} // no phone, not opted in
	svc := newSettingsService(contacts, prefs, &fakeHouseholdReader{})

	err := svc.SetPreference(context.Background(), household.NewHouseholdID(), memberID, domain.EventTypeClaimExpiring, domain.ChannelSMS)
	if !errors.Is(err, domain.ErrMemberNotSMSReady) {
		t.Fatalf("SetPreference error = %v, want ErrMemberNotSMSReady", err)
	}
	if _, err := prefs.Get(context.Background(), memberID, domain.EventTypeClaimExpiring); !errors.Is(err, domain.ErrPreferenceNotFound) {
		t.Error("a rejected sms preference must not be persisted")
	}
}

func TestSettingsService_SetPreference_InvalidChannel_Rejected(t *testing.T) {
	svc := newSettingsService(&fakeContactDirectory{}, &fakePreferenceRepo{}, &fakeHouseholdReader{})

	err := svc.SetPreference(context.Background(), household.NewHouseholdID(), household.NewMemberID(), domain.EventTypeClaimExpiring, domain.Channel("carrier_pigeon"))
	if err == nil {
		t.Fatal("SetPreference(invalid channel) error = nil, want non-nil")
	}
}

// ----------------------------------------------------------------------------
// SetPreferences (batch) — CodeRabbit round 2, trivial finding #6.
// ----------------------------------------------------------------------------

func TestSettingsService_SetPreferences_MultipleSMSRows_ChecksReadinessOnce(t *testing.T) {
	memberID := household.NewMemberID()
	prefs := &fakePreferenceRepo{}
	contacts := &fakeContactDirectory{contact: readySMSContact(memberID)}
	svc := newSettingsService(contacts, prefs, &fakeHouseholdReader{})

	updates := map[domain.EventType]domain.Channel{
		domain.EventTypeClaimExpiring:     domain.ChannelSMS,
		domain.EventTypeTaskOverdue:       domain.ChannelSMS,
		domain.EventTypeChoreTradeExpired: domain.ChannelSMS,
	}
	if err := svc.SetPreferences(context.Background(), household.NewHouseholdID(), memberID, updates); err != nil {
		t.Fatalf("SetPreferences: %v", err)
	}
	if contacts.getContactCalls != 1 {
		t.Errorf("GetContact calls = %d, want exactly 1 for a 3-row all-sms batch", contacts.getContactCalls)
	}
	for et := range updates {
		got, err := prefs.Get(context.Background(), memberID, et)
		if err != nil || got != domain.ChannelSMS {
			t.Errorf("stored preference for %s = (%v, %v), want (ChannelSMS, nil)", et, got, err)
		}
	}
}

func TestSettingsService_SetPreferences_NoSMSRows_NeverCallsGetContact(t *testing.T) {
	memberID := household.NewMemberID()
	contacts := &fakeContactDirectory{getErr: errors.New("must not be called")}
	svc := newSettingsService(contacts, &fakePreferenceRepo{}, &fakeHouseholdReader{})

	updates := map[domain.EventType]domain.Channel{
		domain.EventTypeClaimExpiring: domain.ChannelInApp,
		domain.EventTypeTaskOverdue:   domain.ChannelInApp,
	}
	if err := svc.SetPreferences(context.Background(), household.NewHouseholdID(), memberID, updates); err != nil {
		t.Fatalf("SetPreferences: %v", err)
	}
	if contacts.getContactCalls != 0 {
		t.Errorf("GetContact calls = %d, want 0 when no row chooses sms", contacts.getContactCalls)
	}
}

func TestSettingsService_SetPreferences_NotReady_RejectsWholeBatch(t *testing.T) {
	memberID := household.NewMemberID()
	prefs := &fakePreferenceRepo{}
	contacts := &fakeContactDirectory{} // no phone, not opted in
	svc := newSettingsService(contacts, prefs, &fakeHouseholdReader{})

	updates := map[domain.EventType]domain.Channel{
		domain.EventTypeClaimExpiring: domain.ChannelInApp,
		domain.EventTypeTaskOverdue:   domain.ChannelSMS,
	}
	err := svc.SetPreferences(context.Background(), household.NewHouseholdID(), memberID, updates)
	if !errors.Is(err, domain.ErrMemberNotSMSReady) {
		t.Fatalf("SetPreferences error = %v, want ErrMemberNotSMSReady", err)
	}
	// Nothing in the batch is persisted — not even the valid in-app row —
	// since readiness is resolved before any preferences.Set call.
	if _, err := prefs.Get(context.Background(), memberID, domain.EventTypeClaimExpiring); !errors.Is(err, domain.ErrPreferenceNotFound) {
		t.Error("a rejected batch must not persist any of its rows, including the valid ones")
	}
}

func TestSettingsService_SetPreferences_InvalidChannel_RejectsWholeBatch(t *testing.T) {
	memberID := household.NewMemberID()
	prefs := &fakePreferenceRepo{}
	svc := newSettingsService(&fakeContactDirectory{}, prefs, &fakeHouseholdReader{})

	updates := map[domain.EventType]domain.Channel{
		domain.EventTypeClaimExpiring: domain.ChannelInApp,
		domain.EventTypeTaskOverdue:   domain.Channel("carrier_pigeon"),
	}
	err := svc.SetPreferences(context.Background(), household.NewHouseholdID(), memberID, updates)
	if err == nil {
		t.Fatal("SetPreferences(one invalid channel) error = nil, want non-nil")
	}
	if _, err := prefs.Get(context.Background(), memberID, domain.EventTypeClaimExpiring); !errors.Is(err, domain.ErrPreferenceNotFound) {
		t.Error("a rejected batch must not persist any of its rows, including the valid ones")
	}
}

// TestSettingsService_SetPreferences_UndeliverableChannel_Rejected is the
// regression test for CodeRabbit round 3 (major finding #2): push and
// email are valid domain.Channel values (Channel.Valid() accepts them —
// see TestSettingsService_SetPreferences_UndeliverableChannel_IsAValidChannel
// below) but have no wired Sender in this deployment yet, so a crafted
// POST choosing either must be rejected at the preference-write boundary,
// not merely at the settings UI's own <select> (which never offers them).
func TestSettingsService_SetPreferences_UndeliverableChannel_Rejected(t *testing.T) {
	for _, channel := range []domain.Channel{domain.ChannelPush, domain.ChannelEmail} {
		t.Run(channel.String(), func(t *testing.T) {
			memberID := household.NewMemberID()
			prefs := &fakePreferenceRepo{}
			svc := newSettingsService(&fakeContactDirectory{}, prefs, &fakeHouseholdReader{})

			updates := map[domain.EventType]domain.Channel{domain.EventTypeClaimExpiring: channel}
			err := svc.SetPreferences(context.Background(), household.NewHouseholdID(), memberID, updates)
			if !errors.Is(err, domain.ErrChannelNotDeliverable) {
				t.Fatalf("SetPreferences(%s) error = %v, want ErrChannelNotDeliverable", channel, err)
			}
			if _, err := prefs.Get(context.Background(), memberID, domain.EventTypeClaimExpiring); !errors.Is(err, domain.ErrPreferenceNotFound) {
				t.Errorf("SetPreferences(%s) must not persist anything", channel)
			}
		})
	}
}

// TestSettingsService_SetPreferences_UndeliverableChannel_IsAValidChannel
// pins the OTHER half of finding #2's contract: domain.ParseChannel/
// Channel.Valid() themselves must stay unchanged — push and email remain
// valid domain.Channel values for a future ticket's own Sender; only the
// PREFERENCE-WRITE boundary narrows further.
func TestSettingsService_SetPreferences_UndeliverableChannel_IsAValidChannel(t *testing.T) {
	for _, channel := range []domain.Channel{domain.ChannelPush, domain.ChannelEmail} {
		if !channel.Valid() {
			t.Errorf("domain.Channel(%q).Valid() = false, want true (ParseChannel must stay unrestricted)", channel)
		}
	}
}

func TestSettingsService_SetPreferences_UndeliverableChannel_MixedWithValid_RejectsWholeBatch(t *testing.T) {
	memberID := household.NewMemberID()
	prefs := &fakePreferenceRepo{}
	svc := newSettingsService(&fakeContactDirectory{}, prefs, &fakeHouseholdReader{})

	updates := map[domain.EventType]domain.Channel{
		domain.EventTypeClaimExpiring: domain.ChannelInApp,
		domain.EventTypeTaskOverdue:   domain.ChannelEmail,
	}
	err := svc.SetPreferences(context.Background(), household.NewHouseholdID(), memberID, updates)
	if !errors.Is(err, domain.ErrChannelNotDeliverable) {
		t.Fatalf("SetPreferences error = %v, want ErrChannelNotDeliverable", err)
	}
	if _, err := prefs.Get(context.Background(), memberID, domain.EventTypeClaimExpiring); !errors.Is(err, domain.ErrPreferenceNotFound) {
		t.Error("a rejected batch must not persist any of its rows, including the valid in-app one")
	}
}

// ----------------------------------------------------------------------------
// QuietHours / SetQuietHours
// ----------------------------------------------------------------------------

func TestSettingsService_QuietHours_ReturnsHouseholdBounds(t *testing.T) {
	start, end := 22*time.Hour, 7*time.Hour
	households := &fakeHouseholdReader{household: &household.Household{QuietHoursStart: &start, QuietHoursEnd: &end}}
	svc := newSettingsService(&fakeContactDirectory{}, &fakePreferenceRepo{}, households)

	gotStart, gotEnd, err := svc.QuietHours(context.Background(), household.NewHouseholdID())
	if err != nil {
		t.Fatalf("QuietHours: %v", err)
	}
	if gotStart == nil || *gotStart != start || gotEnd == nil || *gotEnd != end {
		t.Errorf("QuietHours() = (%v, %v), want (%v, %v)", gotStart, gotEnd, start, end)
	}
}

func TestSettingsService_SetQuietHours_PassesThrough(t *testing.T) {
	households := &fakeHouseholdReader{}
	svc := newSettingsService(&fakeContactDirectory{}, &fakePreferenceRepo{}, households)

	start, end := 22*time.Hour, 7*time.Hour
	hhID := household.NewHouseholdID()
	if err := svc.SetQuietHours(context.Background(), hhID, &start, &end); err != nil {
		t.Fatalf("SetQuietHours: %v", err)
	}
	if households.setCalls != 1 {
		t.Fatalf("SetQuietHours calls = %d, want 1", households.setCalls)
	}
	if households.lastCalled != hhID {
		t.Error("SetQuietHours called with the wrong household id")
	}
	if households.lastStart == nil || *households.lastStart != start || households.lastEnd == nil || *households.lastEnd != end {
		t.Errorf("SetQuietHours called with (%v, %v), want (%v, %v)", households.lastStart, households.lastEnd, start, end)
	}
}

func TestSettingsService_SetQuietHours_NilDisables(t *testing.T) {
	households := &fakeHouseholdReader{}
	svc := newSettingsService(&fakeContactDirectory{}, &fakePreferenceRepo{}, households)

	if err := svc.SetQuietHours(context.Background(), household.NewHouseholdID(), nil, nil); err != nil {
		t.Fatalf("SetQuietHours: %v", err)
	}
	if households.lastStart != nil || households.lastEnd != nil {
		t.Errorf("SetQuietHours(nil, nil) called with (%v, %v), want (nil, nil)", households.lastStart, households.lastEnd)
	}
}

// ----------------------------------------------------------------------------
// NewSettingsService
// ----------------------------------------------------------------------------

func TestNewSettingsService_NilDependencies_Panic(t *testing.T) {
	contacts := &fakeContactDirectory{}
	prefs := &fakePreferenceRepo{}
	households := &fakeHouseholdReader{}

	tests := []struct {
		name string
		fn   func()
	}{
		{"nil contacts", func() { app.NewSettingsService(nil, prefs, households) }},
		{"nil preferences", func() { app.NewSettingsService(contacts, nil, households) }},
		{"nil households", func() { app.NewSettingsService(contacts, prefs, nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("NewSettingsService did not panic")
				}
			}()
			tt.fn()
		})
	}
}
