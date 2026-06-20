package domain_test

import (
	"errors"
	"testing"
	"time"

	calendar "github.com/ericfisherdev/nestova/internal/calendar/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func validAccount() calendar.CalendarAccount {
	return calendar.CalendarAccount{
		ID:              calendar.NewCalendarAccountID(),
		MemberID:        household.NewMemberID(),
		HouseholdID:     household.NewHouseholdID(),
		Provider:        calendar.ProviderGoogle,
		AccessTokenEnc:  []byte{0x01, 0x02},
		RefreshTokenEnc: []byte{0x03, 0x04},
		TokenExpiry:     time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		CalendarIDs:     []string{"primary"},
	}
}

func TestCalendarAccountValidate(t *testing.T) {
	if err := validAccount().Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestCalendarAccountValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*calendar.CalendarAccount)
	}{
		{"zero id", func(a *calendar.CalendarAccount) { a.ID = calendar.CalendarAccountID{} }},
		{"zero member id", func(a *calendar.CalendarAccount) { a.MemberID = household.MemberID{} }},
		{"zero household id", func(a *calendar.CalendarAccount) { a.HouseholdID = household.HouseholdID{} }},
		{"unknown provider", func(a *calendar.CalendarAccount) { a.Provider = calendar.Provider("outlook") }},
		{"empty access token", func(a *calendar.CalendarAccount) { a.AccessTokenEnc = nil }},
		{"empty refresh token", func(a *calendar.CalendarAccount) { a.RefreshTokenEnc = []byte{} }},
		{"zero token expiry", func(a *calendar.CalendarAccount) { a.TokenExpiry = time.Time{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validAccount()
			tc.mutate(&a)
			if err := a.Validate(); !errors.Is(err, calendar.ErrInvalidCalendarAccount) {
				t.Fatalf("Validate() error = %v, want ErrInvalidCalendarAccount", err)
			}
		})
	}
}

func validEvent() calendar.ExternalEvent {
	return calendar.ExternalEvent{
		ID:                calendar.NewExternalEventID(),
		CalendarAccountID: calendar.NewCalendarAccountID(),
		ExternalID:        "evt-123",
		Title:             "Dentist",
		StartsAt:          time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
		EndsAt:            time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}
}

func TestExternalEventValidate(t *testing.T) {
	if err := validEvent().Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
	// A zero-duration event (end == start) is permitted.
	e := validEvent()
	e.EndsAt = e.StartsAt
	if err := e.Validate(); err != nil {
		t.Fatalf("Validate() zero-duration error = %v, want nil", err)
	}
}

func TestExternalEventValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*calendar.ExternalEvent)
	}{
		{"zero id", func(e *calendar.ExternalEvent) { e.ID = calendar.ExternalEventID{} }},
		{"zero account id", func(e *calendar.ExternalEvent) { e.CalendarAccountID = calendar.CalendarAccountID{} }},
		{"blank external id", func(e *calendar.ExternalEvent) { e.ExternalID = "" }},
		{"whitespace external id", func(e *calendar.ExternalEvent) { e.ExternalID = "   " }},
		{"zero start", func(e *calendar.ExternalEvent) { e.StartsAt = time.Time{} }},
		{"zero end", func(e *calendar.ExternalEvent) { e.EndsAt = time.Time{} }},
		{"end before start", func(e *calendar.ExternalEvent) { e.EndsAt = e.StartsAt.Add(-time.Hour) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent()
			tc.mutate(&e)
			if err := e.Validate(); !errors.Is(err, calendar.ErrInvalidExternalEvent) {
				t.Fatalf("Validate() error = %v, want ErrInvalidExternalEvent", err)
			}
		})
	}
}
