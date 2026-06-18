package domain

import (
	"strings"
	"time"
)

// Member is a person in a household. It is a child entity of the Household
// aggregate root.
type Member struct {
	ID          MemberID
	HouseholdID HouseholdID
	DisplayName string
	Role        Role
	Color       MemberColor
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Initials returns up to two uppercase initials for the avatar, derived from the
// first and last words of DisplayName. Rune-based so multi-byte names work.
func (m Member) Initials() string {
	fields := strings.Fields(m.DisplayName)
	if len(fields) == 0 {
		return ""
	}
	first := []rune(fields[0])
	out := strings.ToUpper(string(first[0]))
	if len(fields) > 1 {
		last := []rune(fields[len(fields)-1])
		out += strings.ToUpper(string(last[0]))
	}
	return out
}

// NextColor returns the color to assign to a new member given the colors already
// in use. It assigns unused palette colors first (in canonical order), then —
// once all five are in use — reuses the least-used color, breaking ties by
// canonical order. The result is deterministic, so a household's Nth member
// always gets the same color.
func NextColor(existing []MemberColor) MemberColor {
	counts := make(map[MemberColor]int, len(MemberColors()))
	for _, c := range MemberColors() {
		counts[c] = 0
	}
	for _, c := range existing {
		if _, ok := counts[c]; ok {
			counts[c]++
		}
	}

	best := MemberColors()[0]
	for _, c := range MemberColors() {
		if counts[c] < counts[best] {
			best = c
		}
	}
	return best
}
