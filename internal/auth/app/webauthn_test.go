package app_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeWebAuthnCredentialRepo is an in-memory authdomain.WebAuthnCredentialRepository.
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

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

func discardWebAuthnLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testWebAuthnRPID/testWebAuthnRPOrigin match the origin encoded in
// webauthnSpecTestVectorNoneES256's clientDataJSON exactly — the W3C test
// vector is only valid against this specific RP configuration.
const (
	testWebAuthnRPID     = "example.org"
	testWebAuthnRPOrigin = "https://example.org"
)

func testHandleDeriver(t *testing.T) *app.WebAuthnUserHandleDeriver {
	t.Helper()
	d, err := app.NewWebAuthnUserHandleDeriver([]byte("a-test-webauthn-user-handle-key"))
	if err != nil {
		t.Fatalf("NewWebAuthnUserHandleDeriver: %v", err)
	}
	return d
}

func testWebAuthn(t *testing.T) *webauthn.WebAuthn {
	t.Helper()
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          testWebAuthnRPID,
		RPDisplayName: "Nestova Test",
		RPOrigins:     []string{testWebAuthnRPOrigin},
	})
	if err != nil {
		t.Fatalf("webauthn.New: %v", err)
	}
	return wa
}

// fakeNotifyEnqueuer is a notifydomain.Enqueuer fake that records every
// enqueued notification, mirroring cmd/server's own recordingEnqueuer
// (login_mfa_handler_test.go) — duplicated rather than shared since that
// one lives in package main and this package cannot import it.
type fakeNotifyEnqueuer struct {
	events []*notifydomain.Notification
}

func (f *fakeNotifyEnqueuer) Enqueue(_ context.Context, n *notifydomain.Notification) error {
	f.events = append(f.events, n)
	return nil
}

var _ notifydomain.Enqueuer = (*fakeNotifyEnqueuer)(nil)

// newWebAuthnServiceFixture wires a WebAuthnService with fully controllable
// fakes, so tests can both exercise the service and assert directly against
// the fake repository's (and, for sign-count-anomaly tests, the fake
// notifier's) state.
func newWebAuthnServiceFixture(t *testing.T) (*app.WebAuthnService, *fakeWebAuthnCredentialRepo, *app.WebAuthnUserHandleDeriver, *fakeNotifyEnqueuer) {
	t.Helper()
	repo := newFakeWebAuthnCredentialRepo()
	handles := testHandleDeriver(t)
	notify := &fakeNotifyEnqueuer{}
	svc, err := app.NewWebAuthnService(repo, testWebAuthn(t), handles, notify, discardWebAuthnLogger())
	if err != nil {
		t.Fatalf("NewWebAuthnService: %v", err)
	}
	return svc, repo, handles, notify
}

// webauthnSpecTestVectorNoneES256 returns the W3C WebAuthn spec's published
// "none" attestation, ES256 registration test vector
// (https://www.w3.org/TR/webauthn-3/#sctn-test-vectors-none-es256), in the
// same JSON shape a browser's PublicKeyCredential.toJSON() produces — a
// real, valid, deterministic registration ceremony response, so
// WebAuthnService.FinishRegistration's success path can be tested without a
// live authenticator or hand-rolled attestation signing. It is only valid
// against testWebAuthnRPID/testWebAuthnRPOrigin (encoded in its
// clientDataJSON) and its own fixed challenge (returned here).
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

// validSessionFor builds the webauthn.SessionData BeginRegistration would
// have produced for the spec test vector's challenge and memberID's derived
// handle — CredParams must list ES256 (webauthncose.AlgES256) to match the
// test vector's actual public key algorithm, or CreateCredential's
// attestation verification rejects it regardless of the challenge/user id
// matching (mirrors the go-webauthn library's own
// TestFinishRegistration_Success).
func validSessionFor(challenge string, handle []byte) webauthn.SessionData {
	return webauthn.SessionData{
		Challenge: challenge,
		UserID:    handle,
		CredParams: []protocol.CredentialParameter{
			{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256},
		},
	}
}

// ---------------------------------------------------------------------------
// Synthetic login/step-up assertions
//
// webauthnSpecTestVectorNoneES256 (above) is fixed at authenticator counter
// 0 — the W3C spec's own published assertion test vector for that
// credential — so it can prove FinishLogin/VerifyStepUp's happy path but
// cannot exercise sign-count comparison at OTHER counter values (the
// counter is part of authenticatorData, which is itself covered by the
// signature — changing it without re-signing would just make verification
// fail, not simulate a different real assertion). syntheticAuthenticator
// generates a real ES256 keypair and signs a real, spec-shaped assertion
// response for ANY chosen counter, RP ID, origin, and challenge, so
// FinishLogin/VerifyStepUp's sign-count handling (NES-137) can be exercised
// through their REAL cryptographic verification path rather than a shortcut
// around it.
// ---------------------------------------------------------------------------

// syntheticAuthenticator is a minimal, in-test ES256 "authenticator" that
// can sign a WebAuthn assertion for one credential at any given counter
// value.
type syntheticAuthenticator struct {
	priv         *ecdsa.PrivateKey
	credentialID []byte
}

// newSyntheticAuthenticator generates a fresh ES256 keypair for
// credentialID.
func newSyntheticAuthenticator(t *testing.T, credentialID []byte) *syntheticAuthenticator {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate synthetic authenticator key: %v", err)
	}
	return &syntheticAuthenticator{priv: priv, credentialID: credentialID}
}

// cosePublicKey returns the CBOR-encoded EC2 COSE public key
// (kty=EC2, alg=ES256, crv=P-256) go-webauthn's own attestation object
// encodes — the same field layout webauthnSpecTestVectorNoneES256's fixed
// credentialPubKeyHex uses (a5 01 02 03 26 20 01 21 58 20 <x32> 22 58 20
// <y32>), built here from the freshly generated key's own X/Y coordinates
// (read via PublicKey.Bytes' SEC 1 uncompressed-point encoding — 0x04 || X
// || Y — rather than the deprecated PublicKey.X/PublicKey.Y fields
// directly) so toWebAuthnCredential-style stored credentials round-trip
// correctly through webauthncose.ParsePublicKey.
func (a *syntheticAuthenticator) cosePublicKey() []byte {
	uncompressed, err := a.priv.PublicKey.Bytes()
	if err != nil {
		// Bytes() can only fail for an invalid key or an unsupported curve;
		// a freshly generated P-256 key is neither.
		panic("webauthn_test: encode synthetic authenticator public key: " + err.Error())
	}
	x, y := uncompressed[1:33], uncompressed[33:65]
	cose := []byte{0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21, 0x58, 0x20}
	cose = append(cose, x...)
	cose = append(cose, 0x22, 0x58, 0x20)
	cose = append(cose, y...)
	return cose
}

// assertionResponseBodyFlagsUP is the authenticatorData flags byte for
// "user present, user NOT verified" — sufficient for testWebAuthn(t)'s test
// fixture, which sets no AuthenticatorSelection.UserVerification (see that
// function's own doc: shouldVerifyUser therefore defaults to false, unlike
// production's cmd/server/main.go, which requires it).
const assertionResponseBodyFlagsUP = 0x01

// sign builds and signs a real WebAuthn assertion response JSON body (the
// same shape PublicKeyCredential.toJSON() produces for
// navigator.credentials.get()) for rpID/origin/challenge/counter, and —
// when userHandle is non-empty — includes it (the discoverable/passkey
// login shape; omit for a targeted step-up assertion, which never reports
// one).
func (a *syntheticAuthenticator) sign(t *testing.T, rpID, origin string, counter uint32, challenge, userHandle []byte) []byte {
	t.Helper()

	rpIDHash := sha256.Sum256([]byte(rpID))
	var counterBytes [4]byte
	binary.BigEndian.PutUint32(counterBytes[:], counter)
	authData := append(append([]byte{}, rpIDHash[:]...), byte(assertionResponseBodyFlagsUP))
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

// ---------------------------------------------------------------------------
// BeginRegistration
// ---------------------------------------------------------------------------

func TestWebAuthnService_BeginRegistration_ReturnsCreationOptionsAndSession(t *testing.T) {
	t.Parallel()
	svc, _, handles, _ := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()

	creation, session, err := svc.BeginRegistration(context.Background(), memberID, "Alice")
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	if creation == nil || session == nil {
		t.Fatal("BeginRegistration returned a nil creation or session")
	}
	if creation.Response.RelyingParty.ID != testWebAuthnRPID {
		t.Errorf("RP ID = %q, want %q", creation.Response.RelyingParty.ID, testWebAuthnRPID)
	}
	if creation.Response.AuthenticatorSelection.ResidentKey != protocol.ResidentKeyRequirementPreferred {
		t.Errorf("ResidentKey = %q, want %q", creation.Response.AuthenticatorSelection.ResidentKey, protocol.ResidentKeyRequirementPreferred)
	}
	wantHandle := handles.Derive(memberID)
	if !bytes.Equal(session.UserID, wantHandle) {
		t.Error("session.UserID does not match the member's derived handle")
	}
	if !bytes.Equal(creation.Response.User.ID.(protocol.URLEncodedBase64), wantHandle) {
		t.Error("creation options' user id does not match the member's derived handle")
	}
}

func TestWebAuthnService_BeginRegistration_SameMemberAlwaysGetsSameHandle(t *testing.T) {
	t.Parallel()
	svc, _, handles, _ := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()

	_, session1, err := svc.BeginRegistration(context.Background(), memberID, "Alice")
	if err != nil {
		t.Fatalf("BeginRegistration (1): %v", err)
	}
	_, session2, err := svc.BeginRegistration(context.Background(), memberID, "Alice")
	if err != nil {
		t.Fatalf("BeginRegistration (2): %v", err)
	}
	if !bytes.Equal(session1.UserID, session2.UserID) {
		t.Error("the SAME member got a DIFFERENT WebAuthn user handle across two BeginRegistration calls")
	}
	if !bytes.Equal(session1.UserID, handles.Derive(memberID)) {
		t.Error("session.UserID does not match WebAuthnUserHandleDeriver.Derive directly")
	}
}

func TestWebAuthnService_BeginRegistration_ExcludesExistingCredentials(t *testing.T) {
	t.Parallel()
	svc, repo, _, _ := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()
	existingCredID := []byte("existing-credential-id-bytes")
	if err := repo.Create(context.Background(), household.NewHouseholdID(), &authdomain.WebAuthnCredential{
		ID: authdomain.NewWebAuthnCredentialID(), MemberID: memberID,
		CredentialID: existingCredID, PublicKey: []byte("pk"), Nickname: "Old device",
	}); err != nil {
		t.Fatalf("seed existing credential: %v", err)
	}

	creation, _, err := svc.BeginRegistration(context.Background(), memberID, "Alice")
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	found := false
	for _, excl := range creation.Response.CredentialExcludeList {
		if bytes.Equal(excl.CredentialID, existingCredID) {
			found = true
		}
	}
	if !found {
		t.Error("BeginRegistration did not exclude the member's existing credential")
	}
}

// ---------------------------------------------------------------------------
// FinishRegistration
// ---------------------------------------------------------------------------

func TestWebAuthnService_FinishRegistration_ValidResponse_PersistsCredential(t *testing.T) {
	t.Parallel()
	svc, repo, handles, _ := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()

	body, challenge, credentialID := webauthnSpecTestVectorNoneES256(t)
	session := validSessionFor(challenge, handles.Derive(memberID))
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ParseCredentialCreationResponseBody: %v", err)
	}

	if err := svc.FinishRegistration(context.Background(), memberID, householdID, "Alice", "My Phone", session, parsed); err != nil {
		t.Fatalf("FinishRegistration: %v", err)
	}

	creds, err := repo.ListByMember(context.Background(), memberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("stored %d credentials, want 1", len(creds))
	}
	got := creds[0]
	if !bytes.Equal(got.CredentialID, credentialID) {
		t.Error("stored CredentialID does not match the ceremony's credential id")
	}
	if got.Nickname != "My Phone" {
		t.Errorf("Nickname = %q, want %q", got.Nickname, "My Phone")
	}
	if !bytes.Equal(got.UserHandle, handles.Derive(memberID)) {
		t.Error("stored UserHandle does not match the member's derived handle")
	}
	if got.HouseholdID != householdID {
		t.Errorf("HouseholdID = %v, want %v", got.HouseholdID, householdID)
	}
	if got.MemberID != memberID {
		t.Errorf("MemberID = %v, want %v", got.MemberID, memberID)
	}
	if len(got.PublicKey) == 0 {
		t.Error("stored PublicKey is empty")
	}
}

func TestWebAuthnService_FinishRegistration_BlankNickname_Defaults(t *testing.T) {
	t.Parallel()
	svc, repo, handles, _ := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()

	body, challenge, _ := webauthnSpecTestVectorNoneES256(t)
	session := validSessionFor(challenge, handles.Derive(memberID))
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ParseCredentialCreationResponseBody: %v", err)
	}

	if err := svc.FinishRegistration(context.Background(), memberID, household.NewHouseholdID(), "Alice", "   ", session, parsed); err != nil {
		t.Fatalf("FinishRegistration: %v", err)
	}
	creds, _ := repo.ListByMember(context.Background(), memberID)
	if len(creds) != 1 {
		t.Fatalf("stored %d credentials, want 1", len(creds))
	}
	if creds[0].Nickname != "Passkey" {
		t.Errorf("Nickname = %q, want the default %q", creds[0].Nickname, "Passkey")
	}
}

func TestWebAuthnService_FinishRegistration_WrongChallenge_Fails(t *testing.T) {
	t.Parallel()
	svc, repo, handles, _ := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()

	body, _, _ := webauthnSpecTestVectorNoneES256(t)
	session := validSessionFor("this-is-not-the-real-challenge", handles.Derive(memberID))
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ParseCredentialCreationResponseBody: %v", err)
	}

	err = svc.FinishRegistration(context.Background(), memberID, household.NewHouseholdID(), "Alice", "x", session, parsed)
	if !errors.Is(err, authdomain.ErrWebAuthnVerificationFailed) {
		t.Errorf("FinishRegistration(wrong challenge): err = %v, want ErrWebAuthnVerificationFailed", err)
	}
	if creds, _ := repo.ListByMember(context.Background(), memberID); len(creds) != 0 {
		t.Error("a failed verification must not persist a credential")
	}
}

func TestWebAuthnService_FinishRegistration_MismatchedUserHandle_Fails(t *testing.T) {
	// Covers CreateCredential's own user-id-matches-session check: a
	// session bound to a DIFFERENT member's handle than the one FinishRegistration
	// is called for must fail, not silently register the credential under
	// the wrong identity.
	t.Parallel()
	svc, repo, _, _ := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()

	body, challenge, _ := webauthnSpecTestVectorNoneES256(t)
	session := validSessionFor(challenge, []byte("some-other-members-handle"))
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ParseCredentialCreationResponseBody: %v", err)
	}

	err = svc.FinishRegistration(context.Background(), memberID, household.NewHouseholdID(), "Alice", "x", session, parsed)
	if !errors.Is(err, authdomain.ErrWebAuthnVerificationFailed) {
		t.Errorf("FinishRegistration(mismatched user handle): err = %v, want ErrWebAuthnVerificationFailed", err)
	}
	if creds, _ := repo.ListByMember(context.Background(), memberID); len(creds) != 0 {
		t.Error("a failed verification must not persist a credential")
	}
}

// ---------------------------------------------------------------------------
// BeginLogin / FinishLogin (NES-137)
// ---------------------------------------------------------------------------

func TestWebAuthnService_BeginLogin_ReturnsEmptyAllowCredentials(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newWebAuthnServiceFixture(t)

	assertion, session, err := svc.BeginLogin(context.Background())
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	if assertion == nil || session == nil {
		t.Fatal("BeginLogin returned a nil assertion or session")
	}
	// AC: "usernameless login ... empty allowCredentials = usernameless" —
	// the browser must be free to offer ANY of its discoverable credentials
	// for this RP, not a specific member's.
	if len(assertion.Response.AllowedCredentials) != 0 {
		t.Errorf("AllowedCredentials = %v, want empty (usernameless)", assertion.Response.AllowedCredentials)
	}
	if len(session.UserID) != 0 {
		t.Errorf("session.UserID = %v, want empty (usernameless — the server does not know who is authenticating yet)", session.UserID)
	}
}

func TestWebAuthnService_FinishLogin_ValidResponse_ResolvesMemberAndUpdatesSignCount(t *testing.T) {
	t.Parallel()
	svc, repo, handles, notify := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()
	handle := handles.Derive(memberID)
	credentialID := []byte("synthetic-cred-valid-login")
	auth := newSyntheticAuthenticator(t, credentialID)

	seed := testWebAuthnCredentialForLogin(memberID, credentialID, handle, auth.cosePublicKey(), 0)
	if err := repo.Create(context.Background(), household.NewHouseholdID(), seed); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	challenge := []byte("valid-login-fixed-test-challeng")
	body := auth.sign(t, testWebAuthnRPID, testWebAuthnRPOrigin, 0, challenge, handle)
	session := webauthn.SessionData{Challenge: base64.RawURLEncoding.EncodeToString(challenge)}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		t.Fatalf("ParseCredentialRequestResponseBytes: %v", err)
	}

	gotMemberID, err := svc.FinishLogin(context.Background(), session, parsed)
	if err != nil {
		t.Fatalf("FinishLogin: %v", err)
	}
	if gotMemberID != memberID {
		t.Errorf("FinishLogin resolved member = %v, want %v", gotMemberID, memberID)
	}

	creds, err := repo.ListByMember(context.Background(), memberID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(creds) != 1 || creds[0].LastUsedAt == nil {
		t.Errorf("credential after login = %+v, want LastUsedAt set", creds)
	}
	if len(notify.events) != 0 {
		t.Errorf("a count-0 assertion against a count-0 stored value must never notify: got %d notifications", len(notify.events))
	}
}

func TestWebAuthnService_FinishLogin_UnknownUserHandle_Fails(t *testing.T) {
	t.Parallel()
	svc, repo, _, _ := newWebAuthnServiceFixture(t)
	auth := newSyntheticAuthenticator(t, []byte("synthetic-cred-unknown-handle"))

	challenge := []byte("unknown-handle-fixed-test-chall")
	// No credential seeded anywhere for this handle — FindByUserHandle
	// returns household.ErrMemberNotFound, which FinishLogin must report
	// identically to any other verification failure (no oracle).
	body := auth.sign(t, testWebAuthnRPID, testWebAuthnRPOrigin, 0, challenge, []byte("nobody-registered-this-handle"))
	session := webauthn.SessionData{Challenge: base64.RawURLEncoding.EncodeToString(challenge)}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		t.Fatalf("ParseCredentialRequestResponseBytes: %v", err)
	}

	_, err = svc.FinishLogin(context.Background(), session, parsed)
	if !errors.Is(err, authdomain.ErrWebAuthnVerificationFailed) {
		t.Errorf("FinishLogin(unknown user handle): err = %v, want ErrWebAuthnVerificationFailed", err)
	}
	if creds, _ := repo.ListByMember(context.Background(), household.NewMemberID()); len(creds) != 0 {
		t.Error("sanity: no credential should exist anywhere")
	}
}

func TestWebAuthnService_FinishLogin_WrongChallenge_Fails(t *testing.T) {
	t.Parallel()
	svc, repo, handles, _ := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()
	handle := handles.Derive(memberID)
	credentialID := []byte("synthetic-cred-wrong-challenge")
	auth := newSyntheticAuthenticator(t, credentialID)

	seed := testWebAuthnCredentialForLogin(memberID, credentialID, handle, auth.cosePublicKey(), 0)
	if err := repo.Create(context.Background(), household.NewHouseholdID(), seed); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	challenge := []byte("wrong-challenge-fixed-test-vect")
	body := auth.sign(t, testWebAuthnRPID, testWebAuthnRPOrigin, 0, challenge, handle)
	// The SESSION's stored challenge does not match the one the assertion
	// was actually signed against.
	session := webauthn.SessionData{Challenge: base64.RawURLEncoding.EncodeToString([]byte("this-is-not-the-real-challenge!"))}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		t.Fatalf("ParseCredentialRequestResponseBytes: %v", err)
	}

	_, err = svc.FinishLogin(context.Background(), session, parsed)
	if !errors.Is(err, authdomain.ErrWebAuthnVerificationFailed) {
		t.Errorf("FinishLogin(wrong challenge): err = %v, want ErrWebAuthnVerificationFailed", err)
	}
	creds, _ := repo.ListByMember(context.Background(), memberID)
	if len(creds) != 1 || creds[0].LastUsedAt != nil {
		t.Error("a failed verification must not update the credential's LastUsedAt/sign count")
	}
}

// ---------------------------------------------------------------------------
// Sign-count handling (NES-137) — exercised with syntheticAuthenticator
// (real ES256 signatures at counter values the fixed W3C vector cannot
// express) through FinishLogin's real cryptographic verification path.
// ---------------------------------------------------------------------------

func TestWebAuthnService_FinishLogin_SignCountIncreases_NoNotification(t *testing.T) {
	t.Parallel()
	svc, repo, handles, notify := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()
	handle := handles.Derive(memberID)
	credentialID := []byte("synthetic-cred-increases")
	auth := newSyntheticAuthenticator(t, credentialID)

	seed := testWebAuthnCredentialForLogin(memberID, credentialID, handle, auth.cosePublicKey(), 5)
	if err := repo.Create(context.Background(), household.NewHouseholdID(), seed); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	challenge := []byte("a-fixed-test-challenge-32-bytes")
	body := auth.sign(t, testWebAuthnRPID, testWebAuthnRPOrigin, 6, challenge, handle)
	session := webauthn.SessionData{Challenge: base64.RawURLEncoding.EncodeToString(challenge)}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		t.Fatalf("ParseCredentialRequestResponseBytes: %v", err)
	}

	if _, err := svc.FinishLogin(context.Background(), session, parsed); err != nil {
		t.Fatalf("FinishLogin: %v", err)
	}
	creds, _ := repo.ListByMember(context.Background(), memberID)
	if len(creds) != 1 || creds[0].SignCount != 6 {
		t.Errorf("stored sign count = %+v, want 6", creds)
	}
	if len(notify.events) != 0 {
		t.Errorf("a normal counter advance (5 -> 6) must never notify: got %d notifications", len(notify.events))
	}
}

func TestWebAuthnService_FinishLogin_SignCountDecreases_NotifiesButStillAllowsLogin(t *testing.T) {
	t.Parallel()
	svc, repo, handles, notify := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	handle := handles.Derive(memberID)
	credentialID := []byte("synthetic-cred-decreases")
	auth := newSyntheticAuthenticator(t, credentialID)

	seed := testWebAuthnCredentialForLogin(memberID, credentialID, handle, auth.cosePublicKey(), 10)
	if err := repo.Create(context.Background(), householdID, seed); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	challenge := []byte("another-fixed-test-challenge-32")
	// A decreased, NONZERO count — the exact "possible cloned
	// authenticator" shape signCountSuspicious flags (NES-137 AC).
	body := auth.sign(t, testWebAuthnRPID, testWebAuthnRPOrigin, 3, challenge, handle)
	session := webauthn.SessionData{Challenge: base64.RawURLEncoding.EncodeToString(challenge)}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		t.Fatalf("ParseCredentialRequestResponseBytes: %v", err)
	}

	gotMemberID, err := svc.FinishLogin(context.Background(), session, parsed)
	if err != nil {
		t.Fatalf("FinishLogin: %v (a suspicious sign count must still ALLOW the login — family-appliance threat model)", err)
	}
	if gotMemberID != memberID {
		t.Errorf("FinishLogin resolved member = %v, want %v", gotMemberID, memberID)
	}
	if len(notify.events) != 1 {
		t.Fatalf("notifications after a decreased sign count = %d, want exactly 1", len(notify.events))
	}
	n := notify.events[0]
	if n.HouseholdID != householdID {
		t.Errorf("notification HouseholdID = %v, want %v", n.HouseholdID, householdID)
	}
	if n.MemberID == nil || *n.MemberID != memberID {
		t.Errorf("notification MemberID = %v, want %v", n.MemberID, memberID)
	}
	// Stored sign count still advances to the NEW (lower) value, per
	// applyAssertionResult's own doc — a flagged decrease is still recorded
	// so the NEXT assertion compares against up-to-date state.
	creds, _ := repo.ListByMember(context.Background(), memberID)
	if len(creds) != 1 || creds[0].SignCount != 3 {
		t.Errorf("stored sign count after a flagged decrease = %+v, want 3", creds)
	}
}

func TestWebAuthnService_FinishLogin_SignCountZero_NeverFlagsRegardlessOfStored(t *testing.T) {
	t.Parallel()
	svc, repo, handles, notify := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()
	handle := handles.Derive(memberID)
	credentialID := []byte("synthetic-cred-permanent-zero")
	auth := newSyntheticAuthenticator(t, credentialID)

	// A high stored count followed by a reported 0 — signCountSuspicious's
	// own doc explains why THIS rule never flags newCount == 0, unlike
	// go-webauthn's own default CloneWarning semantics.
	seed := testWebAuthnCredentialForLogin(memberID, credentialID, handle, auth.cosePublicKey(), 42)
	if err := repo.Create(context.Background(), household.NewHouseholdID(), seed); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	challenge := []byte("yet-another-fixed-test-challeng")
	body := auth.sign(t, testWebAuthnRPID, testWebAuthnRPOrigin, 0, challenge, handle)
	session := webauthn.SessionData{Challenge: base64.RawURLEncoding.EncodeToString(challenge)}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		t.Fatalf("ParseCredentialRequestResponseBytes: %v", err)
	}

	if _, err := svc.FinishLogin(context.Background(), session, parsed); err != nil {
		t.Fatalf("FinishLogin: %v", err)
	}
	if len(notify.events) != 0 {
		t.Errorf("a synced-passkey-shaped zero count must never false-positive: got %d notifications", len(notify.events))
	}
	creds, _ := repo.ListByMember(context.Background(), memberID)
	if len(creds) != 1 || creds[0].SignCount != 0 {
		t.Errorf("stored sign count = %+v, want 0", creds)
	}
}

// ---------------------------------------------------------------------------
// BeginStepUp / VerifyStepUp (NES-137)
// ---------------------------------------------------------------------------

func TestWebAuthnService_BeginStepUp_NoCredentials_Fails(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newWebAuthnServiceFixture(t)

	_, _, err := svc.BeginStepUp(context.Background(), household.NewMemberID())
	if !errors.Is(err, authdomain.ErrWebAuthnVerificationFailed) {
		t.Errorf("BeginStepUp(no credentials): err = %v, want ErrWebAuthnVerificationFailed", err)
	}
}

func TestWebAuthnService_BeginStepUp_ListsMembersOwnCredentials(t *testing.T) {
	t.Parallel()
	svc, repo, handles, _ := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()
	handle := handles.Derive(memberID)
	credentialID := []byte("stepup-cred-allow-list")
	// No signature is verified in this test (only the allow-list itself is
	// asserted), so a nil PublicKey is fine here.
	seed := testWebAuthnCredentialForLogin(memberID, credentialID, handle, nil, 0)
	if err := repo.Create(context.Background(), household.NewHouseholdID(), seed); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	assertion, session, err := svc.BeginStepUp(context.Background(), memberID)
	if err != nil {
		t.Fatalf("BeginStepUp: %v", err)
	}
	if len(assertion.Response.AllowedCredentials) != 1 || !bytes.Equal(assertion.Response.AllowedCredentials[0].CredentialID, credentialID) {
		t.Errorf("AllowedCredentials = %v, want exactly [%x] (memberID's own credential — TARGETED, unlike BeginLogin's usernameless empty list)", assertion.Response.AllowedCredentials, credentialID)
	}
	if !bytes.Equal(session.UserID, handle) {
		t.Error("session.UserID does not match memberID's derived handle")
	}
}

func TestWebAuthnService_VerifyStepUp_ValidResponse_Succeeds(t *testing.T) {
	t.Parallel()
	svc, repo, handles, notify := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	handle := handles.Derive(memberID)
	credentialID := []byte("stepup-cred-valid")
	auth := newSyntheticAuthenticator(t, credentialID)

	seed := testWebAuthnCredentialForLogin(memberID, credentialID, handle, auth.cosePublicKey(), 1)
	if err := repo.Create(context.Background(), householdID, seed); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	challenge := []byte("step-up-fixed-test-challenge-32")
	// Step-up is a TARGETED assertion — no userHandle is reported (mirrors
	// a real navigator.credentials.get() call with a non-empty
	// allowCredentials list, unlike a discoverable/usernameless one).
	body := auth.sign(t, testWebAuthnRPID, testWebAuthnRPOrigin, 2, challenge, nil)
	session := webauthn.SessionData{Challenge: base64.RawURLEncoding.EncodeToString(challenge), UserID: handle}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		t.Fatalf("ParseCredentialRequestResponseBytes: %v", err)
	}

	if err := svc.VerifyStepUp(context.Background(), memberID, session, parsed); err != nil {
		t.Fatalf("VerifyStepUp: %v", err)
	}
	creds, _ := repo.ListByMember(context.Background(), memberID)
	if len(creds) != 1 || creds[0].SignCount != 2 || creds[0].LastUsedAt == nil {
		t.Errorf("credential after step-up = %+v, want SignCount 2 and LastUsedAt set", creds)
	}
	if len(notify.events) != 0 {
		t.Errorf("a normal counter advance must never notify: got %d notifications", len(notify.events))
	}
}

func TestWebAuthnService_VerifyStepUp_NoCredentials_Fails(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newWebAuthnServiceFixture(t)
	session := webauthn.SessionData{Challenge: "x"}
	parsed := &protocol.ParsedCredentialAssertionData{}

	err := svc.VerifyStepUp(context.Background(), household.NewMemberID(), session, parsed)
	if !errors.Is(err, authdomain.ErrWebAuthnVerificationFailed) {
		t.Errorf("VerifyStepUp(no credentials): err = %v, want ErrWebAuthnVerificationFailed", err)
	}
}

func TestWebAuthnService_VerifyStepUp_SignCountDecrease_NotifiesAndStillSucceeds(t *testing.T) {
	t.Parallel()
	svc, repo, handles, notify := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	handle := handles.Derive(memberID)
	credentialID := []byte("stepup-cred-decrease")
	auth := newSyntheticAuthenticator(t, credentialID)

	seed := testWebAuthnCredentialForLogin(memberID, credentialID, handle, auth.cosePublicKey(), 20)
	if err := repo.Create(context.Background(), householdID, seed); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	challenge := []byte("step-up-decrease-test-challenge")
	body := auth.sign(t, testWebAuthnRPID, testWebAuthnRPOrigin, 4, challenge, nil)
	session := webauthn.SessionData{Challenge: base64.RawURLEncoding.EncodeToString(challenge), UserID: handle}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		t.Fatalf("ParseCredentialRequestResponseBytes: %v", err)
	}

	if err := svc.VerifyStepUp(context.Background(), memberID, session, parsed); err != nil {
		t.Fatalf("VerifyStepUp: %v (a suspicious sign count must still ALLOW step-up to succeed)", err)
	}
	if len(notify.events) != 1 {
		t.Fatalf("notifications after a decreased step-up sign count = %d, want exactly 1", len(notify.events))
	}
	if notify.events[0].HouseholdID != householdID {
		t.Errorf("notification HouseholdID = %v, want %v", notify.events[0].HouseholdID, householdID)
	}
}

// testWebAuthnCredentialForLogin builds a stored authdomain.WebAuthnCredential
// shaped for a login/step-up test. publicKey is normally a
// syntheticAuthenticator's own cosePublicKey() — nil is fine for a test
// that never verifies a signature (e.g. one that only inspects
// BeginStepUp's allow-list).
func testWebAuthnCredentialForLogin(memberID household.MemberID, credentialID, userHandle, publicKey []byte, signCount uint32) *authdomain.WebAuthnCredential {
	return &authdomain.WebAuthnCredential{
		ID:           authdomain.NewWebAuthnCredentialID(),
		MemberID:     memberID,
		CredentialID: credentialID,
		PublicKey:    publicKey,
		SignCount:    signCount,
		Nickname:     "Test device",
		UserHandle:   userHandle,
	}
}

// ---------------------------------------------------------------------------
// ListDevices / Rename / Revoke
// ---------------------------------------------------------------------------

func TestWebAuthnService_ListDevices_EmptyForNewMember(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newWebAuthnServiceFixture(t)
	creds, err := svc.ListDevices(context.Background(), household.NewMemberID())
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(creds) != 0 {
		t.Errorf("got %d devices for a new member, want 0", len(creds))
	}
}

func TestWebAuthnService_Rename_UpdatesNickname(t *testing.T) {
	t.Parallel()
	svc, repo, _, _ := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	id := authdomain.NewWebAuthnCredentialID()
	if err := repo.Create(context.Background(), householdID, &authdomain.WebAuthnCredential{
		ID: id, MemberID: memberID, CredentialID: []byte("cred"), PublicKey: []byte("pk"), Nickname: "Old",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	if err := svc.Rename(context.Background(), householdID, memberID, id, "New Name"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	creds, _ := repo.ListByMember(context.Background(), memberID)
	if len(creds) != 1 || creds[0].Nickname != "New Name" {
		t.Errorf("Nickname after rename = %+v, want New Name", creds)
	}
}

func TestWebAuthnService_Rename_BlankNickname_Defaults(t *testing.T) {
	t.Parallel()
	svc, repo, _, _ := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	id := authdomain.NewWebAuthnCredentialID()
	if err := repo.Create(context.Background(), householdID, &authdomain.WebAuthnCredential{
		ID: id, MemberID: memberID, CredentialID: []byte("cred"), PublicKey: []byte("pk"), Nickname: "Old",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	if err := svc.Rename(context.Background(), householdID, memberID, id, "  "); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	creds, _ := repo.ListByMember(context.Background(), memberID)
	if len(creds) != 1 || creds[0].Nickname != "Passkey" {
		t.Errorf("Nickname after blank rename = %+v, want the default %q", creds, "Passkey")
	}
}

func TestWebAuthnService_Rename_NotFound(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newWebAuthnServiceFixture(t)
	err := svc.Rename(context.Background(), household.NewHouseholdID(), household.NewMemberID(), authdomain.NewWebAuthnCredentialID(), "x")
	if !errors.Is(err, authdomain.ErrWebAuthnCredentialNotFound) {
		t.Errorf("Rename(unknown id): err = %v, want ErrWebAuthnCredentialNotFound", err)
	}
}

func TestWebAuthnService_Revoke_RemovesCredential(t *testing.T) {
	t.Parallel()
	svc, repo, _, _ := newWebAuthnServiceFixture(t)
	memberID := household.NewMemberID()
	householdID := household.NewHouseholdID()
	id := authdomain.NewWebAuthnCredentialID()
	if err := repo.Create(context.Background(), householdID, &authdomain.WebAuthnCredential{
		ID: id, MemberID: memberID, CredentialID: []byte("cred"), PublicKey: []byte("pk"), Nickname: "Device",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	if err := svc.Revoke(context.Background(), householdID, memberID, id); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	creds, _ := repo.ListByMember(context.Background(), memberID)
	if len(creds) != 0 {
		t.Errorf("credentials after revoke = %d, want 0 (revocation must be immediate)", len(creds))
	}
}

func TestWebAuthnService_Revoke_NotFound(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newWebAuthnServiceFixture(t)
	err := svc.Revoke(context.Background(), household.NewHouseholdID(), household.NewMemberID(), authdomain.NewWebAuthnCredentialID())
	if !errors.Is(err, authdomain.ErrWebAuthnCredentialNotFound) {
		t.Errorf("Revoke(unknown id): err = %v, want ErrWebAuthnCredentialNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// NewWebAuthnService validation
// ---------------------------------------------------------------------------

func TestNewWebAuthnService_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	repo := newFakeWebAuthnCredentialRepo()
	wa := testWebAuthn(t)
	handles := testHandleDeriver(t)
	notify := &fakeNotifyEnqueuer{}
	logger := discardWebAuthnLogger()

	if _, err := app.NewWebAuthnService(nil, wa, handles, notify, logger); err == nil {
		t.Error("NewWebAuthnService(nil repo) must return an error")
	}
	if _, err := app.NewWebAuthnService(repo, nil, handles, notify, logger); err == nil {
		t.Error("NewWebAuthnService(nil *webauthn.WebAuthn) must return an error")
	}
	if _, err := app.NewWebAuthnService(repo, wa, nil, notify, logger); err == nil {
		t.Error("NewWebAuthnService(nil handle deriver) must return an error")
	}
	if _, err := app.NewWebAuthnService(repo, wa, handles, nil, logger); err == nil {
		t.Error("NewWebAuthnService(nil notify Enqueuer) must return an error")
	}
	if _, err := app.NewWebAuthnService(repo, wa, handles, notify, nil); err == nil {
		t.Error("NewWebAuthnService(nil logger) must return an error")
	}
}
