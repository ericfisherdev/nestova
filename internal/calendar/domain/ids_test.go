package domain_test

import (
	"testing"

	calendar "github.com/ericfisherdev/nestova/internal/calendar/domain"
)

func TestCalendarAccountIDRoundTrip(t *testing.T) {
	id := calendar.NewCalendarAccountID()
	parsed, err := calendar.ParseCalendarAccountID(id.String())
	if err != nil {
		t.Fatalf("ParseCalendarAccountID(%q) error = %v", id.String(), err)
	}
	if parsed != id {
		t.Errorf("ParseCalendarAccountID round-trip: got %v, want %v", parsed, id)
	}
}

func TestExternalEventIDRoundTrip(t *testing.T) {
	id := calendar.NewExternalEventID()
	parsed, err := calendar.ParseExternalEventID(id.String())
	if err != nil {
		t.Fatalf("ParseExternalEventID(%q) error = %v", id.String(), err)
	}
	if parsed != id {
		t.Errorf("ParseExternalEventID round-trip: got %v, want %v", parsed, id)
	}
}

func TestParseCalendarIDsInvalid(t *testing.T) {
	if _, err := calendar.ParseCalendarAccountID("not-a-uuid"); err == nil {
		t.Error("ParseCalendarAccountID(\"not-a-uuid\") error = nil, want non-nil")
	}
	if _, err := calendar.ParseExternalEventID("not-a-uuid"); err == nil {
		t.Error("ParseExternalEventID(\"not-a-uuid\") error = nil, want non-nil")
	}
}
