package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
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

// newWebAuthnServiceFixture wires a WebAuthnService with fully controllable
// fakes, so tests can both exercise the service and assert directly against
// the fake repository's state.
func newWebAuthnServiceFixture(t *testing.T) (*app.WebAuthnService, *fakeWebAuthnCredentialRepo, *app.WebAuthnUserHandleDeriver) {
	t.Helper()
	repo := newFakeWebAuthnCredentialRepo()
	handles := testHandleDeriver(t)
	svc, err := app.NewWebAuthnService(repo, testWebAuthn(t), handles, discardWebAuthnLogger())
	if err != nil {
		t.Fatalf("NewWebAuthnService: %v", err)
	}
	return svc, repo, handles
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
// BeginRegistration
// ---------------------------------------------------------------------------

func TestWebAuthnService_BeginRegistration_ReturnsCreationOptionsAndSession(t *testing.T) {
	t.Parallel()
	svc, _, handles := newWebAuthnServiceFixture(t)
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
	svc, _, handles := newWebAuthnServiceFixture(t)
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
	svc, repo, _ := newWebAuthnServiceFixture(t)
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
	svc, repo, handles := newWebAuthnServiceFixture(t)
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
	svc, repo, handles := newWebAuthnServiceFixture(t)
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
	svc, repo, handles := newWebAuthnServiceFixture(t)
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
	svc, repo, _ := newWebAuthnServiceFixture(t)
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
// ListDevices / Rename / Revoke
// ---------------------------------------------------------------------------

func TestWebAuthnService_ListDevices_EmptyForNewMember(t *testing.T) {
	t.Parallel()
	svc, _, _ := newWebAuthnServiceFixture(t)
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
	svc, repo, _ := newWebAuthnServiceFixture(t)
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
	svc, repo, _ := newWebAuthnServiceFixture(t)
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
	svc, _, _ := newWebAuthnServiceFixture(t)
	err := svc.Rename(context.Background(), household.NewHouseholdID(), household.NewMemberID(), authdomain.NewWebAuthnCredentialID(), "x")
	if !errors.Is(err, authdomain.ErrWebAuthnCredentialNotFound) {
		t.Errorf("Rename(unknown id): err = %v, want ErrWebAuthnCredentialNotFound", err)
	}
}

func TestWebAuthnService_Revoke_RemovesCredential(t *testing.T) {
	t.Parallel()
	svc, repo, _ := newWebAuthnServiceFixture(t)
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
	svc, _, _ := newWebAuthnServiceFixture(t)
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
	logger := discardWebAuthnLogger()

	if _, err := app.NewWebAuthnService(nil, wa, handles, logger); err == nil {
		t.Error("NewWebAuthnService(nil repo) must return an error")
	}
	if _, err := app.NewWebAuthnService(repo, nil, handles, logger); err == nil {
		t.Error("NewWebAuthnService(nil *webauthn.WebAuthn) must return an error")
	}
	if _, err := app.NewWebAuthnService(repo, wa, nil, logger); err == nil {
		t.Error("NewWebAuthnService(nil handle deriver) must return an error")
	}
	if _, err := app.NewWebAuthnService(repo, wa, handles, nil); err == nil {
		t.Error("NewWebAuthnService(nil logger) must return an error")
	}
}
