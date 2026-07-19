package domain_test

import (
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

func testPhone(t *testing.T) domain.E164Phone {
	t.Helper()
	p, err := domain.ParseE164Phone("+15551234567")
	if err != nil {
		t.Fatalf("ParseE164Phone: %v", err)
	}
	return p
}

func TestMemberContact_ReadyForSMS(t *testing.T) {
	phone := testPhone(t)
	memberID := household.NewMemberID()

	tests := []struct {
		name string
		c    domain.MemberContact
		want bool
	}{
		{"no phone, not opted in", domain.MemberContact{MemberID: memberID}, false},
		{"phone but not opted in", domain.MemberContact{MemberID: memberID, Phone: &phone}, false},
		{"opted in but no phone (should not happen, still not ready)", domain.MemberContact{MemberID: memberID, SMSOptedIn: true}, false},
		{"phone and opted in", domain.MemberContact{MemberID: memberID, Phone: &phone, SMSOptedIn: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.ReadyForSMS(); got != tt.want {
				t.Errorf("ReadyForSMS() = %v, want %v", got, tt.want)
			}
		})
	}
}
