package domain_test

import (
	"slices"
	"testing"

	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"Olive Oil", "olive oil"},
		{"  flour ", "flour"},
		{"All\tPurpose   Flour", "all purpose flour"},
		{"", ""},
		{"   ", ""},
	}
	for _, tt := range tests {
		if got := domain.NormalizeName(tt.in); got != tt.want {
			t.Errorf("NormalizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResolutionCandidates(t *testing.T) {
	// Candidate generation is deliberately generous: extra forms are harmless
	// because the resolver only matches them against rows that actually exist.
	// So the contract is "the original and the correct singular are present" plus
	// "no wrong fold for -ss words" — not an exact list.
	tests := []struct {
		in             string
		mustContain    []string
		mustNotContain []string
	}{
		{"Eggs", []string{"eggs", "egg"}, nil},
		{"Tomatoes", []string{"tomatoes", "tomato"}, nil},
		{"Potatoes", []string{"potatoes", "potato"}, nil},
		{"Onions", []string{"onions", "onion"}, nil},
		{"Berries", []string{"berries", "berry"}, nil},
		{"Cookies", []string{"cookies", "cookie"}, nil}, // ie+s, not consonant+ies
		{"Boxes", []string{"boxes", "box"}, nil},
		{"Dishes", []string{"dishes", "dish"}, nil},
		{"Glasses", []string{"glasses", "glass"}, nil},
		{"glass", []string{"glass"}, []string{"glas"}}, // -ss is not a plural
		{"flour", []string{"flour"}, nil},              // not a plural
	}
	for _, tt := range tests {
		got := domain.ResolutionCandidates(tt.in)
		for _, want := range tt.mustContain {
			if !slices.Contains(got, want) {
				t.Errorf("ResolutionCandidates(%q) = %v, missing %q", tt.in, got, want)
			}
		}
		for _, bad := range tt.mustNotContain {
			if slices.Contains(got, bad) {
				t.Errorf("ResolutionCandidates(%q) = %v, should not contain %q", tt.in, got, bad)
			}
		}
		if dupes := duplicates(got); len(dupes) > 0 {
			t.Errorf("ResolutionCandidates(%q) = %v, has duplicates %v", tt.in, got, dupes)
		}
	}

	for _, empty := range []string{"", "   "} {
		if got := domain.ResolutionCandidates(empty); got != nil {
			t.Errorf("ResolutionCandidates(%q) = %v, want nil", empty, got)
		}
	}
}

func duplicates(xs []string) []string {
	seen := make(map[string]bool, len(xs))
	var dupes []string
	for _, x := range xs {
		if seen[x] {
			dupes = append(dupes, x)
		}
		seen[x] = true
	}
	return dupes
}
