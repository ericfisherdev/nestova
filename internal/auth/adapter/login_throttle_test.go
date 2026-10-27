package adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"

	"github.com/ericfisherdev/nestcore/httpserver/middleware"
	"github.com/ericfisherdev/nestcore/ratelimit"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

const (
	throttleTestCSRF     = "csrf-token-for-tests"
	throttleTestPassword = "correct-horse"
	throttleTestPeer     = "192.0.2.10:4000"
	throttleTestLoopback = "127.0.0.1:4000"
	hugeBudget           = 1_000
)

// countingAuthenticator is a loginAuthenticator fake that records how many
// times the (expensive) real authenticator was reached. Only throttleTestPassword
// signs in, and only for knownEmail; every other pair, like the real
// Authenticator, collapses to ErrInvalidCredentials.
type countingAuthenticator struct {
	calls      atomic.Int64
	knownEmail string
	failWith   error
}

func (a *countingAuthenticator) Login(_ context.Context, email, password string) (household.MemberID, error) {
	a.calls.Add(1)
	if a.failWith != nil {
		return household.MemberID{}, a.failWith
	}
	if email == a.knownEmail && password == throttleTestPassword {
		return household.MemberID{}, nil
	}
	return household.MemberID{}, authdomain.ErrInvalidCredentials
}

// throttleHarness drives Handlers.Login through a real session manager with a
// fixed CSRF token seeded into every request's session.
type throttleHarness struct {
	t        *testing.T
	handler  http.Handler
	authn    *countingAuthenticator
	throttle *LoginThrottle
	logs     *bytes.Buffer
}

type loginRequest struct {
	email      string
	password   string
	remoteAddr string
	xff        string
	csrf       string
}

func policy(threshold int) ratelimit.Policy {
	return ratelimit.Policy{Threshold: threshold, LockoutWindow: loginLockoutWindow, MaxTrackedKeys: hugeBudget}
}

// newThrottleHarness builds the harness; wrap, when non-nil, is applied
// outermost (e.g. middleware.ForwardedHeaders).
func newThrottleHarness(t *testing.T, throttle *LoginThrottle, wrap func(http.Handler) http.Handler) *throttleHarness {
	t.Helper()
	sm := scs.New()
	sm.Store = memstore.New()
	sm.Cookie.Secure = false

	logs := &bytes.Buffer{}
	authn := &countingAuthenticator{knownEmail: "alice@example.com"}
	h := NewHandlers(sm, authn, nil, nil, nil, throttle, slog.New(slog.NewTextHandler(logs, nil)))

	seedCSRF := func(next http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sm.Put(r.Context(), sessionKeyCSRF, throttleTestCSRF)
			next(w, r)
		})
	}
	handler := sm.LoadAndSave(seedCSRF(h.Login))
	if wrap != nil {
		handler = wrap(handler)
	}
	return &throttleHarness{t: t, handler: handler, authn: authn, throttle: throttle, logs: logs}
}

func (h *throttleHarness) post(req loginRequest) *httptest.ResponseRecorder {
	h.t.Helper()
	if req.remoteAddr == "" {
		req.remoteAddr = throttleTestPeer
	}
	if req.csrf == "" {
		req.csrf = throttleTestCSRF
	}
	form := url.Values{"email": {req.email}, "password": {req.password}, "csrf_token": {req.csrf}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = req.remoteAddr
	if req.xff != "" {
		r.Header.Set("X-Forwarded-For", req.xff)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func (h *throttleHarness) wrong(email string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.post(loginRequest{email: email, password: "wrong-password"})
}

func assertStatus(t *testing.T, label string, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("%s: status = %d, want %d", label, w.Code, want)
	}
}

func assertThrottled(t *testing.T, label string, w *httptest.ResponseRecorder) {
	t.Helper()
	assertStatus(t, label, w, http.StatusTooManyRequests)
	if !strings.Contains(w.Body.String(), loginThrottledMessage) {
		t.Errorf("%s: body does not carry the throttled message", label)
	}
}

func assertAuthenticatorCalls(t *testing.T, h *throttleHarness, want int64) {
	t.Helper()
	if got := h.authn.calls.Load(); got != want {
		t.Errorf("authenticator reached %d times, want %d", got, want)
	}
}

func trustLoopback() func(http.Handler) http.Handler {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	return func(next http.Handler) http.Handler { return middleware.ForwardedHeaders(trusted)(next) }
}

func TestLoginThrottle_SixthWrongPasswordIsThrottledBeforeTheAuthenticator(t *testing.T) {
	h := newThrottleHarness(t, newLoginThrottle(policy(accountLoginAttempts), policy(hugeBudget)), nil)

	for i := 1; i <= accountLoginAttempts; i++ {
		assertStatus(t, "wrong password", h.wrong("alice@example.com"), http.StatusUnauthorized)
	}
	assertThrottled(t, "sixth wrong password", h.wrong("alice@example.com"))
	assertThrottled(t, "seventh wrong password", h.wrong("alice@example.com"))

	assertAuthenticatorCalls(t, h, accountLoginAttempts)
}

func TestLoginThrottle_CorrectPasswordWhileLockedIsStillRejected(t *testing.T) {
	h := newThrottleHarness(t, newLoginThrottle(policy(accountLoginAttempts), policy(hugeBudget)), nil)
	for range accountLoginAttempts {
		h.wrong("alice@example.com")
	}

	w := h.post(loginRequest{email: "alice@example.com", password: throttleTestPassword})

	assertThrottled(t, "correct password during lockout", w)
	assertAuthenticatorCalls(t, h, accountLoginAttempts)
}

func TestLoginThrottle_AccountKeyIgnoresCaseAndWhitespace(t *testing.T) {
	h := newThrottleHarness(t, newLoginThrottle(policy(accountLoginAttempts), policy(hugeBudget)), nil)
	variants := []string{"alice@example.com", "ALICE@example.com", "Alice@Example.com", "alice@example.COM", "ALICE@EXAMPLE.COM"}
	for _, email := range variants {
		assertStatus(t, email, h.wrong(email), http.StatusUnauthorized)
	}

	assertThrottled(t, "a further case variant", h.wrong("aLiCe@example.com"))
	assertAuthenticatorCalls(t, h, int64(len(variants)))
}

// TestLoginThrottle_LockoutIsNotAnAccountExistenceOracle: a real and a
// non-existent account must produce the identical status and body sequence.
func TestLoginThrottle_LockoutIsNotAnAccountExistenceOracle(t *testing.T) {
	sequence := func(email string) (statuses []int, bodies []string) {
		h := newThrottleHarness(t, newLoginThrottle(policy(accountLoginAttempts), policy(hugeBudget)), nil)
		for range accountLoginAttempts + 3 {
			w := h.wrong(email)
			statuses = append(statuses, w.Code)
			// The login form echoes the submitted email back; mask it so the
			// comparison sees only what could differ by account existence.
			bodies = append(bodies, strings.ReplaceAll(w.Body.String(), email, "EMAIL"))
		}
		return statuses, bodies
	}

	realStatuses, realBodies := sequence("alice@example.com")
	ghostStatuses, ghostBodies := sequence("ghost@example.com")

	for i := range realStatuses {
		if realStatuses[i] != ghostStatuses[i] {
			t.Errorf("attempt %d: status %d for a real account vs %d for a ghost", i+1, realStatuses[i], ghostStatuses[i])
		}
		if realBodies[i] != ghostBodies[i] {
			t.Errorf("attempt %d: bodies differ between a real account and a ghost", i+1)
		}
	}
}

func TestLoginThrottle_SuccessResetsTheAccountStrikes(t *testing.T) {
	h := newThrottleHarness(t, newLoginThrottle(policy(accountLoginAttempts), policy(hugeBudget)), nil)
	for range accountLoginAttempts - 1 {
		h.wrong("alice@example.com")
	}

	assertStatus(t, "correct password", h.post(loginRequest{email: "alice@example.com", password: throttleTestPassword}), http.StatusSeeOther)

	for i := 1; i <= accountLoginAttempts; i++ {
		assertStatus(t, "wrong password after the reset", h.wrong("alice@example.com"), http.StatusUnauthorized)
	}
	assertThrottled(t, "first attempt past the fresh budget", h.wrong("alice@example.com"))
}

func TestLoginThrottle_IPLimitThrottlesDistinctEmailsFromOneAddress(t *testing.T) {
	const ipBudget = 20
	h := newThrottleHarness(t, newLoginThrottle(policy(hugeBudget), policy(ipBudget)), nil)

	for i := range ipBudget {
		assertStatus(t, "distinct email", h.wrong(emailN(i)), http.StatusUnauthorized)
	}
	assertThrottled(t, "next distinct email from the same address", h.wrong(emailN(ipBudget)))
	assertAuthenticatorCalls(t, h, ipBudget)
}

// TestLoginThrottle_IPIsConsultedBeforeTheAccount proves the ordering that
// bounds a flood of distinct account keys: once a source is IP-throttled, its
// further emails never reach (and never occupy slots in) the account limiter.
func TestLoginThrottle_IPIsConsultedBeforeTheAccount(t *testing.T) {
	const ipBudget, emails = 3, 53
	// An account budget of one locks a key on its first admitted attempt, so
	// Locked tells which emails the account limiter ever saw.
	h := newThrottleHarness(t, newLoginThrottle(policy(1), policy(ipBudget)), nil)

	for i := range emails {
		h.wrong(emailN(i))
	}

	now := time.Now()
	for i := range emails {
		seen := h.throttle.account.Locked(accountKey(emailN(i)), now)
		if want := i < ipBudget; seen != want {
			t.Errorf("email %d reached the account limiter = %v, want %v (the IP limiter must gate first)", i, seen, want)
		}
	}
}

func TestLoginThrottle_RetryingALockedAccountDoesNotBurnTheIPBudget(t *testing.T) {
	h := newThrottleHarness(t, newLoginThrottle(policy(2), policy(3)), nil)
	h.wrong("alice@example.com")
	h.wrong("alice@example.com")
	for range 10 {
		assertThrottled(t, "locked account retry", h.wrong("alice@example.com"))
	}

	assertStatus(t, "another member from the same address", h.wrong("bob@example.com"), http.StatusUnauthorized)
}

// TestLoginThrottle_SuccessNeverWipesTheIPStrikes: an attacker holding one
// valid account must not be able to interleave successful logins to reset
// their address's strike count while guessing other accounts.
func TestLoginThrottle_SuccessNeverWipesTheIPStrikes(t *testing.T) {
	h := newThrottleHarness(t, newLoginThrottle(policy(hugeBudget), policy(3)), nil)
	valid := loginRequest{email: "alice@example.com", password: throttleTestPassword}
	assertStatus(t, "guess #1", h.wrong(emailN(1)), http.StatusUnauthorized)
	assertStatus(t, "guess #2", h.wrong(emailN(2)), http.StatusUnauthorized)

	// The success is itself the address's third attempt; it must not give
	// the earlier strikes back.
	assertStatus(t, "interleaved success", h.post(valid), http.StatusSeeOther)

	assertThrottled(t, "guess #3 after the success", h.wrong(emailN(3)))
	assertThrottled(t, "a second success", h.post(valid))
}

func TestLoginThrottle_ForwardedForFromATrustedProxyGivesIndependentBuckets(t *testing.T) {
	h := newThrottleHarness(t, newLoginThrottle(policy(hugeBudget), policy(2)), trustLoopback())
	post := func(xff string, i int) *httptest.ResponseRecorder {
		return h.post(loginRequest{email: emailN(i), password: "x", remoteAddr: throttleTestLoopback, xff: xff})
	}

	assertStatus(t, "client A #1", post("203.0.113.1", 1), http.StatusUnauthorized)
	assertStatus(t, "client A #2", post("203.0.113.1", 2), http.StatusUnauthorized)
	assertThrottled(t, "client A #3", post("203.0.113.1", 3))

	assertStatus(t, "client B #1", post("203.0.113.2", 4), http.StatusUnauthorized)
}

func TestLoginThrottle_ForwardedForFromAnUntrustedPeerIsIgnored(t *testing.T) {
	h := newThrottleHarness(t, newLoginThrottle(policy(hugeBudget), policy(2)), trustLoopback())
	post := func(xff string, i int) *httptest.ResponseRecorder {
		return h.post(loginRequest{email: emailN(i), password: "x", remoteAddr: throttleTestPeer, xff: xff})
	}

	assertStatus(t, "#1", post("203.0.113.1", 1), http.StatusUnauthorized)
	assertStatus(t, "#2", post("203.0.113.2", 2), http.StatusUnauthorized)

	assertThrottled(t, "#3 with a spoofed fresh address", post("203.0.113.3", 3))
}

func TestLoginThrottle_CSRFFailureCountsNoStrike(t *testing.T) {
	h := newThrottleHarness(t, newLoginThrottle(policy(accountLoginAttempts), policy(accountLoginAttempts)), nil)
	for range accountLoginAttempts * 3 {
		w := h.post(loginRequest{email: "alice@example.com", password: "x", csrf: "forged"})
		assertStatus(t, "forged CSRF", w, http.StatusForbidden)
	}

	assertStatus(t, "first real attempt", h.wrong("alice@example.com"), http.StatusUnauthorized)
	assertAuthenticatorCalls(t, h, 1)
}

func TestLoginThrottle_InternalAuthenticatorErrorCountsNoStrike(t *testing.T) {
	h := newThrottleHarness(t, newLoginThrottle(policy(accountLoginAttempts), policy(accountLoginAttempts)), nil)
	h.authn.failWith = errors.New("repository outage")
	for range accountLoginAttempts * 3 {
		assertStatus(t, "authenticator outage", h.wrong("alice@example.com"), http.StatusInternalServerError)
	}
	h.authn.failWith = nil

	for i := 1; i <= accountLoginAttempts; i++ {
		assertStatus(t, "wrong password after the outage", h.wrong("alice@example.com"), http.StatusUnauthorized)
	}
	assertThrottled(t, "past the budget", h.wrong("alice@example.com"))
}

// TestLoginThrottle_InFlightInternalErrorsNeverEngageALockout covers requests
// that are admitted concurrently, before any of them has failed: the last one
// to be admitted engages the lockout, and when every one of them then hits an
// internal error, none should have counted.
func TestLoginThrottle_InFlightInternalErrorsNeverEngageALockout(t *testing.T) {
	const budget = 3
	th := newLoginThrottle(policy(budget), policy(budget))
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	now := time.Now()

	var inFlight []loginAdmission
	for range budget {
		a := th.admit(r, "alice@example.com", now)
		if !a.allowed {
			t.Fatal("setup: attempt within the budget was not admitted")
		}
		inFlight = append(inFlight, a)
	}
	for _, a := range inFlight {
		th.recordInternalError(a)
	}

	if th.account.Locked(accountKey("alice@example.com"), now) {
		t.Error("account locked by attempts that all ended in internal errors")
	}
	if th.ip.Locked(ipKey(r), now) {
		t.Error("client IP locked by attempts that all ended in internal errors")
	}
}

// TestLoginThrottle_InternalErrorOfAnEarlierAttemptDoesNotUndoALockout: the
// attempt that engaged the lockout failed for real, so a different attempt's
// internal error must not lift it.
func TestLoginThrottle_InternalErrorOfAnEarlierAttemptDoesNotUndoALockout(t *testing.T) {
	const budget = 3
	th := newLoginThrottle(policy(budget), policy(hugeBudget))
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	now := time.Now()

	earlier := th.admit(r, "alice@example.com", now)
	th.admit(r, "alice@example.com", now)
	th.admit(r, "alice@example.com", now) // engages the lockout

	th.recordInternalError(earlier)

	if !th.account.Locked(accountKey("alice@example.com"), now) {
		t.Error("an unrelated internal error lifted the account lockout")
	}
}

func TestAccountKey_IsAFixedSizeDigestOfTheNormalisedEmail(t *testing.T) {
	huge := strings.Repeat("a", 1<<20) + "@example.com"

	if got, want := len(accountKey(huge)), len(accountKey("a@b.c")); got != want {
		t.Errorf("key length for a 1 MiB email = %d, want the fixed %d", got, want)
	}
	if accountKey("  Alice@Example.COM ") != accountKey("alice@example.com") {
		t.Error("case and whitespace variants must share a key")
	}
	if accountKey("alice@example.com") == accountKey("bob@example.com") {
		t.Error("distinct emails must not share a key")
	}
	if strings.Contains(accountKey("alice@example.com"), "alice") {
		t.Error("the key must not retain the email (PII, unbounded size)")
	}
}

func TestLoginThrottle_LockoutIsLoggedOnceWithoutTheEmail(t *testing.T) {
	h := newThrottleHarness(t, newLoginThrottle(policy(accountLoginAttempts), policy(hugeBudget)), nil)
	for range accountLoginAttempts + 5 {
		h.wrong("alice@example.com")
	}

	logs := h.logs.String()
	if got := strings.Count(logs, "login throttle engaged"); got != 1 {
		t.Errorf("lockout logged %d times, want exactly once; logs: %s", got, logs)
	}
	if strings.Contains(logs, "alice@example.com") {
		t.Errorf("lockout log leaks the email (PII): %s", logs)
	}
	if !strings.Contains(logs, "account_locked=true") {
		t.Errorf("lockout log does not name the locked dimension: %s", logs)
	}
}

// TestLoginThrottle_IPLockoutEngagedByASuccessIsStillLogged: a success counts
// against the IP, so the attempt that exhausts the IP budget may be a correct
// password. The lockout then stands and an operator must be able to see it.
func TestLoginThrottle_IPLockoutEngagedByASuccessIsStillLogged(t *testing.T) {
	h := newThrottleHarness(t, newLoginThrottle(policy(hugeBudget), policy(2)), nil)
	h.wrong(emailN(1))

	assertStatus(t, "success", h.post(loginRequest{email: "alice@example.com", password: throttleTestPassword}), http.StatusSeeOther)

	logs := h.logs.String()
	if got := strings.Count(logs, "login throttle engaged"); got != 1 {
		t.Fatalf("lockout logged %d times, want once; logs: %s", got, logs)
	}
	if !strings.Contains(logs, "ip_locked=true") || !strings.Contains(logs, "account_locked=false") {
		t.Errorf("log should name only the IP dimension: %s", logs)
	}
	assertThrottled(t, "next attempt", h.wrong(emailN(2)))
}

func TestLoginThrottle_BareHandlerSharesOneIPBucket(t *testing.T) {
	// ForwardedHeaders never ran, so ClientIP is empty: every request lands in
	// one bucket, which fails closed rather than open.
	h := newThrottleHarness(t, newLoginThrottle(policy(hugeBudget), policy(2)), nil)

	assertStatus(t, "#1", h.post(loginRequest{email: emailN(1), password: "x", remoteAddr: "192.0.2.1:1"}), http.StatusUnauthorized)
	assertStatus(t, "#2", h.post(loginRequest{email: emailN(2), password: "x", remoteAddr: "198.51.100.1:1"}), http.StatusUnauthorized)
	assertThrottled(t, "#3 from a third address", h.post(loginRequest{email: emailN(3), password: "x", remoteAddr: "203.0.113.1:1"}))
}

func TestNewHandlers_NilThrottlePanics(t *testing.T) {
	sm := scs.New()
	sm.Store = memstore.New()
	defer func() {
		if recover() == nil {
			t.Error("NewHandlers did not panic on a nil LoginThrottle")
		}
	}()
	NewHandlers(sm, &countingAuthenticator{}, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestNewLoginThrottle_UsesTheDocumentedPolicies(t *testing.T) {
	th := NewLoginThrottle()
	now := time.Now()
	for range accountLoginAttempts {
		th.account.Attempt("k", now)
	}
	if !th.account.Locked("k", now) {
		t.Errorf("account limiter did not lock after %d attempts", accountLoginAttempts)
	}
	for range ipLoginAttempts - 1 {
		th.ip.Attempt("k", now)
	}
	if th.ip.Locked("k", now) {
		t.Errorf("ip limiter locked before %d attempts", ipLoginAttempts)
	}
	th.ip.Attempt("k", now)
	if !th.ip.Locked("k", now) {
		t.Errorf("ip limiter did not lock after %d attempts", ipLoginAttempts)
	}
}

func emailN(i int) string { return fmt.Sprintf("user-%d@example.com", i) }
