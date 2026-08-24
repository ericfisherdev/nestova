package app

import (
	"sync"
	"time"
)

// PIN attempt-limiting tuning (NES-165). A 4-8 digit PIN's guess space is
// far smaller than a 6-digit TOTP code's, but the threat model differs too:
// this gates a household chore action a sibling might fumble a couple of
// times, not an account-security credential facing a remote attacker, so
// the same shape as loginAttemptLimiter's tuning (adapter package) is
// reused rather than tightened further.
const (
	// pinAttemptThreshold is how many consecutive wrong PINs a member may
	// submit before backoff engages.
	pinAttemptThreshold = 5
	// pinBackoffWindow is how long a member is locked out of PIN
	// verification after the (threshold+1)th consecutive wrong PIN.
	pinBackoffWindow = 5 * time.Minute
)

// pinAttemptState is one member's in-memory PIN strike state.
type pinAttemptState struct {
	failures    int
	lockedUntil time.Time
}

// pinAttemptLimiter tracks consecutive wrong PINs per member and enforces a
// backoff window after pinAttemptThreshold consecutive failures, reset on
// the next successful verification. It is a distinct type from
// internal/auth/adapter's loginAttemptLimiter — same in-memory,
// process-lifetime strike-counter shape (see that type's own doc for the
// accepted tradeoffs of a single-household, local-first appliance), but a
// separate instance because PINService owns its own gate independent of
// login MFA's.
//
// Unlike loginAttemptLimiter, this type also exposes lockedUntil (so a
// lockout can be shown in settings) and resetLockout (so an owner or adult
// can clear it without a database change — the lockout state lives only
// here, never persisted).
type pinAttemptLimiter struct {
	mu    sync.Mutex
	state map[string]*pinAttemptState
}

// newPINAttemptLimiter constructs an empty limiter.
func newPINAttemptLimiter() *pinAttemptLimiter {
	return &pinAttemptLimiter{state: make(map[string]*pinAttemptState)}
}

// locked reports whether memberKey is currently in a backoff window as of
// now.
func (l *pinAttemptLimiter) locked(memberKey string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.state[memberKey]
	if !ok {
		return false
	}
	return now.Before(st.lockedUntil)
}

// lockedUntil returns memberKey's current lockout expiry and true, or a
// zero time and false when memberKey is not currently locked as of now.
func (l *pinAttemptLimiter) lockedUntil(memberKey string, now time.Time) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.state[memberKey]
	if !ok || !now.Before(st.lockedUntil) {
		return time.Time{}, false
	}
	return st.lockedUntil, true
}

// recordFailure records a wrong PIN for memberKey as of now, returning
// lockedOut=true exactly once — on the attempt that CROSSES the threshold —
// mirroring loginAttemptLimiter.recordFailure's own contract, including
// resetting the strike count when a prior lockout has fully expired.
func (l *pinAttemptLimiter) recordFailure(memberKey string, now time.Time) (lockedOut bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.state[memberKey]
	switch {
	case !ok:
		st = &pinAttemptState{}
		l.state[memberKey] = st
	case !st.lockedUntil.IsZero() && !now.Before(st.lockedUntil):
		// A previous lockout has fully expired: start counting fresh.
		st.failures = 0
		st.lockedUntil = time.Time{}
	}
	st.failures++
	if st.failures == pinAttemptThreshold+1 {
		st.lockedUntil = now.Add(pinBackoffWindow)
		return true
	}
	return false
}

// recordSuccess clears memberKey's strike state after a successful
// verification.
func (l *pinAttemptLimiter) recordSuccess(memberKey string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.state, memberKey)
}

// resetLockout clears memberKey's strike state unconditionally — the
// owner/adult admin action (PINService.ResetForMember), letting a member
// verify again immediately rather than waiting out pinBackoffWindow.
func (l *pinAttemptLimiter) resetLockout(memberKey string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.state, memberKey)
}
