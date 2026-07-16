package adapter

import (
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/web/components"
)

// TestGroupTaskRows_PerMemberOrdering verifies that groupTaskRows produces one
// group per assignee (ordered by display name), a trailing "Up for grabs" group
// for claimable rows, and that the member color is carried onto the group.
func TestGroupTaskRows_PerMemberOrdering(t *testing.T) {
	due := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	rows := []components.TaskRow{
		{InstanceID: "1", Title: "Trash", AssigneeID: "id-bob", AssigneeName: "Bob", AssigneeColor: "blue", Status: "pending", DueOn: due},
		{InstanceID: "2", Title: "Dishes", AssigneeID: "id-alice", AssigneeName: "Alice", AssigneeColor: "clay", Status: "overdue", DueOn: due},
		{InstanceID: "3", Title: "Lawn", Claimable: true, Status: "pending", DueOn: due},
		{InstanceID: "4", Title: "Mop", AssigneeID: "id-alice", AssigneeName: "Alice", AssigneeColor: "clay", Status: "pending", DueOn: due},
	}

	groups := groupTaskRows(rows)

	// Expect: Alice (2 rows), Bob (1 row), Up for grabs (1 row) — in that order.
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3", len(groups))
	}
	if groups[0].Label != "Alice" {
		t.Errorf("groups[0].Label = %q, want Alice", groups[0].Label)
	}
	if groups[0].AssigneeColor != "clay" {
		t.Errorf("groups[0].AssigneeColor = %q, want clay", groups[0].AssigneeColor)
	}
	if len(groups[0].Rows) != 2 {
		t.Errorf("Alice group rows = %d, want 2", len(groups[0].Rows))
	}
	if groups[1].Label != "Bob" {
		t.Errorf("groups[1].Label = %q, want Bob", groups[1].Label)
	}
	if groups[1].AssigneeColor != "blue" {
		t.Errorf("groups[1].AssigneeColor = %q, want blue", groups[1].AssigneeColor)
	}
	if groups[2].Label != upForGrabsLabel {
		t.Errorf("groups[2].Label = %q, want %q", groups[2].Label, upForGrabsLabel)
	}
	if len(groups[2].Rows) != 1 || groups[2].Rows[0].Title != "Lawn" {
		t.Errorf("Up for grabs group = %+v, want a single Lawn row", groups[2].Rows)
	}
}

// TestGroupTaskRows_SameDisplayNameDistinctIDs is the regression test for the
// group-by-id fix: two distinct members who happen to share a display name must
// form two separate groups (keyed by their stable ids), not collapse into one.
func TestGroupTaskRows_SameDisplayNameDistinctIDs(t *testing.T) {
	due := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	rows := []components.TaskRow{
		{InstanceID: "1", Title: "Dishes", AssigneeID: "id-sam-1", AssigneeName: "Sam", AssigneeColor: "clay", Status: "pending", DueOn: due},
		{InstanceID: "2", Title: "Trash", AssigneeID: "id-sam-2", AssigneeName: "Sam", AssigneeColor: "blue", Status: "pending", DueOn: due},
	}

	groups := groupTaskRows(rows)

	// Two members named "Sam" with different ids → two groups, not one collapsed.
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2 (one per distinct member id)", len(groups))
	}
	for i, g := range groups {
		if g.Label != "Sam" {
			t.Errorf("groups[%d].Label = %q, want Sam", i, g.Label)
		}
		if len(g.Rows) != 1 {
			t.Errorf("groups[%d] rows = %d, want 1 (rows must not collapse across ids)", i, len(g.Rows))
		}
	}
	// Tiebreak by id: id-sam-1 sorts before id-sam-2, so its row (Dishes) is first.
	if groups[0].Rows[0].AssigneeID != "id-sam-1" {
		t.Errorf("groups[0] assignee id = %q, want id-sam-1 (id tiebreaker)", groups[0].Rows[0].AssigneeID)
	}
	if groups[1].Rows[0].AssigneeID != "id-sam-2" {
		t.Errorf("groups[1] assignee id = %q, want id-sam-2 (id tiebreaker)", groups[1].Rows[0].AssigneeID)
	}
}

// TestGroupTaskRows_AnytimeSection verifies that as-needed standing rows
// (NES-116) are pulled out of both the per-member and "Up for grabs" buckets
// into a trailing "Anytime" section, regardless of whether they are claimed.
func TestGroupTaskRows_AnytimeSection(t *testing.T) {
	due := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	rows := []components.TaskRow{
		{InstanceID: "1", Title: "Dishes", AssigneeID: "id-alice", AssigneeName: "Alice", AssigneeColor: "clay", Status: "pending", DueOn: due},
		{InstanceID: "2", Title: "Lawn", Claimable: true, Status: "pending", DueOn: due},
		// An unclaimed standing instance: Claimable is true but Standing must win.
		{InstanceID: "3", Title: "Water plants", Claimable: true, Status: "pending", Standing: true},
		// A claimed standing instance: assigned to Alice but must still land in
		// Anytime, not Alice's per-member group.
		{InstanceID: "4", Title: "Restock paper towels", AssigneeID: "id-alice", AssigneeName: "Alice", AssigneeColor: "clay", Status: "pending", Standing: true},
	}

	groups := groupTaskRows(rows)

	// Expect: Alice (1 dated row), Up for grabs (1 row), Anytime (2 rows).
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3", len(groups))
	}
	if groups[0].Label != "Alice" || len(groups[0].Rows) != 1 {
		t.Errorf("groups[0] = %+v, want Alice with 1 dated row", groups[0])
	}
	if groups[1].Label != upForGrabsLabel || len(groups[1].Rows) != 1 {
		t.Errorf("groups[1] = %+v, want %q with 1 row", groups[1], upForGrabsLabel)
	}
	if groups[2].Label != anytimeLabel {
		t.Errorf("groups[2].Label = %q, want %q", groups[2].Label, anytimeLabel)
	}
	if len(groups[2].Rows) != 2 {
		t.Fatalf("Anytime group rows = %d, want 2", len(groups[2].Rows))
	}
	for _, row := range groups[2].Rows {
		if !row.Standing {
			t.Errorf("Anytime group contains non-standing row: %+v", row)
		}
	}
}

// TestGroupTaskRows_NoClaimable verifies that no "Up for grabs" group is emitted
// when every row is assigned.
func TestGroupTaskRows_NoClaimable(t *testing.T) {
	rows := []components.TaskRow{
		{InstanceID: "1", Title: "Dishes", AssigneeID: "id-alice", AssigneeName: "Alice", AssigneeColor: "clay", Status: "pending"},
	}
	groups := groupTaskRows(rows)
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if groups[0].Label != "Alice" {
		t.Errorf("groups[0].Label = %q, want Alice", groups[0].Label)
	}
}

// TestGroupTaskRows_OnlyClaimable verifies that a list of only claimable rows
// produces a single "Up for grabs" group and no member groups.
func TestGroupTaskRows_OnlyClaimable(t *testing.T) {
	rows := []components.TaskRow{
		{InstanceID: "1", Title: "Lawn", Claimable: true, Status: "overdue"},
		{InstanceID: "2", Title: "Gutter", Claimable: true, Status: "pending"},
	}
	groups := groupTaskRows(rows)
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if groups[0].Label != upForGrabsLabel {
		t.Errorf("groups[0].Label = %q, want %q", groups[0].Label, upForGrabsLabel)
	}
	if len(groups[0].Rows) != 2 {
		t.Errorf("Up for grabs rows = %d, want 2", len(groups[0].Rows))
	}
}

// TestGroupTaskRows_UnresolvedAssigneeFallsBack verifies that an assigned row
// whose assignee name could not be resolved is grouped under "(unknown member)"
// rather than being dropped. The row still carries a stable AssigneeID, so the
// group key is the id while the label is the fallback string.
func TestGroupTaskRows_UnresolvedAssigneeFallsBack(t *testing.T) {
	rows := []components.TaskRow{
		// Assigned (not claimable) with an id but no resolved name — e.g. a
		// referential anomaly where the member row is missing.
		{InstanceID: "1", Title: "Orphan", AssigneeID: "id-ghost", AssigneeName: "", Claimable: false, Status: "pending"},
	}
	groups := groupTaskRows(rows)
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if groups[0].Label != "(unknown member)" {
		t.Errorf("groups[0].Label = %q, want (unknown member)", groups[0].Label)
	}
}

// TestDueLabel verifies the deterministic relative due-date label against a
// fixed reference date (no time.Now()).
func TestDueLabel(t *testing.T) {
	today := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		due  time.Time
		want string
	}{
		{"today", today, "Today"},
		{"tomorrow", today.AddDate(0, 0, 1), "Tomorrow"},
		{"future date", time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC), "Jun 25"},
		{"past date", time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC), "Jun 10"},
		{"non-midnight input normalizes to date", time.Date(2026, 6, 18, 23, 59, 0, 0, time.UTC), "Today"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dueLabel(tc.due, today); got != tc.want {
				t.Errorf("dueLabel(%v) = %q, want %q", tc.due, got, tc.want)
			}
		})
	}
}
