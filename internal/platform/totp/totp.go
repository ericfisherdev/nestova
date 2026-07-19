// Package totp is a thin platform seam over github.com/pquerna/otp (RFC 6238
// TOTP), mirroring internal/platform/qrcode and internal/platform/crypto:
// it isolates the third-party dependency behind a small, stateless surface
// so the auth application layer depends on its own minimal interface (see
// internal/auth/app's totpProvider) rather than the library directly,
// keeping that layer's tests hermetic and the library swappable.
package totp

import (
	"crypto/subtle"
	"fmt"
	"time"

	pquernatotp "github.com/pquerna/otp/totp"
)

// period and skew mirror Validate's own Google-Authenticator-compatible
// tolerance (pquerna/otp's 30-second default period, ±1 period skew) —
// MatchStep must apply the IDENTICAL window Validate does, just with
// visibility into WHICH step matched.
const (
	period = 30
	skew   = 1
)

// Provider generates and validates RFC 6238 TOTP secrets using the standard
// Google-Authenticator-compatible parameters (30-second period, 6 digits,
// SHA1, ±1 period clock-skew tolerance) via pquerna/otp's defaults. It holds
// no state, so the zero value is ready to use.
type Provider struct{}

// NewProvider constructs a Provider. It takes no dependencies; the
// constructor exists for symmetry with this codebase's other platform seams
// (e.g. crypto.NewCipher, qrcode's package functions) and so composition
// roots inject it the same way as every other dependency.
func NewProvider() *Provider { return &Provider{} }

// GenerateSecret creates a new random TOTP secret for accountName under
// issuer, returning the base32-encoded secret (for manual entry) and its
// otpauth:// provisioning URI (for QR code rendering). The secret is never
// logged or returned to any caller other than the one enrolling.
func (Provider) GenerateSecret(issuer, accountName string) (secret, otpauthURL string, err error) {
	key, err := pquernatotp.Generate(pquernatotp.GenerateOpts{
		Issuer:      issuer,
		AccountName: accountName,
	})
	if err != nil {
		return "", "", fmt.Errorf("totp: generate secret: %w", err)
	}
	return key.Secret(), key.URL(), nil
}

// Validate reports whether code is currently valid for secret.
func (Provider) Validate(code, secret string) bool {
	return pquernatotp.Validate(code, secret)
}

// MatchStep reports whether code is valid for secret at any RFC 6238 step
// (the counter, floor(unix/period)) within the ±1-period skew window of now
// — the SAME tolerance Validate applies — and, when it is, which step
// matched. Unlike Validate's plain bool, the matched step lets a caller
// enforce a durable replay guard across steps (NES-135's login verification:
// the same code must never be accepted twice, even across the skew window,
// which requires knowing WHICH of the up to three candidate steps a
// submitted code actually corresponds to).
//
// When more than one candidate step matches — an astronomically unlikely
// coincidence for a 6-digit code, but not provably impossible — the HIGHEST
// step is returned, so a caller's replay guard stays maximally restrictive
// rather than accidentally permissive.
func (Provider) MatchStep(code, secret string) (step int64, ok bool) {
	current := time.Now().UTC().Unix() / period
	for i := -skew; i <= skew; i++ {
		candidate := current + int64(i)
		want, err := pquernatotp.GenerateCodeCustom(secret, time.Unix(candidate*period, 0).UTC(), pquernatotp.ValidateOpts{
			Period: period,
		})
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 && (!ok || candidate > step) {
			step = candidate
			ok = true
		}
	}
	return step, ok
}
