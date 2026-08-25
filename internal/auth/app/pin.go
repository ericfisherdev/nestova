package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// pinFormat matches a 4-8 digit PIN. Digits only — no letters, no
// separators — so a member can enter it on a numeric keypad.
var pinFormat = regexp.MustCompile(`^[0-9]{4,8}$`)

// PINService orchestrates per-member PIN enrolment, strike-limited
// verification with a visible and resettable lockout, and
// AuthorizeTaskAction — the single gate the tasks adapter and the deeplink
// adapter will call in the follow-up ticket "require a member PIN to
// complete or skip a chore" (NES-166). Nothing calls AuthorizeTaskAction
// yet; it is fully unit-tested here regardless.
type PINService struct {
	repo    authdomain.PINRepository
	hasher  passwordHasher
	limiter *pinAttemptLimiter
	now     func() time.Time
	logger  *slog.Logger
}

// NewPINService constructs the service with injected dependencies. now is
// the clock seam (production callers pass time.Now); all four arguments are
// required.
func NewPINService(repo authdomain.PINRepository, hasher passwordHasher, now func() time.Time, logger *slog.Logger) (*PINService, error) {
	if repo == nil {
		return nil, errors.New("auth: NewPINService requires a non-nil PINRepository")
	}
	if hasher == nil {
		return nil, errors.New("auth: NewPINService requires a non-nil password hasher")
	}
	if now == nil {
		return nil, errors.New("auth: NewPINService requires a non-nil clock func")
	}
	if logger == nil {
		return nil, errors.New("auth: NewPINService requires a non-nil logger")
	}
	return &PINService{repo: repo, hasher: hasher, limiter: newPINAttemptLimiter(), now: now, logger: logger}, nil
}

// IsEnrolled reports whether memberID has a PIN on file.
func (s *PINService) IsEnrolled(ctx context.Context, memberID household.MemberID) (bool, error) {
	_, err := s.repo.GetPINHash(ctx, memberID)
	if err != nil {
		if errors.Is(err, authdomain.ErrPINNotEnrolled) {
			return false, nil
		}
		return false, fmt.Errorf("pin: check enrollment: %w", err)
	}
	return true, nil
}

// EnrolledMembers returns every member id in householdID with a PIN on
// file, for the settings page's admin member list.
func (s *PINService) EnrolledMembers(ctx context.Context, householdID household.HouseholdID) ([]household.MemberID, error) {
	ids, err := s.repo.EnrolledMembers(ctx, householdID)
	if err != nil {
		return nil, fmt.Errorf("pin: list enrolled members: %w", err)
	}
	return ids, nil
}

// Set stores (or replaces) memberID's own PIN, validating pin is 4-8
// digits. Setting a fresh PIN also clears any active lockout — a new PIN
// deserves a full fresh run of attempts, not one inherited from the old
// one.
func (s *PINService) Set(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID, pin string) error {
	return s.setPIN(ctx, memberID, householdID, pin, "member set their own pin")
}

// SetForMember stores (or replaces) targetMemberID's PIN on behalf of a
// parent (owner/adult) acting on a child. Authorization
// (household.Role.CanAdminister()) is enforced by the HTTP handler layer
// (pin_web.go), mirroring kioskadapter's established parent-gate
// convention — SetForMember performs the identical write as Set, differing
// only in the audit log line, so a settings-page admin action is
// distinguishable from self-service.
func (s *PINService) SetForMember(ctx context.Context, targetMemberID household.MemberID, targetHouseholdID household.HouseholdID, pin string) error {
	return s.setPIN(ctx, targetMemberID, targetHouseholdID, pin, "parent set a member's pin")
}

// Clear removes memberID's own PIN, scoped to householdID. Returns
// authdomain.ErrPINNotEnrolled when the member has no PIN to clear.
func (s *PINService) Clear(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID) error {
	return s.clearPIN(ctx, memberID, householdID, "member cleared their own pin")
}

// ResetForMember removes targetMemberID's PIN AND clears any active
// lockout on behalf of a parent (owner/adult) acting on a child —
// authorization enforced by the HTTP handler layer, mirroring
// SetForMember. Returns authdomain.ErrPINNotEnrolled when there is no PIN to
// clear WITHIN targetHouseholdID, in which case no lockout is cleared
// either — see the body for why those two have to stay tied together.
func (s *PINService) ResetForMember(ctx context.Context, targetMemberID household.MemberID, targetHouseholdID household.HouseholdID) error {
	// clearPIN resets the lockout itself, but ONLY when it actually deleted a
	// row. That is deliberate: a cross-household call and a member with no
	// PIN both surface as ErrPINNotEnrolled, and the lockout lives in memory
	// where no SQL predicate reaches it, so resetting on the error path would
	// let an admin of another household hand a locked-out member unlimited
	// fresh attempts against a PIN that is still enrolled. Nothing is lost by
	// the stricter rule: a member cannot be both locked out and unenrolled,
	// since Verify never counts a strike for an unenrolled member and a
	// successful Clear resets the lockout on its way out.
	return s.clearPIN(ctx, targetMemberID, targetHouseholdID, "parent reset a member's pin")
}

// Verify checks pin against memberID's stored hash, strike-limited via the
// injected pinAttemptLimiter (mirroring internal/auth/adapter's
// loginAttemptLimiter pattern, see that limiter's own doc).
//
// Returns authdomain.ErrPINLocked while memberID is in the backoff window
// (without even reading the stored hash — a locked member's PIN is never
// consulted), authdomain.ErrPINNotEnrolled when the member has no PIN on
// file (this does NOT count as a strike — see AuthorizeTaskAction's
// nil-gate, which relies on an unenrolled member never becoming
// lockable), and authdomain.ErrPINMismatch when the submitted PIN is wrong
// (this DOES count as a strike).
func (s *PINService) Verify(ctx context.Context, memberID household.MemberID, pin string) error {
	key := memberID.String()
	now := s.now()
	if s.limiter.locked(key, now) {
		return authdomain.ErrPINLocked
	}

	hash, err := s.repo.GetPINHash(ctx, memberID)
	if err != nil {
		if errors.Is(err, authdomain.ErrPINNotEnrolled) {
			return authdomain.ErrPINNotEnrolled
		}
		return fmt.Errorf("pin: get hash: %w", err)
	}

	ok, err := s.hasher.Verify(pin, hash)
	if err != nil || !ok {
		if s.limiter.recordFailure(key, now) {
			s.logger.InfoContext(ctx, "pin verification locked", "member_id", key)
		}
		return authdomain.ErrPINMismatch
	}

	s.limiter.recordSuccess(key)
	return nil
}

// AuthorizeTaskAction is the single authorization gate a task-mutating
// endpoint calls before crediting a completion/skip to an actor (NES-166,
// not wired yet): it verifies submittedPIN against assigneeID's own PIN and
// reports who actually performed the action.
//
//   - A correct PIN returns assigneeID itself as the actor, with a nil
//     error.
//   - An assignee with NO PIN enrolled is a nil-gate: (household.MemberID{},
//     nil) — the caller must leave its actor unchanged (whatever it already
//     resolved, e.g. the currently signed-in member), since there is
//     nothing to verify against. This is what lets NES-165 ship the gate
//     unused: every existing caller path has no assignee enrolled yet, so
//     AuthorizeTaskAction is a no-op until a member actually sets a PIN.
//   - A wrong or locked PIN returns (household.MemberID{},
//     authdomain.ErrPINMismatch / authdomain.ErrPINLocked) — the caller
//     must refuse the action, not fall back to any other actor.
func (s *PINService) AuthorizeTaskAction(ctx context.Context, assigneeID household.MemberID, submittedPIN string) (household.MemberID, error) {
	err := s.Verify(ctx, assigneeID, submittedPIN)
	switch {
	case err == nil:
		return assigneeID, nil
	case errors.Is(err, authdomain.ErrPINNotEnrolled):
		return household.MemberID{}, nil
	default:
		return household.MemberID{}, err
	}
}

// LockedUntil reports memberID's current PIN lockout expiry, if any — used
// by settings to show an active lockout. Lockout state lives only in the
// in-memory limiter (never persisted, mirroring loginAttemptLimiter's own
// tradeoff), so this requires no database change to check or clear.
func (s *PINService) LockedUntil(memberID household.MemberID) (time.Time, bool) {
	return s.limiter.lockedUntil(memberID.String(), s.now())
}

// setPIN validates, hashes, and persists pin for memberID, resetting any
// lockout on success. logMsg distinguishes a self-service Set from a parent
// SetForMember in the log line.
func (s *PINService) setPIN(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID, pin, logMsg string) error {
	if !pinFormat.MatchString(pin) {
		return authdomain.ErrInvalidPINFormat
	}
	hash, err := s.hasher.Hash(pin)
	if err != nil {
		return fmt.Errorf("pin: hash: %w", err)
	}
	if err := s.repo.SetPIN(ctx, memberID, householdID, hash); err != nil {
		return err
	}
	s.limiter.resetLockout(memberID.String())
	s.logger.InfoContext(ctx, logMsg, "member_id", memberID.String())
	return nil
}

// clearPIN deletes memberID's PIN, resetting any lockout on success.
// logMsg distinguishes a self-service Clear from a parent ResetForMember in
// the log line. Returns authdomain.ErrPINNotEnrolled unwrapped when there
// is no PIN to clear.
func (s *PINService) clearPIN(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID, logMsg string) error {
	if err := s.repo.ClearPIN(ctx, memberID, householdID); err != nil {
		return err
	}
	s.limiter.resetLockout(memberID.String())
	s.logger.InfoContext(ctx, logMsg, "member_id", memberID.String())
	return nil
}
