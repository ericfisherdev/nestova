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
// Fakes
// ----------------------------------------------------------------------------

// fakePreferenceRepo is an in-memory domain.PreferenceRepository.
type fakePreferenceRepo struct {
	prefs  map[string]domain.Channel
	getErr error
}

func prefKey(memberID household.MemberID, eventType domain.EventType) string {
	return memberID.String() + "|" + eventType.String()
}

func (f *fakePreferenceRepo) Get(_ context.Context, memberID household.MemberID, eventType domain.EventType) (domain.Channel, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	ch, ok := f.prefs[prefKey(memberID, eventType)]
	if !ok {
		return "", domain.ErrPreferenceNotFound
	}
	return ch, nil
}

func (f *fakePreferenceRepo) Set(_ context.Context, pref domain.MemberPreference) error {
	if f.prefs == nil {
		f.prefs = make(map[string]domain.Channel)
	}
	f.prefs[prefKey(pref.MemberID, pref.EventType)] = pref.Channel
	return nil
}

func (f *fakePreferenceRepo) ListForMember(_ context.Context, _ household.MemberID) ([]domain.MemberPreference, error) {
	return nil, nil
}

// fakeContactDirectory is an in-memory domain.ContactDirectory.
type fakeContactDirectory struct {
	contact         *domain.MemberContact
	getErr          error
	getContactCalls int
}

func (f *fakeContactDirectory) GetContact(_ context.Context, memberID household.MemberID) (*domain.MemberContact, error) {
	f.getContactCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.contact != nil {
		return f.contact, nil
	}
	return &domain.MemberContact{MemberID: memberID}, nil
}

func (f *fakeContactDirectory) SetPhone(_ context.Context, _ household.MemberID, _ *domain.E164Phone) error {
	return nil
}

func (f *fakeContactDirectory) SetOptedIn(_ context.Context, _ household.MemberID, _ bool) error {
	return nil
}

// fakeHouseholdReader satisfies notify/app's own narrow householdReader
// port AND household.QuietHoursWriter — used both as a bare
// householdReader (routing tests) and as a full quietHoursStore
// (settings_test.go), via Go's ordinary structural interface typing.
type fakeHouseholdReader struct {
	household  *household.Household
	getErr     error
	setErr     error
	setCalls   int
	lastStart  *time.Duration
	lastEnd    *time.Duration
	lastCalled household.HouseholdID
}

func (f *fakeHouseholdReader) GetHousehold(_ context.Context, id household.HouseholdID) (*household.Household, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.household != nil {
		return f.household, nil
	}
	return &household.Household{ID: id}, nil
}

func (f *fakeHouseholdReader) SetQuietHours(_ context.Context, id household.HouseholdID, start, end *time.Duration) error {
	f.setCalls++
	f.lastStart, f.lastEnd, f.lastCalled = start, end, id
	return f.setErr
}

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

func readySMSContact(memberID household.MemberID) *domain.MemberContact {
	phone, _ := domain.ParseE164Phone("+15551234567")
	return &domain.MemberContact{MemberID: memberID, Phone: &phone, SMSOptedIn: true}
}

func newRoutedNotification(memberID household.MemberID, eventType domain.EventType, scheduledFor time.Time) *domain.Notification {
	return &domain.Notification{
		ID:           domain.NewNotificationID(),
		HouseholdID:  household.NewHouseholdID(),
		MemberID:     &memberID,
		Channel:      domain.ChannelInApp,
		Title:        "Title",
		Body:         "Body",
		ScheduledFor: scheduledFor,
		Status:       domain.StatusPending,
		EventType:    eventType,
	}
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

func TestRoutingEnqueuer_NoMemberID_PassesThroughUnchanged(t *testing.T) {
	outbox := &fakeOutbox{}
	e := app.NewRoutingEnqueuer(outbox, &fakePreferenceRepo{}, &fakeContactDirectory{}, &fakeHouseholdReader{}, silentLogger())

	n := &domain.Notification{
		ID:          domain.NewNotificationID(),
		HouseholdID: household.NewHouseholdID(),
		Channel:     domain.ChannelInApp,
		EventType:   domain.EventTypeRestockSoon,
		// MemberID deliberately nil — a household-wide notification.
	}
	if err := e.Enqueue(context.Background(), n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if n.Channel != domain.ChannelInApp {
		t.Errorf("Channel = %v, want unchanged ChannelInApp (no member to route for)", n.Channel)
	}
	if len(outbox.due) != 1 {
		t.Fatalf("outbox.due len = %d, want 1", len(outbox.due))
	}
}

func TestRoutingEnqueuer_NoEventType_PassesThroughUnchanged(t *testing.T) {
	outbox := &fakeOutbox{}
	memberID := household.NewMemberID()
	// A preference exists, but since EventType is empty, it must never be
	// consulted.
	prefs := &fakePreferenceRepo{prefs: map[string]domain.Channel{
		prefKey(memberID, domain.EventTypeClaimExpiring): domain.ChannelSMS,
	}}
	e := app.NewRoutingEnqueuer(outbox, prefs, &fakeContactDirectory{contact: readySMSContact(memberID)}, &fakeHouseholdReader{}, silentLogger())

	n := newRoutedNotification(memberID, "", time.Now())
	if err := e.Enqueue(context.Background(), n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if n.Channel != domain.ChannelInApp {
		t.Errorf("Channel = %v, want unchanged ChannelInApp (no event type to route for)", n.Channel)
	}
}

func TestRoutingEnqueuer_NoPreference_DefaultsToInApp(t *testing.T) {
	outbox := &fakeOutbox{}
	memberID := household.NewMemberID()
	e := app.NewRoutingEnqueuer(outbox, &fakePreferenceRepo{}, &fakeContactDirectory{}, &fakeHouseholdReader{}, silentLogger())

	n := newRoutedNotification(memberID, domain.EventTypeClaimExpiring, time.Now())
	if err := e.Enqueue(context.Background(), n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if n.Channel != domain.ChannelInApp {
		t.Errorf("Channel = %v, want ChannelInApp (sparse-table default)", n.Channel)
	}
}

func TestRoutingEnqueuer_SMSPreference_MemberReady_RoutesToSMS(t *testing.T) {
	outbox := &fakeOutbox{}
	memberID := household.NewMemberID()
	prefs := &fakePreferenceRepo{prefs: map[string]domain.Channel{
		prefKey(memberID, domain.EventTypeClaimExpiring): domain.ChannelSMS,
	}}
	contacts := &fakeContactDirectory{contact: readySMSContact(memberID)}
	e := app.NewRoutingEnqueuer(outbox, prefs, contacts, &fakeHouseholdReader{}, silentLogger())

	n := newRoutedNotification(memberID, domain.EventTypeClaimExpiring, time.Now())
	if err := e.Enqueue(context.Background(), n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if n.Channel != domain.ChannelSMS {
		t.Errorf("Channel = %v, want ChannelSMS", n.Channel)
	}
}

func TestRoutingEnqueuer_SMSPreference_MemberNotReady_FallsBackToInApp(t *testing.T) {
	tests := []struct {
		name    string
		contact *domain.MemberContact
	}{
		{"no phone on file", &domain.MemberContact{}},
		{"phone but not opted in", func() *domain.MemberContact {
			phone, _ := domain.ParseE164Phone("+15551234567")
			return &domain.MemberContact{Phone: &phone, SMSOptedIn: false}
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outbox := &fakeOutbox{}
			memberID := household.NewMemberID()
			prefs := &fakePreferenceRepo{prefs: map[string]domain.Channel{
				prefKey(memberID, domain.EventTypeClaimExpiring): domain.ChannelSMS,
			}}
			e := app.NewRoutingEnqueuer(outbox, prefs, &fakeContactDirectory{contact: tt.contact}, &fakeHouseholdReader{}, silentLogger())

			n := newRoutedNotification(memberID, domain.EventTypeClaimExpiring, time.Now())
			if err := e.Enqueue(context.Background(), n); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			// NES-139 AC: "removing a phone number or opting out stops SMS
			// immediately without losing notifications" — the fallback IS
			// in-app, not an error.
			if n.Channel != domain.ChannelInApp {
				t.Errorf("Channel = %v, want ChannelInApp (member not sms-ready)", n.Channel)
			}
		})
	}
}

func TestRoutingEnqueuer_SMSPreference_InsideQuietHours_ShiftsScheduledFor(t *testing.T) {
	outbox := &fakeOutbox{}
	memberID := household.NewMemberID()
	prefs := &fakePreferenceRepo{prefs: map[string]domain.Channel{
		prefKey(memberID, domain.EventTypeClaimExpiring): domain.ChannelSMS,
	}}
	contacts := &fakeContactDirectory{contact: readySMSContact(memberID)}

	start, end := 22*time.Hour, 7*time.Hour
	hh := &household.Household{QuietHoursStart: &start, QuietHoursEnd: &end}
	households := &fakeHouseholdReader{household: hh}
	e := app.NewRoutingEnqueuer(outbox, prefs, contacts, households, silentLogger())

	scheduledFor := time.Date(2026, time.July, 19, 23, 0, 0, 0, time.UTC) // inside 22:00-07:00
	n := newRoutedNotification(memberID, domain.EventTypeClaimExpiring, scheduledFor)
	if err := e.Enqueue(context.Background(), n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if n.Channel != domain.ChannelSMS {
		t.Fatalf("Channel = %v, want ChannelSMS", n.Channel)
	}
	want := hh.QuietHoursEndAfter(scheduledFor)
	if !n.ScheduledFor.Equal(want) {
		t.Errorf("ScheduledFor = %v, want %v (shifted to quiet-hours end)", n.ScheduledFor, want)
	}
	if n.ScheduledFor.Equal(scheduledFor) {
		t.Error("ScheduledFor was not shifted at all")
	}
}

func TestRoutingEnqueuer_SMSPreference_OutsideQuietHours_NoShift(t *testing.T) {
	outbox := &fakeOutbox{}
	memberID := household.NewMemberID()
	prefs := &fakePreferenceRepo{prefs: map[string]domain.Channel{
		prefKey(memberID, domain.EventTypeClaimExpiring): domain.ChannelSMS,
	}}
	contacts := &fakeContactDirectory{contact: readySMSContact(memberID)}

	start, end := 22*time.Hour, 7*time.Hour
	hh := &household.Household{QuietHoursStart: &start, QuietHoursEnd: &end}
	households := &fakeHouseholdReader{household: hh}
	e := app.NewRoutingEnqueuer(outbox, prefs, contacts, households, silentLogger())

	scheduledFor := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC) // midday, outside 22:00-07:00
	n := newRoutedNotification(memberID, domain.EventTypeClaimExpiring, scheduledFor)
	if err := e.Enqueue(context.Background(), n); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !n.ScheduledFor.Equal(scheduledFor) {
		t.Errorf("ScheduledFor = %v, want unchanged %v", n.ScheduledFor, scheduledFor)
	}
}

func TestRoutingEnqueuer_PreferenceLookupError_KeepsDefaultChannel(t *testing.T) {
	outbox := &fakeOutbox{}
	memberID := household.NewMemberID()
	prefs := &fakePreferenceRepo{getErr: errors.New("db unavailable")}
	e := app.NewRoutingEnqueuer(outbox, prefs, &fakeContactDirectory{}, &fakeHouseholdReader{}, silentLogger())

	n := newRoutedNotification(memberID, domain.EventTypeClaimExpiring, time.Now())
	if err := e.Enqueue(context.Background(), n); err != nil {
		t.Fatalf("Enqueue: %v, want nil (a routing failure must not block enqueueing)", err)
	}
	if n.Channel != domain.ChannelInApp {
		t.Errorf("Channel = %v, want unchanged ChannelInApp (caller's own default)", n.Channel)
	}
}

func TestRoutingEnqueuer_ContactLookupError_KeepsDefaultChannel(t *testing.T) {
	outbox := &fakeOutbox{}
	memberID := household.NewMemberID()
	prefs := &fakePreferenceRepo{prefs: map[string]domain.Channel{
		prefKey(memberID, domain.EventTypeClaimExpiring): domain.ChannelSMS,
	}}
	contacts := &fakeContactDirectory{getErr: errors.New("db unavailable")}
	e := app.NewRoutingEnqueuer(outbox, prefs, contacts, &fakeHouseholdReader{}, silentLogger())

	n := newRoutedNotification(memberID, domain.EventTypeClaimExpiring, time.Now())
	if err := e.Enqueue(context.Background(), n); err != nil {
		t.Fatalf("Enqueue: %v, want nil", err)
	}
	if n.Channel != domain.ChannelInApp {
		t.Errorf("Channel = %v, want unchanged ChannelInApp", n.Channel)
	}
}

// TestRoutingEnqueuer_HouseholdLookupError_FallsBackToInApp is the
// regression test for CodeRabbit round 3 (major finding #1): a household
// lookup failure must reset Channel to ChannelInApp, not leave it on SMS
// with the quiet-hours check simply skipped — the earlier behavior risked
// sending SMS at any hour, including inside quiet hours, whenever the
// household lookup happened to fail.
func TestRoutingEnqueuer_HouseholdLookupError_FallsBackToInApp(t *testing.T) {
	outbox := &fakeOutbox{}
	memberID := household.NewMemberID()
	prefs := &fakePreferenceRepo{prefs: map[string]domain.Channel{
		prefKey(memberID, domain.EventTypeClaimExpiring): domain.ChannelSMS,
	}}
	contacts := &fakeContactDirectory{contact: readySMSContact(memberID)}
	households := &fakeHouseholdReader{getErr: errors.New("db unavailable")}
	e := app.NewRoutingEnqueuer(outbox, prefs, contacts, households, silentLogger())

	scheduledFor := time.Now()
	n := newRoutedNotification(memberID, domain.EventTypeClaimExpiring, scheduledFor)
	if err := e.Enqueue(context.Background(), n); err != nil {
		t.Fatalf("Enqueue: %v, want nil", err)
	}
	if n.Channel != domain.ChannelInApp {
		t.Errorf("Channel = %v, want ChannelInApp (the household lookup failure must fall back, not leave Channel on SMS with no deferral check)", n.Channel)
	}
	if !n.ScheduledFor.Equal(scheduledFor) {
		t.Errorf("ScheduledFor = %v, want unchanged %v (no deferral is applied on the in-app fallback path)", n.ScheduledFor, scheduledFor)
	}
}

func TestRoutingEnqueuer_NilNotification_PassesThrough(t *testing.T) {
	outbox := &fakeOutbox{}
	e := app.NewRoutingEnqueuer(outbox, &fakePreferenceRepo{}, &fakeContactDirectory{}, &fakeHouseholdReader{}, silentLogger())

	if err := e.Enqueue(context.Background(), nil); err != nil {
		t.Fatalf("Enqueue(nil): %v", err)
	}
}

func TestNewRoutingEnqueuer_NilDependencies_Panic(t *testing.T) {
	outbox := &fakeOutbox{}
	prefs := &fakePreferenceRepo{}
	contacts := &fakeContactDirectory{}
	households := &fakeHouseholdReader{}
	logger := silentLogger()

	tests := []struct {
		name string
		fn   func()
	}{
		{"nil next", func() { app.NewRoutingEnqueuer(nil, prefs, contacts, households, logger) }},
		{"nil preferences", func() { app.NewRoutingEnqueuer(outbox, nil, contacts, households, logger) }},
		{"nil contacts", func() { app.NewRoutingEnqueuer(outbox, prefs, nil, households, logger) }},
		{"nil households", func() { app.NewRoutingEnqueuer(outbox, prefs, contacts, nil, logger) }},
		{"nil logger", func() { app.NewRoutingEnqueuer(outbox, prefs, contacts, households, nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("NewRoutingEnqueuer did not panic")
				}
			}()
			tt.fn()
		})
	}
}
