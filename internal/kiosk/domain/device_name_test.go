package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/kiosk/domain"
)

func TestDeviceAndCodeValidate_NameLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		label string
		name  string
		want  error
	}{
		{label: "at the limit", name: strings.Repeat("a", domain.MaxDeviceNameLength), want: nil},
		{label: "multibyte at the limit", name: strings.Repeat("家", domain.MaxDeviceNameLength), want: nil},
		{label: "one over", name: strings.Repeat("a", domain.MaxDeviceNameLength+1), want: domain.ErrDeviceNameTooLong},
		{label: "far over", name: strings.Repeat("a", 10_000), want: domain.ErrDeviceNameTooLong},
	}
	for _, tc := range tests {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			device := validDevice()
			device.Name = tc.name
			if err := device.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("KioskDevice.Validate() = %v, want %v", err, tc.want)
			}

			code := &domain.ActivationCode{
				ID:          domain.NewActivationCodeID(),
				HouseholdID: household.NewHouseholdID(),
				CodeHash:    domain.HashToken("raw"),
				Name:        tc.name,
				ExpiresAt:   time.Now().Add(time.Hour),
			}
			if err := code.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("ActivationCode.Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}
