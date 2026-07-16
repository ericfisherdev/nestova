package domain_test

import (
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// Category
// ---------------------------------------------------------------------------

func TestCategoryParseAndValid(t *testing.T) {
	cases := []struct {
		input string
		want  domain.Category
	}{
		{"chore", domain.ChoreCategory},
		{"maintenance", domain.MaintenanceCategory},
	}
	for _, tc := range cases {
		got, err := domain.ParseCategory(tc.input)
		if err != nil {
			t.Errorf("ParseCategory(%q) error = %v, want nil", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("ParseCategory(%q) = %v, want %v", tc.input, got, tc.want)
		}
		if !got.Valid() {
			t.Errorf("Category(%q).Valid() = false, want true", tc.input)
		}
		if got.String() != tc.input {
			t.Errorf("Category(%q).String() = %q, want %q", tc.input, got.String(), tc.input)
		}
	}
}

func TestCategoryParseUnknown(t *testing.T) {
	_, err := domain.ParseCategory("laundry")
	if err == nil {
		t.Error("ParseCategory(unknown) error = nil, want non-nil")
	}
}

func TestCategoryValid(t *testing.T) {
	if domain.Category("laundry").Valid() {
		t.Error("Category(laundry).Valid() = true, want false")
	}
}

// ---------------------------------------------------------------------------
// RotationPolicy
// ---------------------------------------------------------------------------

func TestRotationPolicyParseAndValid(t *testing.T) {
	cases := []struct {
		input string
		want  domain.RotationPolicy
	}{
		{"fixed", domain.RotationFixed},
		{"round_robin", domain.RotationRoundRobin},
		{"claimable", domain.RotationClaimable},
	}
	for _, tc := range cases {
		got, err := domain.ParseRotationPolicy(tc.input)
		if err != nil {
			t.Errorf("ParseRotationPolicy(%q) error = %v, want nil", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("ParseRotationPolicy(%q) = %v, want %v", tc.input, got, tc.want)
		}
		if !got.Valid() {
			t.Errorf("RotationPolicy(%q).Valid() = false, want true", tc.input)
		}
		if got.String() != tc.input {
			t.Errorf("RotationPolicy(%q).String() = %q, want %q", tc.input, got.String(), tc.input)
		}
	}
}

func TestRotationPolicyParseUnknown(t *testing.T) {
	_, err := domain.ParseRotationPolicy("lottery")
	if err == nil {
		t.Error("ParseRotationPolicy(unknown) error = nil, want non-nil")
	}
}

func TestRotationPolicyValid(t *testing.T) {
	if domain.RotationPolicy("lottery").Valid() {
		t.Error("RotationPolicy(lottery).Valid() = true, want false")
	}
}

// ---------------------------------------------------------------------------
// InstanceStatus
// ---------------------------------------------------------------------------

func TestInstanceStatusParseAndValid(t *testing.T) {
	cases := []struct {
		input string
		want  domain.InstanceStatus
	}{
		{"pending", domain.StatusPending},
		{"done", domain.StatusDone},
		{"skipped", domain.StatusSkipped},
		{"overdue", domain.StatusOverdue},
	}
	for _, tc := range cases {
		got, err := domain.ParseInstanceStatus(tc.input)
		if err != nil {
			t.Errorf("ParseInstanceStatus(%q) error = %v, want nil", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("ParseInstanceStatus(%q) = %v, want %v", tc.input, got, tc.want)
		}
		if !got.Valid() {
			t.Errorf("InstanceStatus(%q).Valid() = false, want true", tc.input)
		}
		if got.String() != tc.input {
			t.Errorf("InstanceStatus(%q).String() = %q, want %q", tc.input, got.String(), tc.input)
		}
	}
}

func TestInstanceStatusParseUnknown(t *testing.T) {
	_, err := domain.ParseInstanceStatus("cancelled")
	if err == nil {
		t.Error("ParseInstanceStatus(unknown) error = nil, want non-nil")
	}
}

func TestInstanceStatusValid(t *testing.T) {
	if domain.InstanceStatus("cancelled").Valid() {
		t.Error("InstanceStatus(cancelled).Valid() = true, want false")
	}
}

// ---------------------------------------------------------------------------
// InstanceKind (NES-116)
// ---------------------------------------------------------------------------

func TestInstanceKindParseAndValid(t *testing.T) {
	cases := []struct {
		input string
		want  domain.InstanceKind
	}{
		{"scheduled", domain.KindScheduled},
		{"standing", domain.KindStanding},
	}
	for _, tc := range cases {
		got, err := domain.ParseInstanceKind(tc.input)
		if err != nil {
			t.Errorf("ParseInstanceKind(%q) error = %v, want nil", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("ParseInstanceKind(%q) = %v, want %v", tc.input, got, tc.want)
		}
		if !got.Valid() {
			t.Errorf("InstanceKind(%q).Valid() = false, want true", tc.input)
		}
		if got.String() != tc.input {
			t.Errorf("InstanceKind(%q).String() = %q, want %q", tc.input, got.String(), tc.input)
		}
	}
}

func TestInstanceKindParseUnknown(t *testing.T) {
	_, err := domain.ParseInstanceKind("recurring")
	if err == nil {
		t.Error("ParseInstanceKind(unknown) error = nil, want non-nil")
	}
}

func TestInstanceKindValid(t *testing.T) {
	if domain.InstanceKind("recurring").Valid() {
		t.Error("InstanceKind(recurring).Valid() = true, want false")
	}
}

// ---------------------------------------------------------------------------
// IDs
// ---------------------------------------------------------------------------

func TestRecurringTaskIDRoundTrip(t *testing.T) {
	id := domain.NewRecurringTaskID()
	s := id.String()
	parsed, err := domain.ParseRecurringTaskID(s)
	if err != nil {
		t.Fatalf("ParseRecurringTaskID(%q) error = %v", s, err)
	}
	if parsed != id {
		t.Errorf("ParseRecurringTaskID round-trip: got %v, want %v", parsed, id)
	}
}

func TestRecurringTaskIDParseInvalid(t *testing.T) {
	_, err := domain.ParseRecurringTaskID("not-a-uuid")
	if err == nil {
		t.Error("ParseRecurringTaskID(invalid) error = nil, want non-nil")
	}
}

func TestTaskInstanceIDRoundTrip(t *testing.T) {
	id := domain.NewTaskInstanceID()
	s := id.String()
	parsed, err := domain.ParseTaskInstanceID(s)
	if err != nil {
		t.Fatalf("ParseTaskInstanceID(%q) error = %v", s, err)
	}
	if parsed != id {
		t.Errorf("ParseTaskInstanceID round-trip: got %v, want %v", parsed, id)
	}
}

func TestTaskInstanceIDParseInvalid(t *testing.T) {
	_, err := domain.ParseTaskInstanceID("not-a-uuid")
	if err == nil {
		t.Error("ParseTaskInstanceID(invalid) error = nil, want non-nil")
	}
}

// ---------------------------------------------------------------------------
// Entity construction
// ---------------------------------------------------------------------------

func TestRecurringTaskConstruction(t *testing.T) {
	hid := household.NewHouseholdID()
	cadence := household.Cadence{
		Freq:     household.FreqWeekly,
		Interval: 1,
		Anchor:   time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
	}
	rt := domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    hid,
		Title:          "Take out the bins",
		Category:       domain.ChoreCategory,
		Cadence:        cadence,
		RotationPolicy: domain.RotationRoundRobin,
		Points:         10,
		LeadTimeDays:   1,
		Active:         true,
	}

	if rt.Title != "Take out the bins" {
		t.Errorf("Title = %q, want %q", rt.Title, "Take out the bins")
	}
	if rt.Category != domain.ChoreCategory {
		t.Errorf("Category = %v, want %v", rt.Category, domain.ChoreCategory)
	}
	if rt.RotationPolicy != domain.RotationRoundRobin {
		t.Errorf("RotationPolicy = %v, want %v", rt.RotationPolicy, domain.RotationRoundRobin)
	}
	if rt.Points != 10 {
		t.Errorf("Points = %d, want 10", rt.Points)
	}
	if rt.LeadTimeDays != 1 {
		t.Errorf("LeadTimeDays = %d, want 1", rt.LeadTimeDays)
	}
	if !rt.Active {
		t.Error("Active = false, want true")
	}
}

func TestTaskInstanceConstruction(t *testing.T) {
	hid := household.NewHouseholdID()
	mid := household.NewMemberID()
	rtid := domain.NewRecurringTaskID()
	due := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)

	inst := domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: rtid,
		HouseholdID:     hid,
		AssigneeID:      &mid,
		DueOn:           &due,
		Status:          domain.StatusPending,
		Kind:            domain.KindScheduled,
	}

	if inst.AssigneeID == nil {
		t.Fatal("AssigneeID = nil, want non-nil")
	}
	if *inst.AssigneeID != mid {
		t.Errorf("AssigneeID = %v, want %v", *inst.AssigneeID, mid)
	}
	if inst.Status != domain.StatusPending {
		t.Errorf("Status = %v, want %v", inst.Status, domain.StatusPending)
	}
	if inst.CompletedAt != nil {
		t.Error("CompletedAt should be nil for a pending instance")
	}
	if inst.CompletedBy != nil {
		t.Error("CompletedBy should be nil for a pending instance")
	}
}

func TestTaskInstanceNilAssignee(t *testing.T) {
	// Claimable instances start with no assignee.
	inst := domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: domain.NewRecurringTaskID(),
		HouseholdID:     household.NewHouseholdID(),
		AssigneeID:      nil,
		DueOn:           domain.DueOnPtr(time.Now()),
		Status:          domain.StatusPending,
	}
	if inst.AssigneeID != nil {
		t.Error("AssigneeID = non-nil, want nil for claimable instance")
	}
}

// TestTaskInstanceStandingHasNoDueOn verifies that a standing instance
// (NES-116) — the single open occurrence of an as-needed task — is
// constructed with a nil DueOn, matching the nullable due_on column.
func TestTaskInstanceStandingHasNoDueOn(t *testing.T) {
	inst := domain.TaskInstance{
		ID:              domain.NewTaskInstanceID(),
		RecurringTaskID: domain.NewRecurringTaskID(),
		HouseholdID:     household.NewHouseholdID(),
		Status:          domain.StatusPending,
		Kind:            domain.KindStanding,
	}
	if inst.DueOn != nil {
		t.Errorf("DueOn = %v, want nil for a standing instance", inst.DueOn)
	}
}
