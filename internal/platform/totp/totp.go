// Package totp is a thin platform seam over github.com/pquerna/otp (RFC 6238
// TOTP), mirroring internal/platform/qrcode and internal/platform/crypto:
// it isolates the third-party dependency behind a small, stateless surface
// so the auth application layer depends on its own minimal interface (see
// internal/auth/app's totpProvider) rather than the library directly,
// keeping that layer's tests hermetic and the library swappable.
package totp

import (
	"fmt"

	pquernatotp "github.com/pquerna/otp/totp"
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
