package adapter_test

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"testing"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	"github.com/ericfisherdev/nestova/internal/platform/totp"
)

// newTestMFARepo returns an MFARepository (and the household repo + member id
// it seeds) backed by NESTOVA_TEST_DATABASE_URL, reusing newTestRepos' schema
// setup/teardown.
func newTestMFARepo(t *testing.T) (*authadapter.MFARepository, household.HouseholdID, household.MemberID) {
	t.Helper()
	_, hhRepo, pool := newTestRepos(t)
	memberID := seedMember(t, hhRepo)
	member, err := hhRepo.GetMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetMember: %v", err)
	}
	return authadapter.NewMFARepository(pool), member.HouseholdID, memberID
}

// mfaTestCipher returns a real AES-256-GCM cipher (the same construction
// production uses via crypto.NewCipher) keyed with a fixed 32-byte test key,
// for the gated tests that need actual encryption/decryption in the loop.
func mfaTestCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

// discardMFALogger returns a logger that writes nowhere, for constructing an
// MFAService in tests that don't assert on log output.
func discardMFALogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMFABeginEnrollment_PersistsUnconfirmedAndEncrypted(t *testing.T) {
	repo, householdID, memberID := newTestMFARepo(t)

	secretEnc := []byte("not-a-real-plaintext-secret-ciphertext-bytes")
	if err := repo.BeginEnrollment(testCtx(t), memberID, householdID, secretEnc); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}

	enrollment, err := repo.GetEnrollment(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetEnrollment: %v", err)
	}
	if enrollment.Confirmed() {
		t.Error("a fresh enrollment must not be confirmed")
	}
	if string(enrollment.TOTPSecretEnc) != string(secretEnc) {
		t.Error("stored ciphertext must round-trip exactly (the repository must not re-encode/mutate it)")
	}
	if enrollment.HouseholdID != householdID {
		t.Errorf("HouseholdID = %v, want %v", enrollment.HouseholdID, householdID)
	}
}

// TestMFABeginEnrollment_PersistsCiphertextBytesUnmodified is a plain
// persistence round-trip check: the repository has no knowledge of
// encryption (that is MFAService's job via its injected cipher — see
// TestMFAServiceBeginEnrollment_SecretNotStoredInPlaintext below for the
// actual AC4 plaintext-absence assertion, which needs the real cipher in the
// loop). This test only asserts the repository stores and returns opaque
// bytes byte-for-byte, with no re-encoding or mutation of its own.
func TestMFABeginEnrollment_PersistsCiphertextBytesUnmodified(t *testing.T) {
	repo, householdID, memberID := newTestMFARepo(t)
	secretEnc := []byte("opaque-ciphertext-bytes-the-repository-must-not-transform")

	if err := repo.BeginEnrollment(testCtx(t), memberID, householdID, secretEnc); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	enrollment, err := repo.GetEnrollment(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetEnrollment: %v", err)
	}
	if string(enrollment.TOTPSecretEnc) != string(secretEnc) {
		t.Error("the repository must persist exactly the ciphertext bytes it was given, with no additional transformation")
	}
}

// TestMFAServiceBeginEnrollment_SecretNotStoredInPlaintext is the real AC4
// gated check: it drives enrollment through authapp.MFAService with the
// SAME cipher construction production uses (crypto.NewCipher, AES-256-GCM),
// then reads the totp_secret_enc column directly (bypassing the
// repository's own Scan, which would just hand back the same opaque bytes)
// and asserts the raw secret MFAService generated is nowhere in those
// stored bytes, while still decrypting back to it exactly. The earlier
// repository-only test above cannot make this assertion: it never runs a
// real cipher, so a hypothetical regression that stored the secret in
// plaintext would not be caught by it.
func TestMFAServiceBeginEnrollment_SecretNotStoredInPlaintext(t *testing.T) {
	credRepo, hhRepo, pool := newTestRepos(t)
	memberID := seedMember(t, hhRepo)
	member, err := hhRepo.GetMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetMember: %v", err)
	}

	cipher := mfaTestCipher(t)
	svc, err := authapp.NewMFAService(authadapter.NewMFARepository(pool), cipher, totp.NewProvider(), credRepo, discardMFALogger())
	if err != nil {
		t.Fatalf("NewMFAService: %v", err)
	}

	rawSecret, _, err := svc.BeginEnrollment(testCtx(t), memberID, member.HouseholdID, "Alice")
	if err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if rawSecret == "" {
		t.Fatal("BeginEnrollment returned an empty secret")
	}

	var storedEnc []byte
	err = pool.QueryRow(testCtx(t), `SELECT totp_secret_enc FROM member_mfa WHERE member_id = $1`, memberID.String()).Scan(&storedEnc)
	if err != nil {
		t.Fatalf("query stored totp_secret_enc: %v", err)
	}

	if bytes.Contains(storedEnc, []byte(rawSecret)) {
		t.Errorf("stored totp_secret_enc contains the raw secret as a substring — it is not actually encrypted: %x", storedEnc)
	}
	if string(storedEnc) == rawSecret {
		t.Error("stored totp_secret_enc equals the raw secret exactly")
	}

	decrypted, err := cipher.Decrypt(storedEnc)
	if err != nil {
		t.Fatalf("Decrypt stored ciphertext: %v", err)
	}
	if string(decrypted) != rawSecret {
		t.Errorf("decrypted secret = %q, want the original %q", decrypted, rawSecret)
	}
}

func TestMFABeginEnrollment_ReplacesUnconfirmedInPlace(t *testing.T) {
	repo, householdID, memberID := newTestMFARepo(t)
	if err := repo.BeginEnrollment(testCtx(t), memberID, householdID, []byte("secret-1")); err != nil {
		t.Fatalf("first BeginEnrollment: %v", err)
	}
	if err := repo.BeginEnrollment(testCtx(t), memberID, householdID, []byte("secret-2")); err != nil {
		t.Fatalf("second BeginEnrollment (replace unconfirmed): %v", err)
	}

	enrollment, err := repo.GetEnrollment(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetEnrollment: %v", err)
	}
	if string(enrollment.TOTPSecretEnc) != "secret-2" {
		t.Errorf("TOTPSecretEnc = %q, want the replaced secret-2", enrollment.TOTPSecretEnc)
	}
}

func TestMFABeginEnrollment_AlreadyConfirmedRejected(t *testing.T) {
	repo, householdID, memberID := newTestMFARepo(t)
	if err := repo.BeginEnrollment(testCtx(t), memberID, householdID, []byte("secret-1")); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if err := repo.ConfirmEnrollment(testCtx(t), memberID); err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}

	err := repo.BeginEnrollment(testCtx(t), memberID, householdID, []byte("secret-2"))
	if !errors.Is(err, authdomain.ErrMFAAlreadyEnrolled) {
		t.Errorf("BeginEnrollment over a confirmed enrollment: err = %v, want ErrMFAAlreadyEnrolled", err)
	}
	// And the confirmed secret must be untouched.
	enrollment, err := repo.GetEnrollment(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetEnrollment: %v", err)
	}
	if string(enrollment.TOTPSecretEnc) != "secret-1" {
		t.Error("a rejected BeginEnrollment must not overwrite the confirmed secret")
	}
}

func TestMFABeginEnrollment_UnknownMemberInHousehold(t *testing.T) {
	repo, householdID, _ := newTestMFARepo(t)
	err := repo.BeginEnrollment(testCtx(t), household.NewMemberID(), householdID, []byte("secret"))
	if !errors.Is(err, household.ErrMemberNotFound) {
		t.Errorf("BeginEnrollment for an unknown member: err = %v, want ErrMemberNotFound", err)
	}
}

func TestMFAGetEnrollment_NotEnrolled(t *testing.T) {
	repo, _, memberID := newTestMFARepo(t)
	_, err := repo.GetEnrollment(testCtx(t), memberID)
	if !errors.Is(err, authdomain.ErrMFANotEnrolled) {
		t.Errorf("GetEnrollment(never enrolled) error = %v, want ErrMFANotEnrolled", err)
	}
}

func TestMFAConfirmEnrollment_NotEnrolled(t *testing.T) {
	repo, _, memberID := newTestMFARepo(t)
	if err := repo.ConfirmEnrollment(testCtx(t), memberID); !errors.Is(err, authdomain.ErrMFANotEnrolled) {
		t.Errorf("ConfirmEnrollment(never enrolled) error = %v, want ErrMFANotEnrolled", err)
	}
}

func TestMFADeleteEnrollment_CascadesRecoveryCodes(t *testing.T) {
	repo, householdID, memberID := newTestMFARepo(t)
	if err := repo.BeginEnrollment(testCtx(t), memberID, householdID, []byte("secret")); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if err := repo.ConfirmEnrollment(testCtx(t), memberID); err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}
	if err := repo.ReplaceRecoveryCodes(testCtx(t), memberID, []string{"hash-a", "hash-b", "hash-c"}); err != nil {
		t.Fatalf("ReplaceRecoveryCodes: %v", err)
	}

	if err := repo.DeleteEnrollment(testCtx(t), householdID, memberID); err != nil {
		t.Fatalf("DeleteEnrollment: %v", err)
	}

	if _, err := repo.GetEnrollment(testCtx(t), memberID); !errors.Is(err, authdomain.ErrMFANotEnrolled) {
		t.Errorf("GetEnrollment after delete: err = %v, want ErrMFANotEnrolled", err)
	}
	codes, err := repo.ListUnusedRecoveryCodes(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListUnusedRecoveryCodes after delete: %v", err)
	}
	if len(codes) != 0 {
		t.Errorf("recovery codes survived DeleteEnrollment: got %d, want 0 (should cascade)", len(codes))
	}
}

func TestMFADeleteEnrollment_WrongHouseholdRejected(t *testing.T) {
	// Defense-in-depth tenant check: deleting with a household id that does
	// not match the enrollment's own must fail, not silently succeed.
	repo, householdID, memberID := newTestMFARepo(t)
	if err := repo.BeginEnrollment(testCtx(t), memberID, householdID, []byte("secret")); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}

	err := repo.DeleteEnrollment(testCtx(t), household.NewHouseholdID(), memberID)
	if !errors.Is(err, authdomain.ErrMFANotEnrolled) {
		t.Errorf("DeleteEnrollment with a mismatched household: err = %v, want ErrMFANotEnrolled", err)
	}
	if _, err := repo.GetEnrollment(testCtx(t), memberID); err != nil {
		t.Errorf("the enrollment must survive a mismatched-household delete attempt, got: %v", err)
	}
}

func TestMFADeleteEnrollment_NotEnrolled(t *testing.T) {
	repo, householdID, memberID := newTestMFARepo(t)
	err := repo.DeleteEnrollment(testCtx(t), householdID, memberID)
	if !errors.Is(err, authdomain.ErrMFANotEnrolled) {
		t.Errorf("DeleteEnrollment(never enrolled) error = %v, want ErrMFANotEnrolled", err)
	}
}

func TestMFAReplaceRecoveryCodes_AtomicallyReplacesFullSet(t *testing.T) {
	repo, householdID, memberID := newTestMFARepo(t)
	if err := repo.BeginEnrollment(testCtx(t), memberID, householdID, []byte("secret")); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if err := repo.ConfirmEnrollment(testCtx(t), memberID); err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}

	if err := repo.ReplaceRecoveryCodes(testCtx(t), memberID, []string{"hash-1", "hash-2"}); err != nil {
		t.Fatalf("first ReplaceRecoveryCodes: %v", err)
	}
	if err := repo.ReplaceRecoveryCodes(testCtx(t), memberID, []string{"hash-3", "hash-4", "hash-5"}); err != nil {
		t.Fatalf("second ReplaceRecoveryCodes: %v", err)
	}

	codes, err := repo.ListUnusedRecoveryCodes(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListUnusedRecoveryCodes: %v", err)
	}
	if len(codes) != 3 {
		t.Fatalf("unused recovery codes = %d, want 3 (the second replace's set)", len(codes))
	}
	gotHashes := make(map[string]bool, len(codes))
	for _, c := range codes {
		gotHashes[c.CodeHash] = true
	}
	for _, want := range []string{"hash-3", "hash-4", "hash-5"} {
		if !gotHashes[want] {
			t.Errorf("missing expected recovery code hash %q after replace", want)
		}
	}
	for _, stale := range []string{"hash-1", "hash-2"} {
		if gotHashes[stale] {
			t.Errorf("stale recovery code hash %q survived ReplaceRecoveryCodes", stale)
		}
	}
}

func TestMFAMarkRecoveryCodeUsed_ExcludesFromUnusedList(t *testing.T) {
	repo, householdID, memberID := newTestMFARepo(t)
	if err := repo.BeginEnrollment(testCtx(t), memberID, householdID, []byte("secret")); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if err := repo.ConfirmEnrollment(testCtx(t), memberID); err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}
	if err := repo.ReplaceRecoveryCodes(testCtx(t), memberID, []string{"hash-1", "hash-2", "hash-3"}); err != nil {
		t.Fatalf("ReplaceRecoveryCodes: %v", err)
	}
	codes, err := repo.ListUnusedRecoveryCodes(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListUnusedRecoveryCodes: %v", err)
	}
	target := codes[0].ID

	if err := repo.MarkRecoveryCodeUsed(testCtx(t), target); err != nil {
		t.Fatalf("MarkRecoveryCodeUsed: %v", err)
	}

	remaining, err := repo.ListUnusedRecoveryCodes(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListUnusedRecoveryCodes after mark-used: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("unused recovery codes after marking one used = %d, want 2", len(remaining))
	}
	for _, c := range remaining {
		if c.ID == target {
			t.Error("a used recovery code must not appear in ListUnusedRecoveryCodes")
		}
	}

	// A used code cannot be marked used again by a second, independent call
	// (idempotency guard: the WHERE used_at IS NULL clause means a repeat
	// call affects zero rows and reports the failure rather than silently
	// succeeding).
	if err := repo.MarkRecoveryCodeUsed(testCtx(t), target); !errors.Is(err, authdomain.ErrRecoveryCodeInvalid) {
		t.Errorf("marking an already-used code used again: err = %v, want ErrRecoveryCodeInvalid", err)
	}
}

func TestMFAListUnusedRecoveryCodes_EmptyForNoEnrollment(t *testing.T) {
	repo, _, memberID := newTestMFARepo(t)
	codes, err := repo.ListUnusedRecoveryCodes(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListUnusedRecoveryCodes: %v", err)
	}
	if len(codes) != 0 {
		t.Errorf("got %d recovery codes for a member with no enrollment, want 0", len(codes))
	}
}
