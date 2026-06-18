package domain_test

import (
	"testing"

	"github.com/ericfisherdev/nestova/internal/household/domain"
)

func TestRoleParse(t *testing.T) {
	for _, s := range []string{"owner", "adult", "child"} {
		if r, err := domain.ParseRole(s); err != nil || r.String() != s {
			t.Errorf("ParseRole(%q) = (%q, %v), want valid", s, r, err)
		}
	}
	if _, err := domain.ParseRole("monarch"); err == nil {
		t.Error("ParseRole(monarch) = nil error, want error")
	}
}

func TestMemberColorParse(t *testing.T) {
	for _, c := range domain.MemberColors() {
		if got, err := domain.ParseMemberColor(c.String()); err != nil || got != c {
			t.Errorf("ParseMemberColor(%q) = (%q, %v), want valid", c, got, err)
		}
	}
	if _, err := domain.ParseMemberColor("chartreuse"); err == nil {
		t.Error("ParseMemberColor(chartreuse) = nil error, want error")
	}
	if len(domain.MemberColors()) != 5 {
		t.Errorf("MemberColors() has %d colors, want 5", len(domain.MemberColors()))
	}
}

func TestIDRoundTrip(t *testing.T) {
	hid := domain.NewHouseholdID()
	got, err := domain.ParseHouseholdID(hid.String())
	if err != nil || got != hid {
		t.Errorf("household id round-trip = (%v, %v), want %v", got, err, hid)
	}
	mid := domain.NewMemberID()
	gotMID, err := domain.ParseMemberID(mid.String())
	if err != nil || gotMID != mid {
		t.Errorf("member id round-trip = (%v, %v), want %v", gotMID, err, mid)
	}
	if _, err := domain.ParseMemberID("not-a-uuid"); err == nil {
		t.Error("ParseMemberID(not-a-uuid) = nil error, want error")
	}
	if _, err := domain.ParseHouseholdID("not-a-uuid"); err == nil {
		t.Error("ParseHouseholdID(not-a-uuid) = nil error, want error")
	}
}

func TestMemberInitials(t *testing.T) {
	tests := map[string]string{
		"Maya":             "M",
		"mary jane":        "MJ",
		"Anne Marie Smith": "AS",
		"":                 "",
		"Étienne":          "É",
	}
	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			m := domain.Member{DisplayName: name}
			if got := m.Initials(); got != want {
				t.Errorf("Initials(%q) = %q, want %q", name, got, want)
			}
		})
	}
}

func TestNextColor(t *testing.T) {
	tests := []struct {
		name     string
		existing []domain.MemberColor
		want     domain.MemberColor
	}{
		{"empty -> first palette color", nil, domain.ColorSage},
		{"sage used -> next is clay", []domain.MemberColor{domain.ColorSage}, domain.ColorClay},
		{
			name:     "four used -> remaining color",
			existing: []domain.MemberColor{domain.ColorSage, domain.ColorClay, domain.ColorOchre, domain.ColorBlue},
			want:     domain.ColorPlum,
		},
		{
			name:     "all five used -> reuse least-used (sage) by canonical order",
			existing: domain.MemberColors(),
			want:     domain.ColorSage,
		},
		{
			name:     "sage used twice -> next reuse is clay",
			existing: append(domain.MemberColors(), domain.ColorSage),
			want:     domain.ColorClay,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.NextColor(tt.existing); got != tt.want {
				t.Errorf("NextColor(%v) = %q, want %q", tt.existing, got, tt.want)
			}
		})
	}
}
