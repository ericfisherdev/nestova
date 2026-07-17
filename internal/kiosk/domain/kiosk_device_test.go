package domain_test

import (
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/kiosk/domain"
)

func validDevice() *domain.KioskDevice {
	return &domain.KioskDevice{
		ID:          domain.NewKioskDeviceID(),
		HouseholdID: household.NewHouseholdID(),
		TokenHash:   domain.HashToken("some-raw-token"),
		Name:        "Kitchen wall display",
	}
}

func TestKioskDeviceValidate(t *testing.T) {
	if err := validDevice().Validate(); err != nil {
		t.Fatalf("valid device rejected: %v", err)
	}

	blankName := validDevice()
	blankName.Name = "   "
	if !errors.Is(blankName.Validate(), domain.ErrInvalidKioskDevice) {
		t.Error("blank name accepted")
	}

	blankHash := validDevice()
	blankHash.TokenHash = ""
	if !errors.Is(blankHash.Validate(), domain.ErrInvalidKioskDevice) {
		t.Error("blank token hash accepted")
	}
}

func TestKioskDeviceActive(t *testing.T) {
	d := validDevice()
	if !d.Active() {
		t.Error("a freshly provisioned device should be active")
	}
	now := time.Now()
	d.RevokedAt = &now
	if d.Active() {
		t.Error("a revoked device should not be active")
	}
}
