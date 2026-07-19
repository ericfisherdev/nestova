package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// RememberDeviceTTL bounds how long a "remember this device" cookie skips
// the login MFA step (NES-135's acceptance criterion: 30 days). A
// remembered device is exempt from the LOGIN-time prompt only — it is not
// exempt from RequireStepUp's own freshness check on a security-sensitive
// action.
const RememberDeviceTTL = 30 * 24 * time.Hour

// ErrInvalidRememberToken is returned by RememberDeviceSigner.Verify when a
// presented remember-device token is malformed, has a bad signature, or has
// expired — deliberately coarse (mirrors calendarapp.OAuthStateSigner's
// ErrInvalidState) so a caller cannot distinguish tampering from expiry.
var ErrInvalidRememberToken = errors.New("auth: invalid remember-device token")

// RememberDeviceSigner signs and verifies the "remember this device" cookie
// value: a stateless (no server-side storage) HMAC-SHA256-signed token
// binding a memberID and an expiry, mirroring
// internal/deeplink/app.Signer and internal/calendar/app.OAuthStateSigner's
// own purpose-scoped derivation pattern. The signature is NOT itself an
// authorization grant beyond "skip the login MFA prompt": every
// security-sensitive action still goes through RequireStepUp independently.
type RememberDeviceSigner struct {
	key []byte
}

// NewRememberDeviceSigner constructs a signer from a non-empty HMAC key.
func NewRememberDeviceSigner(key []byte) (*RememberDeviceSigner, error) {
	if len(key) == 0 {
		return nil, errors.New("auth: remember-device signer requires a non-empty key")
	}
	return &RememberDeviceSigner{key: key}, nil
}

// NewRememberDeviceSignerFromSecret derives a purpose-scoped HMAC key from
// secret via HMAC-SHA256(secret, purpose) and constructs a
// RememberDeviceSigner from it — mirroring
// internal/deeplink/app.NewSignerFromSecret's doc for why: cfg.Session.Secret
// is shared by every signing consumer in this codebase, and deriving a
// distinct subkey per purpose keeps each cryptographically independent even
// though they trace back to the same root secret.
func NewRememberDeviceSignerFromSecret(secret []byte, purpose string) (*RememberDeviceSigner, error) {
	if len(secret) == 0 {
		return nil, errors.New("auth: remember-device signer requires a non-empty secret")
	}
	if purpose == "" {
		return nil, errors.New("auth: remember-device signer requires a non-empty purpose label")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(purpose))
	return NewRememberDeviceSigner(mac.Sum(nil))
}

// Sign returns a signed token binding memberID, valid until now+RememberDeviceTTL.
func (s *RememberDeviceSigner) Sign(memberID household.MemberID, now time.Time) string {
	payload := []byte(memberID.String() + "|" + strconv.FormatInt(now.Add(RememberDeviceTTL).Unix(), 10))
	return rememberEncode(payload) + "." + rememberEncode(s.mac(payload))
}

// Verify checks token's signature and expiry as of now and returns the
// member id it carries, or ErrInvalidRememberToken.
func (s *RememberDeviceSigner) Verify(token string, now time.Time) (household.MemberID, error) {
	encPayload, encMAC, ok := strings.Cut(token, ".")
	if !ok {
		return household.MemberID{}, ErrInvalidRememberToken
	}
	payload, err := rememberDecode(encPayload)
	if err != nil {
		return household.MemberID{}, ErrInvalidRememberToken
	}
	gotMAC, err := rememberDecode(encMAC)
	if err != nil {
		return household.MemberID{}, ErrInvalidRememberToken
	}
	if !hmac.Equal(gotMAC, s.mac(payload)) {
		return household.MemberID{}, ErrInvalidRememberToken
	}

	memberIDStr, expiryStr, ok := strings.Cut(string(payload), "|")
	if !ok {
		return household.MemberID{}, ErrInvalidRememberToken
	}
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return household.MemberID{}, ErrInvalidRememberToken
	}
	if now.Unix() > expiry {
		return household.MemberID{}, ErrInvalidRememberToken
	}
	memberID, err := household.ParseMemberID(memberIDStr)
	if err != nil {
		return household.MemberID{}, ErrInvalidRememberToken
	}
	return memberID, nil
}

func (s *RememberDeviceSigner) mac(payload []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write(payload)
	return m.Sum(nil)
}

func rememberEncode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func rememberDecode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
