package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// webauthnRegistrationResidentKey is the discoverable-credential preference
// BeginRegistration requests for every registration: "preferred" asks the
// authenticator to create a client-side discoverable (passkey) credential
// when it can, without hard-failing registration on an authenticator that
// cannot.
const webauthnRegistrationResidentKey = protocol.ResidentKeyRequirementPreferred

// defaultCredentialNickname is used when a member submits a blank nickname,
// mirroring kioskapp's own "Kiosk" default-name convention.
const defaultCredentialNickname = "Passkey"

// WebAuthnService orchestrates passkey registration and device management.
// It is the auth context's use-case boundary for NES-136; login enforcement
// is a follow-up ticket (NES-137) and is not implemented here.
type WebAuthnService struct {
	repo    authdomain.WebAuthnCredentialRepository
	wa      *webauthn.WebAuthn
	handles *WebAuthnUserHandleDeriver
	logger  *slog.Logger
}

// NewWebAuthnService constructs the service with injected dependencies. All
// four are required. wa is built once, in the composition root, from
// Server.PublicBaseURL (see cmd/server/main.go) — a *webauthn.WebAuthn
// instance holds only static Relying Party configuration and is safe to
// share across every request.
func NewWebAuthnService(repo authdomain.WebAuthnCredentialRepository, wa *webauthn.WebAuthn, handles *WebAuthnUserHandleDeriver, logger *slog.Logger) (*WebAuthnService, error) {
	if repo == nil {
		return nil, errors.New("auth: NewWebAuthnService requires a non-nil WebAuthnCredentialRepository")
	}
	if wa == nil {
		return nil, errors.New("auth: NewWebAuthnService requires a non-nil *webauthn.WebAuthn")
	}
	if handles == nil {
		return nil, errors.New("auth: NewWebAuthnService requires a non-nil WebAuthnUserHandleDeriver")
	}
	if logger == nil {
		return nil, errors.New("auth: NewWebAuthnService requires a non-nil logger")
	}
	return &WebAuthnService{repo: repo, wa: wa, handles: handles, logger: logger}, nil
}

// ListDevices returns memberID's registered credentials, oldest first, for
// the settings page's "Your devices" list.
func (s *WebAuthnService) ListDevices(ctx context.Context, memberID household.MemberID) ([]authdomain.WebAuthnCredential, error) {
	creds, err := s.repo.ListByMember(ctx, memberID)
	if err != nil {
		return nil, fmt.Errorf("webauthn: list devices: %w", err)
	}
	return creds, nil
}

// BeginRegistration starts a new passkey registration ceremony for
// memberID, returning the credential creation options to send to the
// client (JSON, for the browser's navigator.credentials.create()) and the
// SessionData the caller MUST persist (e.g. the scs session, under a
// dedicated key) until FinishRegistration — and discard afterward
// regardless of outcome; a SessionData/challenge must never be usable
// twice (NES-136 AC: "challenges are single-use and expire").
//
// The registration excludes every credential memberID has already
// registered (WithExclusions), so a browser will not let the SAME physical
// authenticator be registered a second time as a confusing duplicate.
func (s *WebAuthnService) BeginRegistration(ctx context.Context, memberID household.MemberID, displayName string) (*protocol.CredentialCreation, *webauthn.SessionData, error) {
	existing, err := s.repo.ListByMember(ctx, memberID)
	if err != nil {
		return nil, nil, fmt.Errorf("webauthn: begin registration: list existing credentials: %w", err)
	}
	user := s.newUser(memberID, displayName, existing)

	creation, session, err := s.wa.BeginRegistration(user,
		webauthn.WithResidentKeyRequirement(webauthnRegistrationResidentKey),
		webauthn.WithExclusions(webauthn.Credentials(user.WebAuthnCredentials()).CredentialDescriptors()),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("webauthn: begin registration: %w", err)
	}
	return creation, session, nil
}

// FinishRegistration completes a passkey registration ceremony: verifies
// parsedResponse (the browser's attestation response, already parsed by the
// adapter layer from the raw HTTP request body — see
// protocol.ParseCredentialCreationResponseBody, kept out of this layer so
// WebAuthnService never depends on net/http) against session (the value
// BeginRegistration returned), and — on success — persists a new credential
// row under nickname (defaulted to defaultCredentialNickname when blank).
//
// Returns authdomain.ErrWebAuthnVerificationFailed when verification fails
// for any reason (challenge mismatch, expired challenge, replayed
// challenge, RP ID/origin mismatch, signature invalid) — deliberately
// undifferentiated; see that sentinel's own doc.
func (s *WebAuthnService) FinishRegistration(ctx context.Context, memberID household.MemberID, householdID household.HouseholdID, displayName, nickname string, session webauthn.SessionData, parsedResponse *protocol.ParsedCredentialCreationData) error {
	existing, err := s.repo.ListByMember(ctx, memberID)
	if err != nil {
		return fmt.Errorf("webauthn: finish registration: list existing credentials: %w", err)
	}
	user := s.newUser(memberID, displayName, existing)

	cred, err := s.wa.CreateCredential(user, session, parsedResponse)
	if err != nil {
		return fmt.Errorf("%w: %v", authdomain.ErrWebAuthnVerificationFailed, err)
	}

	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		nickname = defaultCredentialNickname
	}
	stored := &authdomain.WebAuthnCredential{
		ID:           authdomain.NewWebAuthnCredentialID(),
		MemberID:     memberID,
		HouseholdID:  householdID,
		CredentialID: cred.ID,
		PublicKey:    cred.PublicKey,
		SignCount:    cred.Authenticator.SignCount,
		Transports:   transportsToStrings(cred.Transport),
		AAGUID:       aaguidToUUID(cred.Authenticator.AAGUID),
		Nickname:     nickname,
		UserHandle:   user.WebAuthnID(),
	}
	if err := s.repo.Create(ctx, householdID, stored); err != nil {
		return fmt.Errorf("webauthn: finish registration: store credential: %w", err)
	}
	s.logger.InfoContext(ctx, "webauthn credential registered", "member_id", memberID.String())
	return nil
}

// Rename updates the nickname on memberID's credential id.
func (s *WebAuthnService) Rename(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID, id authdomain.WebAuthnCredentialID, nickname string) error {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		nickname = defaultCredentialNickname
	}
	if err := s.repo.Rename(ctx, householdID, memberID, id, nickname); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "webauthn credential renamed", "member_id", memberID.String())
	return nil
}

// Revoke removes memberID's credential id immediately (NES-136 AC:
// "revoking a credential removes it immediately").
func (s *WebAuthnService) Revoke(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID, id authdomain.WebAuthnCredentialID) error {
	if err := s.repo.Delete(ctx, householdID, memberID, id); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "webauthn credential revoked", "member_id", memberID.String())
	return nil
}

// newUser builds the webauthn.User adapter for memberID, deriving its
// stable handle via s.handles and carrying existing so
// WebAuthnCredentials() (used for both the exclude-list at registration and
// CreateCredential's internal verification) reflects the member's full,
// current credential set.
func (s *WebAuthnService) newUser(memberID household.MemberID, displayName string, existing []authdomain.WebAuthnCredential) *webAuthnUser {
	return &webAuthnUser{
		id:          s.handles.Derive(memberID),
		displayName: displayName,
		credentials: existing,
	}
}

// webAuthnUser adapts a household.Member (identified only by its derived
// handle and display name — no other member fields are needed by any
// webauthn.User method) plus its existing credentials to satisfy
// webauthn.User.
type webAuthnUser struct {
	id          []byte
	displayName string
	credentials []authdomain.WebAuthnCredential
}

// WebAuthnID returns the member's stable, HMAC-derived handle — never the
// raw member UUID (see WebAuthnUserHandleDeriver's doc).
func (u *webAuthnUser) WebAuthnID() []byte { return u.id }

// WebAuthnName and WebAuthnDisplayName both return the member's display
// name: members are not guaranteed to have an email (mirrors
// MFAService.BeginEnrollment's own accountName parameter doc), so there is
// no separate "name" vs. "display name" distinction to make here.
func (u *webAuthnUser) WebAuthnName() string        { return u.displayName }
func (u *webAuthnUser) WebAuthnDisplayName() string { return u.displayName }

// WebAuthnCredentials converts the member's stored credentials into the
// shape the go-webauthn library operates on.
func (u *webAuthnUser) WebAuthnCredentials() []webauthn.Credential {
	creds := make([]webauthn.Credential, 0, len(u.credentials))
	for _, c := range u.credentials {
		creds = append(creds, toWebAuthnCredential(c))
	}
	return creds
}

// toWebAuthnCredential converts a stored authdomain.WebAuthnCredential back
// into the go-webauthn library's own Credential shape — the inverse of the
// field mapping FinishRegistration performs when persisting one.
func toWebAuthnCredential(c authdomain.WebAuthnCredential) webauthn.Credential {
	wc := webauthn.Credential{
		ID:        c.CredentialID,
		PublicKey: c.PublicKey,
		Authenticator: webauthn.Authenticator{
			SignCount: c.SignCount,
		},
	}
	if len(c.Transports) > 0 {
		wc.Transport = make([]protocol.AuthenticatorTransport, 0, len(c.Transports))
		for _, t := range c.Transports {
			wc.Transport = append(wc.Transport, protocol.AuthenticatorTransport(t))
		}
	}
	if c.AAGUID != nil {
		if raw, err := c.AAGUID.MarshalBinary(); err == nil {
			wc.Authenticator.AAGUID = raw
		}
	}
	return wc
}

// transportsToStrings converts the go-webauthn library's transport hint
// type to the plain strings member_credential.transports stores.
func transportsToStrings(transports []protocol.AuthenticatorTransport) []string {
	if len(transports) == 0 {
		return nil
	}
	out := make([]string, 0, len(transports))
	for _, t := range transports {
		out = append(out, string(t))
	}
	return out
}

// aaguidToUUID converts the go-webauthn library's raw AAGUID bytes to a
// *uuid.UUID, returning nil both when the authenticator reported no AAGUID
// at all (a length other than 16) and when it reported the all-zero AAGUID
// (uuid.Nil) some authenticator models use to mean the same thing — both
// are "unknown model", not two different states worth distinguishing in
// storage.
func aaguidToUUID(raw []byte) *uuid.UUID {
	if len(raw) != 16 {
		return nil
	}
	id, err := uuid.FromBytes(raw)
	if err != nil || id == uuid.Nil {
		return nil
	}
	return &id
}
