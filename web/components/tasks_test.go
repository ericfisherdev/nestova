package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

// ---------------------------------------------------------------------------
// TaskRowItem
// ---------------------------------------------------------------------------

func TestTaskRowItemPendingAssigned(t *testing.T) {
	row := components.TaskRow{
		InstanceID: "aaaa-bbbb-cccc-dddd",
		Title:      "Vacuum living room",
		Category:   "chore",
		DueLabel:   "Jun 20",
		Status:     "pending",
		Claimable:  false,
		CSRFToken:  "tok-pending",
	}
	out := renderString(t, components.TaskRowItem(row))

	// Stable anchor id for HTMX swap.
	if !strings.Contains(out, `id="task-aaaa-bbbb-cccc-dddd"`) {
		t.Errorf("row missing stable anchor id: %q", out)
	}
	// Title rendered.
	if !strings.Contains(out, "Vacuum living room") {
		t.Errorf("row missing title: %q", out)
	}
	// Chore badge.
	if !strings.Contains(out, "Chore") {
		t.Errorf("row missing chore badge: %q", out)
	}
	// Due label rendered verbatim (deterministic — handler computes it).
	if !strings.Contains(out, "Jun 20") {
		t.Errorf("row missing due label: %q", out)
	}
	// Done and Skip action buttons present.
	if !strings.Contains(out, "Done") {
		t.Errorf("row missing Done button: %q", out)
	}
	if !strings.Contains(out, "Skip") {
		t.Errorf("row missing Skip button: %q", out)
	}
	// Claim button must NOT appear for an assigned task.
	if strings.Contains(out, "Claim") {
		t.Errorf("row should not show Claim for assigned pending task: %q", out)
	}
	// CSRF token embedded.
	if !strings.Contains(out, `name="csrf_token"`) || !strings.Contains(out, "tok-pending") {
		t.Errorf("row missing csrf_token field or value: %q", out)
	}
	// hx-post attribute points to correct action.
	if !strings.Contains(out, `/tasks/aaaa-bbbb-cccc-dddd/complete`) {
		t.Errorf("row missing hx-post complete action: %q", out)
	}
	if !strings.Contains(out, `/tasks/aaaa-bbbb-cccc-dddd/skip`) {
		t.Errorf("row missing hx-post skip action: %q", out)
	}
}

func TestTaskRowItemClaimable(t *testing.T) {
	row := components.TaskRow{
		InstanceID: "1111-2222-3333-4444",
		Title:      "Change HVAC filter",
		Category:   "maintenance",
		DueLabel:   "Jun 25",
		Status:     "pending",
		Claimable:  true,
		CSRFToken:  "tok-claim",
	}
	out := renderString(t, components.TaskRowItem(row))

	// Maintenance badge.
	if !strings.Contains(out, "Maintenance") {
		t.Errorf("row missing Maintenance badge: %q", out)
	}
	// Claim button present for claimable row.
	if !strings.Contains(out, "Claim") {
		t.Errorf("row missing Claim button for claimable task: %q", out)
	}
	// Done and Skip must NOT appear for a claimable (unassigned) row.
	if strings.Contains(out, ">Done<") {
		t.Errorf("row should not show Done for claimable task: %q", out)
	}
	if strings.Contains(out, ">Skip<") {
		t.Errorf("row should not show Skip for claimable task: %q", out)
	}
	// hx-post points to claim endpoint.
	if !strings.Contains(out, `/tasks/1111-2222-3333-4444/claim`) {
		t.Errorf("row missing hx-post claim action: %q", out)
	}
	// hx-swap should swap the outer row element.
	if !strings.Contains(out, `hx-swap="outerHTML"`) {
		t.Errorf("row missing hx-swap outerHTML: %q", out)
	}
	// hx-target references the row's own id.
	if !strings.Contains(out, `#task-1111-2222-3333-4444`) {
		t.Errorf("row missing hx-target referencing row id: %q", out)
	}
}

func TestTaskRowItemOverdue(t *testing.T) {
	row := components.TaskRow{
		InstanceID: "dead-beef-0000-1234",
		Title:      "Take out bins",
		Category:   "chore",
		DueLabel:   "Jun 10",
		Status:     "overdue",
		Claimable:  false,
		CSRFToken:  "tok-overdue",
	}
	out := renderString(t, components.TaskRowItem(row))

	// Overdue visual cue.
	if !strings.Contains(out, "Overdue") {
		t.Errorf("row missing overdue indicator: %q", out)
	}
	// Done and Skip still present for overdue rows.
	if !strings.Contains(out, "Done") {
		t.Errorf("overdue row missing Done button: %q", out)
	}
	if !strings.Contains(out, "Skip") {
		t.Errorf("overdue row missing Skip button: %q", out)
	}
}

func TestTaskRowItemDone(t *testing.T) {
	row := components.TaskRow{
		InstanceID: "done-0000-0000-0001",
		Title:      "Mop floors",
		Category:   "chore",
		DueLabel:   "Jun 18",
		Status:     "done",
		CSRFToken:  "tok-done",
	}
	out := renderString(t, components.TaskRowItem(row))

	if !strings.Contains(out, "Completed") {
		t.Errorf("done row missing Completed label: %q", out)
	}
	// No action buttons for completed rows.
	if strings.Contains(out, "Done") || strings.Contains(out, "Skip") || strings.Contains(out, "Claim") {
		t.Errorf("done row should not show action buttons: %q", out)
	}
}

func TestTaskRowItemStanding(t *testing.T) {
	row := components.TaskRow{
		InstanceID: "standing-0000-0001",
		Title:      "Water the plants",
		Category:   "chore",
		Status:     "pending",
		Claimable:  true,
		Standing:   true,
		CSRFToken:  "tok-standing",
	}
	out := renderString(t, components.TaskRowItem(row))

	// Standing rows render "Anytime" instead of a due-date label.
	if !strings.Contains(out, "Anytime") {
		t.Errorf("standing row missing Anytime label: %q", out)
	}
	// A standing row still reuses the normal Claim action when unclaimed.
	if !strings.Contains(out, "Claim") {
		t.Errorf("standing row missing Claim button: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Claim countdown badge (NES-118)
// ---------------------------------------------------------------------------

// TestTaskRowItemClaimedWithExpiryShowsBadge verifies that a claimed row
// carrying an at-risk claim expiry renders the countdown badge's Alpine
// scaffolding and the passive-refresh HTMX trigger. Unlike the row's own
// mutation forms, the refresh targets the enclosing #task-groups container
// (not this row) via GET /tasks/groups, so a reverted claim's row re-renders
// under its correct group instead of only updating in place.
func TestTaskRowItemClaimedWithExpiryShowsBadge(t *testing.T) {
	row := components.TaskRow{
		InstanceID:        "risky-0001",
		Title:             "Take out recycling",
		Category:          "chore",
		DueLabel:          "Today",
		Status:            "pending",
		AssigneeID:        "id-alice",
		AssigneeName:      "Alice",
		Claimable:         false,
		ClaimExpiresAtISO: "2026-07-16T14:00:00Z",
		CSRFToken:         "tok-risky",
	}
	out := renderString(t, components.TaskRowItem(row))

	// templ HTML-escapes the attribute value, turning the JS call's single
	// quotes into &#39; entities; browsers decode these back to ' before
	// Alpine ever reads the attribute, so this is the correct rendered form.
	if !strings.Contains(out, `x-data="claimCountdown(&#39;2026-07-16T14:00:00Z&#39;)"`) {
		t.Errorf("row missing claimCountdown x-data with the claim expiry instant: %q", out)
	}
	if !strings.Contains(out, `x-text="label"`) {
		t.Errorf("row missing the countdown badge's x-text binding: %q", out)
	}
	if !strings.Contains(out, `hx-trigger="claim-expired"`) {
		t.Errorf("row missing the passive-refresh hx-trigger: %q", out)
	}
	if !strings.Contains(out, `hx-get="/tasks/groups"`) {
		t.Errorf("row missing the passive-refresh hx-get endpoint: %q", out)
	}
	if !strings.Contains(out, `hx-target="#task-groups"`) {
		t.Errorf("row missing the group-container hx-target (must not self-target): %q", out)
	}
}

// TestTaskRowItemUnclaimedShowsNoBadge verifies that a claimable (unassigned)
// row never renders the countdown badge or its passive-refresh wiring.
func TestTaskRowItemUnclaimedShowsNoBadge(t *testing.T) {
	row := components.TaskRow{
		InstanceID: "safe-0001",
		Title:      "Wash dishes",
		Category:   "chore",
		DueLabel:   "Today",
		Status:     "pending",
		Claimable:  true,
		CSRFToken:  "tok-safe",
	}
	out := renderString(t, components.TaskRowItem(row))

	if strings.Contains(out, "claimCountdown") {
		t.Errorf("unclaimed row should not render the countdown badge: %q", out)
	}
	if strings.Contains(out, "claim-expired") {
		t.Errorf("unclaimed row should not carry the passive-refresh trigger: %q", out)
	}
}

// TestTaskRowItemSelfClaimedShowsNoBadge verifies that a claimed row with no
// claim risk (self-claim or rotation assignment — ClaimExpiresAtISO empty)
// never renders the countdown badge, even though the row is assigned.
func TestTaskRowItemSelfClaimedShowsNoBadge(t *testing.T) {
	row := components.TaskRow{
		InstanceID:   "noexpiry-0001",
		Title:        "Feed the cat",
		Category:     "chore",
		DueLabel:     "Today",
		Status:       "pending",
		AssigneeID:   "id-bob",
		AssigneeName: "Bob",
		Claimable:    false,
		CSRFToken:    "tok-noexpiry",
	}
	out := renderString(t, components.TaskRowItem(row))

	if strings.Contains(out, "claimCountdown") {
		t.Errorf("a claim with no expiry risk should not render the countdown badge: %q", out)
	}
}

// ---------------------------------------------------------------------------
// TasksPage
// ---------------------------------------------------------------------------

func TestTasksPageEmpty(t *testing.T) {
	out := renderString(t, components.TasksPage(nil))
	if !strings.Contains(out, "all caught up") {
		t.Errorf("empty tasks page missing empty-state message: %q", out)
	}
	// The stable container id must be present even in the empty state, since
	// it is the NES-118 group-refresh swap target regardless of whether the
	// list happens to be empty when the refresh lands.
	if !strings.Contains(out, `id="task-groups"`) {
		t.Errorf("empty tasks page missing the #task-groups container: %q", out)
	}
}

// ---------------------------------------------------------------------------
// TaskGroupsFragment (NES-118)
// ---------------------------------------------------------------------------

// TestTaskGroupsFragment_RendersContainerAndGroups verifies that
// TaskGroupsFragment wraps its groups in the stable id="task-groups"
// container the claim countdown's group refresh targets.
func TestTaskGroupsFragment_RendersContainerAndGroups(t *testing.T) {
	groups := []components.TaskGroup{
		{
			Label: "Up for grabs",
			Rows: []components.TaskRow{
				{InstanceID: "row-1", Title: "Mow the lawn", Category: "chore", DueLabel: "Today", Status: "pending", Claimable: true, CSRFToken: "tok"},
			},
		},
	}
	out := renderString(t, components.TaskGroupsFragment(groups, false))

	if !strings.Contains(out, `id="task-groups"`) {
		t.Errorf("fragment missing the stable #task-groups container id: %q", out)
	}
	if !strings.Contains(out, "Up for grabs") {
		t.Errorf("fragment missing group heading: %q", out)
	}
	if !strings.Contains(out, "Mow the lawn") {
		t.Errorf("fragment missing row content: %q", out)
	}
}

// TestTaskGroupsFragment_Empty verifies that TaskGroupsFragment renders the
// "all caught up" empty state, still wrapped in id="task-groups", when there
// are no groups — the same shape GET /tasks/groups returns after the last
// remaining claim on the page reverts to claimable with nothing else pending.
func TestTaskGroupsFragment_Empty(t *testing.T) {
	out := renderString(t, components.TaskGroupsFragment(nil, false))

	if !strings.Contains(out, `id="task-groups"`) {
		t.Errorf("empty fragment missing the #task-groups container: %q", out)
	}
	if !strings.Contains(out, "all caught up") {
		t.Errorf("empty fragment missing empty-state message: %q", out)
	}
}

// TestTaskGroupsFragment_OOB verifies that oob=true adds hx-swap-oob="true"
// to the #task-groups container (NES-122), so a chore trade's Accept
// response can refresh an already-open /tasks page's groups list as an
// out-of-band update alongside its own primary swap target.
func TestTaskGroupsFragment_OOB(t *testing.T) {
	out := renderString(t, components.TaskGroupsFragment(nil, true))

	if !strings.Contains(out, `id="task-groups"`) {
		t.Errorf("oob fragment missing the #task-groups container: %q", out)
	}
	if !strings.Contains(out, `hx-swap-oob="true"`) {
		t.Errorf("oob fragment missing hx-swap-oob=\"true\": %q", out)
	}
}

// TestTaskGroupsFragment_NotOOB verifies that oob=false (the default used by
// TasksPage and GET /tasks/groups) never adds hx-swap-oob, so the normal
// primary-swap response shape is unchanged.
func TestTaskGroupsFragment_NotOOB(t *testing.T) {
	out := renderString(t, components.TaskGroupsFragment(nil, false))

	if strings.Contains(out, "hx-swap-oob") {
		t.Errorf("non-oob fragment must not carry hx-swap-oob: %q", out)
	}
}

func TestTasksPageGroupsAndRows(t *testing.T) {
	groups := []components.TaskGroup{
		{
			Label:         "Alice",
			AssigneeColor: "clay",
			Rows: []components.TaskRow{
				{
					InstanceID:    "row-0001",
					Title:         "Dishes",
					Category:      "chore",
					DueLabel:      "Today",
					Status:        "pending",
					AssigneeID:    "id-alice",
					AssigneeName:  "Alice",
					AssigneeColor: "clay",
					Claimable:     false,
					CSRFToken:     "tok-grp",
				},
			},
		},
		{
			Label:         "Bob",
			AssigneeColor: "blue",
			Rows: []components.TaskRow{
				{
					InstanceID:    "row-0003",
					Title:         "Trash",
					Category:      "chore",
					DueLabel:      "Today",
					Status:        "overdue",
					AssigneeID:    "id-bob",
					AssigneeName:  "Bob",
					AssigneeColor: "blue",
					Claimable:     false,
					CSRFToken:     "tok-grp",
				},
			},
		},
		{
			Label: "Up for grabs",
			Rows: []components.TaskRow{
				{
					InstanceID: "row-0002",
					Title:      "Lawn mowing",
					Category:   "maintenance",
					DueLabel:   "Jun 21",
					Status:     "pending",
					Claimable:  true,
					CSRFToken:  "tok-grp",
				},
			},
		},
	}

	out := renderString(t, components.TasksPage(groups))

	// Page heading.
	if !strings.Contains(out, "Chores") {
		t.Errorf("tasks page missing heading: %q", out)
	}
	// Per-member group headings.
	if !strings.Contains(out, "Alice") {
		t.Errorf("tasks page missing Alice group heading: %q", out)
	}
	if !strings.Contains(out, "Bob") {
		t.Errorf("tasks page missing Bob group heading: %q", out)
	}
	if !strings.Contains(out, "Up for grabs") {
		t.Errorf("tasks page missing Up for grabs group: %q", out)
	}
	// The member color tint must render on the group heading avatar.
	if !strings.Contains(out, "bg-member-clay-tint") {
		t.Errorf("tasks page missing Alice's clay tint on group heading: %q", out)
	}
	if !strings.Contains(out, "bg-member-blue-tint") {
		t.Errorf("tasks page missing Bob's blue tint on group heading: %q", out)
	}
	// Group ordering: Alice's heading must appear before Bob's, and both before
	// the Up for grabs group, matching the slice order the handler produces.
	aliceIdx := strings.Index(out, "Alice")
	bobIdx := strings.Index(out, "Bob")
	grabsIdx := strings.Index(out, "Up for grabs")
	if aliceIdx >= bobIdx || bobIdx >= grabsIdx {
		t.Errorf("group order wrong: Alice=%d Bob=%d UpForGrabs=%d (want Alice<Bob<UpForGrabs)", aliceIdx, bobIdx, grabsIdx)
	}
	// Row content.
	if !strings.Contains(out, "Dishes") {
		t.Errorf("tasks page missing Dishes row: %q", out)
	}
	if !strings.Contains(out, "Trash") {
		t.Errorf("tasks page missing Trash row: %q", out)
	}
	if !strings.Contains(out, "Lawn mowing") {
		t.Errorf("tasks page missing Lawn mowing row: %q", out)
	}
	// Today shows "Today".
	if !strings.Contains(out, "Today") {
		t.Errorf("tasks page missing Today label for today's due date: %q", out)
	}
}
