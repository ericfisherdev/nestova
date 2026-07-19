package domain_test

import (
	"testing"

	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

func TestChannelParseAndValid(t *testing.T) {
	cases := []struct {
		input string
		want  domain.Channel
	}{
		{"push", domain.ChannelPush},
		{"email", domain.ChannelEmail},
		{"inapp", domain.ChannelInApp},
		{"sms", domain.ChannelSMS},
	}
	for _, tc := range cases {
		got, err := domain.ParseChannel(tc.input)
		if err != nil {
			t.Errorf("ParseChannel(%q) error = %v, want nil", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("ParseChannel(%q) = %v, want %v", tc.input, got, tc.want)
		}
		if !got.Valid() {
			t.Errorf("Channel(%q).Valid() = false, want true", tc.input)
		}
		if got.String() != tc.input {
			t.Errorf("Channel(%q).String() = %q, want %q", tc.input, got.String(), tc.input)
		}
	}
}

func TestChannelParseUnknown(t *testing.T) {
	_, err := domain.ParseChannel("carrier_pigeon")
	if err == nil {
		t.Error("ParseChannel(unknown) error = nil, want non-nil")
	}
}

func TestChannelValid(t *testing.T) {
	if domain.Channel("carrier_pigeon").Valid() {
		t.Error("Channel(carrier_pigeon).Valid() = true, want false")
	}
}

func TestStatusParseAndValid(t *testing.T) {
	cases := []struct {
		input string
		want  domain.Status
	}{
		{"pending", domain.StatusPending},
		{"sent", domain.StatusSent},
		{"failed", domain.StatusFailed},
		{"cancelled", domain.StatusCancelled},
	}
	for _, tc := range cases {
		got, err := domain.ParseStatus(tc.input)
		if err != nil {
			t.Errorf("ParseStatus(%q) error = %v, want nil", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("ParseStatus(%q) = %v, want %v", tc.input, got, tc.want)
		}
		if !got.Valid() {
			t.Errorf("Status(%q).Valid() = false, want true", tc.input)
		}
		if got.String() != tc.input {
			t.Errorf("Status(%q).String() = %q, want %q", tc.input, got.String(), tc.input)
		}
	}
}

func TestStatusParseUnknown(t *testing.T) {
	_, err := domain.ParseStatus("unknown")
	if err == nil {
		t.Error("ParseStatus(unknown) error = nil, want non-nil")
	}
}

func TestStatusValid(t *testing.T) {
	if domain.Status("unknown").Valid() {
		t.Error("Status(unknown).Valid() = true, want false")
	}
}

func TestEventTypeParseAndValid(t *testing.T) {
	for _, et := range domain.AllEventTypes() {
		got, err := domain.ParseEventType(et.String())
		if err != nil {
			t.Errorf("ParseEventType(%q) error = %v, want nil", et.String(), err)
		}
		if got != et {
			t.Errorf("ParseEventType(%q) = %v, want %v", et.String(), got, et)
		}
		if !got.Valid() {
			t.Errorf("EventType(%q).Valid() = false, want true", et.String())
		}
		if got.Label() == "" {
			t.Errorf("EventType(%q).Label() is empty, want a non-empty member-facing label", et.String())
		}
	}
}

func TestEventTypeParseUnknown(t *testing.T) {
	_, err := domain.ParseEventType("carrier_pigeon_arrived")
	if err == nil {
		t.Error("ParseEventType(unknown) error = nil, want non-nil")
	}
}

func TestEventTypeValid(t *testing.T) {
	if domain.EventType("carrier_pigeon_arrived").Valid() {
		t.Error("EventType(carrier_pigeon_arrived).Valid() = true, want false")
	}
	// The zero value (an empty EventType, meaning "not preference-routable"
	// — see Notification.EventType's own doc) is deliberately NOT a valid
	// event type: it must never be looked up in member_notification_pref.
	if domain.EventType("").Valid() {
		t.Error(`EventType("").Valid() = true, want false`)
	}
}

func TestAllEventTypes_NoDuplicates(t *testing.T) {
	seen := make(map[domain.EventType]bool)
	for _, et := range domain.AllEventTypes() {
		if seen[et] {
			t.Errorf("AllEventTypes() contains duplicate %q", et.String())
		}
		seen[et] = true
	}
}

func TestNotificationIDRoundTrip(t *testing.T) {
	id := domain.NewNotificationID()
	s := id.String()
	parsed, err := domain.ParseNotificationID(s)
	if err != nil {
		t.Fatalf("ParseNotificationID(%q) error = %v", s, err)
	}
	if parsed != id {
		t.Errorf("ParseNotificationID round-trip: got %v, want %v", parsed, id)
	}
}

func TestParseNotificationIDInvalid(t *testing.T) {
	_, err := domain.ParseNotificationID("not-a-uuid")
	if err == nil {
		t.Error("ParseNotificationID(invalid) error = nil, want non-nil")
	}
}
