package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	kioskadapter "github.com/ericfisherdev/nestova/internal/kiosk/adapter"
	kioskapp "github.com/ericfisherdev/nestova/internal/kiosk/app"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	"github.com/ericfisherdev/nestova/internal/platform/totp"
)

// ---------------------------------------------------------------------------
// Test harness for the "Your devices" passkey settings section (NES-136).
// ---------------------------------------------------------------------------

// webauthnTestRPID/webauthnTestRPOrigin match the origin encoded in
// webauthnSpecTestVectorNoneES256's clientDataJSON — see that function's
// own doc.
const (
	webauthnTestRPID     = "example.org"
	webauthnTestRPOrigin = "https://example.org"
)

// fakeWebAuthnCredentialRepo is an in-memory authdomain.WebAuthnCredentialRepository,
// local to this file (the internal/auth/app package has its own,
// unexported-across-packages fake of the same shape — see that package's
// own webauthn_test.go).
type fakeWebAuthnCredentialRepo struct {
	byMember map[household.MemberID][]authdomain.WebAuthnCredential
}

func newFakeWebAuthnCredentialRepo() *fakeWebAuthnCredentialRepo {
	return &fakeWebAuthnCredentialRepo{byMember: make(map[household.MemberID][]authdomain.WebAuthnCredential)}
}

func (f *fakeWebAuthnCredentialRepo) ListByMember(_ context.Context, memberID household.MemberID) ([]authdomain.WebAuthnCredential, error) {
	out := make([]authdomain.WebAuthnCredential, len(f.byMember[memberID]))
	copy(out, f.byMember[memberID])
	return out, nil
}

func (f *fakeWebAuthnCredentialRepo) Create(_ context.Context, householdID household.HouseholdID, cred *authdomain.WebAuthnCredential) error {
	cp := *cred
	cp.HouseholdID = householdID
	f.byMember[cred.MemberID] = append(f.byMember[cred.MemberID], cp)
	return nil
}

func (f *fakeWebAuthnCredentialRepo) Rename(_ context.Context, _ household.HouseholdID, memberID household.MemberID, id authdomain.WebAuthnCredentialID, nickname string) error {
	creds := f.byMember[memberID]
	for i := range creds {
		if creds[i].ID == id {
			creds[i].Nickname = nickname
			return nil
		}
	}
	return authdomain.ErrWebAuthnCredentialNotFound
}

func (f *fakeWebAuthnCredentialRepo) Delete(_ context.Context, _ household.HouseholdID, memberID household.MemberID, id authdomain.WebAuthnCredentialID) error {
	creds := f.byMember[memberID]
	for i := range creds {
		if creds[i].ID == id {
			f.byMember[memberID] = append(creds[:i], creds[i+1:]...)
			return nil
		}
	}
	return authdomain.ErrWebAuthnCredentialNotFound
}

func (f *fakeWebAuthnCredentialRepo) FindByUserHandle(_ context.Context, handle []byte) (household.MemberID, []authdomain.WebAuthnCredential, error) {
	for memberID, creds := range f.byMember {
		for _, c := range creds {
			if bytes.Equal(c.UserHandle, handle) {
				out := make([]authdomain.WebAuthnCredential, len(creds))
				copy(out, creds)
				return memberID, out, nil
			}
		}
	}
	return household.MemberID{}, nil, household.ErrMemberNotFound
}

func (f *fakeWebAuthnCredentialRepo) UpdateAfterAssertion(_ context.Context, credentialID []byte, signCount uint32, usedAt time.Time) error {
	for memberID, creds := range f.byMember {
		for i := range creds {
			if bytes.Equal(creds[i].CredentialID, credentialID) {
				f.byMember[memberID][i].SignCount = signCount
				f.byMember[memberID][i].LastUsedAt = &usedAt
				return nil
			}
		}
	}
	return authdomain.ErrWebAuthnCredentialNotFound
}

var _ authdomain.WebAuthnCredentialRepository = (*fakeWebAuthnCredentialRepo)(nil)

// buildWebAuthnSettingsTestHandler wires the /settings route surface with
// the "Your devices" passkey section actually enabled (unlike
// buildSettingsTestHandler, which always passes nil for webauthnHandlers),
// mirroring buildSettingsTestHandler's own construction otherwise. Returns
// the handler, the session manager, the underlying credential fake (for
// direct seeding/assertions), and the household repo (multi-member, for the
// MFA enrollment helper needed to test RequireStepUp).
func buildWebAuthnSettingsTestHandler(t *testing.T) (http.Handler, *scs.SessionManager, *fakeWebAuthnCredentialRepo, *multiMemberHouseholdRepo, *authapp.MFAService) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	hhRepo := newMultiMemberHouseholdRepo()
	credRepo := newFakeMemberCredRepo()

	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	kioskSvc, err := kioskapp.NewKioskService(devices, codes, nil)
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	settingsHandlers := kioskadapter.NewSettingsWebHandlers(kioskSvc, sm, logger)

	mfaCipher, err := crypto.NewCipher([]byte("webauthn-test-harness-mfa-cipher"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	mfaService, err := authapp.NewMFAService(newFakeMFARepo(), mfaCipher, totp.NewProvider(), credRepo, hhRepo, logger)
	if err != nil {
		t.Fatalf("NewMFAService: %v", err)
	}
	mfaHandlers := authadapter.NewMFAWebHandlers(mfaService, hhRepo, sm, logger)

	wa, err := webauthn.New(&webauthn.Config{
		RPID:          webauthnTestRPID,
		RPDisplayName: "Nestova Test",
		RPOrigins:     []string{webauthnTestRPOrigin},
	})
	if err != nil {
		t.Fatalf("webauthn.New: %v", err)
	}
	webauthnRepo := newFakeWebAuthnCredentialRepo()
	handles, err := authapp.NewWebAuthnUserHandleDeriver([]byte("webauthn-test-harness-handle-key"))
	if err != nil {
		t.Fatalf("NewWebAuthnUserHandleDeriver: %v", err)
	}
	webauthnService, err := authapp.NewWebAuthnService(webauthnRepo, wa, handles, &recordingEnqueuer{}, logger)
	if err != nil {
		t.Fatalf("NewWebAuthnService: %v", err)
	}
	webauthnHandlers := authadapter.NewWebAuthnWebHandlers(webauthnService, sm, logger)

	authHandlers := authadapter.NewHandlers(sm, authapp.New(credRepo), nil, nil, nil, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	mux.HandleFunc("GET /login/mfa", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("login mfa page"))
	})
	registerSettingsPage(mux, logger, sm, hhRepo, settingsHandlers, mfaHandlers, mfaService, webauthnHandlers, webauthnService, newTestNotifyWebHandlers(hhRepo, sm, logger))

	handler := sm.LoadAndSave(authadapter.Authenticate(sm, hhRepo)(mux))
	return handler, sm, webauthnRepo, hhRepo, mfaService
}

// seedWebAuthnChallenge injects session directly into the live session
// store under cookie, bypassing a real POST /register/begin — mirroring
// seedAuthedSession's own "one-shot LoadAndSave handler" injection
// technique. Used to drive RegisterFinish against the W3C spec test
// vector's FIXED challenge, which a real (random) BeginRegistration call
// could never produce.
func seedWebAuthnChallenge(t *testing.T, _ http.Handler, sm *scs.SessionManager, cookie string, session webauthn.SessionData) {
	t.Helper()
	injectHandler := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sm.Put(r.Context(), authadapter.WebAuthnRegChallengeSessionKeyForTests, session)
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	injectHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("seedWebAuthnChallenge: injection request status = %d, want 204", rec.Code)
	}
}

// webauthnSpecTestVectorNoneES256 returns the W3C WebAuthn spec's published
// "none" attestation, ES256 registration test vector
// (https://www.w3.org/TR/webauthn-3/#sctn-test-vectors-none-es256) — see
// internal/auth/app/webauthn_test.go's identical helper for the full
// rationale. Duplicated here (not exported/shared) because it is a small,
// self-contained fixture and cmd/server tests do not import
// internal/auth/app's test-only symbols.
func webauthnSpecTestVectorNoneES256(t *testing.T) (body []byte, challenge string, credentialID []byte) {
	t.Helper()
	const (
		attestationObjectHex = "a363666d74646e6f6e656761747453746d74a068617574684461746158a4bfabc37432958b063360d3ad6461c9c4735ae7f8edd46592a5e0f01452b2e4b559000000008446ccb9ab1db374750b2367ff6f3a1f0020f91f391db4c9b2fde0ea70189cba3fb63f579ba6122b33ad94ff3ec330084be4a5010203262001215820afefa16f97ca9b2d23eb86ccb64098d20db90856062eb249c33a9b672f26df61225820930a56b87a2fca66334b03458abf879717c12cc68ed73290af2e2664796b9220"
		clientDataJSONHex    = "7b2274797065223a22776562617574686e2e637265617465222c226368616c6c656e6765223a22414d4d507434557878475453746e63647134313759447742466938767049612d7077386f4f755657345441222c226f726967696e223a2268747470733a2f2f6578616d706c652e6f7267222c2263726f73734f726967696e223a66616c73652c22657874726144617461223a22636c69656e74446174614a534f4e206d617920626520657874656e6465642077697468206164646974696f6e616c206669656c647320696e20746865206675747572652c207375636820617320746869733a20426b5165446a646354427258426941774a544c453551227d"
		credentialIDHex      = "f91f391db4c9b2fde0ea70189cba3fb63f579ba6122b33ad94ff3ec330084be4"
		challengeHex         = "00c30fb78531c464d2b6771dab8d7b603c01162f2fa486bea70f283ae556e130"
	)
	credentialID, err := hex.DecodeString(credentialIDHex)
	if err != nil {
		t.Fatalf("decode credential id: %v", err)
	}
	challengeBytes, err := hex.DecodeString(challengeHex)
	if err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	challenge = base64.RawURLEncoding.EncodeToString(challengeBytes)
	attObjBytes, err := hex.DecodeString(attestationObjectHex)
	if err != nil {
		t.Fatalf("decode attestation object: %v", err)
	}
	cdjBytes, err := hex.DecodeString(clientDataJSONHex)
	if err != nil {
		t.Fatalf("decode client data json: %v", err)
	}
	id := base64.RawURLEncoding.EncodeToString(credentialID)
	response := map[string]any{
		"id":    id,
		"rawId": id,
		"type":  "public-key",
		"response": map[string]any{
			"attestationObject": base64.RawURLEncoding.EncodeToString(attObjBytes),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(cdjBytes),
		},
	}
	body, err = json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return body, challenge, credentialID
}

// withNickname merges a top-level "nickname" field into a raw JSON
// credential body, mirroring webauthn-register.js's own
// { ...credentialJSON, nickname } request body shape for POST
// /settings/webauthn/register/finish (NES-136 round 2: nickname moved out
// of the URL query string into the JSON body alongside the credential's own
// fields).
func withNickname(t *testing.T, body []byte, nickname string) []byte {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("withNickname: unmarshal: %v", err)
	}
	fields["nickname"] = nickname
	merged, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("withNickname: marshal: %v", err)
	}
	return merged
}

// ---------------------------------------------------------------------------
// AC: "A member can register a passkey ... after step-up, and it appears
// in their device list with a nickname." / "Challenges are single-use."
// ---------------------------------------------------------------------------

func TestWebAuthnSettings_FullRegistration_PersistsAndListsDevice(t *testing.T) {
	handler, sm, repo, hhRepo, _ := buildWebAuthnSettingsTestHandler(t)
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo.members[member.ID] = member
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	handles, err := authapp.NewWebAuthnUserHandleDeriver([]byte("webauthn-test-harness-handle-key"))
	if err != nil {
		t.Fatalf("NewWebAuthnUserHandleDeriver: %v", err)
	}
	_, challenge, credentialID := webauthnSpecTestVectorNoneES256(t)
	seedWebAuthnChallenge(t, handler, sm, cookie, webauthn.SessionData{
		Challenge: challenge,
		UserID:    handles.Derive(member.ID),
		CredParams: []protocol.CredentialParameter{
			{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256},
		},
	})

	body, _, _ := webauthnSpecTestVectorNoneES256(t)
	finishReq := httptest.NewRequest(http.MethodPost, "/settings/webauthn/register/finish", strings.NewReader(string(withNickname(t, body, "My Phone"))))
	finishReq.Header.Set("Content-Type", "application/json")
	finishReq.Header.Set("X-CSRF-Token", csrfToken)
	finishReq.Header.Set("Cookie", cookie)
	finishRec := httptest.NewRecorder()
	handler.ServeHTTP(finishRec, finishReq)
	if finishRec.Code != http.StatusOK {
		t.Fatalf("POST /settings/webauthn/register/finish: status = %d, want 200; body: %s", finishRec.Code, finishRec.Body.String())
	}

	creds, err := repo.ListByMember(context.Background(), member.ID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("stored %d credentials, want 1", len(creds))
	}
	if creds[0].Nickname != "My Phone" {
		t.Errorf("Nickname = %q, want %q", creds[0].Nickname, "My Phone")
	}
	if string(creds[0].CredentialID) != string(credentialID) {
		t.Error("stored CredentialID does not match the ceremony's credential id")
	}

	// AC: the device appears in the settings page's list.
	settingsRec := httptest.NewRecorder()
	settingsReq := httptest.NewRequest(http.MethodGet, "/settings", nil)
	settingsReq.Header.Set("Cookie", cookie)
	handler.ServeHTTP(settingsRec, settingsReq)
	if settingsRec.Code != http.StatusOK {
		t.Fatalf("GET /settings: status = %d, want 200", settingsRec.Code)
	}
	if !strings.Contains(settingsRec.Body.String(), "My Phone") {
		t.Error("GET /settings does not show the newly registered device's nickname")
	}
}

func TestWebAuthnSettings_RegisterFinish_NoChallengeRejected(t *testing.T) {
	// AC: "Challenges are single-use ... replayed registration responses
	// fail." A finish attempt with no pending challenge at all (never
	// began, or already consumed) must be rejected.
	handler, sm, _, hhRepo, _ := buildWebAuthnSettingsTestHandler(t)
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo.members[member.ID] = member
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	body, _, _ := webauthnSpecTestVectorNoneES256(t)
	req := httptest.NewRequest(http.MethodPost, "/settings/webauthn/register/finish", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfToken)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /settings/webauthn/register/finish with no pending challenge: status = %d, want 400", rec.Code)
	}
}

func TestWebAuthnSettings_RegisterFinish_ChallengeIsSingleUse(t *testing.T) {
	handler, sm, _, hhRepo, _ := buildWebAuthnSettingsTestHandler(t)
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo.members[member.ID] = member
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	handles, err := authapp.NewWebAuthnUserHandleDeriver([]byte("webauthn-test-harness-handle-key"))
	if err != nil {
		t.Fatalf("NewWebAuthnUserHandleDeriver: %v", err)
	}
	body, challenge, _ := webauthnSpecTestVectorNoneES256(t)
	session := webauthn.SessionData{
		Challenge: challenge,
		UserID:    handles.Derive(member.ID),
		CredParams: []protocol.CredentialParameter{
			{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256},
		},
	}
	seedWebAuthnChallenge(t, handler, sm, cookie, session)

	// First finish: succeeds and consumes the challenge.
	first := httptest.NewRequest(http.MethodPost, "/settings/webauthn/register/finish", strings.NewReader(string(body)))
	first.Header.Set("Content-Type", "application/json")
	first.Header.Set("X-CSRF-Token", csrfToken)
	first.Header.Set("Cookie", cookie)
	firstRec := httptest.NewRecorder()
	handler.ServeHTTP(firstRec, first)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first finish: status = %d, want 200; body: %s", firstRec.Code, firstRec.Body.String())
	}

	// Replaying the EXACT same finish request (same body, same cookie, no
	// new begin) must fail — the challenge was already consumed.
	replay := httptest.NewRequest(http.MethodPost, "/settings/webauthn/register/finish", strings.NewReader(string(body)))
	replay.Header.Set("Content-Type", "application/json")
	replay.Header.Set("X-CSRF-Token", csrfToken)
	replay.Header.Set("Cookie", cookie)
	replayRec := httptest.NewRecorder()
	handler.ServeHTTP(replayRec, replay)
	if replayRec.Code != http.StatusBadRequest {
		t.Fatalf("replayed finish: status = %d, want 400", replayRec.Code)
	}
}

// ---------------------------------------------------------------------------
// AC: "Registration without fresh step-up is rejected."
// ---------------------------------------------------------------------------

func TestWebAuthnSettings_RegisterBegin_NoMFAEnrolled_Succeeds(t *testing.T) {
	// A member with NO confirmed TOTP MFA has nothing to step up from
	// (RequireStepUp's own documented behavior — mirrors NES-135) and must
	// reach RegisterBegin normally.
	handler, sm, _, hhRepo, _ := buildWebAuthnSettingsTestHandler(t)
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo.members[member.ID] = member
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/settings/webauthn/register/begin", nil)
	req.Header.Set("X-CSRF-Token", csrfToken)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/webauthn/register/begin (no MFA enrolled): status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var creation protocol.CredentialCreation
	if err := json.Unmarshal(rec.Body.Bytes(), &creation); err != nil {
		t.Fatalf("unmarshal creation options: %v", err)
	}
	if creation.Response.RelyingParty.ID != webauthnTestRPID {
		t.Errorf("RP ID = %q, want %q", creation.Response.RelyingParty.ID, webauthnTestRPID)
	}
}

func TestWebAuthnSettings_RegisterBegin_WithoutFreshStepUp_Redirects(t *testing.T) {
	handler, sm, _, hhRepo, mfaService := buildWebAuthnSettingsTestHandler(t)
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo.members[member.ID] = member

	// Confirm TOTP MFA for this member so RequireStepUp actually evaluates
	// freshness instead of passing through unconditionally. mfaService uses
	// the REAL totp.Provider (RFC 6238 math), so the confirm code must be
	// computed from the actual generated secret (computeTOTPCode,
	// mfa_settings_handler_test.go) — not a fixed placeholder.
	secret, _, err := mfaService.BeginEnrollment(context.Background(), member.ID, member.HouseholdID, member.DisplayName)
	if err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if _, err := mfaService.ConfirmEnrollment(context.Background(), member.ID, computeTOTPCode(t, secret)); err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}

	// seedAuthedSession stamps member_id directly (bypassing real login),
	// so mfa_verified_at is never set — the session is exactly the "never
	// completed a fresh login MFA verification" case RequireStepUp must
	// reject.
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/settings/webauthn/register/begin", nil)
	req.Header.Set("X-CSRF-Token", csrfToken)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings/webauthn/register/begin (stale/no step-up, MFA enrolled): status = %d, want 303 (redirect to /login/mfa); body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login/mfa") {
		t.Errorf("Location = %q, want a /login/mfa redirect", loc)
	}
}

func TestWebAuthnSettings_RegisterFinish_WithoutFreshStepUp_Redirects(t *testing.T) {
	// Mirrors TestWebAuthnSettings_RegisterBegin_WithoutFreshStepUp_Redirects:
	// RegisterFinish is the endpoint that actually persists the durable
	// credential (Begin only returns creation options), so its own
	// requireStepUp gate (cmd/server/home.go) needs its own coverage, not
	// just an inference from Begin's.
	handler, sm, _, hhRepo, mfaService := buildWebAuthnSettingsTestHandler(t)
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo.members[member.ID] = member

	secret, _, err := mfaService.BeginEnrollment(context.Background(), member.ID, member.HouseholdID, member.DisplayName)
	if err != nil {
		t.Fatalf("BeginEnrollment: %v", err)
	}
	if _, err := mfaService.ConfirmEnrollment(context.Background(), member.ID, computeTOTPCode(t, secret)); err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}

	// seedAuthedSession stamps member_id directly (bypassing real login), so
	// mfa_verified_at is never set — the same "never completed a fresh
	// login MFA verification" case RequireStepUp must reject.
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	body, _, _ := webauthnSpecTestVectorNoneES256(t)
	req := httptest.NewRequest(http.MethodPost, "/settings/webauthn/register/finish", strings.NewReader(string(withNickname(t, body, "My Phone"))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfToken)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings/webauthn/register/finish (stale/no step-up, MFA enrolled): status = %d, want 303 (redirect to /login/mfa); body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login/mfa") {
		t.Errorf("Location = %q, want a /login/mfa redirect", loc)
	}
}

// ---------------------------------------------------------------------------
// Rename / Revoke
// ---------------------------------------------------------------------------

func TestWebAuthnSettings_Rename_UpdatesNicknameAndRedirects(t *testing.T) {
	handler, sm, repo, hhRepo, _ := buildWebAuthnSettingsTestHandler(t)
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo.members[member.ID] = member
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	id := authdomain.NewWebAuthnCredentialID()
	if err := repo.Create(context.Background(), member.HouseholdID, &authdomain.WebAuthnCredential{
		ID: id, MemberID: member.ID, CredentialID: []byte("cred-rename"), PublicKey: []byte("pk"), Nickname: "Old",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/settings/webauthn/"+id.String()+"/rename", strings.NewReader("csrf_token="+csrfToken+"&nickname=New+Name"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST rename: status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	creds, _ := repo.ListByMember(context.Background(), member.ID)
	if len(creds) != 1 || creds[0].Nickname != "New Name" {
		t.Errorf("Nickname after rename = %+v, want New Name", creds)
	}
}

func TestWebAuthnSettings_Revoke_RemovesDeviceImmediately(t *testing.T) {
	handler, sm, repo, hhRepo, _ := buildWebAuthnSettingsTestHandler(t)
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo.members[member.ID] = member
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	id := authdomain.NewWebAuthnCredentialID()
	if err := repo.Create(context.Background(), member.HouseholdID, &authdomain.WebAuthnCredential{
		ID: id, MemberID: member.ID, CredentialID: []byte("cred-revoke"), PublicKey: []byte("pk"), Nickname: "Doomed",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/settings/webauthn/"+id.String()+"/revoke", strings.NewReader("csrf_token="+csrfToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST revoke: status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	creds, _ := repo.ListByMember(context.Background(), member.ID)
	if len(creds) != 0 {
		t.Errorf("credentials after revoke = %d, want 0", len(creds))
	}
}

// TestWebAuthnSettings_Rename_RejectsOtherMembersCredential and
// TestWebAuthnSettings_Revoke_RejectsOtherMembersCredential cover the
// authorization boundary Rename/Revoke's tenant-scoped lookup exists for:
// WebAuthnWebHandlers always scopes both mutations to the CURRENT
// member (member.ID from the session, never a client-supplied value — see
// WebAuthnWebHandlers.Rename/.Revoke), so one member's id in the URL path
// naming another member's credential must fail with
// authdomain.ErrWebAuthnCredentialNotFound's mapped 404, not succeed.

func TestWebAuthnSettings_Rename_RejectsOtherMembersCredential(t *testing.T) {
	handler, sm, repo, hhRepo, _ := buildWebAuthnSettingsTestHandler(t)
	owner := settingsTestAdultInHousehold(household.NewHouseholdID())
	attacker := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo.members[owner.ID] = owner
	hhRepo.members[attacker.ID] = attacker
	cookie, csrfToken := seedAuthedSession(t, handler, sm, attacker.ID.String())

	id := authdomain.NewWebAuthnCredentialID()
	if err := repo.Create(context.Background(), owner.HouseholdID, &authdomain.WebAuthnCredential{
		ID: id, MemberID: owner.ID, CredentialID: []byte("cred-other-rename"), PublicKey: []byte("pk"), Nickname: "Owner's",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/settings/webauthn/"+id.String()+"/rename", strings.NewReader("csrf_token="+csrfToken+"&nickname=Hijacked"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("attacker renaming another member's credential: status = %d, want 404 (ErrWebAuthnCredentialNotFound); body: %s", rec.Code, rec.Body.String())
	}

	creds, err := repo.ListByMember(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 1 || creds[0].Nickname != "Owner's" {
		t.Errorf("owner's credential nickname was changed by another member's request: %+v", creds)
	}
}

func TestWebAuthnSettings_Revoke_RejectsOtherMembersCredential(t *testing.T) {
	handler, sm, repo, hhRepo, _ := buildWebAuthnSettingsTestHandler(t)
	owner := settingsTestAdultInHousehold(household.NewHouseholdID())
	attacker := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo.members[owner.ID] = owner
	hhRepo.members[attacker.ID] = attacker
	cookie, csrfToken := seedAuthedSession(t, handler, sm, attacker.ID.String())

	id := authdomain.NewWebAuthnCredentialID()
	if err := repo.Create(context.Background(), owner.HouseholdID, &authdomain.WebAuthnCredential{
		ID: id, MemberID: owner.ID, CredentialID: []byte("cred-other-revoke"), PublicKey: []byte("pk"), Nickname: "Owner's",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/settings/webauthn/"+id.String()+"/revoke", strings.NewReader("csrf_token="+csrfToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("attacker revoking another member's credential: status = %d, want 404 (ErrWebAuthnCredentialNotFound); body: %s", rec.Code, rec.Body.String())
	}

	creds, err := repo.ListByMember(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 1 {
		t.Errorf("owner's credential was removed by another member's request: got %d, want 1", len(creds))
	}
}

// ---------------------------------------------------------------------------
// Section visibility
// ---------------------------------------------------------------------------

func TestWebAuthnSettings_SectionAbsentWhenNotWired(t *testing.T) {
	// Mirrors buildSettingsTestHandler (mfa_settings_handler_test.go),
	// which always passes nil for webauthnHandlers — the deployment
	// equivalent of Server.PublicBaseURL being unset.
	hhRepo := newMultiMemberHouseholdRepo()
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo.members[member.ID] = member
	handler, sm := buildSettingsTestHandler(t, hhRepo, newFakeMemberCredRepo())
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings: status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Your devices") {
		t.Error("the passkey section must not render when webauthnHandlers is nil")
	}

	// The routes themselves must not exist either.
	beginReq := httptest.NewRequest(http.MethodPost, "/settings/webauthn/register/begin", nil)
	beginReq.Header.Set("Cookie", cookie)
	beginRec := httptest.NewRecorder()
	handler.ServeHTTP(beginRec, beginReq)
	if beginRec.Code != http.StatusNotFound {
		t.Errorf("POST /settings/webauthn/register/begin when unwired: status = %d, want 404", beginRec.Code)
	}
}

func TestWebAuthnSettings_SectionShowsEmptyState(t *testing.T) {
	handler, sm, _, hhRepo, _ := buildWebAuthnSettingsTestHandler(t)
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo.members[member.ID] = member
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Your devices") {
		t.Error("the passkey section must render when webauthnHandlers is wired")
	}
	if !strings.Contains(rec.Body.String(), "No passkeys registered yet.") {
		t.Error("a member with no credentials must see the empty-state message")
	}
}
