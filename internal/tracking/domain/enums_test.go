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

func TestItemSourceRoundTrip(t *testing.T) {
	for _, src := range domain.ItemSources() {
		if !src.Valid() {
			t.Errorf("ItemSources() returned invalid source %q", src)
		}
		got, err := domain.ParseItemSource(src.String())
		if err != nil || got != src {
			t.Errorf("ParseItemSource(%q) = (%q, %v), want (%q, nil)", src, got, err, src)
		}
	}
	if _, err := domain.ParseItemSource("gifted"); err == nil {
		t.Error("ParseItemSource(gifted) = nil error, want error")
	}
}

func TestItemStatusRoundTrip(t *testing.T) {
	for _, st := range domain.ItemStatuses() {
		if !st.Valid() {
			t.Errorf("ItemStatuses() returned invalid status %q", st)
		}
		got, err := domain.ParseItemStatus(st.String())
		if err != nil || got != st {
			t.Errorf("ParseItemStatus(%q) = (%q, %v), want (%q, nil)", st, got, err, st)
		}
	}
	if _, err := domain.ParseItemStatus("returned"); err == nil {
		t.Error("ParseItemStatus(returned) = nil error, want error")
	}
}
