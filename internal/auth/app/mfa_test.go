package app_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/crypto/cryptotest"

	"github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeMFARepo is an in-memory authdomain.MFARepository.
type fakeMFARepo struct {
	enrollments map[household.MemberID]*authdomain.MFAEnrollment
	codes       map[household.MemberID][]authdomain.RecoveryCode
	nextCodeID  int
}

func newFakeMFARepo() *fakeMFARepo {
	return &fakeMFARepo{
		enrollments: make(map[household.MemberID]*authdomain.MFAEnrollment),
		codes:       make(map[household.MemberID][]authdomain.RecoveryCode),
	}
}

func (f *fakeMFARepo) GetEnrollment(_ context.Context, memberID household.MemberID) (*authdomain.MFAEnrollment, error) {
	e, ok := f.enrollments[memberID]
	if !ok {
		return nil, authdomain.ErrMFANotEnrolled
	}
	cp := *e
	return &cp, nil
}

// BeginEnrollment mirrors the real repository's household guard (NES-134
// CodeRabbit round 3, finding 3): an existing row belonging to a DIFFERENT
// household than householdID must never be touched.
func (f *fakeMFARepo) BeginEnrollment(_ context.Context, memberID household.MemberID, householdID household.HouseholdID, secretEnc []byte) error {
	if existing, ok := f.enrollments[memberID]; ok {
		if existing.HouseholdID != householdID {
			return household.ErrMemberNotFound
		}
		if existing.Confirmed() {
			return authdomain.ErrMFAAlreadyEnrolled
		}
	}
	f.enrollments[memberID] = &authdomain.MFAEnrollment{MemberID: memberID, HouseholdID: householdID, TOTPSecretEnc: secretEnc}
	return nil
}

// ConfirmEnrollmentWithCodes mirrors the real repository's atomic
// confirm+store-codes contract (NES-134 CodeRabbit round 3, finding 4): both
// happen together, or neither does.
func (f *fakeMFARepo) ConfirmEnrollmentWithCodes(_ context.Context, memberID household.MemberID, hashes []string) error {
	e, ok := f.enrollments[memberID]
	if !ok {
		return authdomain.ErrMFANotEnrolled
	}
	if e.Confirmed() {
		return authdomain.ErrMFAAlreadyEnrolled
	}
	now := time.Now()
	e.ConfirmedAt = &now

	codes := make([]authdomain.RecoveryCode, 0, len(hashes))
	for _, h := range hashes {
		f.nextCodeID++
		codes = append(codes, authdomain.RecoveryCode{
			ID:       recoveryCodeIDFromInt(f.nextCodeID),
			MemberID: memberID,
			CodeHash: h,
		})
	}
	f.codes[memberID] = codes
	return nil
}

func (f *fakeMFARepo) DeleteEnrollment(_ context.Context, householdID household.HouseholdID, memberID household.MemberID) error {
	e, ok := f.enrollments[memberID]
	if !ok || e.HouseholdID != householdID {
		return authdomain.ErrMFANotEnrolled
	}
	delete(f.enrollments, memberID)
	delete(f.codes, memberID)
	return nil
}

func (f *fakeMFARepo) ReplaceRecoveryCodes(_ context.Context, memberID household.MemberID, hashes []string) error {
	codes := make([]authdomain.RecoveryCode, 0, len(hashes))
	for _, h := range hashes {
		f.nextCodeID++
		codes = append(codes, authdomain.RecoveryCode{
			ID:       recoveryCodeIDFromInt(f.nextCodeID),
			MemberID: memberID,
			CodeHash: h,
		})
	}
	f.codes[memberID] = codes
	return nil
}

func (f *fakeMFARepo) ListUnusedRecoveryCodes(_ context.Context, memberID household.MemberID) ([]authdomain.RecoveryCode, error) {
	var out []authdomain.RecoveryCode
	for _, c := range f.codes[memberID] {
		if !c.Used() {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeMFARepo) MarkRecoveryCodeUsed(_ context.Context, codeID authdomain.RecoveryCodeID) error {
	for memberID, codes := range f.codes {
		for i := range codes {
			if codes[i].ID == codeID {
				now := time.Now()
				codes[i].UsedAt = &now
				f.codes[memberID] = codes
				return nil
			}
		}
	}
	return errors.New("recovery code not found")
}

// RecordLoginStep mirrors the real repository's atomic replay guard: it
// only accepts step when the stored LastTOTPStep is nil or strictly less
// than step (NES-135).
func (f *fakeMFARepo) RecordLoginStep(_ context.Context, memberID household.MemberID, step int64) error {
	e, ok := f.enrollments[memberID]
	if !ok {
		return authdomain.ErrInvalidTOTPCode
	}
	if e.LastTOTPStep != nil && step <= *e.LastTOTPStep {
		return authdomain.ErrInvalidTOTPCode
	}
	e.LastTOTPStep = &step
	return nil
}

var _ authdomain.MFARepository = (*fakeMFARepo)(nil)

// recoveryCodeIDFromInt derives a deterministic RecoveryCodeID from a small
// int for fixture bookkeeping (a real id is a UUIDv7; tests only need
// distinct, comparable ids).
func recoveryCodeIDFromInt(n int) authdomain.RecoveryCodeID {
	id := authdomain.NewRecoveryCodeID()
	id[0] = byte(n)
	return id
}

// fakeTOTPProvider is a controllable totpProvider fake: GenerateSecret always
// returns a fixed secret/URL pair (recording the issuer/accountName it was
// called with), and Validate reports true only for the configured validCode
// against the configured expectedSecret — so tests can simulate "member
// enters the right code" or "member enters the wrong code" without any real
// clock-synchronized TOTP math. MatchStep is separately controllable via
// loginCode/loginStep (NES-135): tests configure which code maps to which
// RFC 6238 step without needing real clock-synchronized math either.
type fakeTOTPProvider struct {
	secret         string
	otpauthURL     string
	validCode      string
	lastIssuer     string
	lastAccount    string
	lastValidateAt string // last secret Validate was called with

	// loginCode/loginStep configure MatchStep: it reports (loginStep, true)
	// when code == loginCode and secret == the fixture's secret; otherwise
	// (0, false). Both zero by default, so a test that never sets them gets
	// a clean "wrong code" baseline (mirroring validCode's own convention).
	loginCode string
	loginStep int64
}

func (f *fakeTOTPProvider) GenerateSecret(issuer, accountName string) (string, string, error) {
	f.lastIssuer = issuer
	f.lastAccount = accountName
	return f.secret, f.otpauthURL, nil
}

func (f *fakeTOTPProvider) Validate(code, secret string) bool {
	f.lastValidateAt = secret
	return code == f.validCode && secret == f.secret
}

func (f *fakeTOTPProvider) MatchStep(code, secret string) (int64, bool) {
	if code == f.loginCode && secret == f.secret {
		return f.loginStep, true
	}
	return 0, false
}

// fakePasswordVerifier is a controllable passwordVerifier fake for the owner
// re-auth flow.
type fakePasswordVerifier struct {
	credentials map[household.MemberID]*authdomain.Credential
}

func (f *fakePasswordVerifier) FindByMemberID(_ context.Context, memberID household.MemberID) (*authdomain.Credential, error) {
	c, ok := f.credentials[memberID]
	if !ok {
		return nil, authdomain.ErrInvalidCredentials
	}
	return c, nil
}

// fakeMemberLookup is a controllable memberLookup fake: ResetMemberMFA
// resolves the acting owner's (and target's) role/household through this,
// never trusting a caller-supplied claim (NES-134 CodeRabbit round 3,
// finding 5).
type fakeMemberLookup struct {
	members map[household.MemberID]*household.Member
}

func newFakeMemberLookup() *fakeMemberLookup {
	return &fakeMemberLookup{members: make(map[household.MemberID]*household.Member)}
}

func (f *fakeMemberLookup) seed(m *household.Member) { f.members[m.ID] = m }

func (f *fakeMemberLookup) GetMember(_ context.Context, id household.MemberID) (*household.Member, error) {
	m, ok := f.members[id]
	if !ok {
		return nil, household.ErrMemberNotFound
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

func discardLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func testCipher(t *testing.T) *crypto.Cipher {
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

// mfaFixture bundles an MFAService with its fully controllable fakes, so
// tests can both exercise the service and assert directly against its
// dependencies' state.
type mfaFixture struct {
	svc       *app.MFAService
	repo      *fakeMFARepo
	totp      *fakeTOTPProvider
	passwords *fakePasswordVerifier
	members   *fakeMemberLookup
	logs      *bytes.Buffer
}

// newMFAFixture wires an MFAService with fully controllable fakes.
func newMFAFixture(t *testing.T) *mfaFixture {
	t.Helper()
	repo := newFakeMFARepo()
	totpFake := &fakeTOTPProvider{secret: "JBSWY3DPEHPK3PXP", otpauthURL: "otpauth://totp/Nestova:alice?secret=JBSWY3DPEHPK3PXP&issuer=Nestova", validCode: "123456"}
	passwords := &fakePasswordVerifier{credentials: make(map[household.MemberID]*authdomain.Credential)}
	members := newFakeMemberLookup()
	logger, buf := discardLogger()
	svc, err := app.NewMFAService(repo, testCipher(t), totpFake, passwords, members, cryptotest.Hasher(), logger)
	if err != nil {
		t.Fatalf("NewMFAService: %v", err)
	}
	return &mfaFixture{svc: svc, repo: repo, totp: totpFake, passwords: passwords, members: members, logs: buf}
}

// ---------------------------------------------------------------------------
// AC1: enroll → confirm; invalid code rejected; unconfirmed = not active
// ---------------------------------------------------------------------------

func TestBeginEnrollment_GeneratesAndPersistsUnconfirmedSecret(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()

	secret, otpauthURL, err := f.svc.BeginEnrollment(context.Background(), memberID, householdID, "Alice")
	if err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if secret != f.totp.secret || otpauthURL != f.totp.otpauthURL {
		t.Errorf("BeginEnrollment returned (%q, %q), want the generated (%q, %q)", secret, otpauthURL, f.totp.secret, f.totp.otpauthURL)
	}
	if f.totp.lastAccount != "Alice" || f.totp.lastIssuer != "Nestova" {
		t.Errorf("GenerateSecret called with issuer=%q accountName=%q, want issuer=Nestova accountName=Alice", f.totp.lastIssuer, f.totp.lastAccount)
	}

	enrollment, err := f.repo.GetEnrollment(context.Background(), memberID)
	if err != nil {
		t.Fatalf("GetEnrollment after BeginEnrollment: %v", err)
	}
	if enrollment.Confirmed() {
		t.Error("a fresh enrollment must not be confirmed")
	}
	if string(enrollment.TOTPSecretEnc) == secret {
		t.Error("the stored secret must be encrypted, not the raw secret")
	}

	status, err := f.svc.Status(context.Background(), memberID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Confirmed() {
		t.Error("Status must report an unconfirmed enrollment as not confirmed")
	}
}

func TestBeginEnrollment_ReplacesUnconfirmedEnrollment(t *testing.T) {
	// AC5: unconfirmed enrollments never lock anyone out — re-enrolling
	// before confirming simply replaces the still-unconfirmed row.
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()

	if _, _, err := f.svc.BeginEnrollment(context.Background(), memberID, householdID, "Alice"); err != nil {
		t.Fatalf("first BeginEnrollment: %v", err)
	}
	f.totp.secret = "ANOTHERSECRETVALUE"
	if _, _, err := f.svc.BeginEnrollment(context.Background(), memberID, householdID, "Alice"); err != nil {
		t.Fatalf("second BeginEnrollment (re-enroll over unconfirmed): %v", err)
	}

	enrollment, err := f.repo.GetEnrollment(context.Background(), memberID)
	if err != nil {
		t.Fatalf("GetEnrollment: %v", err)
	}
	if enrollment.Confirmed() {
		t.Error("a replaced enrollment must still be unconfirmed")
	}
}

func TestBeginEnrollment_AlreadyConfirmed_ReturnsErrMFAAlreadyEnrolled(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	confirmEnrollment(t, f.svc, memberID, householdID)

	if _, _, err := f.svc.BeginEnrollment(context.Background(), memberID, householdID, "Alice"); !errors.Is(err, authdomain.ErrMFAAlreadyEnrolled) {
		t.Errorf("BeginEnrollment over a confirmed enrollment: err = %v, want ErrMFAAlreadyEnrolled", err)
	}
}

func TestBeginEnrollment_CrossHouseholdRejected(t *testing.T) {
	// Defense-in-depth tenant guard (NES-134 CodeRabbit round 3, finding
	// 3): BeginEnrollment must never overwrite a row belonging to a
	// DIFFERENT household than the caller supplied.
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	victimHousehold := household.NewHouseholdID()
	attackerHousehold := household.NewHouseholdID()

	if _, _, err := f.svc.BeginEnrollment(context.Background(), memberID, victimHousehold, "Alice"); err != nil {
		t.Fatalf("seed BeginEnrollment: %v", err)
	}
	_, _, err := f.svc.BeginEnrollment(context.Background(), memberID, attackerHousehold, "Attacker")
	if !errors.Is(err, household.ErrMemberNotFound) {
		t.Errorf("cross-household BeginEnrollment: err = %v, want ErrMemberNotFound", err)
	}

	enrollment, err := f.repo.GetEnrollment(context.Background(), memberID)
	if err != nil {
		t.Fatalf("GetEnrollment: %v", err)
	}
	if enrollment.HouseholdID != victimHousehold {
		t.Error("a cross-household BeginEnrollment attempt must not change the victim's household_id")
	}
}

func TestConfirmEnrollment_WrongCodeRejected_EnrollmentStaysUnconfirmed(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	if _, _, err := f.svc.BeginEnrollment(context.Background(), memberID, householdID, "Alice"); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}

	_, err := f.svc.ConfirmEnrollment(context.Background(), memberID, "000000")
	if !errors.Is(err, authdomain.ErrInvalidTOTPCode) {
		t.Fatalf("ConfirmEnrollment(wrong code): err = %v, want ErrInvalidTOTPCode", err)
	}

	enrollment, err := f.repo.GetEnrollment(context.Background(), memberID)
	if err != nil {
		t.Fatalf("GetEnrollment: %v", err)
	}
	if enrollment.Confirmed() {
		t.Error("a wrong code must not confirm the enrollment")
	}
}

func TestConfirmEnrollment_NotEnrolled(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	_, err := f.svc.ConfirmEnrollment(context.Background(), household.NewMemberID(), "123456")
	if !errors.Is(err, authdomain.ErrMFANotEnrolled) {
		t.Errorf("ConfirmEnrollment with no enrollment: err = %v, want ErrMFANotEnrolled", err)
	}
}

func TestConfirmEnrollment_ValidCode_ActivatesAndReturnsTenRecoveryCodes(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	if _, _, err := f.svc.BeginEnrollment(context.Background(), memberID, householdID, "Alice"); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}

	codes, err := f.svc.ConfirmEnrollment(context.Background(), memberID, "123456")
	if err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}
	if len(codes) != 10 {
		t.Fatalf("ConfirmEnrollment returned %d recovery codes, want 10", len(codes))
	}
	seen := make(map[string]bool, len(codes))
	for _, c := range codes {
		if seen[c] {
			t.Errorf("duplicate recovery code returned: %q", c)
		}
		seen[c] = true
	}

	enrollment, err := f.repo.GetEnrollment(context.Background(), memberID)
	if err != nil {
		t.Fatalf("GetEnrollment: %v", err)
	}
	if !enrollment.Confirmed() {
		t.Error("a valid code must confirm the enrollment")
	}

	unused, err := f.repo.ListUnusedRecoveryCodes(context.Background(), memberID)
	if err != nil {
		t.Fatalf("ListUnusedRecoveryCodes: %v", err)
	}
	if len(unused) != 10 {
		t.Fatalf("stored %d unused recovery codes, want 10", len(unused))
	}
	for _, c := range unused {
		if c.CodeHash == codes[0] || !strings.HasPrefix(c.CodeHash, "$argon2id$") {
			t.Errorf("recovery code hash %q does not look like an argon2id PHC string (or stores the raw code)", c.CodeHash)
		}
	}
}

func TestConfirmEnrollment_AlreadyConfirmed(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	confirmEnrollment(t, f.svc, memberID, householdID)

	if _, err := f.svc.ConfirmEnrollment(context.Background(), memberID, "123456"); !errors.Is(err, authdomain.ErrMFAAlreadyEnrolled) {
		t.Errorf("re-confirming an already-confirmed enrollment: err = %v, want ErrMFAAlreadyEnrolled", err)
	}
}

// ---------------------------------------------------------------------------
// AC2: recovery codes shown once, work once, used codes cannot be reused
// ---------------------------------------------------------------------------

func TestRegenerateRecoveryCodes_ValidCode_InvalidatesOldCodes(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	oldCodes := confirmEnrollment(t, f.svc, memberID, householdID)

	newCodes, err := f.svc.RegenerateRecoveryCodes(context.Background(), memberID, "123456")
	if err != nil {
		t.Fatalf("RegenerateRecoveryCodes: %v", err)
	}
	if len(newCodes) != 10 {
		t.Fatalf("RegenerateRecoveryCodes returned %d codes, want 10", len(newCodes))
	}
	for _, c := range newCodes {
		for _, old := range oldCodes {
			if c == old {
				t.Errorf("regenerated code %q collides with a previous code", c)
			}
		}
	}

	// The old codes must no longer verify via Disenroll (they were replaced,
	// not merely appended to).
	err = f.svc.Disenroll(context.Background(), memberID, householdID, "", oldCodes[0])
	if !errors.Is(err, authdomain.ErrRecoveryCodeInvalid) {
		t.Errorf("disenroll with a pre-regeneration recovery code: err = %v, want ErrRecoveryCodeInvalid", err)
	}
}

func TestRegenerateRecoveryCodes_WrongCodeRejected(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	confirmEnrollment(t, f.svc, memberID, householdID)

	if _, err := f.svc.RegenerateRecoveryCodes(context.Background(), memberID, "000000"); !errors.Is(err, authdomain.ErrInvalidTOTPCode) {
		t.Errorf("RegenerateRecoveryCodes(wrong code): err = %v, want ErrInvalidTOTPCode", err)
	}
}

func TestRegenerateRecoveryCodes_RecoveryCodeNotAcceptedInstead(t *testing.T) {
	// Regenerating requires possessing the authenticator itself; a recovery
	// code must not substitute for the TOTP code here (unlike Disenroll).
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	codes := confirmEnrollment(t, f.svc, memberID, householdID)

	if _, err := f.svc.RegenerateRecoveryCodes(context.Background(), memberID, codes[0]); !errors.Is(err, authdomain.ErrInvalidTOTPCode) {
		t.Errorf("RegenerateRecoveryCodes(recovery code instead of totp): err = %v, want ErrInvalidTOTPCode", err)
	}
}

func TestDisenroll_RecoveryCode_WorksOnceThenRejected(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	codes := confirmEnrollment(t, f.svc, memberID, householdID)

	if err := f.svc.Disenroll(context.Background(), memberID, householdID, "", codes[3]); err != nil {
		t.Fatalf("Disenroll with a fresh recovery code: %v", err)
	}

	// The successful Disenroll removed the whole enrollment; re-enroll so
	// there is an active enrollment again, then confirm the OLD code (from
	// the now-deleted enrollment) cannot be reused against it.
	confirmEnrollment(t, f.svc, memberID, householdID)
	err := f.svc.Disenroll(context.Background(), memberID, householdID, "", codes[3])
	if !errors.Is(err, authdomain.ErrRecoveryCodeInvalid) {
		t.Errorf("reusing a code from a deleted enrollment: err = %v, want ErrRecoveryCodeInvalid", err)
	}
}

// TestMarkRecoveryCodeUsed_ExcludesCodeFromFutureVerification is a
// repository-level test of AC2's "used codes visibly consumed" contract: once
// a code is marked used, it drops out of ListUnusedRecoveryCodes (the set
// Disenroll/RegenerateRecoveryCodes's matchRecoveryCode scans), so it can
// never verify again even though the enrollment itself is untouched.
func TestMarkRecoveryCodeUsed_ExcludesCodeFromFutureVerification(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	codes := confirmEnrollment(t, f.svc, memberID, householdID)

	unused, err := f.repo.ListUnusedRecoveryCodes(context.Background(), memberID)
	if err != nil {
		t.Fatalf("ListUnusedRecoveryCodes: %v", err)
	}
	target := findRecoveryCodeID(t, unused, codes[7])

	if err := f.repo.MarkRecoveryCodeUsed(context.Background(), target); err != nil {
		t.Fatalf("MarkRecoveryCodeUsed: %v", err)
	}

	stillUnused, err := f.repo.ListUnusedRecoveryCodes(context.Background(), memberID)
	if err != nil {
		t.Fatalf("ListUnusedRecoveryCodes after mark-used: %v", err)
	}
	if len(stillUnused) != 9 {
		t.Fatalf("unused recovery codes after marking one used = %d, want 9", len(stillUnused))
	}
	for _, c := range stillUnused {
		if c.ID == target {
			t.Error("a used recovery code must not appear in ListUnusedRecoveryCodes")
		}
	}

	// Consuming it via Disenroll must now fail as invalid, not succeed.
	err = f.svc.Disenroll(context.Background(), memberID, householdID, "", codes[7])
	if !errors.Is(err, authdomain.ErrRecoveryCodeInvalid) {
		t.Errorf("Disenroll with an already-used recovery code: err = %v, want ErrRecoveryCodeInvalid", err)
	}
}

// findRecoveryCodeID locates the RecoveryCodeID whose hash matches rawCode
// among candidates.
func findRecoveryCodeID(t *testing.T, candidates []authdomain.RecoveryCode, rawCode string) authdomain.RecoveryCodeID {
	t.Helper()
	normalized := authdomain.NormalizeRecoveryCode(rawCode)
	for _, c := range candidates {
		ok, err := crypto.Verify(normalized, c.CodeHash)
		if err == nil && ok {
			return c.ID
		}
	}
	t.Fatalf("no recovery code row matched %q", rawCode)
	return authdomain.RecoveryCodeID{}
}

func TestDisenroll_InvalidRecoveryCodeRejected(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	confirmEnrollment(t, f.svc, memberID, householdID)

	err := f.svc.Disenroll(context.Background(), memberID, householdID, "", "NOT-A-REAL-CODE")
	if !errors.Is(err, authdomain.ErrRecoveryCodeInvalid) {
		t.Errorf("Disenroll(bogus recovery code): err = %v, want ErrRecoveryCodeInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// AC3: household owner reset (with owner re-auth); non-owner rejected
// ---------------------------------------------------------------------------

func TestResetMemberMFA_NonOwnerRejected(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	householdID := household.NewHouseholdID()
	actingAdult := household.NewMemberID()
	target := household.NewMemberID()
	f.members.seed(&household.Member{ID: actingAdult, HouseholdID: householdID, Role: household.RoleAdult})
	f.members.seed(&household.Member{ID: target, HouseholdID: householdID, Role: household.RoleChild})

	err := f.svc.ResetMemberMFA(context.Background(), actingAdult, "irrelevant", target)
	if !errors.Is(err, authdomain.ErrNotHouseholdOwner) {
		t.Errorf("ResetMemberMFA as an adult (not owner): err = %v, want ErrNotHouseholdOwner", err)
	}
}

func TestResetMemberMFA_WrongPasswordRejected(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	householdID := household.NewHouseholdID()
	owner := household.NewMemberID()
	target := household.NewMemberID()
	f.members.seed(&household.Member{ID: owner, HouseholdID: householdID, Role: household.RoleOwner})
	f.members.seed(&household.Member{ID: target, HouseholdID: householdID, Role: household.RoleChild})
	seedOwnerPassword(t, f.passwords, owner, "correct-horse-battery-staple")
	confirmEnrollment(t, f.svc, target, householdID)

	err := f.svc.ResetMemberMFA(context.Background(), owner, "wrong-password", target)
	if !errors.Is(err, authdomain.ErrOwnerReauthRequired) {
		t.Errorf("ResetMemberMFA with wrong owner password: err = %v, want ErrOwnerReauthRequired", err)
	}

	// The target's enrollment must be untouched by a failed reset attempt.
	status, err := f.svc.Status(context.Background(), target)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Confirmed() {
		t.Error("a failed reset (wrong password) must not remove the target's enrollment")
	}
}

func TestResetMemberMFA_CorrectPassword_RemovesTargetEnrollment(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	householdID := household.NewHouseholdID()
	owner := household.NewMemberID()
	target := household.NewMemberID()
	f.members.seed(&household.Member{ID: owner, HouseholdID: householdID, Role: household.RoleOwner})
	f.members.seed(&household.Member{ID: target, HouseholdID: householdID, Role: household.RoleChild})
	seedOwnerPassword(t, f.passwords, owner, "correct-horse-battery-staple")
	confirmEnrollment(t, f.svc, target, householdID)

	if err := f.svc.ResetMemberMFA(context.Background(), owner, "correct-horse-battery-staple", target); err != nil {
		t.Fatalf("ResetMemberMFA: %v", err)
	}

	status, err := f.svc.Status(context.Background(), target)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status != nil {
		t.Error("after a successful reset, the target must have no enrollment (able to log in with password only)")
	}
}

func TestResetMemberMFA_TargetNotEnrolled(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	householdID := household.NewHouseholdID()
	owner := household.NewMemberID()
	target := household.NewMemberID()
	f.members.seed(&household.Member{ID: owner, HouseholdID: householdID, Role: household.RoleOwner})
	f.members.seed(&household.Member{ID: target, HouseholdID: householdID, Role: household.RoleChild})
	seedOwnerPassword(t, f.passwords, owner, "correct-horse-battery-staple")

	err := f.svc.ResetMemberMFA(context.Background(), owner, "correct-horse-battery-staple", target)
	if !errors.Is(err, authdomain.ErrMFANotEnrolled) {
		t.Errorf("ResetMemberMFA on an unenrolled target: err = %v, want ErrMFANotEnrolled", err)
	}
}

// TestResetMemberMFA_CrossHouseholdTargetRejected is the app-layer coverage
// for finding 5 (NES-134 CodeRabbit round 3): the service resolves the
// target's household itself and rejects a target outside the acting
// owner's own household, even though the acting member genuinely is an
// owner (of a DIFFERENT household).
func TestResetMemberMFA_CrossHouseholdTargetRejected(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	ownerHousehold := household.NewHouseholdID()
	otherHousehold := household.NewHouseholdID()
	owner := household.NewMemberID()
	target := household.NewMemberID()
	f.members.seed(&household.Member{ID: owner, HouseholdID: ownerHousehold, Role: household.RoleOwner})
	f.members.seed(&household.Member{ID: target, HouseholdID: otherHousehold, Role: household.RoleAdult})
	seedOwnerPassword(t, f.passwords, owner, "correct-horse-battery-staple")
	confirmEnrollment(t, f.svc, target, otherHousehold)

	err := f.svc.ResetMemberMFA(context.Background(), owner, "correct-horse-battery-staple", target)
	if !errors.Is(err, authdomain.ErrMFANotEnrolled) {
		t.Errorf("ResetMemberMFA against a cross-household target: err = %v, want ErrMFANotEnrolled", err)
	}

	// The victim's enrollment in the OTHER household must be untouched.
	status, err := f.svc.Status(context.Background(), target)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Confirmed() {
		t.Error("a cross-household reset attempt must not remove the victim's enrollment")
	}
}

func TestResetMemberMFA_UnknownActingMember(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	err := f.svc.ResetMemberMFA(context.Background(), household.NewMemberID(), "irrelevant", household.NewMemberID())
	if err == nil {
		t.Fatal("ResetMemberMFA with an unknown acting member must fail")
	}
	if errors.Is(err, authdomain.ErrNotHouseholdOwner) || errors.Is(err, authdomain.ErrOwnerReauthRequired) {
		t.Errorf("unexpected specific sentinel for an unknown acting member: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC4: secrets never stored or logged in plaintext
// ---------------------------------------------------------------------------

func TestBeginEnrollment_SecretNeverLogged(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()

	secret, _, err := f.svc.BeginEnrollment(context.Background(), memberID, householdID, "Alice")
	if err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if strings.Contains(f.logs.String(), secret) {
		t.Errorf("BeginEnrollment logged the raw secret: %s", f.logs.String())
	}

	if _, err := f.svc.ConfirmEnrollment(context.Background(), memberID, "123456"); err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}
	if strings.Contains(f.logs.String(), secret) {
		t.Errorf("ConfirmEnrollment logged the raw secret: %s", f.logs.String())
	}
}

// ---------------------------------------------------------------------------
// AC5: unconfirmed enrollments never lock anyone out
// ---------------------------------------------------------------------------

func TestDisenroll_NoCredentialSupplied(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	confirmEnrollment(t, f.svc, memberID, householdID)

	err := f.svc.Disenroll(context.Background(), memberID, householdID, "", "")
	if !errors.Is(err, authdomain.ErrMFAVerificationRequired) {
		t.Errorf("Disenroll with neither code nor recovery code: err = %v, want ErrMFAVerificationRequired", err)
	}
}

func TestDisenroll_UnconfirmedEnrollment_NotEnrolled(t *testing.T) {
	// Disenroll (and every other action requiring active MFA) must treat an
	// unconfirmed enrollment as though it doesn't exist.
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	if _, _, err := f.svc.BeginEnrollment(context.Background(), memberID, householdID, "Alice"); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}

	err := f.svc.Disenroll(context.Background(), memberID, householdID, "123456", "")
	if !errors.Is(err, authdomain.ErrMFANotEnrolled) {
		t.Errorf("Disenroll against an unconfirmed enrollment: err = %v, want ErrMFANotEnrolled", err)
	}
}

// ---------------------------------------------------------------------------
// NES-135: VerifyLoginCode — login-time TOTP/recovery verification with a
// durable replay guard.
// ---------------------------------------------------------------------------

func TestVerifyLoginCode_ValidTOTP_RecordsStep(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	confirmEnrollment(t, f.svc, memberID, householdID)
	f.totp.loginCode = "654321"
	f.totp.loginStep = 42

	if err := f.svc.VerifyLoginCode(context.Background(), memberID, "654321", ""); err != nil {
		t.Fatalf("VerifyLoginCode: %v", err)
	}

	enrollment, err := f.repo.GetEnrollment(context.Background(), memberID)
	if err != nil {
		t.Fatalf("GetEnrollment: %v", err)
	}
	if enrollment.LastTOTPStep == nil || *enrollment.LastTOTPStep != 42 {
		t.Errorf("LastTOTPStep = %v, want 42", enrollment.LastTOTPStep)
	}
}

func TestVerifyLoginCode_WrongTOTPRejected(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	confirmEnrollment(t, f.svc, memberID, householdID)
	f.totp.loginCode = "654321"
	f.totp.loginStep = 42

	err := f.svc.VerifyLoginCode(context.Background(), memberID, "000000", "")
	if !errors.Is(err, authdomain.ErrInvalidTOTPCode) {
		t.Errorf("VerifyLoginCode(wrong code): err = %v, want ErrInvalidTOTPCode", err)
	}
}

// TestVerifyLoginCode_ReplayedStepRejected is the direct AC coverage for "a
// TOTP code cannot be used twice": the SAME code (and therefore the SAME
// step) accepted once must be rejected on a second submission, even though
// totpProvider.MatchStep would report a match again — the replay guard is
// enforced by VerifyLoginCode comparing against LastTOTPStep, not by the
// TOTP math itself.
func TestVerifyLoginCode_ReplayedStepRejected(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	confirmEnrollment(t, f.svc, memberID, householdID)
	f.totp.loginCode = "654321"
	f.totp.loginStep = 42

	if err := f.svc.VerifyLoginCode(context.Background(), memberID, "654321", ""); err != nil {
		t.Fatalf("first VerifyLoginCode: %v", err)
	}
	err := f.svc.VerifyLoginCode(context.Background(), memberID, "654321", "")
	if !errors.Is(err, authdomain.ErrInvalidTOTPCode) {
		t.Errorf("replayed VerifyLoginCode: err = %v, want ErrInvalidTOTPCode", err)
	}
}

// TestVerifyLoginCode_LowerStepRejectedAfterHigherAccepted covers the
// "codes outside the skew window fail" AC from the other direction: once a
// LATER step has been accepted, an EARLIER step's code (still otherwise
// "valid" from MatchStep's perspective) must never be accepted afterward.
func TestVerifyLoginCode_LowerStepRejectedAfterHigherAccepted(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	confirmEnrollment(t, f.svc, memberID, householdID)

	f.totp.loginCode = "222222"
	f.totp.loginStep = 100
	if err := f.svc.VerifyLoginCode(context.Background(), memberID, "222222", ""); err != nil {
		t.Fatalf("VerifyLoginCode at step 100: %v", err)
	}

	f.totp.loginCode = "111111"
	f.totp.loginStep = 99
	err := f.svc.VerifyLoginCode(context.Background(), memberID, "111111", "")
	if !errors.Is(err, authdomain.ErrInvalidTOTPCode) {
		t.Errorf("VerifyLoginCode at an earlier step than already accepted: err = %v, want ErrInvalidTOTPCode", err)
	}
}

func TestVerifyLoginCode_RecoveryCode_ConsumesIt(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	codes := confirmEnrollment(t, f.svc, memberID, householdID)

	if err := f.svc.VerifyLoginCode(context.Background(), memberID, "", codes[2]); err != nil {
		t.Fatalf("VerifyLoginCode with a recovery code: %v", err)
	}

	// The same recovery code must not verify again.
	err := f.svc.VerifyLoginCode(context.Background(), memberID, "", codes[2])
	if !errors.Is(err, authdomain.ErrRecoveryCodeInvalid) {
		t.Errorf("reusing a login recovery code: err = %v, want ErrRecoveryCodeInvalid", err)
	}
}

// TestVerifyLoginCode_WrongTOTPFallsBackToRecoveryCode covers the
// either/or precedence: when BOTH a wrong TOTP code and a valid recovery
// code are supplied, the recovery code still verifies (mirrors Disenroll's
// verifyTOTPOrRecovery precedence).
func TestVerifyLoginCode_WrongTOTPFallsBackToRecoveryCode(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	codes := confirmEnrollment(t, f.svc, memberID, householdID)

	if err := f.svc.VerifyLoginCode(context.Background(), memberID, "000000", codes[5]); err != nil {
		t.Fatalf("VerifyLoginCode(wrong totp + valid recovery): %v", err)
	}
}

// TestVerifyLoginCode_HardTOTPErrorPropagates covers the OTHER branch of
// VerifyLoginCode's error handling: verifyLoginTOTP can fail for a reason
// that is NOT authdomain.ErrInvalidTOTPCode — here, cipher.Decrypt failing
// on a corrupted stored secret (crypto.ErrMalformedCiphertext, wrapped) —
// and that hard, non-sentinel error must propagate to the caller as-is
// rather than being silently treated as "wrong code".
func TestVerifyLoginCode_HardTOTPErrorPropagates(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	confirmEnrollment(t, f.svc, memberID, householdID)
	corruptStoredSecret(t, f.repo, memberID)

	err := f.svc.VerifyLoginCode(context.Background(), memberID, "654321", "")
	if err == nil {
		t.Fatal("VerifyLoginCode must propagate a hard decrypt error, not silently succeed")
	}
	if errors.Is(err, authdomain.ErrInvalidTOTPCode) || errors.Is(err, authdomain.ErrRecoveryCodeInvalid) || errors.Is(err, authdomain.ErrMFAVerificationRequired) {
		t.Errorf("VerifyLoginCode masked a hard decrypt failure as a well-known sentinel: %v", err)
	}
	if !errors.Is(err, crypto.ErrMalformedCiphertext) {
		t.Errorf("VerifyLoginCode error = %v, want it to wrap crypto.ErrMalformedCiphertext", err)
	}
}

// TestVerifyLoginCode_HardTOTPErrorDoesNotFallBackToRecoveryCode pins down
// the intended behavior alongside
// TestVerifyLoginCode_WrongTOTPFallsBackToRecoveryCode's happy path: the
// fallback to a supplied recovery code is conditioned on the TOTP failure
// specifically being authdomain.ErrInvalidTOTPCode (VerifyLoginCode's
// `!errors.Is(err, authdomain.ErrInvalidTOTPCode) || recoveryCode == ""`
// check) — a HARD error (e.g. a decrypt failure) must be returned
// immediately instead, even when a perfectly valid recovery code was also
// supplied, because a decrypt failure is an infrastructure problem the
// caller needs to see (and log/500) rather than a "wrong code" the member
// can just retry with their recovery code.
func TestVerifyLoginCode_HardTOTPErrorDoesNotFallBackToRecoveryCode(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	codes := confirmEnrollment(t, f.svc, memberID, householdID)
	corruptStoredSecret(t, f.repo, memberID)

	err := f.svc.VerifyLoginCode(context.Background(), memberID, "654321", codes[0])
	if err == nil {
		t.Fatal("VerifyLoginCode must propagate the hard decrypt error, not silently succeed via the recovery code")
	}
	if errors.Is(err, authdomain.ErrInvalidTOTPCode) || errors.Is(err, authdomain.ErrRecoveryCodeInvalid) {
		t.Errorf("a hard (non-ErrInvalidTOTPCode) TOTP error must be returned as-is, not masked as a wrong-code or recovery-code sentinel: %v", err)
	}
	if !errors.Is(err, crypto.ErrMalformedCiphertext) {
		t.Errorf("VerifyLoginCode error = %v, want it to wrap crypto.ErrMalformedCiphertext", err)
	}

	// The recovery code must NOT have been consumed — proof VerifyLoginCode
	// never reached matchRecoveryCode for it.
	stillUnused, err := f.repo.ListUnusedRecoveryCodes(context.Background(), memberID)
	if err != nil {
		t.Fatalf("ListUnusedRecoveryCodes: %v", err)
	}
	if len(stillUnused) != 10 {
		t.Errorf("unused recovery codes after a hard TOTP error = %d, want still 10 (the recovery code must not have been attempted)", len(stillUnused))
	}
}

func TestVerifyLoginCode_NoCredentialSupplied(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	confirmEnrollment(t, f.svc, memberID, householdID)

	err := f.svc.VerifyLoginCode(context.Background(), memberID, "", "")
	if !errors.Is(err, authdomain.ErrMFAVerificationRequired) {
		t.Errorf("VerifyLoginCode with neither code: err = %v, want ErrMFAVerificationRequired", err)
	}
}

func TestVerifyLoginCode_UnenrolledMemberRejected(t *testing.T) {
	t.Parallel()
	f := newMFAFixture(t)
	err := f.svc.VerifyLoginCode(context.Background(), household.NewMemberID(), "123456", "")
	if !errors.Is(err, authdomain.ErrMFANotEnrolled) {
		t.Errorf("VerifyLoginCode for an unenrolled member: err = %v, want ErrMFANotEnrolled", err)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// confirmEnrollment drives BeginEnrollment + ConfirmEnrollment to completion
// for memberID/householdID using the fixture's fixed valid code, returning
// the ten raw recovery codes.
func confirmEnrollment(t *testing.T, svc *app.MFAService, memberID household.MemberID, householdID household.HouseholdID) []string {
	t.Helper()
	if _, _, err := svc.BeginEnrollment(context.Background(), memberID, householdID, "Member"); err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	codes, err := svc.ConfirmEnrollment(context.Background(), memberID, "123456")
	if err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}
	return codes
}

// corruptStoredSecret flips a byte in memberID's stored TOTPSecretEnc
// directly in repo (bypassing the service entirely), so a SUBSEQUENT
// cipher.Decrypt against it fails with crypto.ErrMalformedCiphertext — the
// GCM authentication tag no longer matches. Used to force a hard,
// non-authdomain.ErrInvalidTOTPCode error out of verifyLoginTOTP for tests
// that need to distinguish "wrong code" from "infrastructure failure".
func corruptStoredSecret(t *testing.T, repo *fakeMFARepo, memberID household.MemberID) {
	t.Helper()
	e, ok := repo.enrollments[memberID]
	if !ok {
		t.Fatalf("corruptStoredSecret: no enrollment on file for %s", memberID)
	}
	corrupted := append([]byte(nil), e.TOTPSecretEnc...)
	corrupted[0] ^= 0xFF
	e.TOTPSecretEnc = corrupted
}

func seedOwnerPassword(t *testing.T, passwords *fakePasswordVerifier, ownerID household.MemberID, password string) {
	t.Helper()
	hash, err := crypto.Hash(password)
	if err != nil {
		t.Fatalf("crypto.Hash: %v", err)
	}
	passwords.credentials[ownerID] = &authdomain.Credential{MemberID: ownerID, PasswordHash: hash}
}
