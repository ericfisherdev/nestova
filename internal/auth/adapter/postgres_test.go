package adapter_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	householdadapter "github.com/ericfisherdev/nestova/internal/household/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	"github.com/ericfisherdev/nestova/internal/platform/db/dbtest"
)

// newTestRepos returns a credential repository (and household repository
// for seeding) over this package's own derived database (NES-149), freshly
// reset and migrated. dbtest.NewIsolatedPool owns the safety rail, the
// on-demand CREATE DATABASE, and the reset/migrate lifecycle.
func newTestRepos(t *testing.T) (*authadapter.CredentialRepository, *householdadapter.PostgresRepository, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.NewIsolatedPool(t, "auth")
	return authadapter.NewCredentialRepository(pool), householdadapter.NewPostgresRepository(pool), pool
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// seedMember creates a household and a member, returning the member's ID.
func seedMember(t *testing.T, repo *householdadapter.PostgresRepository) household.MemberID {
	t.Helper()
	h := &household.Household{ID: household.NewHouseholdID(), Name: "Test Household"}
	if err := repo.CreateHousehold(testCtx(t), h); err != nil {
		t.Fatalf("CreateHousehold: %v", err)
	}
	m := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: h.ID,
		DisplayName: "Testuser",
		Role:        household.RoleAdult,
		Color:       household.ColorSage,
	}
	if err := repo.AddMember(testCtx(t), m); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	return m.ID
}

// seedHousehold creates a bare household with no member and returns its ID —
// for cross-household tenant-isolation tests that need a second, GENUINELY
// EXISTING household distinct from a victim member's own (as opposed to a
// fabricated, nonexistent household.NewHouseholdID(), which would trip the
// table's plain household_id FK before ever reaching the composite tenant
// FK the test means to exercise). Mirrors media/adapter's own seedHousehold
// helper.
func seedHousehold(t *testing.T, repo *householdadapter.PostgresRepository) household.HouseholdID {
	t.Helper()
	h := &household.Household{ID: household.NewHouseholdID(), Name: "Attacker Household"}
	if err := repo.CreateHousehold(testCtx(t), h); err != nil {
		t.Fatalf("CreateHousehold: %v", err)
	}
	return h.ID
}

func TestSetPasswordAndFindByEmail(t *testing.T) {
	credRepo, hhRepo, _ := newTestRepos(t)
	memberID := seedMember(t, hhRepo)

	hash, err := crypto.Hash("supersecret")
	if err != nil {
		t.Fatalf("crypto.Hash: %v", err)
	}

	if err := credRepo.SetPassword(testCtx(t), memberID, "user@example.com", hash); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	cred, err := credRepo.FindByEmail(testCtx(t), "user@example.com")
	if err != nil {
		t.Fatalf("FindByEmail: %v", err)
	}
	if cred.MemberID != memberID {
		t.Errorf("FindByEmail MemberID = %v, want %v", cred.MemberID, memberID)
	}
	if cred.PasswordHash != hash {
		t.Errorf("FindByEmail PasswordHash differs from stored value")
	}
}

func TestFindByEmailCaseInsensitive(t *testing.T) {
	credRepo, hhRepo, _ := newTestRepos(t)
	memberID := seedMember(t, hhRepo)

	hash, err := crypto.Hash("supersecret")
	if err != nil {
		t.Fatalf("crypto.Hash: %v", err)
	}

	if err := credRepo.SetPassword(testCtx(t), memberID, "User@Example.COM", hash); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// citext column means lookup is case-insensitive.
	cred, err := credRepo.FindByEmail(testCtx(t), "user@example.com")
	if err != nil {
		t.Fatalf("FindByEmail (lowercase): %v", err)
	}
	if cred.MemberID != memberID {
		t.Errorf("FindByEmail MemberID = %v, want %v", cred.MemberID, memberID)
	}
}

func TestFindByEmailUnknownReturnsErrInvalidCredentials(t *testing.T) {
	credRepo, _, _ := newTestRepos(t)

	_, err := credRepo.FindByEmail(testCtx(t), "nobody@example.com")
	if !errors.Is(err, authdomain.ErrInvalidCredentials) {
		t.Errorf("FindByEmail(unknown) error = %v, want ErrInvalidCredentials", err)
	}
}

// TestFindByMemberID covers NES-134's owner-reauth lookup: finding a
// credential by member id (rather than email) once a password is set, and
// the ErrInvalidCredentials sentinel both for a member with no password set
// and for an unknown member id.
func TestFindByMemberID(t *testing.T) {
	credRepo, hhRepo, _ := newTestRepos(t)
	memberID := seedMember(t, hhRepo)

	hash, err := crypto.Hash("supersecret")
	if err != nil {
		t.Fatalf("crypto.Hash: %v", err)
	}
	if err := credRepo.SetPassword(testCtx(t), memberID, "owner@example.com", hash); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	cred, err := credRepo.FindByMemberID(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("FindByMemberID: %v", err)
	}
	if cred.MemberID != memberID {
		t.Errorf("FindByMemberID MemberID = %v, want %v", cred.MemberID, memberID)
	}
	if cred.PasswordHash != hash {
		t.Errorf("FindByMemberID PasswordHash differs from stored value")
	}
}

func TestFindByMemberIDNoPasswordSet(t *testing.T) {
	credRepo, hhRepo, _ := newTestRepos(t)
	memberID := seedMember(t, hhRepo)

	_, err := credRepo.FindByMemberID(testCtx(t), memberID)
	if !errors.Is(err, authdomain.ErrInvalidCredentials) {
		t.Errorf("FindByMemberID(no password set) error = %v, want ErrInvalidCredentials", err)
	}
}

func TestFindByMemberIDUnknownMember(t *testing.T) {
	credRepo, _, _ := newTestRepos(t)

	_, err := credRepo.FindByMemberID(testCtx(t), household.NewMemberID())
	if !errors.Is(err, authdomain.ErrInvalidCredentials) {
		t.Errorf("FindByMemberID(unknown member) error = %v, want ErrInvalidCredentials", err)
	}
}

func TestCredentialColumnsArePaired(t *testing.T) {
	credRepo, hhRepo, pool := newTestRepos(t)
	memberID := seedMember(t, hhRepo)

	hash, err := crypto.Hash("supersecret")
	if err != nil {
		t.Fatalf("crypto.Hash: %v", err)
	}
	if err := credRepo.SetPassword(testCtx(t), memberID, "user@example.com", hash); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// The member_credentials_complete CHECK forbids an email without a password
	// (and vice versa), so an inconsistent credential state cannot exist.
	if _, err := pool.Exec(testCtx(t), `UPDATE member SET password_hash = NULL WHERE id = $1`, memberID.String()); err == nil {
		t.Error("nulling password_hash while email is set should violate the credentials CHECK constraint")
	}
}

func TestSetPasswordDuplicateEmail(t *testing.T) {
	credRepo, hhRepo, _ := newTestRepos(t)
	hash, err := crypto.Hash("supersecret")
	if err != nil {
		t.Fatalf("crypto.Hash: %v", err)
	}

	first := seedMember(t, hhRepo)
	if err := credRepo.SetPassword(testCtx(t), first, "shared@example.com", hash); err != nil {
		t.Fatalf("SetPassword(first): %v", err)
	}
	// A second member cannot claim the same email.
	second := seedMember(t, hhRepo)
	if err := credRepo.SetPassword(testCtx(t), second, "shared@example.com", hash); !errors.Is(err, authdomain.ErrEmailAlreadyInUse) {
		t.Errorf("SetPassword(duplicate email) error = %v, want ErrEmailAlreadyInUse", err)
	}
}

func TestSetPasswordUpdatesExistingCredentials(t *testing.T) {
	credRepo, hhRepo, _ := newTestRepos(t)
	memberID := seedMember(t, hhRepo)

	hash1, _ := crypto.Hash("firstpass")
	if err := credRepo.SetPassword(testCtx(t), memberID, "user@example.com", hash1); err != nil {
		t.Fatalf("SetPassword(first): %v", err)
	}
	// Update both email and password; the UPDATE path must replace them.
	hash2, _ := crypto.Hash("secondpass")
	if err := credRepo.SetPassword(testCtx(t), memberID, "updated@example.com", hash2); err != nil {
		t.Fatalf("SetPassword(update): %v", err)
	}

	cred, err := credRepo.FindByEmail(testCtx(t), "updated@example.com")
	if err != nil {
		t.Fatalf("FindByEmail(updated): %v", err)
	}
	if cred.PasswordHash != hash2 {
		t.Error("FindByEmail returned the old password hash after update")
	}
	if _, err := credRepo.FindByEmail(testCtx(t), "user@example.com"); !errors.Is(err, authdomain.ErrInvalidCredentials) {
		t.Errorf("old email after update: error = %v, want ErrInvalidCredentials", err)
	}
}

func TestSetPasswordUnknownMemberReturnsErrMemberNotFound(t *testing.T) {
	credRepo, _, _ := newTestRepos(t)

	hash, err := crypto.Hash("pw")
	if err != nil {
		t.Fatalf("crypto.Hash: %v", err)
	}
	err = credRepo.SetPassword(testCtx(t), household.NewMemberID(), "ghost@example.com", hash)
	if !errors.Is(err, household.ErrMemberNotFound) {
		t.Errorf("SetPassword(unknown member) error = %v, want ErrMemberNotFound", err)
	}
}
