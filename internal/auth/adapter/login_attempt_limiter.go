package adapter

import (
	"sync"
	"time"
)

// Login MFA attempt-limiting tuning (NES-135). This closes the NES-86 gap
// (login brute-force limiting) for the login MFA step specifically: NES-86
// itself was never implemented (see this ticket's Description), so there is
// no other rate/backoff/lockout code in internal/ to build alongside.
const (
	// loginMFAAttemptThreshold is how many consecutive wrong login MFA codes
	// (TOTP or recovery) a member may submit before backoff engages. Five
	// wrong codes is generous enough to absorb a fumbled entry or two (a
	// human squinting at a rotating 6-digit code on a small phone screen)
	// while still bounding a scripted guesser far below anything that could
	// brute-force a 6-digit TOTP code (1,000,000 combinations) or a 10-code
	// recovery set before the TOTP window rotates or the lockout engages.
	loginMFAAttemptThreshold = 5
	// loginMFABackoffWindow is how long a member is locked out of login MFA
	// verification after the (threshold+1)th consecutive wrong code.
	loginMFABackoffWindow = 5 * time.Minute
)

// loginAttemptState is one member's in-memory login-MFA strike state.
type loginAttemptState struct {
	failures    int
	lockedUntil time.Time
}

// loginAttemptLimiter tracks consecutive wrong login-MFA codes per member
// and enforces a backoff window after loginMFAAttemptThreshold consecutive
// failures. It is a distinct type from deeplink/adapter's perKeyLimiter:
// that is a steady-rate token bucket for a different action shape (a
// legitimate confirm tap vs. an adversary guessing a 6-digit code), while
// this is a strike counter with a hard lockout window, reset on the next
// successful verification.
//
// State is in-memory and process-lifetime (never evicted, and lost on
// restart) — an accepted tradeoff for Nestova's deployment shape, mirroring
// perKeyLimiter's own doc comment: a single-household, local-first
// appliance has a small, stable member roster, so the map can never grow
// unbounded, and "a restart clears lockouts" is no worse than the member
// simply waiting out the window.
//
// Once locked, a member stays locked until lockedUntil regardless of
// further attempts — recordFailure does not extend the window on a locked
// member, since the caller (LoginMFAHandlers) checks locked() BEFORE
// calling VerifyLoginCode at all and never reaches recordFailure again
// until the window has passed. Once it HAS passed, recordFailure resets the
// strike count before counting the new failure, so a member who waited out
// a lockout gets a full fresh run of loginMFAAttemptThreshold attempts
// rather than the counter continuing to climb from where the expired
// lockout left it.
type loginAttemptLimiter struct {
	mu    sync.Mutex
	state map[string]*loginAttemptState
}

// newLoginAttemptLimiter constructs an empty limiter.
func newLoginAttemptLimiter() *loginAttemptLimiter {
	return &loginAttemptLimiter{state: make(map[string]*loginAttemptState)}
}

// locked reports whether memberKey is currently in a backoff window as of
// now.
func (l *loginAttemptLimiter) locked(memberKey string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.state[memberKey]
	if !ok {
		return false
	}
	return now.Before(st.lockedUntil)
}

// recordFailure records a wrong code for memberKey as of now, returning
// lockedOut=true exactly once — on the attempt that CROSSES the threshold —
// so the caller enqueues exactly one lockout notification per lockout
// rather than one per subsequent attempt.
//
// If memberKey's PRIOR lockout has already expired as of now, the strike
// count is reset to zero before counting this failure: without this, a
// member who waits out a lockout and then enters one more wrong code would
// never be locked out again (failures would climb past
// loginMFAAttemptThreshold+1 without ever landing on it exactly), and the
// notification in LoginMFAHandlers would never fire a second time either.
func (l *loginAttemptLimiter) recordFailure(memberKey string, now time.Time) (lockedOut bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.state[memberKey]
	switch {
	case !ok:
		st = &loginAttemptState{}
		l.state[memberKey] = st
	case !st.lockedUntil.IsZero() && !now.Before(st.lockedUntil):
		// A previous lockout has fully expired: start counting fresh.
		st.failures = 0
		st.lockedUntil = time.Time{}
	}
	st.failures++
	if st.failures == loginMFAAttemptThreshold+1 {
		st.lockedUntil = now.Add(loginMFABackoffWindow)
		return true
	}
	return false
}

// recordSuccess clears memberKey's strike state after a successful
// verification.
func (l *loginAttemptLimiter) recordSuccess(memberKey string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.state, memberKey)
}
