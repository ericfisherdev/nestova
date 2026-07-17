package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
)

// recoveryCodeCount is how many single-use recovery codes are generated at
// confirmation and on every regeneration.
const recoveryCodeCount = 10

// mfaIssuer is the TOTP issuer string shown inside an authenticator app next
// to the account entry, matching this codebase's hardcoded branding
// elsewhere (e.g. web/components/login.templ's page title).
const mfaIssuer = "Nestova"

// secretCipher is the slice of the crypto cipher MFAService depends on
// (ISP), satisfied by *crypto.Cipher — the same cipher instance the calendar
// context uses to protect OAuth tokens at rest (see calendar/app's
// AccountService, whose secretCipher interface this mirrors).
type secretCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// totpProvider is the minimal seam over RFC 6238 TOTP generation/validation
// MFAService depends on (ISP + DIP), satisfied by
// internal/platform/totp.Provider and faked in tests so they never need a
// real clock-synchronized authenticator.
type totpProvider interface {
	GenerateSecret(issuer, accountName string) (secret, otpauthURL string, err error)
	Validate(code, secret string) bool
}

// passwordVerifier is the minimal seam over the credential store MFAService
// depends on to enforce the household owner's fresh re-auth before a reset
// (ISP): it needs only the member-id lookup, not SetPassword or FindByEmail.
// Satisfied by authdomain.CredentialRepository (a superset).
type passwordVerifier interface {
	FindByMemberID(ctx context.Context, memberID household.MemberID) (*authdomain.Credential, error)
}

// MFAService orchestrates TOTP enrollment, confirmation, recovery codes, and
// the household-owner admin reset. It is the auth context's use-case
// boundary for NES-134; login enforcement is NES-135 and is not implemented
// here.
type MFAService struct {
	repo      authdomain.MFARepository
	cipher    secretCipher
	totp      totpProvider
	passwords passwordVerifier
	logger    *slog.Logger
}

// NewMFAService constructs the service with injected dependencies. All five
// are required.
func NewMFAService(repo authdomain.MFARepository, cipher secretCipher, totp totpProvider, passwords passwordVerifier, logger *slog.Logger) (*MFAService, error) {
	if repo == nil {
		return nil, errors.New("auth: NewMFAService requires a non-nil MFARepository")
	}
	if cipher == nil {
		return nil, errors.New("auth: NewMFAService requires a non-nil cipher")
	}
	if totp == nil {
		return nil, errors.New("auth: NewMFAService requires a non-nil totp provider")
	}
	if passwords == nil {
		return nil, errors.New("auth: NewMFAService requires a non-nil password verifier")
	}
	if logger == nil {
		return nil, errors.New("auth: NewMFAService requires a non-nil logger")
	}
	return &MFAService{repo: repo, cipher: cipher, totp: totp, passwords: passwords, logger: logger}, nil
}

// Status returns the member's current enrollment, or nil if none exists
// (confirmed or not). It never returns authdomain.ErrMFANotEnrolled — that
// sentinel is folded into the (nil, nil) result — so callers building a
// display status do not need to special-case the not-enrolled error.
func (s *MFAService) Status(ctx context.Context, memberID household.MemberID) (*authdomain.MFAEnrollment, error) {
	enrollment, err := s.repo.GetEnrollment(ctx, memberID)
	if err != nil {
		if errors.Is(err, authdomain.ErrMFANotEnrolled) {
			return nil, nil
		}
		return nil, fmt.Errorf("mfa: get status: %w", err)
	}
	return enrollment, nil
}

// BeginEnrollment generates a fresh TOTP secret for memberID and persists it
// unconfirmed, replacing any existing unconfirmed enrollment in place (a
// re-enroll before confirming simply starts over — see the member_mfa
// migration's doc comment for why no separate cleanup sweep is needed). It
// returns the raw secret (for manual entry) and its otpauth:// URL (for QR
// rendering); neither is persisted in plaintext, and the caller must not log
// either. accountName labels the entry inside the member's authenticator app
// (their display name — members are not guaranteed to have an email).
//
// Returns authdomain.ErrMFAAlreadyEnrolled when the member already has a
// CONFIRMED enrollment (it must be disabled or disenrolled first).
func (s *MFAService) BeginEnrollment(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID, accountName string) (secret, otpauthURL string, err error) {
	secret, otpauthURL, err = s.totp.GenerateSecret(mfaIssuer, accountName)
	if err != nil {
		return "", "", fmt.Errorf("mfa: generate secret: %w", err)
	}
	secretEnc, err := s.cipher.Encrypt([]byte(secret))
	if err != nil {
		return "", "", fmt.Errorf("mfa: encrypt secret: %w", err)
	}
	if err := s.repo.BeginEnrollment(ctx, memberID, householdID, secretEnc); err != nil {
		return "", "", err
	}
	s.logger.InfoContext(ctx, "mfa enrollment started", "member_id", memberID.String())
	return secret, otpauthURL, nil
}

// ConfirmEnrollment validates code against the member's pending secret and,
// on success, marks the enrollment confirmed and generates a fresh batch of
// recoveryCodeCount recovery codes (only ever generated AFTER confirmation,
// never before). It returns the raw codes — shown to the member exactly
// once; only their hashes are persisted.
//
// Returns authdomain.ErrMFANotEnrolled when no enrollment exists,
// authdomain.ErrMFAAlreadyEnrolled when it is already confirmed, and
// authdomain.ErrInvalidTOTPCode when code does not validate.
func (s *MFAService) ConfirmEnrollment(ctx context.Context, memberID household.MemberID, code string) ([]string, error) {
	enrollment, err := s.repo.GetEnrollment(ctx, memberID)
	if err != nil {
		return nil, err
	}
	if enrollment.Confirmed() {
		return nil, authdomain.ErrMFAAlreadyEnrolled
	}
	secret, err := s.cipher.Decrypt(enrollment.TOTPSecretEnc)
	if err != nil {
		return nil, fmt.Errorf("mfa: decrypt secret: %w", err)
	}
	if !s.totp.Validate(strings.TrimSpace(code), string(secret)) {
		return nil, authdomain.ErrInvalidTOTPCode
	}
	if err := s.repo.ConfirmEnrollment(ctx, memberID); err != nil {
		return nil, fmt.Errorf("mfa: confirm enrollment: %w", err)
	}
	s.logger.InfoContext(ctx, "mfa enrollment confirmed", "member_id", memberID.String())

	codes, err := s.regenerateRecoveryCodes(ctx, memberID)
	if err != nil {
		return nil, err
	}
	return codes, nil
}

// RegenerateRecoveryCodes validates code against the member's active
// enrollment and, on success, replaces their entire recovery code set with a
// fresh batch, invalidating every previous code. It returns the raw codes —
// shown exactly once.
//
// Returns authdomain.ErrMFANotEnrolled when no CONFIRMED enrollment exists
// and authdomain.ErrInvalidTOTPCode when code does not validate. A recovery
// code is deliberately NOT accepted here (unlike Disenroll): regenerating
// requires possessing the authenticator itself, not just a recovery code.
func (s *MFAService) RegenerateRecoveryCodes(ctx context.Context, memberID household.MemberID, code string) ([]string, error) {
	enrollment, err := s.requireConfirmedEnrollment(ctx, memberID)
	if err != nil {
		return nil, err
	}
	secret, err := s.cipher.Decrypt(enrollment.TOTPSecretEnc)
	if err != nil {
		return nil, fmt.Errorf("mfa: decrypt secret: %w", err)
	}
	if !s.totp.Validate(strings.TrimSpace(code), string(secret)) {
		return nil, authdomain.ErrInvalidTOTPCode
	}
	return s.regenerateRecoveryCodes(ctx, memberID)
}

// Disenroll removes the member's own MFA enrollment (and its recovery
// codes), after verifying EITHER a current TOTP code OR an unused recovery
// code — whichever the caller supplies (exactly one of totpCode/recoveryCode
// is expected to be non-blank; if both are, the TOTP code is tried first). A
// matched recovery code is consumed even though the enrollment is about to
// be deleted entirely, so the audit trail (used_at) reflects that it was the
// credential that authorized the disenroll.
//
// Returns authdomain.ErrMFAVerificationRequired when neither is supplied,
// authdomain.ErrMFANotEnrolled when the member has no confirmed enrollment,
// authdomain.ErrInvalidTOTPCode / ErrRecoveryCodeInvalid when the supplied
// credential does not verify.
func (s *MFAService) Disenroll(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID, totpCode, recoveryCode string) error {
	if err := s.verifyTOTPOrRecovery(ctx, memberID, totpCode, recoveryCode); err != nil {
		return err
	}
	if err := s.repo.DeleteEnrollment(ctx, householdID, memberID); err != nil {
		return fmt.Errorf("mfa: disenroll: %w", err)
	}
	s.logger.InfoContext(ctx, "mfa disenrolled", "member_id", memberID.String())
	return nil
}

// ResetMemberMFA is the household-owner admin action (e.g. a lost-device
// recovery path): it removes targetMemberID's MFA enrollment entirely,
// requiring the ACTING owner to re-enter their own password first (fresh
// re-auth). Only the household owner may call this — an adult member with
// parent privileges is not sufficient (see authdomain.ErrNotHouseholdOwner's
// doc). Both members must belong to householdID; the caller (the HTTP
// handler) is responsible for having already verified that targetMemberID
// resolves within the acting owner's own household before calling this, so
// DeleteEnrollment's householdID-scoped delete is a defense-in-depth check,
// not the only one.
//
// Returns authdomain.ErrNotHouseholdOwner when actingRole is not
// household.RoleOwner, authdomain.ErrOwnerReauthRequired when ownerPassword
// does not match the acting owner's stored credential, and
// authdomain.ErrMFANotEnrolled when the target member has no enrollment to
// reset.
func (s *MFAService) ResetMemberMFA(ctx context.Context, actingOwnerID household.MemberID, actingRole household.Role, ownerPassword string, householdID household.HouseholdID, targetMemberID household.MemberID) error {
	if actingRole != household.RoleOwner {
		return authdomain.ErrNotHouseholdOwner
	}
	cred, err := s.passwords.FindByMemberID(ctx, actingOwnerID)
	if err != nil {
		if errors.Is(err, authdomain.ErrInvalidCredentials) {
			return authdomain.ErrOwnerReauthRequired
		}
		return fmt.Errorf("mfa: look up owner credential: %w", err)
	}
	ok, err := crypto.Verify(ownerPassword, cred.PasswordHash)
	if err != nil || !ok {
		return authdomain.ErrOwnerReauthRequired
	}

	if err := s.repo.DeleteEnrollment(ctx, householdID, targetMemberID); err != nil {
		return fmt.Errorf("mfa: reset member mfa: %w", err)
	}
	s.logger.InfoContext(ctx, "mfa reset by household owner", "member_id", targetMemberID.String(), "owner_id", actingOwnerID.String())
	return nil
}

// requireConfirmedEnrollment fetches memberID's enrollment and returns
// authdomain.ErrMFANotEnrolled when there is none or it is still
// unconfirmed (an unconfirmed enrollment must never satisfy an action that
// requires active MFA).
func (s *MFAService) requireConfirmedEnrollment(ctx context.Context, memberID household.MemberID) (*authdomain.MFAEnrollment, error) {
	enrollment, err := s.repo.GetEnrollment(ctx, memberID)
	if err != nil {
		return nil, err
	}
	if !enrollment.Confirmed() {
		return nil, authdomain.ErrMFANotEnrolled
	}
	return enrollment, nil
}

// verifyTOTPOrRecovery checks totpCode first (if non-blank), falling back to
// recoveryCode (if non-blank); a matched recovery code is marked used. See
// Disenroll's doc for the precedence and error contract.
func (s *MFAService) verifyTOTPOrRecovery(ctx context.Context, memberID household.MemberID, totpCode, recoveryCode string) error {
	totpCode = strings.TrimSpace(totpCode)
	recoveryCode = strings.TrimSpace(recoveryCode)
	if totpCode == "" && recoveryCode == "" {
		return authdomain.ErrMFAVerificationRequired
	}

	enrollment, err := s.requireConfirmedEnrollment(ctx, memberID)
	if err != nil {
		return err
	}

	if totpCode != "" {
		secret, err := s.cipher.Decrypt(enrollment.TOTPSecretEnc)
		if err != nil {
			return fmt.Errorf("mfa: decrypt secret: %w", err)
		}
		if s.totp.Validate(totpCode, string(secret)) {
			return nil
		}
		if recoveryCode == "" {
			return authdomain.ErrInvalidTOTPCode
		}
	}

	codeID, ok, err := s.matchRecoveryCode(ctx, memberID, recoveryCode)
	if err != nil {
		return err
	}
	if !ok {
		return authdomain.ErrRecoveryCodeInvalid
	}
	if err := s.repo.MarkRecoveryCodeUsed(ctx, codeID); err != nil {
		return fmt.Errorf("mfa: mark recovery code used: %w", err)
	}
	return nil
}

// matchRecoveryCode normalizes raw and compares it (via crypto.Verify, the
// same argon2id KDF as member passwords) against every unused recovery code
// on file for memberID, returning the matched code's id. The unused set is
// bounded by recoveryCodeCount (ten), so a linear scan of argon2id
// verifications is an acceptable, bounded cost per attempt.
func (s *MFAService) matchRecoveryCode(ctx context.Context, memberID household.MemberID, raw string) (authdomain.RecoveryCodeID, bool, error) {
	normalized := authdomain.NormalizeRecoveryCode(raw)
	if normalized == "" {
		return authdomain.RecoveryCodeID{}, false, nil
	}
	codes, err := s.repo.ListUnusedRecoveryCodes(ctx, memberID)
	if err != nil {
		return authdomain.RecoveryCodeID{}, false, fmt.Errorf("mfa: list recovery codes: %w", err)
	}
	for _, c := range codes {
		ok, err := crypto.Verify(normalized, c.CodeHash)
		if err != nil {
			// A malformed stored hash should never happen (every hash this
			// service writes comes from crypto.Hash); skip defensively
			// rather than failing the whole lookup for one bad row.
			s.logger.ErrorContext(ctx, "mfa: malformed recovery code hash", "recovery_code_id", c.ID.String())
			continue
		}
		if ok {
			return c.ID, true, nil
		}
	}
	return authdomain.RecoveryCodeID{}, false, nil
}

// regenerateRecoveryCodes generates recoveryCodeCount fresh raw codes,
// hashes each with crypto.Hash, and atomically replaces the member's stored
// set. It returns the raw codes for one-time display.
func (s *MFAService) regenerateRecoveryCodes(ctx context.Context, memberID household.MemberID) ([]string, error) {
	codes := make([]string, 0, recoveryCodeCount)
	hashes := make([]string, 0, recoveryCodeCount)
	for range recoveryCodeCount {
		raw, err := authdomain.GenerateRecoveryCode()
		if err != nil {
			return nil, err
		}
		hash, err := crypto.Hash(authdomain.NormalizeRecoveryCode(raw))
		if err != nil {
			return nil, fmt.Errorf("mfa: hash recovery code: %w", err)
		}
		codes = append(codes, raw)
		hashes = append(hashes, hash)
	}
	if err := s.repo.ReplaceRecoveryCodes(ctx, memberID, hashes); err != nil {
		return nil, fmt.Errorf("mfa: store recovery codes: %w", err)
	}
	s.logger.InfoContext(ctx, "mfa recovery codes regenerated", "member_id", memberID.String(), "count", recoveryCodeCount)
	return codes, nil
}
