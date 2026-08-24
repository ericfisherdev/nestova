package adapter_test

import (
	"errors"
	"testing"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// newTestPINRepo returns a PINRepository (and the household id + member id
// it seeds) backed by NESTOVA_TEST_DATABASE_URL, reusing newTestRepos'
// schema setup/teardown — mirroring newTestMFARepo.
func newTestPINRepo(t *testing.T) (*authadapter.PINRepository, household.HouseholdID, household.MemberID) {
	t.Helper()
	_, hhRepo, pool := newTestRepos(t)
	memberID := seedMember(t, hhRepo)
	member, err := hhRepo.GetMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetMember: %v", err)
	}
	return authadapter.NewPINRepository(pool), member.HouseholdID, memberID
}

func TestPINSetPIN_PersistsHash(t *testing.T) {
	repo, householdID, memberID := newTestPINRepo(t)

	if err := repo.SetPIN(testCtx(t), memberID, householdID, "argon2id-fixture-hash"); err != nil {
		t.Fatalf("SetPIN: %v", err)
	}

	hash, err := repo.GetPINHash(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetPINHash: %v", err)
	}
	if hash != "argon2id-fixture-hash" {
		t.Errorf("GetPINHash = %q, want the exact hash SetPIN stored", hash)
	}
}

func TestPINSetPIN_ReplacesExistingHash(t *testing.T) {
	repo, householdID, memberID := newTestPINRepo(t)

	if err := repo.SetPIN(testCtx(t), memberID, householdID, "first-hash"); err != nil {
		t.Fatalf("SetPIN: %v", err)
	}
	if err := repo.SetPIN(testCtx(t), memberID, householdID, "second-hash"); err != nil {
		t.Fatalf("SetPIN (replace): %v", err)
	}

	hash, err := repo.GetPINHash(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetPINHash: %v", err)
	}
	if hash != "second-hash" {
		t.Errorf("GetPINHash = %q, want the replaced hash", hash)
	}
}

// TestPINSetPIN_UnknownMemberReturnsErrMemberNotFound uses a real,
// known household id with an unknown member id, so the composite tenant FK
// (member_pin_member_fk) is the constraint that fires — a household id
// that does not exist at all would instead trip the plain household_id
// foreign key first, mirroring
// TestMFABeginEnrollment_UnknownMemberInHousehold's own setup.
func TestPINSetPIN_UnknownMemberReturnsErrMemberNotFound(t *testing.T) {
	repo, householdID, _ := newTestPINRepo(t)

	err := repo.SetPIN(testCtx(t), household.NewMemberID(), householdID, "some-hash")
	if !errors.Is(err, household.ErrMemberNotFound) {
		t.Errorf("SetPIN for an unknown member = %v, want household.ErrMemberNotFound", err)
	}
}

func TestPINGetPINHash_NoRowReturnsErrPINNotEnrolled(t *testing.T) {
	repo, _, memberID := newTestPINRepo(t)

	_, err := repo.GetPINHash(testCtx(t), memberID)
	if !errors.Is(err, authdomain.ErrPINNotEnrolled) {
		t.Errorf("GetPINHash with no row = %v, want authdomain.ErrPINNotEnrolled", err)
	}
}

func TestPINClearPIN_DeletesRow(t *testing.T) {
	repo, householdID, memberID := newTestPINRepo(t)
	if err := repo.SetPIN(testCtx(t), memberID, householdID, "a-hash"); err != nil {
		t.Fatalf("SetPIN: %v", err)
	}

	if err := repo.ClearPIN(testCtx(t), memberID, householdID); err != nil {
		t.Fatalf("ClearPIN: %v", err)
	}

	_, err := repo.GetPINHash(testCtx(t), memberID)
	if !errors.Is(err, authdomain.ErrPINNotEnrolled) {
		t.Errorf("GetPINHash after ClearPIN = %v, want authdomain.ErrPINNotEnrolled", err)
	}
}

func TestPINClearPIN_NoRowReturnsErrPINNotEnrolled(t *testing.T) {
	repo, householdID, memberID := newTestPINRepo(t)

	err := repo.ClearPIN(testCtx(t), memberID, householdID)
	if !errors.Is(err, authdomain.ErrPINNotEnrolled) {
		t.Errorf("ClearPIN with no row = %v, want authdomain.ErrPINNotEnrolled", err)
	}
}

func TestPINEnrolledMembers_ScopedToHousehold(t *testing.T) {
	repo, householdID, memberID := newTestPINRepo(t)
	if err := repo.SetPIN(testCtx(t), memberID, householdID, "a-hash"); err != nil {
		t.Fatalf("SetPIN: %v", err)
	}

	ids, err := repo.EnrolledMembers(testCtx(t), householdID)
	if err != nil {
		t.Fatalf("EnrolledMembers: %v", err)
	}
	found := false
	for _, id := range ids {
		if id == memberID {
			found = true
		}
	}
	if !found {
		t.Errorf("EnrolledMembers(%v) = %v, want it to include %v", householdID, ids, memberID)
	}

	otherIDs, err := repo.EnrolledMembers(testCtx(t), household.NewHouseholdID())
	if err != nil {
		t.Fatalf("EnrolledMembers (other household): %v", err)
	}
	if len(otherIDs) != 0 {
		t.Errorf("EnrolledMembers for an unrelated household = %v, want empty", otherIDs)
	}
}

// TestPINClearPIN_OtherHouseholdLeavesRowIntact pins the tenant scope on the
// DELETE. member_pin's primary key is member_id alone, so an unscoped delete
// would let an admin of any household clear any member's PIN given only
// their id — the composite FK that protects SetPIN does not apply to a
// DELETE. A foreign household must be indistinguishable from no row at all.
func TestPINClearPIN_OtherHouseholdLeavesRowIntact(t *testing.T) {
	repo, householdID, memberID := newTestPINRepo(t)
	if err := repo.SetPIN(testCtx(t), memberID, householdID, "a-hash"); err != nil {
		t.Fatalf("SetPIN: %v", err)
	}

	err := repo.ClearPIN(testCtx(t), memberID, household.NewHouseholdID())
	if !errors.Is(err, authdomain.ErrPINNotEnrolled) {
		t.Errorf("ClearPIN from another household = %v, want authdomain.ErrPINNotEnrolled", err)
	}

	hash, err := repo.GetPINHash(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetPINHash after a cross-household ClearPIN: %v", err)
	}
	if hash != "a-hash" {
		t.Errorf("stored hash = %q, want it left intact at %q", hash, "a-hash")
	}
}
