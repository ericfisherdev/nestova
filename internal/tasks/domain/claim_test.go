package domain_test

import (
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// ClaimExpiryPenalty (NES-117)
// ---------------------------------------------------------------------------

// TestClaimExpiryPenalty verifies the penalty formula (half the points,
// rounded down, floor of 1) across zero, one, and several odd/even point
// values, per the NES-117 acceptance criteria.
func TestClaimExpiryPenalty(t *testing.T) {
	cases := []struct {
		points int
		want   int
	}{
		{points: 0, want: 1},  // zero-point task still risks the 1-point floor
		{points: 1, want: 1},  // 1/2 = 0, floored to 1
		{points: 2, want: 1},  // 2/2 = 1
		{points: 3, want: 1},  // 3/2 = 1 (integer division)
		{points: 4, want: 2},  // 4/2 = 2
		{points: 5, want: 2},  // 5/2 = 2 (integer division)
		{points: 10, want: 5}, // 10/2 = 5
		{points: 11, want: 5}, // 11/2 = 5 (integer division)
	}
	for _, tc := range cases {
		if got := domain.ClaimExpiryPenalty(tc.points); got != tc.want {
			t.Errorf("ClaimExpiryPenalty(%d) = %d, want %d", tc.points, got, tc.want)
		}
	}
}

// TestClaimExpiryPenalty_NeverBelowFloor verifies that no points value in a
// wide range ever produces a penalty below the 1-point floor.
func TestClaimExpiryPenalty_NeverBelowFloor(t *testing.T) {
	for points := 0; points <= 50; points++ {
		if got := domain.ClaimExpiryPenalty(points); got < 1 {
			t.Errorf("ClaimExpiryPenalty(%d) = %d, want >= 1", points, got)
		}
	}
}

// ---------------------------------------------------------------------------
// ClaimWindow (NES-117)
// ---------------------------------------------------------------------------

// TestClaimWindow_Is12Hours locks in the acceptance-criteria value so an
// accidental edit is caught by a failing test rather than silently shipping a
// different expiry window.
func TestClaimWindow_Is12Hours(t *testing.T) {
	if domain.ClaimWindow != 12*time.Hour {
		t.Errorf("ClaimWindow = %v, want 12h", domain.ClaimWindow)
	}
}
