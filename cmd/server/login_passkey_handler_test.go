package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	"github.com/ericfisherdev/nestova/internal/platform/totp"
)

// ---------------------------------------------------------------------------
// Test harness for usernameless passkey login and passkey step-up (NES-137).
//
// Unlike buildLoginMFATestHandler (login_mfa_handler_test.go), this harness
// ALSO wires WebAuthn end to end — LoginPasskeyHandlers (pre-auth
// usernameless login) and LoginMFAHandlers' PasskeyBegin/PasskeyFinish
// (step-up) — using fakeWebAuthnCredentialRepo (webauthn_settings_handler_test.go).
// ---------------------------------------------------------------------------

// synthPasskeyAuthenticator is a minimal, in-test ES256 "authenticator"
// that can sign a WebAuthn assertion for one credential at any counter —
// mirroring internal/auth/app/webauthn_test.go's syntheticAuthenticator
// (duplicated, not shared, since it lives in a different package and this
// codebase has no shared test-only package for it).
type synthPasskeyAuthenticator struct {
	priv         *ecdsa.PrivateKey
	credentialID []byte
}

func newSynthPasskeyAuthenticator(t *testing.T, credentialID []byte) *synthPasskeyAuthenticator {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate synthetic authenticator key: %v", err)
	}
	return &synthPasskeyAuthenticator{priv: priv, credentialID: credentialID}
}

// cosePublicKey returns the CBOR-encoded EC2 COSE public key (kty=EC2,
// alg=ES256, crv=P-256) — see internal/auth/app/webauthn_test.go's own
// cosePublicKey for the exact field-layout rationale, including why the
// coordinates are read via PublicKey.Bytes' SEC 1 uncompressed-point
// encoding rather than the deprecated PublicKey.X/PublicKey.Y fields.
func (a *synthPasskeyAuthenticator) cosePublicKey() []byte {
	uncompressed, err := a.priv.PublicKey.Bytes()
	if err != nil {
		panic("login_passkey_handler_test: encode synthetic authenticator public key: " + err.Error())
	}
	x, y := uncompressed[1:33], uncompressed[33:65]
	cose := []byte{0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21, 0x58, 0x20}
	cose = append(cose, x...)
	cose = append(cose, 0x22, 0x58, 0x20)
	cose = append(cose, y...)
	return cose
}

// sign builds and signs a real WebAuthn assertion response JSON body for
// rpID/origin/challenge/counter — see
// internal/auth/app/webauthn_test.go's own sign for the exact wire-format
// rationale. userHandle is included only when non-empty (a targeted
// step-up assertion never reports one).
func (a *synthPasskeyAuthenticator) sign(t *testing.T, rpID, origin string, counter uint32, challenge, userHandle []byte) []byte {
	t.Helper()

	rpIDHash := sha256.Sum256([]byte(rpID))
	var counterBytes [4]byte
	binary.BigEndian.PutUint32(counterBytes[:], counter)
	authData := append(append([]byte{}, rpIDHash[:]...), byte(0x01)) // UP only; testWebAuthn-shaped fixtures do not require UV
	authData = append(authData, counterBytes[:]...)

	clientData := map[string]any{
		"type":        "webauthn.get",
		"challenge":   base64.RawURLEncoding.EncodeToString(challenge),
		"origin":      origin,
		"crossOrigin": false,
	}
	cdj, err := json.Marshal(clientData)
	if err != nil {
		t.Fatalf("marshal client data: %v", err)
	}
	cdHash := sha256.Sum256(cdj)

	signedData := append(append([]byte{}, authData...), cdHash[:]...)
	digest := sha256.Sum256(signedData)
	sig, err := ecdsa.SignASN1(rand.Reader, a.priv, digest[:])
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}

	id := base64.RawURLEncoding.EncodeToString(a.credentialID)
	response := map[string]any{
		"authenticatorData": base64.RawURLEncoding.EncodeToString(authData),
		"clientDataJSON":    base64.RawURLEncoding.EncodeToString(cdj),
		"signature":         base64.RawURLEncoding.EncodeToString(sig),
	}
	if len(userHandle) > 0 {
		response["userHandle"] = base64.RawURLEncoding.EncodeToString(userHandle)
	}
	body, err := json.Marshal(map[string]any{
		"id":       id,
		"rawId":    id,
		"type":     "public-key",
		"response": response,
	})
	if err != nil {
		t.Fatalf("marshal assertion body: %v", err)
	}
	return body
}

// buildLoginPasskeyTestHandler wires the full pre-auth flow with WebAuthn
// enabled: password login (Handlers), login MFA including passkey step-up
// (LoginMFAHandlers), and usernameless passkey login (LoginPasskeyHandlers).
func buildLoginPasskeyTestHandler(t *testing.T, hhRepo household.HouseholdRepository, credRepo *loginTestCredRepo, notify notifydomain.Enqueuer) (handler http.Handler, sm *scs.SessionManager, mfaService *authapp.MFAService, webauthnRepo *fakeWebAuthnCredentialRepo, handles *authapp.WebAuthnUserHandleDeriver) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm = newTestSessionManager()

	cipher, err := crypto.NewCipher([]byte("login-passkey-test-cipher-32byte"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	mfaService, err = authapp.NewMFAService(newFakeMFARepo(), cipher, totp.NewProvider(), credRepo, hhRepo, logger)
	if err != nil {
		t.Fatalf("NewMFAService: %v", err)
	}
	rememberSigner, err := authapp.NewRememberDeviceSigner([]byte("login-passkey-test-harness-remember-key"))
	if err != nil {
		t.Fatalf("NewRememberDeviceSigner: %v", err)
	}

	wa, err := webauthn.New(&webauthn.Config{
		RPID:          webauthnTestRPID,
		RPDisplayName: "Nestova Test",
		RPOrigins:     []string{webauthnTestRPOrigin},
	})
	if err != nil {
		t.Fatalf("webauthn.New: %v", err)
	}
	webauthnRepo = newFakeWebAuthnCredentialRepo()
	handles, err = authapp.NewWebAuthnUserHandleDeriver([]byte("login-passkey-test-harness-handle-key"))
	if err != nil {
		t.Fatalf("NewWebAuthnUserHandleDeriver: %v", err)
	}
	webauthnService, err := authapp.NewWebAuthnService(webauthnRepo, wa, handles, notify, logger)
	if err != nil {
		t.Fatalf("NewWebAuthnService: %v", err)
	}
	loginPasskeyHandlers := authadapter.NewLoginPasskeyHandlers(sm, webauthnService, logger)

	authn := authapp.New(credRepo)
	authHandlers := authadapter.NewHandlers(sm, authn, mfaService, rememberSigner, webauthnService, logger)
	loginMFAHandlers := authadapter.NewLoginMFAHandlers(sm, mfaService, rememberSigner, webauthnService, notify, false, logger)

	requireMember := authadapter.RequireMember(sm)
	requireStepUp := authadapter.RequireStepUp(sm, mfaService, webauthnService, "/settings", logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	mux.HandleFunc("POST /login", authHandlers.Login)
	mux.HandleFunc("POST /logout", authHandlers.Logout)
	mux.HandleFunc("GET /login/mfa", loginMFAHandlers.Page)
	mux.HandleFunc("POST /login/mfa", loginMFAHandlers.Verify)
	mux.HandleFunc("GET /login/mfa/passkey/begin", loginMFAHandlers.PasskeyBegin)
	mux.HandleFunc("POST /login/mfa/passkey/finish", loginMFAHandlers.PasskeyFinish)
	mux.HandleFunc("GET /login/passkey/begin", loginPasskeyHandlers.Begin)
	mux.HandleFunc("POST /login/passkey/finish", loginPasskeyHandlers.Finish)
	mux.Handle("GET /settings", requireMember(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("settings page"))
	})))
	// Stands in for a step-up-gated route, mirroring
	// buildLoginMFATestHandler's own stand-in.
	mux.Handle("POST /settings/kiosk/generate", requireMember(requireStepUp(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("generated"))
	}))))

	handler = sm.LoadAndSave(authadapter.Authenticate(sm, hhRepo)(mux))
	return handler, sm, mfaService, webauthnRepo, handles
}

// seedPasskeyCredential creates a synthetic authenticator and persists a
// matching credential for member, returning the authenticator so callers
// can sign assertions against it.
func seedPasskeyCredential(t *testing.T, repo *fakeWebAuthnCredentialRepo, handles *authapp.WebAuthnUserHandleDeriver, member *household.Member, signCount uint32) *synthPasskeyAuthenticator {
	t.Helper()
	credentialID := []byte("passkey-cred-" + member.ID.String())
	auth := newSynthPasskeyAuthenticator(t, credentialID)
	cred := &authdomain.WebAuthnCredential{
		ID:           authdomain.NewWebAuthnCredentialID(),
		MemberID:     member.ID,
		CredentialID: credentialID,
		PublicKey:    auth.cosePublicKey(),
		SignCount:    signCount,
		Nickname:     "Test passkey",
		UserHandle:   handles.Derive(member.ID),
	}
	if err := repo.Create(context.Background(), member.HouseholdID, cred); err != nil {
		t.Fatalf("seed passkey credential: %v", err)
	}
	return auth
}

// ---------------------------------------------------------------------------
// AC: "A member with a registered passkey signs in with one biometric
// gesture and no typed identifier; the session carries mfa_verified."
// ---------------------------------------------------------------------------

func TestLoginPasskey_UsernamelessLogin_PromotesSessionWithMFAVerified(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	handler, _, _, webauthnRepo, handles := buildLoginPasskeyTestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})
	auth := seedPasskeyCredential(t, webauthnRepo, handles, member, 0)

	flow := newLoginFlow(t, handler)

	beginRec := flow.do(http.MethodGet, "/login/passkey/begin", "")
	if beginRec.Code != http.StatusOK {
		t.Fatalf("GET /login/passkey/begin: status = %d, want 200; body: %s", beginRec.Code, beginRec.Body.String())
	}
	var assertion protocol.CredentialAssertion
	if err := json.Unmarshal(beginRec.Body.Bytes(), &assertion); err != nil {
		t.Fatalf("unmarshal assertion options: %v", err)
	}
	challenge := []byte(assertion.Response.Challenge)

	body := auth.sign(t, webauthnTestRPID, webauthnTestRPOrigin, 1, challenge, handles.Derive(member.ID))
	req := httptest.NewRequest(http.MethodPost, "/login/passkey/finish?next=%2Fsettings", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", flow.csrf)
	req.Header.Set("Cookie", flow.cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /login/passkey/finish: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var finishResp struct {
		Redirect string `json:"redirect"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &finishResp); err != nil {
		t.Fatalf("unmarshal finish response: %v", err)
	}
	if finishResp.Redirect != "/settings" {
		t.Errorf("Finish redirect = %q, want /settings", finishResp.Redirect)
	}
	flow.absorb(rec)

	// The session must ALREADY be authenticated AND mfa_verified — a
	// step-up-gated action must succeed immediately, no /login/mfa
	// hand-off, mirroring the ticket's "counts as both factors in one
	// gesture" AC.
	settingsRec := flow.do(http.MethodGet, "/settings", "")
	if settingsRec.Code != http.StatusOK {
		t.Fatalf("GET /settings after passkey login: status = %d, want 200", settingsRec.Code)
	}
	stepUpRec := flow.do(http.MethodPost, "/settings/kiosk/generate", "csrf_token="+flow.csrf)
	if stepUpRec.Code != http.StatusOK {
		t.Fatalf("step-up action immediately after passkey login: status = %d, want 200; body: %s", stepUpRec.Code, stepUpRec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// AC: "Assertions with a wrong origin, wrong RP ID, stale challenge, or
// unknown credential are rejected."
// ---------------------------------------------------------------------------

func TestLoginPasskey_UnknownCredential_Rejected(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	handler, _, _, _, _ := buildLoginPasskeyTestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})
	// No credential registered anywhere — a fresh, never-seen keypair signs
	// a well-formed assertion for a handle nobody owns.
	auth := newSynthPasskeyAuthenticator(t, []byte("credential-nobody-registered"))

	flow := newLoginFlow(t, handler)
	beginRec := flow.do(http.MethodGet, "/login/passkey/begin", "")
	var assertion protocol.CredentialAssertion
	if err := json.Unmarshal(beginRec.Body.Bytes(), &assertion); err != nil {
		t.Fatalf("unmarshal assertion options: %v", err)
	}
	challenge := []byte(assertion.Response.Challenge)

	body := auth.sign(t, webauthnTestRPID, webauthnTestRPOrigin, 1, challenge, []byte("no-such-user-handle"))
	req := httptest.NewRequest(http.MethodPost, "/login/passkey/finish", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", flow.csrf)
	req.Header.Set("Cookie", flow.cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /login/passkey/finish (unknown credential): status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
}

// TestLoginPasskey_NoPendingChallenge_Rejected covers a finish attempt with
// NO pending challenge at all (never began this ceremony this session) —
// distinct from TestLoginPasskey_ConsumedChallengeReplay_Rejected below,
// which covers a challenge that DID exist but was already consumed by a
// prior finish (this test's own name used to say "stale", which described
// neither case precisely — see round 3 review).
func TestLoginPasskey_NoPendingChallenge_Rejected(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	handler, _, _, webauthnRepo, handles := buildLoginPasskeyTestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})
	auth := seedPasskeyCredential(t, webauthnRepo, handles, member, 0)

	flow := newLoginFlow(t, handler)
	// No GET /login/passkey/begin at all — no pending challenge exists on
	// this session.
	body := auth.sign(t, webauthnTestRPID, webauthnTestRPOrigin, 1, []byte("a-challenge-that-was-never-issued"), handles.Derive(member.ID))
	req := httptest.NewRequest(http.MethodPost, "/login/passkey/finish", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", flow.csrf)
	req.Header.Set("Cookie", flow.cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /login/passkey/finish (no pending challenge): status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

// TestLoginPasskey_ConsumedChallengeReplay_Rejected covers the genuinely
// STALE case the AC means: a challenge that DID exist and was already
// consumed by one successful finish, then replayed — mirroring NES-136's
// TestWebAuthnSettings_RegisterFinish_ChallengeIsSingleUse for registration.
func TestLoginPasskey_ConsumedChallengeReplay_Rejected(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	handler, _, _, webauthnRepo, handles := buildLoginPasskeyTestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})
	auth := seedPasskeyCredential(t, webauthnRepo, handles, member, 0)

	flow := newLoginFlow(t, handler)
	beginRec := flow.do(http.MethodGet, "/login/passkey/begin", "")
	var assertion protocol.CredentialAssertion
	if err := json.Unmarshal(beginRec.Body.Bytes(), &assertion); err != nil {
		t.Fatalf("unmarshal assertion options: %v", err)
	}
	challenge := []byte(assertion.Response.Challenge)
	body := auth.sign(t, webauthnTestRPID, webauthnTestRPOrigin, 1, challenge, handles.Derive(member.ID))

	// First finish: succeeds and consumes the challenge.
	first := httptest.NewRequest(http.MethodPost, "/login/passkey/finish", strings.NewReader(string(body)))
	first.Header.Set("Content-Type", "application/json")
	first.Header.Set("X-CSRF-Token", flow.csrf)
	first.Header.Set("Cookie", flow.cookie)
	firstRec := httptest.NewRecorder()
	handler.ServeHTTP(firstRec, first)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first finish: status = %d, want 200; body: %s", firstRec.Code, firstRec.Body.String())
	}
	// finishLogin rotates the session token on success (session-fixation
	// defense) — absorb it so the replay below resolves to the SAME
	// session data (and so the SAME, still-valid CSRF token) under its NEW
	// token, rather than failing on a stale cookie before ever reaching
	// the challenge check this test means to exercise.
	flow.absorb(firstRec)

	// Replaying the EXACT same finish request body (same credential
	// assertion, no new begin) must fail — the challenge was already
	// consumed by the first finish above.
	replay := httptest.NewRequest(http.MethodPost, "/login/passkey/finish", strings.NewReader(string(body)))
	replay.Header.Set("Content-Type", "application/json")
	replay.Header.Set("X-CSRF-Token", flow.csrf)
	replay.Header.Set("Cookie", flow.cookie)
	replayRec := httptest.NewRecorder()
	handler.ServeHTTP(replayRec, replay)
	if replayRec.Code != http.StatusBadRequest {
		t.Fatalf("replayed finish: status = %d, want 400; body: %s", replayRec.Code, replayRec.Body.String())
	}
}

func TestLoginPasskey_WrongOrigin_Rejected(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	handler, _, _, webauthnRepo, handles := buildLoginPasskeyTestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})
	auth := seedPasskeyCredential(t, webauthnRepo, handles, member, 0)

	flow := newLoginFlow(t, handler)
	beginRec := flow.do(http.MethodGet, "/login/passkey/begin", "")
	var assertion protocol.CredentialAssertion
	if err := json.Unmarshal(beginRec.Body.Bytes(), &assertion); err != nil {
		t.Fatalf("unmarshal assertion options: %v", err)
	}
	challenge := []byte(assertion.Response.Challenge)

	// Signed against the RIGHT RP ID but a WRONG origin.
	body := auth.sign(t, webauthnTestRPID, "https://evil.example", 1, challenge, handles.Derive(member.ID))
	req := httptest.NewRequest(http.MethodPost, "/login/passkey/finish", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", flow.csrf)
	req.Header.Set("Cookie", flow.cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /login/passkey/finish (wrong origin): status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
}

func TestLoginPasskey_WrongRPID_Rejected(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	handler, _, _, webauthnRepo, handles := buildLoginPasskeyTestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})
	auth := seedPasskeyCredential(t, webauthnRepo, handles, member, 0)

	flow := newLoginFlow(t, handler)
	beginRec := flow.do(http.MethodGet, "/login/passkey/begin", "")
	var assertion protocol.CredentialAssertion
	if err := json.Unmarshal(beginRec.Body.Bytes(), &assertion); err != nil {
		t.Fatalf("unmarshal assertion options: %v", err)
	}
	challenge := []byte(assertion.Response.Challenge)

	// Signed against the RIGHT origin but a WRONG RP ID — changes the
	// authenticatorData's rpIdHash, which the server verifies against its
	// OWN configured RP ID (webauthnTestRPID) regardless of what origin
	// the client data claims.
	body := auth.sign(t, "evil.example", webauthnTestRPOrigin, 1, challenge, handles.Derive(member.ID))
	req := httptest.NewRequest(http.MethodPost, "/login/passkey/finish", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", flow.csrf)
	req.Header.Set("Cookie", flow.cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /login/passkey/finish (wrong RP ID): status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// AC: "Password login continues to work for members without passkeys."
// ---------------------------------------------------------------------------

func TestLoginPasskey_PasswordLoginStillWorks_ForMemberWithoutPasskey(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	credRepo.seed(t, member.ID, "adult@example.com", loginMFATestPassword)
	// WebAuthn IS wired (buildLoginPasskeyTestHandler always wires it), but
	// THIS member has no registered passkey — password login must still
	// work unchanged.
	handler, _, _, _, _ := buildLoginPasskeyTestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})

	flow := newLoginFlow(t, handler)
	rec := flow.login("adult@example.com", loginMFATestPassword, "/settings")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login (password, no passkey): status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/settings" {
		t.Errorf("POST /login (password, no passkey): Location = %q, want /settings", loc)
	}
	settingsRec := flow.followRedirect(rec)
	if settingsRec.Code != http.StatusOK {
		t.Fatalf("GET /settings after password login: status = %d, want 200", settingsRec.Code)
	}
}

func TestLoginPasskey_LoginPageShowsPasskeyButtonOnlyWhenWebAuthnWired(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()

	wired, _, _, _, _ := buildLoginPasskeyTestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})
	wiredRec := httptest.NewRecorder()
	wired.ServeHTTP(wiredRec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if !strings.Contains(wiredRec.Body.String(), "Sign in with passkey") {
		t.Error("GET /login must show the passkey button when WebAuthn is wired")
	}

	unwired, _, _ := buildLoginMFATestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})
	unwiredRec := httptest.NewRecorder()
	unwired.ServeHTTP(unwiredRec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if strings.Contains(unwiredRec.Body.String(), "Sign in with passkey") {
		t.Error("GET /login must not show the passkey button when WebAuthn is not wired")
	}
}

// ---------------------------------------------------------------------------
// AC: "Passkey step-up satisfies every flow that accepts TOTP step-up."
// ---------------------------------------------------------------------------

// TestLoginPasskey_StepUp_SatisfiesRequireStepUp covers a member who has a
// PASSKEY but NO TOTP enrollment. Login itself now hands such a member off
// to /login/mfa exactly like a TOTP-confirmed member (needsLoginStepUp
// treats "has a passkey" the same as "has confirmed TOTP" — see its own
// doc): typing a password alone must never mark the session freshly
// verified for a member who has a real second factor available, or
// RequireStepUp's OWN passkey-aware gate (session.go) would be trivially
// satisfied by a stale stamp Login placed there for the wrong reason.
func TestLoginPasskey_StepUp_SatisfiesRequireStepUp(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	credRepo := newLoginTestCredRepo()
	credRepo.seed(t, member.ID, "adult@example.com", loginMFATestPassword)
	handler, _, _, webauthnRepo, handles := buildLoginPasskeyTestHandler(t, hhRepo, credRepo, &recordingEnqueuer{})
	auth := seedPasskeyCredential(t, webauthnRepo, handles, member, 0)

	flow := newLoginFlow(t, handler)
	loginRec := flow.login("adult@example.com", loginMFATestPassword, "/settings")
	if loginRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login: status = %d, want 303; body: %s", loginRec.Code, loginRec.Body.String())
	}
	loc := loginRec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login/mfa") {
		t.Fatalf("POST /login (passkey-only member): Location = %q, want a /login/mfa hand-off (needsLoginStepUp must treat a registered passkey the same as confirmed TOTP)", loc)
	}

	// The password-verified session must NOT yet be authenticated: hitting
	// a protected route now must still be denied, mirroring the TOTP
	// hand-off's own guarantee (TestLoginMFA_EnrolledMember_PasswordAloneDoesNotAuthenticate).
	deniedRec := flow.do(http.MethodGet, "/settings", "")
	if deniedRec.Code == http.StatusOK {
		t.Error("a passkey-only member must not be authenticated by password alone")
	}

	mfaPageRec := flow.followRedirect(loginRec)
	if mfaPageRec.Code != http.StatusOK {
		t.Fatalf("GET /login/mfa: status = %d, want 200", mfaPageRec.Code)
	}
	if !strings.Contains(mfaPageRec.Body.String(), "Use your passkey") {
		t.Error("GET /login/mfa must offer \"use your passkey\" for a member with a registered passkey")
	}

	// Complete the hand-off via the passkey.
	beginRec := flow.do(http.MethodGet, "/login/mfa/passkey/begin", "")
	if beginRec.Code != http.StatusOK {
		t.Fatalf("GET /login/mfa/passkey/begin: status = %d, want 200; body: %s", beginRec.Code, beginRec.Body.String())
	}
	var assertion protocol.CredentialAssertion
	if err := json.Unmarshal(beginRec.Body.Bytes(), &assertion); err != nil {
		t.Fatalf("unmarshal step-up assertion options: %v", err)
	}
	if len(assertion.Response.AllowedCredentials) != 1 {
		t.Fatalf("step-up AllowedCredentials = %v, want exactly 1 (targeted, not usernameless)", assertion.Response.AllowedCredentials)
	}
	challenge := []byte(assertion.Response.Challenge)
	body := auth.sign(t, webauthnTestRPID, webauthnTestRPOrigin, 1, challenge, nil)
	finishReq := httptest.NewRequest(http.MethodPost, "/login/mfa/passkey/finish?next=%2Fsettings", strings.NewReader(string(body)))
	finishReq.Header.Set("Content-Type", "application/json")
	finishReq.Header.Set("X-CSRF-Token", flow.csrf)
	finishReq.Header.Set("Cookie", flow.cookie)
	finishRec := httptest.NewRecorder()
	handler.ServeHTTP(finishRec, finishReq)
	if finishRec.Code != http.StatusOK {
		t.Fatalf("POST /login/mfa/passkey/finish: status = %d, want 200; body: %s", finishRec.Code, finishRec.Body.String())
	}
	var finishResp struct {
		Redirect string `json:"redirect"`
	}
	if err := json.Unmarshal(finishRec.Body.Bytes(), &finishResp); err != nil {
		t.Fatalf("unmarshal step-up finish response: %v", err)
	}
	if finishResp.Redirect != "/settings" {
		t.Errorf("step-up Finish redirect = %q, want /settings", finishResp.Redirect)
	}
	flow.absorb(finishRec)

	settingsRec := flow.do(http.MethodGet, "/settings", "")
	if settingsRec.Code != http.StatusOK {
		t.Fatalf("GET /settings after completing passkey step-up: status = %d, want 200", settingsRec.Code)
	}

	// A step-up-gated action must ALSO succeed immediately — the session is
	// freshly verified via the SAME passkey that satisfied the login
	// hand-off, exactly mirroring a fresh TOTP verification's own effect.
	stepUpRec := flow.do(http.MethodPost, "/settings/kiosk/generate", "csrf_token="+flow.csrf)
	if stepUpRec.Code != http.StatusOK {
		t.Fatalf("step-up action right after completing passkey login hand-off: status = %d, want 200; body: %s", stepUpRec.Code, stepUpRec.Body.String())
	}
}
