package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ericfisherdev/nestcore/crypto/cryptotest"

	"github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakePINRepo is an in-memory authdomain.PINRepository, mirroring the
// composite tenant FK the real Postgres adapter enforces: SetPIN rejects a
// (memberID, householdID) pair that doesn't match any previously-set
// household for that member with household.ErrMemberNotFound.
type fakePINRepo struct {
	hashes      map[household.MemberID]string
	householdOf map[household.MemberID]household.HouseholdID
}

func newFakePINRepo() *fakePINRepo {
	return &fakePINRepo{
		hashes:      make(map[household.MemberID]string),
		householdOf: make(map[household.MemberID]household.HouseholdID),
	}
}

func (f *fakePINRepo) SetPIN(_ context.Context, memberID household.MemberID, householdID household.HouseholdID, pinHash string) error {
	if existing, ok := f.householdOf[memberID]; ok && existing != householdID {
		return household.ErrMemberNotFound
	}
	f.hashes[memberID] = pinHash
	f.householdOf[memberID] = householdID
	return nil
}

func (f *fakePINRepo) GetPINHash(_ context.Context, memberID household.MemberID) (string, error) {
	hash, ok := f.hashes[memberID]
	if !ok {
		return "", authdomain.ErrPINNotEnrolled
	}
	return hash, nil
}

func (f *fakePINRepo) ClearPIN(_ context.Context, memberID household.MemberID, householdID household.HouseholdID) error {
	// Mirrors the real DELETE's predicate: member_id AND household_id. A
	// member of another household must look exactly like no row at all.
	if _, ok := f.hashes[memberID]; !ok || f.householdOf[memberID] != householdID {
		return authdomain.ErrPINNotEnrolled
	}
	delete(f.hashes, memberID)
	delete(f.householdOf, memberID)
	return nil
}

func (f *fakePINRepo) EnrolledMembers(_ context.Context, householdID household.HouseholdID) ([]household.MemberID, error) {
	var ids []household.MemberID
	for memberID, hhID := range f.householdOf {
		if hhID == householdID {
			ids = append(ids, memberID)
		}
	}
	return ids, nil
}

var _ authdomain.PINRepository = (*fakePINRepo)(nil)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

func discardPINLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeClock returns a func() time.Time seam pinned to a mutable time.Time,
// so lockout-expiry tests can advance it deterministically without a real
// sleep.
type fakeClock struct {
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Now()}
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestPINService(t *testing.T, repo authdomain.PINRepository, clock *fakeClock) *app.PINService {
	t.Helper()
	svc, err := app.NewPINService(repo, cryptotest.Hasher(), clock.Now, discardPINLogger())
	if err != nil {
		t.Fatalf("NewPINService: %v", err)
	}
	return svc
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestNewPINService_RequiresDependencies(t *testing.T) {
	repo := newFakePINRepo()
	hasher := cryptotest.Hasher()
	now := time.Now
	logger := discardPINLogger()

	if _, err := app.NewPINService(nil, hasher, now, logger); err == nil {
		t.Error("nil repo: want error")
	}
	if _, err := app.NewPINService(repo, nil, now, logger); err == nil {
		t.Error("nil hasher: want error")
	}
	if _, err := app.NewPINService(repo, hasher, nil, logger); err == nil {
		t.Error("nil clock: want error")
	}
	if _, err := app.NewPINService(repo, hasher, now, nil); err == nil {
		t.Error("nil logger: want error")
	}
}

func TestPINService_SetThenVerify_HashRoundTrip(t *testing.T) {
	repo := newFakePINRepo()
	clock := newFakeClock()
	svc := newTestPINService(t, repo, clock)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()

	if err := svc.Set(context.Background(), memberID, householdID, "1234"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if repo.hashes[memberID] == "1234" {
		t.Error("the stored hash must not equal the plaintext PIN")
	}
	if err := svc.Verify(context.Background(), memberID, "1234"); err != nil {
		t.Errorf("Verify with the correct PIN: %v, want nil", err)
	}
}

func TestPINService_Verify_WrongPINReturnsMismatch(t *testing.T) {
	repo := newFakePINRepo()
	clock := newFakeClock()
	svc := newTestPINService(t, repo, clock)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()

	if err := svc.Set(context.Background(), memberID, householdID, "1234"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	err := svc.Verify(context.Background(), memberID, "9999")
	if !errors.Is(err, authdomain.ErrPINMismatch) {
		t.Errorf("Verify wrong pin = %v, want ErrPINMismatch", err)
	}
}

func TestPINService_Verify_NotEnrolledReturnsErrPINNotEnrolled(t *testing.T) {
	repo := newFakePINRepo()
	clock := newFakeClock()
	svc := newTestPINService(t, repo, clock)
	memberID := household.NewMemberID()

	err := svc.Verify(context.Background(), memberID, "1234")
	if !errors.Is(err, authdomain.ErrPINNotEnrolled) {
		t.Errorf("Verify unenrolled member = %v, want ErrPINNotEnrolled", err)
	}
}

func TestPINService_Set_InvalidFormatRejected(t *testing.T) {
	tests := []struct {
		name string
		pin  string
	}{
		{name: "too short", pin: "123"},
		{name: "too long", pin: "123456789"},
		{name: "contains letters", pin: "12a4"},
		{name: "empty", pin: ""},
		{name: "contains a dash", pin: "12-4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakePINRepo()
			clock := newFakeClock()
			svc := newTestPINService(t, repo, clock)
			memberID := household.NewMemberID()
			householdID := household.NewHouseholdID()

			err := svc.Set(context.Background(), memberID, householdID, tt.pin)
			if !errors.Is(err, authdomain.ErrInvalidPINFormat) {
				t.Errorf("Set(%q) = %v, want ErrInvalidPINFormat", tt.pin, err)
			}
		})
	}
}

func TestPINService_Set_AcceptsBoundaryLengths(t *testing.T) {
	for _, pin := range []string{"1234", "12345678"} {
		repo := newFakePINRepo()
		clock := newFakeClock()
		svc := newTestPINService(t, repo, clock)
		memberID := household.NewMemberID()
		householdID := household.NewHouseholdID()

		if err := svc.Set(context.Background(), memberID, householdID, pin); err != nil {
			t.Errorf("Set(%q): %v, want nil (4 and 8 digits are the inclusive boundary)", pin, err)
		}
	}
}

// TestPINService_Verify_LockoutEngagesAfterThreshold proves the strike
// limiter locks a member out after pinAttemptThreshold+1 consecutive wrong
// PINs, and that a locked member's PIN is not even consulted (a correct PIN
// submitted while locked still fails).
func TestPINService_Verify_LockoutEngagesAfterThreshold(t *testing.T) {
	repo := newFakePINRepo()
	clock := newFakeClock()
	svc := newTestPINService(t, repo, clock)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	ctx := context.Background()

	if err := svc.Set(ctx, memberID, householdID, "1234"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Drive it into lockout. The exact threshold is an implementation
	// detail of pinAttemptLimiter; five wrong PINs (mirroring the login MFA
	// limiter's own threshold, which this package's pinAttemptLimiter
	// documents mirroring) is enough to cross it regardless of the exact
	// constant.
	const wrongAttempts = 6
	var lastErr error
	for range wrongAttempts {
		lastErr = svc.Verify(ctx, memberID, "0000")
	}
	if !errors.Is(lastErr, authdomain.ErrPINLocked) && !errors.Is(lastErr, authdomain.ErrPINMismatch) {
		t.Fatalf("after %d wrong attempts, last error = %v, want ErrPINMismatch or ErrPINLocked", wrongAttempts, lastErr)
	}

	err := svc.Verify(ctx, memberID, "1234")
	if !errors.Is(err, authdomain.ErrPINLocked) {
		t.Errorf("Verify with the CORRECT pin while locked = %v, want ErrPINLocked", err)
	}

	lockedUntil, locked := svc.LockedUntil(memberID)
	if !locked {
		t.Fatal("LockedUntil: locked = false, want true")
	}
	if !lockedUntil.After(clock.Now()) {
		t.Error("LockedUntil must report a time in the future")
	}
}

// TestPINService_Verify_LockoutExpires proves a member can verify again
// once the backoff window has elapsed.
func TestPINService_Verify_LockoutExpires(t *testing.T) {
	repo := newFakePINRepo()
	clock := newFakeClock()
	svc := newTestPINService(t, repo, clock)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	ctx := context.Background()

	if err := svc.Set(ctx, memberID, householdID, "1234"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	for range 6 {
		_ = svc.Verify(ctx, memberID, "0000")
	}
	if _, locked := svc.LockedUntil(memberID); !locked {
		t.Fatal("setup: expected member to be locked")
	}

	clock.Advance(10 * time.Minute)

	if err := svc.Verify(ctx, memberID, "1234"); err != nil {
		t.Errorf("Verify after the backoff window elapsed = %v, want nil", err)
	}
}

// TestPINService_AuthorizeTaskAction_Matrix proves the full gate contract
// (NES-165 AC): a correct PIN returns the assignee as actor, wrong/locked
// PINs return errors, and an UNENROLLED assignee passes through unchanged
// (the nil-gate) rather than erroring.
func TestPINService_AuthorizeTaskAction_Matrix(t *testing.T) {
	t.Run("correct pin returns the assignee as actor", func(t *testing.T) {
		repo := newFakePINRepo()
		clock := newFakeClock()
		svc := newTestPINService(t, repo, clock)
		memberID := household.NewMemberID()
		householdID := household.NewHouseholdID()
		ctx := context.Background()
		if err := svc.Set(ctx, memberID, householdID, "1234"); err != nil {
			t.Fatalf("Set: %v", err)
		}

		actor, err := svc.AuthorizeTaskAction(ctx, memberID, "1234")
		if err != nil {
			t.Fatalf("AuthorizeTaskAction: %v", err)
		}
		if actor != memberID {
			t.Errorf("actor = %v, want the assignee %v", actor, memberID)
		}
	})

	t.Run("wrong pin returns an error and a zero actor", func(t *testing.T) {
		repo := newFakePINRepo()
		clock := newFakeClock()
		svc := newTestPINService(t, repo, clock)
		memberID := household.NewMemberID()
		householdID := household.NewHouseholdID()
		ctx := context.Background()
		if err := svc.Set(ctx, memberID, householdID, "1234"); err != nil {
			t.Fatalf("Set: %v", err)
		}

		actor, err := svc.AuthorizeTaskAction(ctx, memberID, "0000")
		if !errors.Is(err, authdomain.ErrPINMismatch) {
			t.Errorf("err = %v, want ErrPINMismatch", err)
		}
		if actor != (household.MemberID{}) {
			t.Errorf("actor = %v, want zero value", actor)
		}
	})

	t.Run("locked returns an error and a zero actor", func(t *testing.T) {
		repo := newFakePINRepo()
		clock := newFakeClock()
		svc := newTestPINService(t, repo, clock)
		memberID := household.NewMemberID()
		householdID := household.NewHouseholdID()
		ctx := context.Background()
		if err := svc.Set(ctx, memberID, householdID, "1234"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		for range 6 {
			_, _ = svc.AuthorizeTaskAction(ctx, memberID, "0000")
		}

		actor, err := svc.AuthorizeTaskAction(ctx, memberID, "1234")
		if !errors.Is(err, authdomain.ErrPINLocked) {
			t.Errorf("err = %v, want ErrPINLocked", err)
		}
		if actor != (household.MemberID{}) {
			t.Errorf("actor = %v, want zero value", actor)
		}
	})

	t.Run("an unenrolled assignee passes through unchanged", func(t *testing.T) {
		repo := newFakePINRepo()
		clock := newFakeClock()
		svc := newTestPINService(t, repo, clock)
		memberID := household.NewMemberID()
		ctx := context.Background()

		actor, err := svc.AuthorizeTaskAction(ctx, memberID, "1234")
		if err != nil {
			t.Errorf("err = %v, want nil (nil-gate for an unenrolled assignee)", err)
		}
		if actor != (household.MemberID{}) {
			t.Errorf("actor = %v, want zero value (caller must leave its own actor unchanged)", actor)
		}
	})
}

func TestPINService_Clear_RemovesPIN(t *testing.T) {
	repo := newFakePINRepo()
	clock := newFakeClock()
	svc := newTestPINService(t, repo, clock)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	ctx := context.Background()

	if err := svc.Set(ctx, memberID, householdID, "1234"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := svc.Clear(ctx, memberID, householdID); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if err := svc.Verify(ctx, memberID, "1234"); !errors.Is(err, authdomain.ErrPINNotEnrolled) {
		t.Errorf("Verify after Clear = %v, want ErrPINNotEnrolled", err)
	}
}

func TestPINService_Clear_NotEnrolledReturnsError(t *testing.T) {
	repo := newFakePINRepo()
	clock := newFakeClock()
	svc := newTestPINService(t, repo, clock)
	memberID := household.NewMemberID()

	if err := svc.Clear(context.Background(), memberID, household.NewHouseholdID()); !errors.Is(err, authdomain.ErrPINNotEnrolled) {
		t.Errorf("Clear on an unenrolled member = %v, want ErrPINNotEnrolled", err)
	}
}

func TestPINService_SetForMember_ThenVerify(t *testing.T) {
	repo := newFakePINRepo()
	clock := newFakeClock()
	svc := newTestPINService(t, repo, clock)
	targetID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	ctx := context.Background()

	if err := svc.SetForMember(ctx, targetID, householdID, "4321"); err != nil {
		t.Fatalf("SetForMember: %v", err)
	}
	if err := svc.Verify(ctx, targetID, "4321"); err != nil {
		t.Errorf("Verify after SetForMember = %v, want nil", err)
	}
}

// TestPINService_ResetForMember_ClearsPINAndLockout proves a single admin
// action removes both the PIN and any active lockout.
func TestPINService_ResetForMember_ClearsPINAndLockout(t *testing.T) {
	repo := newFakePINRepo()
	clock := newFakeClock()
	svc := newTestPINService(t, repo, clock)
	targetID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	ctx := context.Background()

	if err := svc.SetForMember(ctx, targetID, householdID, "4321"); err != nil {
		t.Fatalf("SetForMember: %v", err)
	}
	for range 6 {
		_ = svc.Verify(ctx, targetID, "0000")
	}
	if _, locked := svc.LockedUntil(targetID); !locked {
		t.Fatal("setup: expected member to be locked")
	}

	if err := svc.ResetForMember(ctx, targetID, householdID); err != nil {
		t.Fatalf("ResetForMember: %v", err)
	}
	if _, locked := svc.LockedUntil(targetID); locked {
		t.Error("after ResetForMember, LockedUntil should report not locked")
	}
	if err := svc.Verify(ctx, targetID, "4321"); !errors.Is(err, authdomain.ErrPINNotEnrolled) {
		t.Errorf("Verify after ResetForMember = %v, want ErrPINNotEnrolled (the PIN was also cleared)", err)
	}
}

func TestPINService_IsEnrolled(t *testing.T) {
	repo := newFakePINRepo()
	clock := newFakeClock()
	svc := newTestPINService(t, repo, clock)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	ctx := context.Background()

	enrolled, err := svc.IsEnrolled(ctx, memberID)
	if err != nil {
		t.Fatalf("IsEnrolled: %v", err)
	}
	if enrolled {
		t.Error("IsEnrolled before Set = true, want false")
	}

	if err := svc.Set(ctx, memberID, householdID, "1234"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	enrolled, err = svc.IsEnrolled(ctx, memberID)
	if err != nil {
		t.Fatalf("IsEnrolled: %v", err)
	}
	if !enrolled {
		t.Error("IsEnrolled after Set = false, want true")
	}
}

func TestPINService_EnrolledMembers_ScopedToHousehold(t *testing.T) {
	repo := newFakePINRepo()
	clock := newFakeClock()
	svc := newTestPINService(t, repo, clock)
	ctx := context.Background()

	householdA := household.NewHouseholdID()
	householdB := household.NewHouseholdID()
	memberA1 := household.NewMemberID()
	memberA2 := household.NewMemberID()
	memberB1 := household.NewMemberID()

	if err := svc.Set(ctx, memberA1, householdA, "1234"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := svc.Set(ctx, memberA2, householdA, "5678"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := svc.Set(ctx, memberB1, householdB, "1111"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	ids, err := svc.EnrolledMembers(ctx, householdA)
	if err != nil {
		t.Fatalf("EnrolledMembers: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("EnrolledMembers(householdA) returned %d ids, want 2", len(ids))
	}
	seen := map[household.MemberID]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen[memberA1] || !seen[memberA2] {
		t.Error("EnrolledMembers(householdA) must include both members A1 and A2")
	}
	if seen[memberB1] {
		t.Error("EnrolledMembers(householdA) must not include a member from a different household")
	}
}

// TestPINService_ResetForMember_OtherHouseholdKeepsLockout pins the other
// half of the tenant scope: the lockout lives in memory, past any SQL
// predicate's reach, so a reset that clears no PIN must not clear the
// lockout either. Otherwise an admin of another household could hand a
// locked-out member unlimited fresh attempts.
func TestPINService_ResetForMember_OtherHouseholdKeepsLockout(t *testing.T) {
	repo := newFakePINRepo()
	clock := newFakeClock()
	svc := newTestPINService(t, repo, clock)
	targetID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	ctx := context.Background()

	if err := svc.SetForMember(ctx, targetID, householdID, "4321"); err != nil {
		t.Fatalf("SetForMember: %v", err)
	}
	for range 6 {
		_ = svc.Verify(ctx, targetID, "0000")
	}
	if _, locked := svc.LockedUntil(targetID); !locked {
		t.Fatal("setup: expected member to be locked")
	}

	err := svc.ResetForMember(ctx, targetID, household.NewHouseholdID())
	if !errors.Is(err, authdomain.ErrPINNotEnrolled) {
		t.Errorf("ResetForMember from another household = %v, want ErrPINNotEnrolled", err)
	}
	if _, locked := svc.LockedUntil(targetID); !locked {
		t.Error("a cross-household reset cleared the lockout; it must stay in place")
	}
	if err := svc.Verify(ctx, targetID, "4321"); errors.Is(err, authdomain.ErrPINNotEnrolled) {
		t.Error("a cross-household reset removed the PIN; it must stay enrolled")
	}
}
