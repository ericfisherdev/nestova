package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
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

// webauthnSignCountAnomalyTitle/Body are the outbox notification raised when
// an assertion's sign count decrease looks suspicious (see
// signCountSuspicious's own doc for the exact rule) — the member is told
// through the SAME channel every other Nestova notification uses, mirroring
// LoginMFAHandlers' login-lockout notification (NES-135).
const (
	webauthnSignCountAnomalyTitle = "Unusual passkey activity detected"
	webauthnSignCountAnomalyBody  = "A passkey sign-in reported an authenticator counter lower than expected, which can indicate the passkey was cloned. If this wasn't you, revoke the passkey from Settings and register a new one."
)

// WebAuthnService orchestrates passkey registration, device management
// (NES-136), and passkey login and step-up verification (NES-137).
type WebAuthnService struct {
	repo    authdomain.WebAuthnCredentialRepository
	wa      *webauthn.WebAuthn
	handles *WebAuthnUserHandleDeriver
	notify  notifydomain.Enqueuer
	logger  *slog.Logger
}

// NewWebAuthnService constructs the service with injected dependencies. All
// five are required. wa is built once, in the composition root, from
// Server.PublicBaseURL (see cmd/server/main.go) — a *webauthn.WebAuthn
// instance holds only static Relying Party configuration and is safe to
// share across every request. notify is the SAME outbox Enqueuer
// LoginMFAHandlers already depends on (NES-135) — a passkey login or
// step-up assertion that reports a suspicious sign count decrease
// (applyAssertionResult) raises a notification through it.
func NewWebAuthnService(repo authdomain.WebAuthnCredentialRepository, wa *webauthn.WebAuthn, handles *WebAuthnUserHandleDeriver, notify notifydomain.Enqueuer, logger *slog.Logger) (*WebAuthnService, error) {
	if repo == nil {
		return nil, errors.New("auth: NewWebAuthnService requires a non-nil WebAuthnCredentialRepository")
	}
	if wa == nil {
		return nil, errors.New("auth: NewWebAuthnService requires a non-nil *webauthn.WebAuthn")
	}
	if handles == nil {
		return nil, errors.New("auth: NewWebAuthnService requires a non-nil WebAuthnUserHandleDeriver")
	}
	if notify == nil {
		return nil, errors.New("auth: NewWebAuthnService requires a non-nil notify Enqueuer")
	}
	if logger == nil {
		return nil, errors.New("auth: NewWebAuthnService requires a non-nil logger")
	}
	return &WebAuthnService{repo: repo, wa: wa, handles: handles, notify: notify, logger: logger}, nil
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

// ---------------------------------------------------------------------------
// Login and step-up (NES-137)
// ---------------------------------------------------------------------------

// BeginLogin starts a new usernameless ("Sign in with passkey") login
// ceremony: an empty allowCredentials list, so the browser offers any of
// its discoverable credentials registered for this Relying Party rather
// than a specific member's — the server does not yet know who is
// authenticating. The caller MUST persist the returned SessionData (e.g.
// the pre-auth scs session, which is already alive before login — see
// finishLogin's own doc in adapter/http.go) until FinishLogin, and discard
// it afterward regardless of outcome, mirroring BeginRegistration's
// single-use challenge contract.
func (s *WebAuthnService) BeginLogin(_ context.Context) (*protocol.CredentialAssertion, *webauthn.SessionData, error) {
	assertion, session, err := s.wa.BeginDiscoverableLogin()
	if err != nil {
		return nil, nil, fmt.Errorf("webauthn: begin login: %w", err)
	}
	return assertion, session, nil
}

// FinishLogin completes a usernameless login ceremony: verifies
// parsedResponse (the browser's assertion response, already parsed by the
// adapter layer — see FinishRegistration's own doc for why parsing stays
// out of this layer) against session, resolving WHICH member is
// authenticating from the assertion's own reported user handle via
// s.repo.FindByUserHandle — the server has no other way to know, since this
// is a usernameless ceremony. On success it returns the resolved member id
// and applies the same post-assertion bookkeeping VerifyStepUp does (see
// applyAssertionResult).
//
// Returns authdomain.ErrWebAuthnVerificationFailed when verification fails
// for any reason (challenge mismatch, expired challenge, wrong RP ID/
// origin, unresolvable or unknown user handle, signature invalid) —
// deliberately undifferentiated, mirroring FinishRegistration's own no-oracle
// convention: a caller must not be able to tell "no such passkey" from "bad
// signature" from "expired challenge".
func (s *WebAuthnService) FinishLogin(ctx context.Context, session webauthn.SessionData, parsedResponse *protocol.ParsedCredentialAssertionData) (household.MemberID, error) {
	var (
		resolvedMemberID    household.MemberID
		resolvedHouseholdID household.HouseholdID
		storedCredentials   []authdomain.WebAuthnCredential
	)
	handler := func(_, userHandle []byte) (webauthn.User, error) {
		memberID, creds, err := s.repo.FindByUserHandle(ctx, userHandle)
		if err != nil {
			return nil, err
		}
		resolvedMemberID = memberID
		resolvedHouseholdID = creds[0].HouseholdID
		storedCredentials = creds
		return s.newUser(memberID, "", creds), nil
	}

	if _, _, err := s.wa.ValidatePasskeyLogin(handler, session, parsedResponse); err != nil {
		return household.MemberID{}, fmt.Errorf("%w: %v", authdomain.ErrWebAuthnVerificationFailed, err)
	}

	if err := s.applyAssertionResult(ctx, resolvedHouseholdID, resolvedMemberID, storedCredentials, parsedResponse); err != nil {
		return household.MemberID{}, err
	}
	s.logger.InfoContext(ctx, "webauthn login verified", "member_id", resolvedMemberID.String())
	return resolvedMemberID, nil
}

// BeginStepUp starts a TARGETED login ceremony for memberID — unlike
// BeginLogin, the member is already known (an authenticated session
// re-proving freshness for RequireStepUp, adapter/session.go), so the
// assertion options list memberID's own registered credentials via
// allowCredentials rather than requesting a discoverable/usernameless
// assertion. Returns authdomain.ErrWebAuthnVerificationFailed when memberID
// has no registered credentials to assert with at all — the caller (the
// step-up prompt page) is expected to only offer this option when it
// already knows the member has at least one (WebAuthnService.ListDevices),
// so this is a defense-in-depth guard, not the primary gate.
func (s *WebAuthnService) BeginStepUp(ctx context.Context, memberID household.MemberID) (*protocol.CredentialAssertion, *webauthn.SessionData, error) {
	creds, err := s.repo.ListByMember(ctx, memberID)
	if err != nil {
		return nil, nil, fmt.Errorf("webauthn: begin step-up: list credentials: %w", err)
	}
	if len(creds) == 0 {
		return nil, nil, authdomain.ErrWebAuthnVerificationFailed
	}
	user := s.newUser(memberID, "", creds)
	assertion, session, err := s.wa.BeginLogin(user)
	if err != nil {
		return nil, nil, fmt.Errorf("webauthn: begin step-up: %w", err)
	}
	return assertion, session, nil
}

// VerifyStepUp completes a targeted step-up assertion for memberID (already
// known from the caller's own authenticated session — unlike FinishLogin,
// there is no identity to resolve from the assertion itself) and applies
// the same post-assertion bookkeeping FinishLogin does (see
// applyAssertionResult). householdID for that bookkeeping is read off
// memberID's own stored credentials (creds[0].HouseholdID) — the SAME
// technique FinishLogin uses — deliberately NOT taken as a caller-supplied
// parameter: a caller (e.g. LoginMFAHandlers, serving a pending member who
// may have no TOTP enrollment at all) may have no other reliable, always-
// available source for it.
//
// A successful return means the assertion was user-verified (this
// codebase's Relying Party config requires UV — see cmd/server/main.go's
// AuthenticatorSelection — so a caller never needs to separately inspect
// the returned credential's flags): the caller is responsible for stamping
// the session's own freshness marker (mfa_verified_at) on success,
// mirroring how a TOTP step-up verification does the same
// (LoginMFAHandlers.Verify).
//
// Returns authdomain.ErrWebAuthnVerificationFailed both when memberID has
// no registered credentials at all (defense-in-depth — BeginStepUp's own
// doc explains why this should not normally be reachable) and on any
// verification failure, identical in shape to FinishLogin's own no-oracle
// convention.
func (s *WebAuthnService) VerifyStepUp(ctx context.Context, memberID household.MemberID, session webauthn.SessionData, parsedResponse *protocol.ParsedCredentialAssertionData) error {
	creds, err := s.repo.ListByMember(ctx, memberID)
	if err != nil {
		return fmt.Errorf("webauthn: verify step-up: list credentials: %w", err)
	}
	if len(creds) == 0 {
		return authdomain.ErrWebAuthnVerificationFailed
	}
	user := s.newUser(memberID, "", creds)

	if _, err := s.wa.ValidateLogin(user, session, parsedResponse); err != nil {
		return fmt.Errorf("%w: %v", authdomain.ErrWebAuthnVerificationFailed, err)
	}

	if err := s.applyAssertionResult(ctx, creds[0].HouseholdID, memberID, creds, parsedResponse); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "webauthn step-up verified", "member_id", memberID.String())
	return nil
}

// signCountSuspicious reports whether newCount — the authenticator's
// signature counter as freshly reported in an assertion — signals a
// possible cloned authenticator, given storedCount, the value on file
// before this assertion.
//
// Suspicious ONLY when newCount is nonzero AND less than storedCount. A
// synced passkey (iCloud Keychain, Google Password Manager) permanently
// reports a counter of 0 — by design, since a synced credential has no
// single physical counter to track — so newCount == 0 must never
// false-positive regardless of storedCount, including a transition FROM a
// nonzero storedCount (e.g. a member switching from a hardware key to a
// synced passkey re-registration is out of scope here; a nonzero-to-zero
// transition on the SAME credential row would be unusual, but this
// deliberately narrow rule still does not flag it — see the package doc's
// note on this being a family-appliance threat model, not a
// high-assurance one).
//
// go-webauthn's own Authenticator.CloneWarning is NOT used for this
// decision: its default semantics (verified via go doc against the
// installed go-webauthn version) flag ANY newCount <= storedCount other
// than "both zero" — including a nonzero-to-zero transition — which is
// broader than the rule this service wants.
func signCountSuspicious(newCount, storedCount uint32) bool {
	return newCount != 0 && newCount < storedCount
}

// applyAssertionResult persists the authenticator's new signature counter
// and last-used timestamp for the asserted credential (identified within
// credentials by matching parsedResponse.RawID) and, when
// signCountSuspicious flags the new count, enqueues a best-effort member
// notification — mirroring LoginMFAHandlers.notifyLockout's convention
// exactly: a failure to enqueue is logged and swallowed, never turned into
// an error for an assertion that has ALREADY succeeded (the login or
// step-up itself is still allowed either way — see signCountSuspicious's
// own doc for the threat model this reflects).
func (s *WebAuthnService) applyAssertionResult(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID, credentials []authdomain.WebAuthnCredential, parsedResponse *protocol.ParsedCredentialAssertionData) error {
	newCount := parsedResponse.Response.AuthenticatorData.Counter

	var storedCount uint32
	for _, c := range credentials {
		if bytes.Equal(c.CredentialID, parsedResponse.RawID) {
			storedCount = c.SignCount
			break
		}
	}

	if err := s.repo.UpdateAfterAssertion(ctx, parsedResponse.RawID, newCount, time.Now()); err != nil {
		return fmt.Errorf("webauthn: update sign count: %w", err)
	}

	if signCountSuspicious(newCount, storedCount) {
		s.notifySuspiciousSignCount(ctx, householdID, memberID)
	}
	return nil
}

// notifySuspiciousSignCount enqueues the sign-count-anomaly notification
// for memberID, best-effort — see applyAssertionResult's own doc for why a
// failure here is logged, not propagated.
func (s *WebAuthnService) notifySuspiciousSignCount(ctx context.Context, householdID household.HouseholdID, memberID household.MemberID) {
	s.logger.WarnContext(ctx, "webauthn: passkey sign count decreased unexpectedly", "member_id", memberID.String())
	n := &notifydomain.Notification{
		ID:           notifydomain.NewNotificationID(),
		HouseholdID:  householdID,
		MemberID:     &memberID,
		Channel:      notifydomain.ChannelInApp,
		Title:        webauthnSignCountAnomalyTitle,
		Body:         webauthnSignCountAnomalyBody,
		ScheduledFor: time.Now(),
		Status:       notifydomain.StatusPending,
	}
	if err := s.notify.Enqueue(ctx, n); err != nil {
		s.logger.ErrorContext(ctx, "webauthn: enqueue sign count anomaly notification", "error", err)
	}
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
