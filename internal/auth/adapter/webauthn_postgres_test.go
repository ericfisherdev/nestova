package adapter_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// newTestWebAuthnCredentialRepo returns a WebAuthnCredentialRepository (and
// the household id + member id it seeds) backed by
// NESTOVA_TEST_DATABASE_URL, reusing newTestRepos' schema setup/teardown —
// mirroring newTestMFARepo's own pattern.
func newTestWebAuthnCredentialRepo(t *testing.T) (*authadapter.WebAuthnCredentialRepository, household.HouseholdID, household.MemberID) {
	t.Helper()
	_, hhRepo, pool := newTestRepos(t)
	memberID := seedMember(t, hhRepo)
	member, err := hhRepo.GetMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetMember: %v", err)
	}
	return authadapter.NewWebAuthnCredentialRepository(pool), member.HouseholdID, memberID
}

// testWebAuthnCredential builds a fully populated WebAuthnCredential for
// memberID, ready for Create.
func testWebAuthnCredential(memberID household.MemberID, credentialID []byte, nickname string) *authdomain.WebAuthnCredential {
	aaguid := uuid.Must(uuid.NewRandom())
	return &authdomain.WebAuthnCredential{
		ID:           authdomain.NewWebAuthnCredentialID(),
		MemberID:     memberID,
		CredentialID: credentialID,
		PublicKey:    []byte("not-a-real-cbor-public-key"),
		SignCount:    0,
		Transports:   []string{"internal", "hybrid"},
		AAGUID:       &aaguid,
		Nickname:     nickname,
		UserHandle:   []byte("a-derived-user-handle"),
	}
}

func TestWebAuthnCredentialCreate_PersistsAndListByMemberReturnsIt(t *testing.T) {
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	cred := testWebAuthnCredential(memberID, []byte("credential-id-1"), "My Phone")

	if err := repo.Create(testCtx(t), householdID, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}

	creds, err := repo.ListByMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("ListByMember returned %d credentials, want 1", len(creds))
	}
	got := creds[0]
	if got.ID != cred.ID {
		t.Errorf("ID = %v, want %v", got.ID, cred.ID)
	}
	if string(got.CredentialID) != string(cred.CredentialID) {
		t.Error("CredentialID did not round-trip exactly")
	}
	if string(got.PublicKey) != string(cred.PublicKey) {
		t.Error("PublicKey did not round-trip exactly")
	}
	if got.Nickname != "My Phone" {
		t.Errorf("Nickname = %q, want %q", got.Nickname, "My Phone")
	}
	if got.HouseholdID != householdID {
		t.Errorf("HouseholdID = %v, want %v", got.HouseholdID, householdID)
	}
	if got.AAGUID == nil || *got.AAGUID != *cred.AAGUID {
		t.Errorf("AAGUID = %v, want %v", got.AAGUID, cred.AAGUID)
	}
	if len(got.Transports) != 2 {
		t.Errorf("Transports = %v, want 2 entries", got.Transports)
	}
	if got.LastUsedAt != nil {
		t.Error("a freshly registered credential must have a nil LastUsedAt")
	}
}

func TestWebAuthnCredentialCreate_NilAAGUID_StoresNull(t *testing.T) {
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	cred := testWebAuthnCredential(memberID, []byte("credential-id-nil-aaguid"), "No AAGUID device")
	cred.AAGUID = nil

	if err := repo.Create(testCtx(t), householdID, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}
	creds, err := repo.ListByMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("ListByMember returned %d credentials, want 1", len(creds))
	}
	if creds[0].AAGUID != nil {
		t.Errorf("AAGUID = %v, want nil", creds[0].AAGUID)
	}
}

func TestWebAuthnCredentialCreate_UnknownMemberInHousehold(t *testing.T) {
	repo, householdID, _ := newTestWebAuthnCredentialRepo(t)
	cred := testWebAuthnCredential(household.NewMemberID(), []byte("credential-id-unknown-member"), "Ghost device")

	err := repo.Create(testCtx(t), householdID, cred)
	if !errors.Is(err, household.ErrMemberNotFound) {
		t.Errorf("Create for an unknown member: err = %v, want ErrMemberNotFound", err)
	}
}

// TestWebAuthnCredentialCreate_CrossHouseholdMemberRejected is the gated
// tenant-isolation check (mirroring
// TestMFABeginEnrollment_CrossHouseholdCannotTouchVictimRow's own pattern):
// unlike TestWebAuthnCredentialCreate_UnknownMemberInHousehold above, which
// uses a member id that does not exist AT ALL, this uses a REAL, existing
// member id — paired with a SECOND, GENUINELY EXISTING household that is
// not that member's own (seedHousehold, not a fabricated
// household.NewHouseholdID()). A fabricated household id would trip the
// table's plain household_id FK (member_credential_household_id_fkey)
// before ever reaching the composite FK below, which would prove nothing
// about tenant isolation specifically. A schema with only a plain member_id
// FK (no household_id component) would happily accept a REAL member paired
// with a REAL-but-wrong household; only the composite
// (household_id, member_id) FK member_credential_member_fk correctly
// rejects it, because that exact pair does not exist in member.
func TestWebAuthnCredentialCreate_CrossHouseholdMemberRejected(t *testing.T) {
	_, hhRepo, pool := newTestRepos(t)
	victimMemberID := seedMember(t, hhRepo)
	attackerHouseholdID := seedHousehold(t, hhRepo)
	repo := authadapter.NewWebAuthnCredentialRepository(pool)
	cred := testWebAuthnCredential(victimMemberID, []byte("credential-id-cross-household"), "Attacker-supplied")

	err := repo.Create(testCtx(t), attackerHouseholdID, cred)
	if !errors.Is(err, household.ErrMemberNotFound) {
		t.Errorf("Create for a real member under a real household that is not theirs: err = %v, want ErrMemberNotFound", err)
	}

	creds, err := repo.ListByMember(testCtx(t), victimMemberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 0 {
		t.Error("a rejected cross-household Create must not persist a credential under the victim member")
	}
}

// TestWebAuthnCredentialCreate_UnknownHouseholdRejected covers the OTHER
// half of member_credential's dual FK (see
// mapWebAuthnCredentialFKViolation): a householdID that does not exist AT
// ALL must trip the plain household_id FK
// (member_credential_household_id_fkey) and map to
// household.ErrHouseholdNotFound, distinctly from the composite member FK's
// household.ErrMemberNotFound above.
func TestWebAuthnCredentialCreate_UnknownHouseholdRejected(t *testing.T) {
	repo, _, memberID := newTestWebAuthnCredentialRepo(t)
	cred := testWebAuthnCredential(memberID, []byte("credential-id-unknown-household"), "Orphan device")

	err := repo.Create(testCtx(t), household.NewHouseholdID(), cred)
	if !errors.Is(err, household.ErrHouseholdNotFound) {
		t.Errorf("Create for an unknown household: err = %v, want ErrHouseholdNotFound", err)
	}
}

func TestWebAuthnCredentialListByMember_EmptyForNoCredentials(t *testing.T) {
	repo, _, memberID := newTestWebAuthnCredentialRepo(t)
	creds, err := repo.ListByMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 0 {
		t.Errorf("got %d credentials for a member with none, want 0", len(creds))
	}
}

// TestWebAuthnCredentialListByMember_MultipleCredentials_OldestFirst
// asserts membership (both created credentials come back) AND the
// repository's actual documented ordering contract (ORDER BY created_at,
// id — see ListByMember's own doc) directly, rather than assuming the two
// Create calls' real-world timing happens to land them in array-index
// order: created_at ties are a real possibility (e.g. two creates inside
// the same transaction, or a database with coarser-than-microsecond clock
// resolution), and when they occur, id — not insertion order — is what
// actually decides the result, since both ids are independently random
// UUIDv7 values with no guaranteed relationship to call order beyond their
// own generation timestamps.
func TestWebAuthnCredentialListByMember_MultipleCredentials_OldestFirst(t *testing.T) {
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	first := testWebAuthnCredential(memberID, []byte("credential-id-first"), "First device")
	if err := repo.Create(testCtx(t), householdID, first); err != nil {
		t.Fatalf("Create (first): %v", err)
	}
	second := testWebAuthnCredential(memberID, []byte("credential-id-second"), "Second device")
	if err := repo.Create(testCtx(t), householdID, second); err != nil {
		t.Fatalf("Create (second): %v", err)
	}

	creds, err := repo.ListByMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 2 {
		t.Fatalf("ListByMember returned %d credentials, want 2", len(creds))
	}

	// Membership: both created credentials are present, in either order.
	returned := map[authdomain.WebAuthnCredentialID]bool{creds[0].ID: true, creds[1].ID: true}
	if !returned[first.ID] || !returned[second.ID] {
		t.Fatalf("ListByMember did not return both created credentials: got %v and %v, want %v and %v",
			creds[0].ID, creds[1].ID, first.ID, second.ID)
	}

	// Ordering: creds[0] must sort <= creds[1] by (created_at, id) — the
	// repository's own contract — not merely "whichever happened first".
	switch {
	case creds[0].CreatedAt.After(creds[1].CreatedAt):
		t.Errorf("ListByMember returned a later created_at before an earlier one: %v then %v", creds[0].CreatedAt, creds[1].CreatedAt)
	case creds[0].CreatedAt.Equal(creds[1].CreatedAt):
		idA, idB := uuid.UUID(creds[0].ID), uuid.UUID(creds[1].ID)
		if bytes.Compare(idA[:], idB[:]) > 0 {
			t.Errorf("ListByMember did not break a created_at tie by ascending id: %v then %v", creds[0].ID, creds[1].ID)
		}
	}
}

func TestWebAuthnCredentialRename_UpdatesNickname(t *testing.T) {
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	cred := testWebAuthnCredential(memberID, []byte("credential-id-rename"), "Old Name")
	if err := repo.Create(testCtx(t), householdID, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := repo.Rename(testCtx(t), householdID, memberID, cred.ID, "New Name"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	creds, err := repo.ListByMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 1 || creds[0].Nickname != "New Name" {
		t.Errorf("Nickname after rename = %+v, want New Name", creds)
	}
}

func TestWebAuthnCredentialRename_WrongMemberRejected(t *testing.T) {
	// Defense-in-depth tenant check: renaming with a member id that does
	// not own the credential must fail, not silently succeed.
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	cred := testWebAuthnCredential(memberID, []byte("credential-id-wrong-member"), "Victim device")
	if err := repo.Create(testCtx(t), householdID, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := repo.Rename(testCtx(t), householdID, household.NewMemberID(), cred.ID, "Hijacked")
	if !errors.Is(err, authdomain.ErrWebAuthnCredentialNotFound) {
		t.Errorf("Rename with the wrong member: err = %v, want ErrWebAuthnCredentialNotFound", err)
	}
	creds, err := repo.ListByMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 1 || creds[0].Nickname != "Victim device" {
		t.Error("a rejected cross-member rename must not change the victim's nickname")
	}
}

func TestWebAuthnCredentialRename_WrongHouseholdRejected(t *testing.T) {
	// The other scoping axis for Rename (mirrors
	// TestWebAuthnCredentialDelete_WrongHouseholdRejected): a household id
	// that does not match the credential's own must fail, even though
	// memberID and id are both correct.
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	cred := testWebAuthnCredential(memberID, []byte("credential-id-rename-wrong-household"), "Victim device")
	if err := repo.Create(testCtx(t), householdID, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := repo.Rename(testCtx(t), household.NewHouseholdID(), memberID, cred.ID, "Hijacked")
	if !errors.Is(err, authdomain.ErrWebAuthnCredentialNotFound) {
		t.Errorf("Rename with a mismatched household: err = %v, want ErrWebAuthnCredentialNotFound", err)
	}
	creds, err := repo.ListByMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 1 || creds[0].Nickname != "Victim device" {
		t.Error("a rejected mismatched-household rename must not change the victim's nickname")
	}
}

func TestWebAuthnCredentialRename_NotFound(t *testing.T) {
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	err := repo.Rename(testCtx(t), householdID, memberID, authdomain.NewWebAuthnCredentialID(), "x")
	if !errors.Is(err, authdomain.ErrWebAuthnCredentialNotFound) {
		t.Errorf("Rename(never created): err = %v, want ErrWebAuthnCredentialNotFound", err)
	}
}

func TestWebAuthnCredentialDelete_RemovesImmediately(t *testing.T) {
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	cred := testWebAuthnCredential(memberID, []byte("credential-id-delete"), "Doomed device")
	if err := repo.Create(testCtx(t), householdID, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := repo.Delete(testCtx(t), householdID, memberID, cred.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	creds, err := repo.ListByMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 0 {
		t.Errorf("credentials after delete = %d, want 0", len(creds))
	}
}

func TestWebAuthnCredentialDelete_WrongMemberRejected(t *testing.T) {
	// The other scoping axis for Delete (mirrors
	// TestWebAuthnCredentialRename_WrongMemberRejected): a member id that
	// does not own the credential must fail, even though householdID and id
	// are both correct.
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	cred := testWebAuthnCredential(memberID, []byte("credential-id-delete-wrong-member"), "Victim device")
	if err := repo.Create(testCtx(t), householdID, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := repo.Delete(testCtx(t), householdID, household.NewMemberID(), cred.ID)
	if !errors.Is(err, authdomain.ErrWebAuthnCredentialNotFound) {
		t.Errorf("Delete with a mismatched member: err = %v, want ErrWebAuthnCredentialNotFound", err)
	}
	creds, err := repo.ListByMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 1 {
		t.Error("the credential must survive a mismatched-member delete attempt")
	}
}

func TestWebAuthnCredentialDelete_WrongHouseholdRejected(t *testing.T) {
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	cred := testWebAuthnCredential(memberID, []byte("credential-id-wrong-household"), "Device")
	if err := repo.Create(testCtx(t), householdID, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := repo.Delete(testCtx(t), household.NewHouseholdID(), memberID, cred.ID)
	if !errors.Is(err, authdomain.ErrWebAuthnCredentialNotFound) {
		t.Errorf("Delete with a mismatched household: err = %v, want ErrWebAuthnCredentialNotFound", err)
	}
	if creds, err := repo.ListByMember(testCtx(t), memberID); err != nil || len(creds) != 1 {
		t.Error("the credential must survive a mismatched-household delete attempt")
	}
}

func TestWebAuthnCredentialDelete_NotFound(t *testing.T) {
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	err := repo.Delete(testCtx(t), householdID, memberID, authdomain.NewWebAuthnCredentialID())
	if !errors.Is(err, authdomain.ErrWebAuthnCredentialNotFound) {
		t.Errorf("Delete(never created): err = %v, want ErrWebAuthnCredentialNotFound", err)
	}
}

func TestWebAuthnCredentialCreate_DuplicateCredentialIDRejected(t *testing.T) {
	// Defense-in-depth: credential_id is UNIQUE — a second Create for the
	// SAME raw WebAuthn credential id must fail rather than silently
	// duplicate the row (see the migration's own doc comment for why this
	// is defense-in-depth, not the primary replay guard).
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	credentialID := []byte("credential-id-duplicate")
	first := testWebAuthnCredential(memberID, credentialID, "First")
	if err := repo.Create(testCtx(t), householdID, first); err != nil {
		t.Fatalf("Create (first): %v", err)
	}

	second := testWebAuthnCredential(memberID, credentialID, "Second")
	err := repo.Create(testCtx(t), householdID, second)
	if err == nil {
		t.Fatal("Create with a duplicate credential_id must fail")
	}
	if errors.Is(err, household.ErrMemberNotFound) {
		t.Errorf("duplicate credential_id was misreported as ErrMemberNotFound: %v", err)
	}
}

// ---------------------------------------------------------------------------
// FindByUserHandle / UpdateAfterAssertion (NES-137)
// ---------------------------------------------------------------------------

// testWebAuthnCredentialWithHandle is testWebAuthnCredential plus an
// explicit UserHandle — unlike testWebAuthnCredential's own fixed
// "a-derived-user-handle" (fine for tests that only ever seed ONE member),
// FindByUserHandle's own tests need per-member, per-test-controlled handles
// to actually exercise handle-scoped lookup.
func testWebAuthnCredentialWithHandle(memberID household.MemberID, credentialID, userHandle []byte, nickname string) *authdomain.WebAuthnCredential {
	cred := testWebAuthnCredential(memberID, credentialID, nickname)
	cred.UserHandle = userHandle
	return cred
}

func TestWebAuthnCredentialFindByUserHandle_ReturnsMemberAndAllTheirCredentials(t *testing.T) {
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	handle := []byte("member-handle-multi-cred")
	first := testWebAuthnCredentialWithHandle(memberID, []byte("credential-id-handle-first"), handle, "Phone")
	if err := repo.Create(testCtx(t), householdID, first); err != nil {
		t.Fatalf("Create (first): %v", err)
	}
	second := testWebAuthnCredentialWithHandle(memberID, []byte("credential-id-handle-second"), handle, "Laptop")
	if err := repo.Create(testCtx(t), householdID, second); err != nil {
		t.Fatalf("Create (second): %v", err)
	}

	gotMemberID, creds, err := repo.FindByUserHandle(testCtx(t), handle)
	if err != nil {
		t.Fatalf("FindByUserHandle: %v", err)
	}
	if gotMemberID != memberID {
		t.Errorf("FindByUserHandle member = %v, want %v", gotMemberID, memberID)
	}
	if len(creds) != 2 {
		t.Fatalf("FindByUserHandle returned %d credentials, want 2", len(creds))
	}
	returned := map[authdomain.WebAuthnCredentialID]bool{creds[0].ID: true, creds[1].ID: true}
	if !returned[first.ID] || !returned[second.ID] {
		t.Errorf("FindByUserHandle did not return both credentials: got %v and %v", creds[0].ID, creds[1].ID)
	}
	for _, c := range creds {
		if c.MemberID != memberID {
			t.Errorf("credential %v MemberID = %v, want %v", c.ID, c.MemberID, memberID)
		}
		if c.HouseholdID != householdID {
			t.Errorf("credential %v HouseholdID = %v, want %v", c.ID, c.HouseholdID, householdID)
		}
	}
}

func TestWebAuthnCredentialFindByUserHandle_UnknownHandleRejected(t *testing.T) {
	repo, _, _ := newTestWebAuthnCredentialRepo(t)

	_, creds, err := repo.FindByUserHandle(testCtx(t), []byte("nobody-registered-this-handle"))
	if !errors.Is(err, household.ErrMemberNotFound) {
		t.Errorf("FindByUserHandle(unknown handle): err = %v, want ErrMemberNotFound", err)
	}
	if len(creds) != 0 {
		t.Errorf("FindByUserHandle(unknown handle) returned %d credentials, want 0", len(creds))
	}
}

// TestWebAuthnCredentialFindByUserHandle_DoesNotReturnOtherMembersCredentials
// is the tenant-isolation check for the login lookup itself: a member's
// handle must never resolve another member's credentials, even when both
// members are seeded in the SAME test run (member_credential_user_handle_idx
// is a plain, non-unique index — the WHERE clause, not the index alone, is
// what must scope correctly).
func TestWebAuthnCredentialFindByUserHandle_DoesNotReturnOtherMembersCredentials(t *testing.T) {
	// A SINGLE newTestRepos call: newTestRepos resets and re-migrates the
	// schema on every call (see its own doc), so calling it a second time
	// (as an earlier version of this test did, once directly and once
	// indirectly via newTestWebAuthnCredentialRepo) wipes out whatever the
	// FIRST call already seeded before the second seed ever runs.
	_, hhRepo, pool := newTestRepos(t)
	repo := authadapter.NewWebAuthnCredentialRepository(pool)

	victimID := seedMember(t, hhRepo)
	victimMember, err := hhRepo.GetMember(testCtx(t), victimID)
	if err != nil {
		t.Fatalf("GetMember (victim): %v", err)
	}
	attackerID := seedMember(t, hhRepo)
	attackerMember, err := hhRepo.GetMember(testCtx(t), attackerID)
	if err != nil {
		t.Fatalf("GetMember (attacker): %v", err)
	}

	victimHandle := []byte("victim-handle")
	attackerHandle := []byte("attacker-handle")
	victimCred := testWebAuthnCredentialWithHandle(victimID, []byte("credential-id-victim"), victimHandle, "Victim's phone")
	if err := repo.Create(testCtx(t), victimMember.HouseholdID, victimCred); err != nil {
		t.Fatalf("Create (victim): %v", err)
	}
	attackerCred := testWebAuthnCredentialWithHandle(attackerID, []byte("credential-id-attacker"), attackerHandle, "Attacker's phone")
	if err := repo.Create(testCtx(t), attackerMember.HouseholdID, attackerCred); err != nil {
		t.Fatalf("Create (attacker): %v", err)
	}

	gotMemberID, creds, err := repo.FindByUserHandle(testCtx(t), victimHandle)
	if err != nil {
		t.Fatalf("FindByUserHandle: %v", err)
	}
	if gotMemberID != victimID {
		t.Errorf("FindByUserHandle(victim's handle) resolved member = %v, want %v", gotMemberID, victimID)
	}
	if len(creds) != 1 || creds[0].ID != victimCred.ID {
		t.Errorf("FindByUserHandle(victim's handle) returned %+v, want only the victim's own credential", creds)
	}
}

func TestWebAuthnCredentialUpdateAfterAssertion_UpdatesSignCountAndLastUsedAt(t *testing.T) {
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	credentialID := []byte("credential-id-update-assertion")
	cred := testWebAuthnCredential(memberID, credentialID, "Device")
	if err := repo.Create(testCtx(t), householdID, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}

	usedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.UpdateAfterAssertion(testCtx(t), credentialID, 7, usedAt); err != nil {
		t.Fatalf("UpdateAfterAssertion: %v", err)
	}

	creds, err := repo.ListByMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("ListByMember returned %d credentials, want 1", len(creds))
	}
	got := creds[0]
	if got.SignCount != 7 {
		t.Errorf("SignCount = %d, want 7", got.SignCount)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(usedAt) {
		t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, usedAt)
	}
}

// TestWebAuthnCredentialUpdateAfterAssertion_OutOfOrderDoesNotRegressState
// is the concurrency regression this method's monotonic guard exists for
// (NES-137 round 3): two assertions on the SAME credential can complete out
// of real-time order — a slower request, carrying an OLDER usedAt, can
// still reach this method AFTER a faster, more-recently-issued assertion
// already won. The later (older) call must not overwrite the fresher
// state already on file, and must not error either — the assertion it
// carries was already cryptographically verified before this call ever
// runs (see the method's own doc for why losing this race is not an
// error).
func TestWebAuthnCredentialUpdateAfterAssertion_OutOfOrderDoesNotRegressState(t *testing.T) {
	repo, householdID, memberID := newTestWebAuthnCredentialRepo(t)
	credentialID := []byte("credential-id-out-of-order")
	cred := testWebAuthnCredential(memberID, credentialID, "Device")
	if err := repo.Create(testCtx(t), householdID, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}

	older := time.Now().UTC().Truncate(time.Microsecond)
	newer := older.Add(time.Second)

	// The FASTER, more-recently-issued assertion (newer usedAt, higher
	// count) completes and writes FIRST.
	if err := repo.UpdateAfterAssertion(testCtx(t), credentialID, 10, newer); err != nil {
		t.Fatalf("UpdateAfterAssertion (newer, first write): %v", err)
	}

	// The SLOWER assertion — an OLDER usedAt, lower count — reaches this
	// method SECOND (out of real-time order). It must be accepted (no
	// error) but must NOT overwrite the fresher state already on file.
	if err := repo.UpdateAfterAssertion(testCtx(t), credentialID, 5, older); err != nil {
		t.Fatalf("UpdateAfterAssertion (older, second write): %v", err)
	}

	creds, err := repo.ListByMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("ListByMember returned %d credentials, want 1", len(creds))
	}
	got := creds[0]
	if got.SignCount != 10 {
		t.Errorf("SignCount after an out-of-order older write = %d, want 10 (the newer write must win)", got.SignCount)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(newer) {
		t.Errorf("LastUsedAt after an out-of-order older write = %v, want %v (the newer write must win)", got.LastUsedAt, newer)
	}
}

func TestWebAuthnCredentialUpdateAfterAssertion_NotFound(t *testing.T) {
	repo, _, _ := newTestWebAuthnCredentialRepo(t)
	err := repo.UpdateAfterAssertion(testCtx(t), []byte("never-registered-credential-id"), 1, time.Now())
	if !errors.Is(err, authdomain.ErrWebAuthnCredentialNotFound) {
		t.Errorf("UpdateAfterAssertion(unknown credential id): err = %v, want ErrWebAuthnCredentialNotFound", err)
	}
}
