package domain_test

import (
	"testing"

	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

func TestUsageTypeRoundTrip(t *testing.T) {
	for _, ut := range domain.UsageTypes() {
		if !ut.Valid() {
			t.Errorf("UsageTypes() returned invalid type %q", ut)
		}
		got, err := domain.ParseUsageType(ut.String())
		if err != nil || got != ut {
			t.Errorf("ParseUsageType(%q) = (%q, %v), want (%q, nil)", ut, got, err, ut)
		}
	}
}

func TestParseUsageTypeRejectsUnknown(t *testing.T) {
	if _, err := domain.ParseUsageType("consumed"); err == nil {
		t.Error("ParseUsageType(consumed) = nil error, want error for unknown type")
	}
	if domain.UsageType("consumed").Valid() {
		t.Error(`UsageType("consumed").Valid() = true, want false`)
	}
}
