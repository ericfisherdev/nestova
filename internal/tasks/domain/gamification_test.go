package domain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// RedemptionStatus
// ---------------------------------------------------------------------------

func TestRedemptionStatusParseAndValid(t *testing.T) {
	cases := []struct {
		input string
		want  domain.RedemptionStatus
	}{
		{"pending", domain.RedemptionPending},
		{"fulfilled", domain.RedemptionFulfilled},
		{"denied", domain.RedemptionDenied},
		{"cancelled", domain.RedemptionCancelled},
	}
	for _, tc := range cases {
		got, err := domain.ParseRedemptionStatus(tc.input)
		if err != nil {
			t.Errorf("ParseRedemptionStatus(%q) error = %v, want nil", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("ParseRedemptionStatus(%q) = %v, want %v", tc.input, got, tc.want)
		}
		if !got.Valid() {
			t.Errorf("RedemptionStatus(%q).Valid() = false, want true", tc.input)
		}
		if got.String() != tc.input {
			t.Errorf("RedemptionStatus(%q).String() = %q, want %q", tc.input, got.String(), tc.input)
		}
	}
}

func TestRedemptionStatusParseUnknown(t *testing.T) {
	_, err := domain.ParseRedemptionStatus("unknown")
	if err == nil {
		t.Error("ParseRedemptionStatus(unknown) error = nil, want non-nil")
	}
}

func TestRedemptionStatusValid(t *testing.T) {
	if domain.RedemptionStatus("unknown").Valid() {
		t.Error("RedemptionStatus(unknown).Valid() = true, want false")
	}
	// "requested" was NES-127's pre-rename value; it must no longer validate,
	// confirming the migration's rename to "pending" is reflected here too.
	if domain.RedemptionStatus("requested").Valid() {
		t.Error(`RedemptionStatus("requested").Valid() = true, want false (renamed to "pending" by NES-127)`)
	}
}

// ---------------------------------------------------------------------------
// PointEntryID
// ---------------------------------------------------------------------------

func TestPointEntryIDRoundTrip(t *testing.T) {
	id := domain.NewPointEntryID()
	s := id.String()
	parsed, err := domain.ParsePointEntryID(s)
	if err != nil {
		t.Fatalf("ParsePointEntryID(%q) error = %v", s, err)
	}
	if parsed != id {
		t.Errorf("ParsePointEntryID round-trip: got %v, want %v", parsed, id)
	}
}

func TestPointEntryIDParseInvalid(t *testing.T) {
	_, err := domain.ParsePointEntryID("not-a-uuid")
	if err == nil {
		t.Error("ParsePointEntryID(invalid) error = nil, want non-nil")
	}
}

// ---------------------------------------------------------------------------
// RewardID
// ---------------------------------------------------------------------------

func TestRewardIDRoundTrip(t *testing.T) {
	id := domain.NewRewardID()
	s := id.String()
	parsed, err := domain.ParseRewardID(s)
	if err != nil {
		t.Fatalf("ParseRewardID(%q) error = %v", s, err)
	}
	if parsed != id {
		t.Errorf("ParseRewardID round-trip: got %v, want %v", parsed, id)
	}
}

func TestRewardIDParseInvalid(t *testing.T) {
	_, err := domain.ParseRewardID("not-a-uuid")
	if err == nil {
		t.Error("ParseRewardID(invalid) error = nil, want non-nil")
	}
}

// ---------------------------------------------------------------------------
// RewardRedemptionID
// ---------------------------------------------------------------------------

func TestRewardRedemptionIDRoundTrip(t *testing.T) {
	id := domain.NewRewardRedemptionID()
	s := id.String()
	parsed, err := domain.ParseRewardRedemptionID(s)
	if err != nil {
		t.Fatalf("ParseRewardRedemptionID(%q) error = %v", s, err)
	}
	if parsed != id {
		t.Errorf("ParseRewardRedemptionID round-trip: got %v, want %v", parsed, id)
	}
}

func TestRewardRedemptionIDParseInvalid(t *testing.T) {
	_, err := domain.ParseRewardRedemptionID("not-a-uuid")
	if err == nil {
		t.Error("ParseRewardRedemptionID(invalid) error = nil, want non-nil")
	}
}

// ---------------------------------------------------------------------------
// Entity construction
// ---------------------------------------------------------------------------

func TestPointEntryConstruction(t *testing.T) {
	hid := household.NewHouseholdID()
	mid := household.NewMemberID()
	srcID := uuid.Must(uuid.NewV7())
	now := time.Now().UTC()

	entry := domain.PointEntry{
		ID:          domain.NewPointEntryID(),
		HouseholdID: hid,
		MemberID:    mid,
		SourceType:  "task_instance",
		SourceID:    &srcID,
		Points:      10,
		CreatedAt:   now,
	}

	if entry.Points != 10 {
		t.Errorf("Points = %d, want 10", entry.Points)
	}
	if entry.SourceType != "task_instance" {
		t.Errorf("SourceType = %q, want %q", entry.SourceType, "task_instance")
	}
	if entry.SourceID == nil {
		t.Fatal("SourceID = nil, want non-nil")
	}
	if *entry.SourceID != srcID {
		t.Errorf("SourceID = %v, want %v", *entry.SourceID, srcID)
	}
}

func TestPointEntryNilSourceID(t *testing.T) {
	// Manual point adjustments have no source row.
	entry := domain.PointEntry{
		ID:          domain.NewPointEntryID(),
		HouseholdID: household.NewHouseholdID(),
		MemberID:    household.NewMemberID(),
		SourceType:  "manual",
		SourceID:    nil,
		Points:      5,
	}
	if entry.SourceID != nil {
		t.Error("SourceID = non-nil, want nil for manual adjustment")
	}
}

func TestPointEntryNegativePoints(t *testing.T) {
	// Redemption debits carry negative point values.
	entry := domain.PointEntry{
		ID:          domain.NewPointEntryID(),
		HouseholdID: household.NewHouseholdID(),
		MemberID:    household.NewMemberID(),
		SourceType:  "redemption",
		SourceID:    nil,
		Points:      -50,
	}
	if entry.Points != -50 {
		t.Errorf("Points = %d, want -50", entry.Points)
	}
}

func TestRewardConstruction(t *testing.T) {
	hid := household.NewHouseholdID()
	now := time.Now().UTC()

	r := domain.Reward{
		ID:          domain.NewRewardID(),
		HouseholdID: hid,
		Name:        "Movie night pick",
		CostPoints:  100,
		Active:      true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if r.Name != "Movie night pick" {
		t.Errorf("Name = %q, want %q", r.Name, "Movie night pick")
	}
	if r.CostPoints != 100 {
		t.Errorf("CostPoints = %d, want 100", r.CostPoints)
	}
	if !r.Active {
		t.Error("Active = false, want true")
	}
}

func TestRewardRedemptionConstruction(t *testing.T) {
	hid := household.NewHouseholdID()
	mid := household.NewMemberID()
	rid := domain.NewRewardID()
	now := time.Now().UTC()

	redemption := domain.RewardRedemption{
		ID:          domain.NewRewardRedemptionID(),
		HouseholdID: hid,
		RewardID:    rid,
		MemberID:    mid,
		Status:      domain.RedemptionPending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if redemption.Status != domain.RedemptionPending {
		t.Errorf("Status = %v, want %v", redemption.Status, domain.RedemptionPending)
	}
	if redemption.MemberID != mid {
		t.Errorf("MemberID = %v, want %v", redemption.MemberID, mid)
	}
	if redemption.RewardID != rid {
		t.Errorf("RewardID = %v, want %v", redemption.RewardID, rid)
	}
}
