package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

// ---------------------------------------------------------------------------
// BalanceCard
// ---------------------------------------------------------------------------

func TestBalanceCard_ShowsPoints(t *testing.T) {
	out := renderString(t, components.BalanceCard(250))
	if !strings.Contains(out, "250") {
		t.Errorf("BalanceCard(250) missing point count: %q", out)
	}
	if !strings.Contains(out, "pts") {
		t.Errorf("BalanceCard missing 'pts' unit label: %q", out)
	}
	if !strings.Contains(out, "Your Balance") {
		t.Errorf("BalanceCard missing heading: %q", out)
	}
}

// ---------------------------------------------------------------------------
// StreakBadge
// ---------------------------------------------------------------------------

func TestStreakBadge_ShowsDayCount(t *testing.T) {
	out := renderString(t, components.StreakBadge(5))
	if !strings.Contains(out, "5") {
		t.Errorf("StreakBadge(5) missing day count: %q", out)
	}
	if !strings.Contains(out, "streak") {
		t.Errorf("StreakBadge missing 'streak' text: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Leaderboard
// ---------------------------------------------------------------------------

func TestLeaderboard_RendersRows(t *testing.T) {
	rows := []components.LeaderboardRow{
		{MemberID: "id-a", Name: "Alice", Color: "sage", Points: 120, Streak: 3, IsCurrentMember: true},
		{MemberID: "id-b", Name: "Bob", Color: "clay", Points: 80, Streak: 0, IsCurrentMember: false},
	}
	out := renderString(t, components.Leaderboard(rows))

	// Both names must appear.
	if !strings.Contains(out, "Alice") {
		t.Errorf("Leaderboard missing Alice: %q", out)
	}
	if !strings.Contains(out, "Bob") {
		t.Errorf("Leaderboard missing Bob: %q", out)
	}

	// Alice's points and streak badge.
	if !strings.Contains(out, "120 pts") {
		t.Errorf("Leaderboard missing Alice's 120 pts: %q", out)
	}
	if !strings.Contains(out, "3 day streak") {
		t.Errorf("Leaderboard missing 3 day streak badge: %q", out)
	}

	// Bob has 0 streak — no badge expected.
	if strings.Contains(out, "0 day streak") {
		t.Errorf("Leaderboard should not render 0 day streak badge: %q", out)
	}

	// Current member (Alice) is highlighted.
	if !strings.Contains(out, "bg-sage-tint") {
		t.Errorf("Leaderboard missing current-member highlight class: %q", out)
	}
}

func TestLeaderboard_EmptyMessage(t *testing.T) {
	out := renderString(t, components.Leaderboard(nil))
	if !strings.Contains(out, "No points yet") {
		t.Errorf("Empty leaderboard missing empty-state message: %q", out)
	}
}

// ---------------------------------------------------------------------------
// RewardCard — affordability gating
// ---------------------------------------------------------------------------

func TestRewardCard_AffordableShowsActiveButton(t *testing.T) {
	r := components.RewardItem{ID: "rwd-1", Name: "Movie night", CostPoints: 50, Affordable: true}
	out := renderString(t, components.RewardCard(r, "tok123"))

	if !strings.Contains(out, "Movie night") {
		t.Errorf("RewardCard missing reward name: %q", out)
	}
	if !strings.Contains(out, "50 pts") {
		t.Errorf("RewardCard missing cost: %q", out)
	}
	// CSRF field must be present.
	if !strings.Contains(out, `value="tok123"`) {
		t.Errorf("RewardCard missing CSRF token: %q", out)
	}
	// A <form> submittable button.
	if !strings.Contains(out, `type="submit"`) {
		t.Errorf("RewardCard affordable: missing submit button: %q", out)
	}
	// Should not be disabled.
	if strings.Contains(out, "disabled") {
		t.Errorf("RewardCard affordable: button should not be disabled: %q", out)
	}
}

func TestRewardCard_UnaffordableShowsDisabledButton(t *testing.T) {
	r := components.RewardItem{ID: "rwd-2", Name: "Weekend off", CostPoints: 200, Affordable: false}
	out := renderString(t, components.RewardCard(r, "tok456"))

	if !strings.Contains(out, "Weekend off") {
		t.Errorf("RewardCard missing reward name: %q", out)
	}
	// Must be disabled.
	if !strings.Contains(out, "disabled") {
		t.Errorf("RewardCard unaffordable: button should be disabled: %q", out)
	}
	// No form (no CSRF field in the disabled case).
	if strings.Contains(out, `value="tok456"`) {
		t.Errorf("RewardCard unaffordable: should not emit CSRF token: %q", out)
	}
}

// ---------------------------------------------------------------------------
// RewardsPageComponent — page structure
// ---------------------------------------------------------------------------

func TestRewardsPageComponent_RendersKeyElements(t *testing.T) {
	page := components.RewardsPage{
		Leaderboard: []components.LeaderboardRow{
			{MemberID: "id-c", Name: "Carol", Color: "ochre", Points: 30, IsCurrentMember: true},
		},
		Balance: 30,
		Rewards: []components.RewardItem{
			{ID: "rwd-3", Name: "Pizza night", CostPoints: 25, Affordable: true},
		},
		CSRFToken: "csrfXYZ",
	}
	out := renderString(t, components.RewardsPageComponent(page))

	for _, want := range []string{
		"Rewards &amp; Scoreboard", // heading (templ escapes &)
		"Your Balance",
		"30",
		"Leaderboard",
		"Carol",
		"Rewards",
		"Pizza night",
		"csrfXYZ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RewardsPageComponent missing %q: %q", want, out)
		}
	}

	// No error banner when InsufficientMessage is empty.
	if strings.Contains(out, `role="alert"`) {
		t.Errorf("RewardsPageComponent should not render alert when InsufficientMessage is empty")
	}
}

func TestRewardsPageComponent_InsufficientMessageBanner(t *testing.T) {
	page := components.RewardsPage{
		InsufficientMessage: "You don't have enough points.",
	}
	out := renderString(t, components.RewardsPageComponent(page))

	if !strings.Contains(out, `role="alert"`) {
		t.Errorf("RewardsPageComponent missing alert role: %q", out)
	}
	if !strings.Contains(out, "enough points") {
		t.Errorf("RewardsPageComponent missing insufficient message text: %q", out)
	}
}

// ---------------------------------------------------------------------------
// RewardsCatalog — empty state
// ---------------------------------------------------------------------------

func TestRewardsCatalog_EmptyMessage(t *testing.T) {
	out := renderString(t, components.RewardsCatalog(nil, "tok"))
	if !strings.Contains(out, "No rewards") {
		t.Errorf("Empty catalog missing empty-state message: %q", out)
	}
}
