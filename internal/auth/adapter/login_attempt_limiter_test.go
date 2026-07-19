package adapter

import (
	"testing"
	"time"
)

func TestLoginAttemptLimiter_AllowsUpToThreshold(t *testing.T) {
	l := newLoginAttemptLimiter()
	now := time.Now()
	const memberKey = "member-a"

	for i := 0; i < loginMFAAttemptThreshold; i++ {
		if l.locked(memberKey, now) {
			t.Fatalf("locked after %d failures, want not locked until the (threshold+1)th", i)
		}
		if lockedOut := l.recordFailure(memberKey, now); lockedOut {
			t.Fatalf("recordFailure #%d reported lockedOut=true, want false (threshold not yet crossed)", i+1)
		}
	}
	if l.locked(memberKey, now) {
		t.Error("locked after exactly threshold failures, want still not locked")
	}
}

func TestLoginAttemptLimiter_LocksOnThresholdPlusOne(t *testing.T) {
	l := newLoginAttemptLimiter()
	now := time.Now()
	const memberKey = "member-a"

	for i := 0; i < loginMFAAttemptThreshold; i++ {
		l.recordFailure(memberKey, now)
	}
	lockedOut := l.recordFailure(memberKey, now)
	if !lockedOut {
		t.Fatal("recordFailure on the (threshold+1)th attempt reported lockedOut=false, want true")
	}
	if !l.locked(memberKey, now) {
		t.Error("member must be locked immediately after crossing the threshold")
	}
}

func TestLoginAttemptLimiter_ReportsLockedOutExactlyOnce(t *testing.T) {
	l := newLoginAttemptLimiter()
	now := time.Now()
	const memberKey = "member-a"

	for i := 0; i < loginMFAAttemptThreshold; i++ {
		l.recordFailure(memberKey, now)
	}
	if lockedOut := l.recordFailure(memberKey, now); !lockedOut {
		t.Fatal("the crossing attempt must report lockedOut=true")
	}
	if lockedOut := l.recordFailure(memberKey, now); lockedOut {
		t.Error("a SUBSEQUENT failure while already locked must not report lockedOut=true again")
	}
}

func TestLoginAttemptLimiter_UnlocksAfterWindow(t *testing.T) {
	l := newLoginAttemptLimiter()
	now := time.Now()
	const memberKey = "member-a"

	for i := 0; i <= loginMFAAttemptThreshold; i++ {
		l.recordFailure(memberKey, now)
	}
	if !l.locked(memberKey, now) {
		t.Fatal("member must be locked immediately after crossing the threshold")
	}
	after := now.Add(loginMFABackoffWindow + time.Second)
	if l.locked(memberKey, after) {
		t.Error("member must not be locked once the backoff window has elapsed")
	}
}

func TestLoginAttemptLimiter_RecordSuccessClearsState(t *testing.T) {
	l := newLoginAttemptLimiter()
	now := time.Now()
	const memberKey = "member-a"

	for i := 0; i < loginMFAAttemptThreshold-1; i++ {
		l.recordFailure(memberKey, now)
	}
	l.recordSuccess(memberKey)

	// After a reset, it must take a FULL fresh run of threshold+1 failures
	// to lock out again — the prior near-threshold count must not carry
	// over.
	for i := 0; i < loginMFAAttemptThreshold; i++ {
		if lockedOut := l.recordFailure(memberKey, now); lockedOut {
			t.Fatalf("recordFailure #%d after a reset reported lockedOut=true too early", i+1)
		}
	}
}

// TestLoginAttemptLimiter_ExpiredLockoutResetsStrikeCount covers the fix for
// a bug where an expired lockout's strike count kept accumulating instead
// of resetting: a member who waits out a lockout and then enters a few more
// wrong codes must get a FULL fresh run of loginMFAAttemptThreshold
// attempts before locking out again — not a lockout on the very next wrong
// code, and not permanently unlockable because the counter had already
// climbed past the exact threshold+1 value the old code checked for.
func TestLoginAttemptLimiter_ExpiredLockoutResetsStrikeCount(t *testing.T) {
	tests := []struct {
		name            string
		freshFailures   int // failures recorded AFTER the first lockout expires
		wantFinalLocked bool
	}{
		{name: "a single fresh failure after expiry does not relock", freshFailures: 1, wantFinalLocked: false},
		{name: "exactly threshold fresh failures after expiry do not relock", freshFailures: loginMFAAttemptThreshold, wantFinalLocked: false},
		{name: "threshold+1 fresh failures after expiry relocks", freshFailures: loginMFAAttemptThreshold + 1, wantFinalLocked: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newLoginAttemptLimiter()
			now := time.Now()
			const memberKey = "member-a"

			// Drive the member into a FIRST lockout.
			for i := 0; i <= loginMFAAttemptThreshold; i++ {
				l.recordFailure(memberKey, now)
			}
			if !l.locked(memberKey, now) {
				t.Fatal("setup: expected member to be locked after crossing the threshold")
			}

			// Wait out the backoff window entirely.
			afterWindow := now.Add(loginMFABackoffWindow + time.Second)
			if l.locked(memberKey, afterWindow) {
				t.Fatal("setup: expected member to be unlocked once the backoff window has elapsed")
			}

			// Record tt.freshFailures wrong codes post-expiry; the LAST
			// call's lockedOut result and the final locked() state must
			// reflect a FRESH count, not one carried over from before expiry.
			var lastLockedOut bool
			for i := 0; i < tt.freshFailures; i++ {
				lastLockedOut = l.recordFailure(memberKey, afterWindow)
			}
			if got := l.locked(memberKey, afterWindow); got != tt.wantFinalLocked {
				t.Errorf("locked() after %d fresh failures post-expiry = %v, want %v (strike count did not reset on expiry)", tt.freshFailures, got, tt.wantFinalLocked)
			}
			if tt.wantFinalLocked && !lastLockedOut {
				t.Error("expected the fresh failure that crossed the threshold to report lockedOut=true")
			}
		})
	}
}

func TestLoginAttemptLimiter_MembersAreIndependent(t *testing.T) {
	l := newLoginAttemptLimiter()
	now := time.Now()

	for i := 0; i <= loginMFAAttemptThreshold; i++ {
		l.recordFailure("member-a", now)
	}
	if !l.locked("member-a", now) {
		t.Fatal("member-a must be locked")
	}
	if l.locked("member-b", now) {
		t.Error("member-b must be unaffected by member-a's lockout")
	}
}
