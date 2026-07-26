package domain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Federation authorization code errors (NSTR-105/NSTR-110): the
// authorize-then-exchange flow's own error vocabulary. It is kept separate
// from Credential's and MFAEnrollment's sentinels even though the concept
// mirrors kiosk's activation code closely — see AuthorizationCodeRepository's
// doc for which of these this package's own Postgres adapter can return
// versus which NSTR-110's exchange handler raises itself.
var (
	// ErrAuthorizationCodeNotFound is returned when a code does not resolve
	// to any row (unknown or never issued).
	ErrAuthorizationCodeNotFound = errors.New("auth: authorization code not found")
	// ErrAuthorizationCodeUsed is returned when a code has already been
	// consumed.
	ErrAuthorizationCodeUsed = errors.New("auth: authorization code already used")
	// ErrAuthorizationCodeExpired is returned when a code's expiry has
	// passed.
	ErrAuthorizationCodeExpired = errors.New("auth: authorization code expired")
	// ErrAuthorizationCodeBindingMismatch is NSTR-110's sentinel for a code
	// redeemed with a client_id or redirect_uri different from the ones it
	// was issued for. AuthorizationCodeRepository.Consume never returns this
	// itself — a hash lookup has no client or redirect of its own to compare
	// against — it is defined here, alongside the rest of this type's error
	// vocabulary, for NSTR-110's exchange handler to return after comparing
	// Consume's resolved ClientID/RedirectURI against its own request.
	ErrAuthorizationCodeBindingMismatch = errors.New("auth: authorization code was issued for a different client or redirect")
	// ErrInvalidAuthorizationCode is returned by AuthorizationCode.Validate
	// for a malformed code.
	ErrInvalidAuthorizationCode = errors.New("auth: invalid authorization code")
)

// AuthorizationCodeTTL is how long a generated federation authorization code
// remains redeemable before it expires. The hop it covers — a browser
// redirect from GET /federation/authorize straight into NSTR-110's
// back-channel POST /federation/token — happens at machine speed, not human
// speed (unlike kiosk's 15-minute ActivationCodeTTL, which waits on a person
// walking to a device); RFC 6749 section 4.1.2 recommends capping this at 10
// minutes, and this is deliberately far inside that ceiling.
const AuthorizationCodeTTL = 2 * time.Minute

// authorizationCodeBytes is the raw entropy of a generated authorization
// code: 32 bytes (256 bits), hex-encoded. Unlike kiosk's hand-typed
// ActivationCode, this code only ever travels as a URL query parameter
// between machines and is never read off a screen and typed, so it has no
// reason to trade entropy for brevity — it mirrors
// kiosk/domain.GenerateToken's sizing, not GenerateActivationCode's.
const authorizationCodeBytes = 32

// GenerateAuthorizationCode returns a new random, hex-encoded federation
// authorization code. It errors only when the system's random source is
// unavailable, which is a fatal condition (mirrors
// kiosk/domain.GenerateToken's identical contract).
func GenerateAuthorizationCode() (string, error) {
	b := make([]byte, authorizationCodeBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate authorization code: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// HashAuthorizationCode returns the SHA-256 hex digest of a raw
// authorization code, the form persisted in
// federation_authorization_code.code_hash.
//
// SHA-256 (not the argon2id KDF internal/platform/crypto applies to member
// passwords) is deliberate: the raw code is 256 bits of crypto/rand output,
// not a human-chosen secret, so it already carries full entropy and there is
// no dictionary/rainbow-table attack to defend against by stretching it —
// mirrors kiosk/domain.HashToken's identical reasoning.
func HashAuthorizationCode(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// AuthorizationCode is a short-lived, single-use credential GET
// /federation/authorize (NSTR-105) issues to the browser once a member has
// authenticated, and NSTR-110's POST /federation/token exchanges for that
// member's identity document. It is bound to the exact client and redirect
// it was issued for, so a code minted for one registered client can never be
// redeemed by presenting different client/redirect values at the exchange.
type AuthorizationCode struct {
	ID       AuthorizationCodeID
	MemberID household.MemberID
	// ClientID is the registered client identifier this code was issued
	// for — validated against config.FederationConfig.ClientID by Authorize
	// before the code is ever minted.
	ClientID string
	// RedirectURI is the exact redirect target this code was issued for —
	// validated against config.FederationConfig.RedirectURL by Authorize.
	RedirectURI string
	// CodeHash is the SHA-256 hex digest of the raw code (see
	// HashAuthorizationCode); the raw code is never persisted.
	CodeHash  string
	CreatedAt time.Time
	ExpiresAt time.Time
	// UsedAt is nil until Consume redeems the code.
	UsedAt *time.Time
}

// Validate reports whether the code is well-formed, wrapping
// ErrInvalidAuthorizationCode.
func (c *AuthorizationCode) Validate() error {
	if c.ID == (AuthorizationCodeID{}) {
		return fmt.Errorf("%w: id is required", ErrInvalidAuthorizationCode)
	}
	if c.MemberID == (household.MemberID{}) {
		return fmt.Errorf("%w: member id is required", ErrInvalidAuthorizationCode)
	}
	if strings.TrimSpace(c.ClientID) == "" {
		return fmt.Errorf("%w: client id must not be blank", ErrInvalidAuthorizationCode)
	}
	if strings.TrimSpace(c.RedirectURI) == "" {
		return fmt.Errorf("%w: redirect uri must not be blank", ErrInvalidAuthorizationCode)
	}
	if strings.TrimSpace(c.CodeHash) == "" {
		return fmt.Errorf("%w: code hash must not be blank", ErrInvalidAuthorizationCode)
	}
	if c.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: expires at is required", ErrInvalidAuthorizationCode)
	}
	return nil
}

// Usable reports whether the code can still be consumed as of now.
func (c *AuthorizationCode) Usable(now time.Time) bool {
	return c.UsedAt == nil && now.Before(c.ExpiresAt)
}

// AuthorizationCodeRepository persists federation authorization codes issued
// by GET /federation/authorize (NSTR-105) and consumed by NSTR-110's POST
// /federation/token exchange.
//
// Contracts:
//   - Create inserts a code (the caller sets ID, MemberID, ClientID,
//     RedirectURI, CodeHash, and ExpiresAt); it populates CreatedAt.
//   - Consume atomically validates codeHash against now (the code must be
//     unused and unexpired), marks it used, and returns the resolved code —
//     including the ClientID/RedirectURI it was bound to, which NSTR-110's
//     exchange compares against its own request and reports as
//     ErrAuthorizationCodeBindingMismatch on a mismatch (this repository
//     never returns that sentinel itself: a hash lookup has no client or
//     redirect of its own to compare against). Returns
//     ErrAuthorizationCodeNotFound for an unknown hash,
//     ErrAuthorizationCodeUsed for an already-consumed code, and
//     ErrAuthorizationCodeExpired for one past its expiry.
type AuthorizationCodeRepository interface {
	Create(ctx context.Context, code *AuthorizationCode) error
	Consume(ctx context.Context, codeHash string, now time.Time) (*AuthorizationCode, error)
}
