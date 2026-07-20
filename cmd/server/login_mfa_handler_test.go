package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/crypto/cryptotest"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	"github.com/ericfisherdev/nestova/internal/platform/totp"
)

// ---------------------------------------------------------------------------
// Test harness for the pre-auth login MFA flow (NES-135): password login,
// the /login/mfa hand-off, the remember-device cookie, attempt limiting, and
// RequireStepUp gating a stand-in security-sensitive route.
// ---------------------------------------------------------------------------

// loginTestCredRepo is an in-memory authdomain.CredentialRepository keyed by
// BOTH email and member id, so it supports the REAL password-login path
// (FindByEmail) that this file's tests drive end to end — unlike testCredRepo
// (home_test.go) and fakeMemberCredRepo (mfa_settings_handler_test.go), which
// are built for harnesses that bypass real login via seedAuthedSession.
type loginTestCredRepo struct {
	byEmail    map[string]*authdomain.Credential
	byMemberID map[household.MemberID]*authdomain.Credential
}

func newLoginTestCredRepo() *loginTestCredRepo {
	return &loginTestCredRepo{
		byEmail:    make(map[string]*authdomain.Credential),
		byMemberID: make(map[household.MemberID]*authdomain.Credential),
	}
}

func (r *loginTestCredRepo) seed(t *testing.T, memberID household.MemberID, email, password string) {
	t.Helper()
	hash, err := crypto.Hash(password)
	if err != nil {
		t.Fatalf("crypto.Hash: %v", err)
	}
	cred := &authdomain.Credential{MemberID: memberID, PasswordHash: hash}
	r.byEmail[email] = cred
	r.byMemberID[memberID] = cred
}

func (r *loginTestCredRepo) FindByEmail(_ context.Context, email string) (*authdomain.Credential, error) {
	c, ok := r.byEmail[email]
	if !ok {
		return nil, authdomain.ErrInvalidCredentials
	}
	return c, nil
}

func (r *loginTestCredRepo) FindByMemberID(_ context.Context, memberID household.MemberID) (*authdomain.Credential, error) {
	c, ok := r.byMemberID[memberID]
	if !ok {
		return nil, authdomain.ErrInvalidCredentials
	}
	return c, nil
}

func (r *loginTestCredRepo) SetPassword(_ context.Context, memberID household.MemberID, email, hash string) error {
	cred := &authdomain.Credential{MemberID: memberID, PasswordHash: hash}
	r.byEmail[email] = cred
	r.byMemberID[memberID] = cred
	return nil
}

var _ authdomain.CredentialRepository = (*loginTestCredRepo)(nil)

// recordingEnqueuer is a notifydomain.Enqueuer fake that records every
// enqueued notification, for asserting the login-MFA lockout notification
// (NES-135) fires exactly once per lockout.
type recordingEnqueuer struct {
	mu     sync.Mutex
	events []*notifydomain.Notification
}

func (r *recordingEnqueuer) Enqueue(_ context.Context, n *notifydomain.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, n)
	return nil
}

func (r *recordingEnqueuer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

const loginMFATestPassword = "correct-horse-battery-staple"

// buildLoginMFATestHandler wires just enough of the server to exercise the
// login MFA flow end to end: real Handlers.Login + LoginMFAHandlers against
// an in-memory session store, plus a minimal stand-in for a step-up-gated
// route (POST /settings/kiosk/generate is the real ticket's only consumer,
// but pulling in the entire kiosk section's dependency graph here would
// test kiosk wiring, not RequireStepUp itself — this narrow stand-in keeps
// the harness focused, mirroring buildSettingsTestHandler/
// buildKioskTestHandler's own scoped-harness convention).
func buildLoginMFATestHandler(t *testing.T, hhRepo household.HouseholdRepository, credRepo *loginTestCredRepo, notify notifydomain.Enqueuer) (http.Handler, *scs.SessionManager, *authapp.MFAService) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()

	cipher, err := crypto.NewCipher([]byte("login-mfa-test-harness-cipher-32"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	mfaService, err := authapp.NewMFAService(newFakeMFARepo(), cipher, totp.NewProvider(), credRepo, hhRepo, cryptotest.Hasher(), logger)
	if err != nil {
		t.Fatalf("NewMFAService: %v", err)
	}
	rememberSigner, err := authapp.NewRememberDeviceSigner([]byte("login-mfa-test-harness-remember-key"))
	if err != nil {
		t.Fatalf("NewRememberDeviceSigner: %v", err)
	}
	authn := authapp.New(credRepo, cryptotest.Hasher())
	authHandlers := authadapter.NewHandlers(sm, authn, mfaService, rememberSigner, nil, logger)
	loginMFAHandlers := authadapter.NewLoginMFAHandlers(sm, mfaService, rememberSigner, nil, notify, false, logger)

	requireMember := authadapter.RequireMember(sm)
	requireStepUp := authadapter.RequireStepUp(sm, mfaService, nil, "/settings", logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	mux.HandleFunc("POST /login", authHandlers.Login)
	mux.HandleFunc("POST /logout", authHandlers.Logout)
	mux.HandleFunc("GET /login/mfa", loginMFAHandlers.Page)
	mux.HandleFunc("POST /login/mfa", loginMFAHandlers.Verify)
	mux.Handle("GET /settings", requireMember(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("settings page"))
	})))
	// Stands in for POST /settings/kiosk/generate (home.go's
	// registerSettingsPage): a security-sensitive mutation gated by
	// RequireStepUp on top of RequireMember.
	mux.Handle("POST /settings/kiosk/generate", requireMember(requireStepUp(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("generated"))
	}))))

	handler := sm.LoadAndSave(authadapter.Authenticate(sm, hhRepo)(mux))
	return handler, sm, mfaService
}

// loginFlow bundles the mutable state (cookie jar of one, CSRF token) an
// end-to-end login test thread needs, updating both after every request —
// scs's RenewToken issues a NEW session cookie value on privilege
// escalation, so a real client's cookie jar (which this emulates) must pick
// up the latest Set-Cookie on every hop.
type loginFlow struct {
	t       *testing.T
	handler http.Handler
	cookie  string
	csrf    string
}

func newLoginFlow(t *testing.T, handler http.Handler) *loginFlow {
	t.Helper()
	f := &loginFlow{t: t, handler: handler}
	rec := f.do(http.MethodGet, "/login", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login: status = %d, want 200", rec.Code)
	}
	f.absorb(rec)
	return f
}

// do issues a form-encoded request (GET when body is empty) carrying the
// flow's current cookie, WITHOUT updating the flow's state — callers use
// absorb (or absorbFrom) explicitly so intermediate inspection is possible.
func (f *loginFlow) do(method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if f.cookie != "" {
		req.Header.Set("Cookie", f.cookie)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// absorb updates the flow's cookie (if the response set a new one) and CSRF
// token (if the response body carries a login-style hidden csrf_token
// field) from rec.
func (f *loginFlow) absorb(rec *httptest.ResponseRecorder) {
	f.t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "session" {
			f.cookie = c.Name + "=" + c.Value
		}
	}
	// Login pages key the CSRF field by name (not id), so it is parsed
	// directly here rather than via extractInputValue (kiosk_handler_test.go),
	// which is id-keyed.
	body := rec.Body.String()
	const marker = `name="csrf_token"`
	tokenStart := strings.Index(body, marker)
	if tokenStart < 0 {
		return
	}
	valStart := strings.Index(body[tokenStart:], `value="`)
	if valStart < 0 {
		return
	}
	s := body[tokenStart+valStart+len(`value="`):]
	end := strings.Index(s, `"`)
	if end < 0 {
		return
	}
	f.csrf = s[:end]
}

// getRememberCookie returns the current remember-device cookie value carried
// in the flow's cookie header, if any.
func (f *loginFlow) getRememberCookie() (string, bool) {
	if f.cookie == "" {
		return "", false
	}
	for _, part := range strings.Split(f.cookie, "; ") {
		name, value, ok := strings.Cut(part, "=")
		if ok && name == authadapter.RememberDeviceCookieName {
			return value, true
		}
	}
	return "", false
}

// absorbRememberCookie appends name=value from rec's Set-Cookie headers into
// the flow's outgoing cookie header, simulating a real browser's cookie jar
// picking up the remember-device cookie alongside the session cookie.
func (f *loginFlow) absorbRememberCookie(rec *httptest.ResponseRecorder) {
	f.t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == authadapter.RememberDeviceCookieName {
			if c.MaxAge < 0 {
				continue
			}
			f.cookie = f.cookie + "; " + c.Name + "=" + c.Value
		}
	}
}

// login POSTs email/password/next through /login and absorbs the response's
// cookie + any re-rendered CSRF token, returning the recorder for the
// caller to inspect (status, Location, body).
func (f *loginFlow) login(email, password, next string) *httptest.ResponseRecorder {
	f.t.Helper()
	form := url.Values{
		"csrf_token": {f.csrf},
		"email":      {email},
		"password":   {password},
		"next":       {next},
	}
	rec := f.do(http.MethodPost, "/login", form.Encode())
	f.absorb(rec)
	f.absorbRememberCookie(rec)
	return rec
}

// followRedirect issues a GET against rec's Location header (a same-origin
// path), absorbing the response.
func (f *loginFlow) followRedirect(rec *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	f.t.Helper()
	loc := rec.Header().Get("Location")
	if loc == "" {
		f.t.Fatal("followRedirect: response has no Location header")
	}
	next := f.do(http.MethodGet, loc, "")
	f.absorb(next)
	return next
}

// verifyMFA POSTs a TOTP/recovery code (and optional remember_device flag)
// through /login/mfa, absorbing the response.
func (f *loginFlow) verifyMFA(totpCode, recoveryCode, next string, remember bool) *httptest.ResponseRecorder {
	f.t.Helper()
	form := url.Values{
		"csrf_token": {f.csrf},
		"code":       {totpCode},
		"next":       {next},
	}
	if recoveryCode != "" {
		form.Set("recovery_code", recoveryCode)
	}
	if remember {
		form.Set("remember_device", "1")
	}
	rec := f.do(http.MethodPost, "/login/mfa", form.Encode())
	f.absorb(rec)
	f.absorbRememberCookie(rec)
	return rec
}

// ---------------------------------------------------------------------------
// AC: "A member with confirmed MFA cannot obtain a session with password
// alone; a member without MFA logs in unchanged."
// ---------------------------------------------------------------------------

func TestLoginMFA_PasswordOnlyMember_LogsInUnchanged(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	credRepo.seed(t, member.ID, "adult@example.com", loginMFATestPassword)
	handler, _, _ := buildLoginMFATestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})

	flow := newLoginFlow(t, handler)
	rec := flow.login("adult@example.com", loginMFATestPassword, "/settings")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login (no MFA enrolled): status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/settings" {
		t.Errorf("POST /login (no MFA enrolled): Location = %q, want /settings", loc)
	}

	// The session must already carry member_id — no /login/mfa hand-off.
	settingsRec := flow.followRedirect(rec)
	if settingsRec.Code != http.StatusOK {
		t.Fatalf("GET /settings immediately after password login: status = %d, want 200", settingsRec.Code)
	}
}

func TestLoginMFA_EnrolledMember_PasswordAloneDoesNotAuthenticate(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	credRepo.seed(t, member.ID, "adult@example.com", loginMFATestPassword)
	handler, _, mfaService := buildLoginMFATestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})

	secret := enrollMemberMFA(t, mfaService, member)

	flow := newLoginFlow(t, handler)
	rec := flow.login("adult@example.com", loginMFATestPassword, "/settings")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login (MFA enrolled): status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login/mfa") {
		t.Fatalf("POST /login (MFA enrolled): Location = %q, want a /login/mfa redirect", loc)
	}

	// The password-verified session must NOT yet be authenticated: hitting
	// a protected route now must still be denied.
	deniedRec := flow.do(http.MethodGet, "/settings", "")
	if deniedRec.Code == http.StatusOK {
		t.Error("a member with confirmed MFA must not be authenticated by password alone")
	}

	mfaPageRec := flow.followRedirect(rec)
	if mfaPageRec.Code != http.StatusOK {
		t.Fatalf("GET /login/mfa: status = %d, want 200", mfaPageRec.Code)
	}

	code := computeTOTPCode(t, secret)
	verifyRec := flow.verifyMFA(code, "", "/settings", false)
	if verifyRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login/mfa (correct code): status = %d, want 303; body: %s", verifyRec.Code, verifyRec.Body.String())
	}
	settingsRec := flow.followRedirect(verifyRec)
	if settingsRec.Code != http.StatusOK {
		t.Fatalf("GET /settings after completing login MFA: status = %d, want 200", settingsRec.Code)
	}
}

// ---------------------------------------------------------------------------
// AC: "A TOTP code cannot be used twice; codes outside the skew window
// fail."
// ---------------------------------------------------------------------------

func TestLoginMFA_TOTPCodeCannotBeReplayed(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	credRepo.seed(t, member.ID, "adult@example.com", loginMFATestPassword)
	handler, _, mfaService := buildLoginMFATestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})
	secret := enrollMemberMFA(t, mfaService, member)

	code := computeTOTPCode(t, secret)

	flow1 := newLoginFlow(t, handler)
	flow1.login("adult@example.com", loginMFATestPassword, "/settings")
	firstVerify := flow1.verifyMFA(code, "", "/settings", false)
	if firstVerify.Code != http.StatusSeeOther {
		t.Fatalf("first use of a valid code: status = %d, want 303; body: %s", firstVerify.Code, firstVerify.Body.String())
	}

	// A SECOND, independent login attempt replaying the SAME code must be
	// rejected — the replay guard is durable across sessions, not merely
	// per-session.
	flow2 := newLoginFlow(t, handler)
	flow2.login("adult@example.com", loginMFATestPassword, "/settings")
	replayRec := flow2.verifyMFA(code, "", "/settings", false)
	if replayRec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed code: status = %d, want 401; body: %s", replayRec.Code, replayRec.Body.String())
	}
	if !strings.Contains(replayRec.Body.String(), "could not be verified") {
		t.Error("replayed code response missing the generic inline error")
	}
}

// ---------------------------------------------------------------------------
// AC: "Recovery-code login works end-to-end and consumes the code."
// ---------------------------------------------------------------------------

func TestLoginMFA_RecoveryCode_WorksAndIsConsumed(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	credRepo.seed(t, member.ID, "adult@example.com", loginMFATestPassword)
	handler, _, mfaService := buildLoginMFATestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})
	_, recoveryCodes := enrollMemberMFAWithRecoveryCodes(t, mfaService, member)

	flow := newLoginFlow(t, handler)
	flow.login("adult@example.com", loginMFATestPassword, "/settings")
	rec := flow.verifyMFA("", recoveryCodes[0], "/settings", false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login/mfa with a recovery code: status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}

	// The SAME recovery code must not work on a second, independent login.
	flow2 := newLoginFlow(t, handler)
	flow2.login("adult@example.com", loginMFATestPassword, "/settings")
	reuseRec := flow2.verifyMFA("", recoveryCodes[0], "/settings", false)
	if reuseRec.Code != http.StatusUnauthorized {
		t.Fatalf("reusing a login recovery code: status = %d, want 401; body: %s", reuseRec.Code, reuseRec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// AC: "Six rapid wrong codes trigger backoff and notify the member through
// the outbox."
// ---------------------------------------------------------------------------

func TestLoginMFA_SixWrongCodes_LocksOutAndNotifies(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	credRepo.seed(t, member.ID, "adult@example.com", loginMFATestPassword)
	notify := &recordingEnqueuer{}
	handler, _, mfaService := buildLoginMFATestHandler(t, hhRepo, credRepo, notify)
	secret := enrollMemberMFA(t, mfaService, member)

	flow := newLoginFlow(t, handler)
	flow.login("adult@example.com", loginMFATestPassword, "/settings")

	// Five wrong codes: rejected, but no lockout yet.
	for i := 0; i < 5; i++ {
		rec := flow.verifyMFA("000000", "", "/settings", false)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong code #%d: status = %d, want 401", i+1, rec.Code)
		}
	}
	if notify.count() != 0 {
		t.Fatalf("notifications after 5 wrong codes = %d, want 0 (lockout not yet reached)", notify.count())
	}

	// The 6th wrong code crosses the threshold: locked out AND notified.
	sixth := flow.verifyMFA("000000", "", "/settings", false)
	if sixth.Code != http.StatusUnauthorized {
		t.Fatalf("6th wrong code: status = %d, want 401", sixth.Code)
	}
	if notify.count() != 1 {
		t.Fatalf("notifications after 6 wrong codes = %d, want exactly 1", notify.count())
	}

	// A 7th attempt — even with the CORRECT code — must be rejected while
	// locked out.
	correctCode := computeTOTPCode(t, secret)
	seventh := flow.verifyMFA(correctCode, "", "/settings", false)
	if seventh.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt while locked out (even with a correct code): status = %d, want 429; body: %s", seventh.Code, seventh.Body.String())
	}
	if !strings.Contains(seventh.Body.String(), "Too many incorrect codes") {
		t.Error("locked-out response missing the backoff message")
	}
}

// ---------------------------------------------------------------------------
// AC: "A remembered device skips the login code for 30 days but still gets
// prompted for step-up actions."
// ---------------------------------------------------------------------------

func TestLoginMFA_RememberedDevice_SkipsPromptButStillStepsUp(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	credRepo.seed(t, member.ID, "adult@example.com", loginMFATestPassword)
	handler, _, mfaService := buildLoginMFATestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})
	secret, recoveryCodes := enrollMemberMFAWithRecoveryCodes(t, mfaService, member)

	// First login: verify the code AND check "remember this device".
	flow := newLoginFlow(t, handler)
	flow.login("adult@example.com", loginMFATestPassword, "/settings")
	code := computeTOTPCode(t, secret)
	verifyRec := flow.verifyMFA(code, "", "/settings", true)
	if verifyRec.Code != http.StatusSeeOther {
		t.Fatalf("first login (remember device): status = %d, want 303; body: %s", verifyRec.Code, verifyRec.Body.String())
	}
	if _, ok := flow.getRememberCookie(); !ok {
		t.Fatal("no remember-device cookie was set after checking 'remember this device'")
	}

	// A step-up-gated action right after finishing THIS login must succeed
	// (freshly verified this session).
	fresh := flow.do(http.MethodPost, "/settings/kiosk/generate", "csrf_token="+flow.csrf)
	if fresh.Code != http.StatusOK {
		t.Fatalf("step-up action immediately after completing login MFA: status = %d, want 200; body: %s", fresh.Code, fresh.Body.String())
	}

	// A SECOND, independent login (new session/cookie jar, same browser
	// profile carrying the remember cookie) must skip the MFA prompt
	// entirely — POST /login promotes the session directly.
	flow2 := newLoginFlow(t, handler)
	rememberValue, _ := flow.getRememberCookie()
	flow2.cookie = flow2.cookie + "; " + authadapter.RememberDeviceCookieName + "=" + rememberValue
	loginRec := flow2.login("adult@example.com", loginMFATestPassword, "/settings")
	if loginRec.Code != http.StatusSeeOther {
		t.Fatalf("second login with a remembered device: status = %d, want 303; body: %s", loginRec.Code, loginRec.Body.String())
	}
	if loc := loginRec.Header().Get("Location"); loc != "/settings" {
		t.Fatalf("second login with a remembered device: Location = %q, want /settings (no MFA hand-off)", loc)
	}
	settingsRec := flow2.followRedirect(loginRec)
	if settingsRec.Code != http.StatusOK {
		t.Fatalf("GET /settings after a remembered-device login: status = %d, want 200", settingsRec.Code)
	}

	// BUT a step-up-gated action on this remembered-device session must
	// still demand a fresh login MFA verification (redirect to
	// /login/mfa), even though the member is fully authenticated.
	stepUpRec := flow2.do(http.MethodPost, "/settings/kiosk/generate", "csrf_token="+flow2.csrf)
	if stepUpRec.Code != http.StatusSeeOther {
		t.Fatalf("step-up action on a remembered-device session (no fresh MFA): status = %d, want 303 (redirect to /login/mfa); body: %s", stepUpRec.Code, stepUpRec.Body.String())
	}
	if loc := stepUpRec.Header().Get("Location"); !strings.HasPrefix(loc, "/login/mfa") {
		t.Fatalf("step-up redirect Location = %q, want a /login/mfa redirect", loc)
	}

	// Completing the re-prompted verification lands back on the landing
	// page (settingsPath); resubmitting the original mutation then succeeds.
	mfaPage := flow2.followRedirect(stepUpRec)
	if mfaPage.Code != http.StatusOK {
		t.Fatalf("GET /login/mfa (step-up re-prompt): status = %d, want 200", mfaPage.Code)
	}
	// Completed with a RECOVERY code rather than a second TOTP code: a TOTP
	// code from the SAME 30-second window as the original login would be
	// rejected as a replay (correctly — see
	// TestLoginMFA_TOTPCodeCannotBeReplayed), and forcing a genuinely later
	// window deterministically would mean sleeping through a real TOTP
	// period. A recovery code exercises the exact same
	// finishLogin(verified=true) tail without that timing dependency.
	stepUpVerify := flow2.verifyMFA("", recoveryCodes[1], "/settings", false)
	if stepUpVerify.Code != http.StatusSeeOther {
		t.Fatalf("step-up verification: status = %d, want 303; body: %s", stepUpVerify.Code, stepUpVerify.Body.String())
	}
	if loc := stepUpVerify.Header().Get("Location"); loc != "/settings" {
		t.Errorf("step-up verification Location = %q, want /settings (RequireStepUp's landingPath)", loc)
	}
	retryRec := flow2.do(http.MethodPost, "/settings/kiosk/generate", "csrf_token="+flow2.csrf)
	if retryRec.Code != http.StatusOK {
		t.Fatalf("step-up action after completing the step-up prompt: status = %d, want 200; body: %s", retryRec.Code, retryRec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// enrollMemberMFA drives BeginEnrollment+ConfirmEnrollment for member
// directly through mfaService (the settings page's own enroll/confirm HTTP
// flow already has dedicated coverage in mfa_settings_handler_test.go — this
// file's harness does not wire those routes at all, since it is scoped to
// the LOGIN flow) and returns the raw base32 TOTP secret.
func enrollMemberMFA(t *testing.T, mfaService *authapp.MFAService, member *household.Member) string {
	t.Helper()
	secret, _ := enrollMemberMFAWithRecoveryCodes(t, mfaService, member)
	return secret
}

// enrollMemberMFAWithRecoveryCodes is enrollMemberMFA plus the ten raw
// recovery codes generated at confirmation.
func enrollMemberMFAWithRecoveryCodes(t *testing.T, mfaService *authapp.MFAService, member *household.Member) (secret string, recoveryCodes []string) {
	t.Helper()
	ctx := context.Background()
	secret, _, err := mfaService.BeginEnrollment(ctx, member.ID, member.HouseholdID, member.DisplayName)
	if err != nil {
		t.Fatalf("enrollMemberMFA: BeginEnrollment: %v", err)
	}
	code := computeTOTPCode(t, secret)
	recoveryCodes, err = mfaService.ConfirmEnrollment(ctx, member.ID, code)
	if err != nil {
		t.Fatalf("enrollMemberMFA: ConfirmEnrollment: %v", err)
	}
	if len(recoveryCodes) != 10 {
		t.Fatalf("enrollMemberMFA: got %d recovery codes, want 10", len(recoveryCodes))
	}
	return secret, recoveryCodes
}
