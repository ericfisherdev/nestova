package adapter

import (
	"net/http"
	"strings"
	"time"

	"github.com/ericfisherdev/nestcore/httpserver/middleware"
	"github.com/ericfisherdev/nestcore/ratelimit"
)

// Login throttle tuning (NES-86). The account policy is identical to
// Nestorage's login throttle so a credential shared through the identity
// schema gets the same guarantee in both apps.
//
// The IP key counts attempts, successful ones included, and a success never
// gives one back (see recordSuccess). The IP threshold is looser because the entryway kiosk is ONE address shared
// by the whole household: a member's typos must not lock the screen for
// everyone, while twenty attempts per fifteen minutes still bounds one host
// spraying many emails.
//
// maxLoginThrottleKeys sizes both tables. An entry costs about 250 bytes, so
// 50,000 keys is about 12 MB per table. The keys are attacker-supplied (any
// submitted email, any resolved client address), and filling a table takes
// roughly this many distinct keys inside one window; because the per-IP
// limiter is consulted first, one source cannot mint distinct emails faster
// than its own threshold, so that needs on the order of this many distinct
// client addresses, i.e. tailnet peers on this appliance. See
// ratelimit.AttemptLimiter for the full threat model.
const (
	accountLoginAttempts = 5
	ipLoginAttempts      = 20
	loginLockoutWindow   = 15 * time.Minute
	maxLoginThrottleKeys = 50_000
)

// loginAdmission is the verdict of LoginThrottle.admit.
type loginAdmission struct {
	// allowed is false when the attempt must be rejected before the
	// authenticator, and therefore the password hasher, runs.
	allowed bool
	// ipLockedOut and accountLockedOut report that THIS attempt used up the
	// budget of the client address or the account, so the caller can log one
	// line per lockout. They are only meaningful when allowed is true.
	ipLockedOut      bool
	accountLockedOut bool
}

// LoginThrottle bounds password login attempts per submitted email and per
// resolved client IP (NES-86), so an unthrottled login form is neither a
// brute-force door to every shared credential nor a way to make the host run
// an unbounded number of 64 MiB Argon2id verifications.
//
// The IP dimension comes from the request, hence an adapter-layer type. The
// client IP is read through nestcore's middleware.ClientIP, which honours
// X-Forwarded-For only from TRUSTED_PROXIES peers. When that middleware has
// not run (bare handler tests) the IP is empty and every request shares one
// bucket, which fails closed, never open.
//
// Strike state is in memory and process-lifetime: a restart clears lockouts,
// which is no worse than an attacker waiting out the window.
type LoginThrottle struct {
	account *ratelimit.AttemptLimiter
	ip      *ratelimit.AttemptLimiter
}

// NewLoginThrottle constructs a LoginThrottle with the production policies.
func NewLoginThrottle() *LoginThrottle {
	return newLoginThrottle(
		ratelimit.Policy{Threshold: accountLoginAttempts, LockoutWindow: loginLockoutWindow, MaxTrackedKeys: maxLoginThrottleKeys},
		ratelimit.Policy{Threshold: ipLoginAttempts, LockoutWindow: loginLockoutWindow, MaxTrackedKeys: maxLoginThrottleKeys},
	)
}

// newLoginThrottle constructs a LoginThrottle from explicit policies, for
// tests that need tiny thresholds.
func newLoginThrottle(account, ip ratelimit.Policy) *LoginThrottle {
	return &LoginThrottle{
		account: ratelimit.NewAttemptLimiter(account),
		ip:      ratelimit.NewAttemptLimiter(ip),
	}
}

// accountKey normalises email: the credential lookup is an exact match, so
// without lower-casing and trimming every case variant would buy a fresh
// bucket and a fresh dummy verification.
func accountKey(email string) string {
	return "email:" + strings.ToLower(strings.TrimSpace(email))
}

func ipKey(r *http.Request) string {
	return "ip:" + middleware.ClientIP(r.Context())
}

// admit atomically checks and counts one password attempt.
//
// A locked account is rejected by a non-counting peek BEFORE the IP is
// touched, so a member retrying a locked account does not burn the
// household's shared address budget, and the peek creates no table entry. The
// attempt is then counted against the client IP FIRST and the account second:
// a single source locks out after its own threshold and cannot mint the many
// distinct account keys needed to flood the account table.
func (t *LoginThrottle) admit(r *http.Request, email string, now time.Time) loginAdmission {
	account := accountKey(email)
	if t.account.Locked(account, now) {
		return loginAdmission{}
	}
	ip := ipKey(r)
	ipOutcome := t.ip.Attempt(ip, now)
	if !ipOutcome.Permitted() {
		return loginAdmission{}
	}
	accountOutcome := t.account.Attempt(account, now)
	if !accountOutcome.Permitted() {
		// Only reachable when the account locked between the peek and here.
		t.ip.Refund(ip)
		return loginAdmission{}
	}
	return loginAdmission{
		allowed:          true,
		ipLockedOut:      ipOutcome == ratelimit.AllowedLastAttempt,
		accountLockedOut: accountOutcome == ratelimit.AllowedLastAttempt,
	}
}

// recordSuccess clears the ACCOUNT's strikes. The IP key is deliberately left
// alone: it counts every attempt from that address, successful or not. If a
// success cleared or refunded it, an attacker holding one valid account could
// interleave successful logins to wipe their address's strike count while
// guessing other accounts. IP strikes age out through the window instead.
func (t *LoginThrottle) recordSuccess(email string) {
	t.account.Reset(accountKey(email))
}

// recordInternalError gives back an attempt that failed for a reason that is
// not the client's fault (a repository outage), so it counts no strike.
func (t *LoginThrottle) recordInternalError(r *http.Request, email string) {
	t.account.Refund(accountKey(email))
	t.ip.Refund(ipKey(r))
}
