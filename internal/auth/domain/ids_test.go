package domain_test

import (
	"testing"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
)

func TestRememberedDeviceID_RoundTrips(t *testing.T) {
	t.Parallel()
	id := authdomain.NewRememberedDeviceID()
	parsed, err := authdomain.ParseRememberedDeviceID(id.String())
	if err != nil {
		t.Fatalf("ParseRememberedDeviceID(%q): %v", id.String(), err)
	}
	if parsed != id {
		t.Errorf("round trip = %s, want %s", parsed, id)
	}
}

func TestParseRememberedDeviceID_RejectsInvalid(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "not-a-uuid", "1234"} {
		if _, err := authdomain.ParseRememberedDeviceID(in); err == nil {
			t.Errorf("ParseRememberedDeviceID(%q) = nil error, want rejection", in)
		}
	}
}
