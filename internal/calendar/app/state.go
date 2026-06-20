package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// stateTTL bounds how long a signed OAuth state is accepted, limiting the window
// for a replayed authorization callback.
const stateTTL = 10 * time.Minute

// OAuth state errors.
var (
	// ErrInvalidState is returned when a state value is malformed, has a bad
	// signature, or has expired. It is deliberately coarse so a caller cannot
	// distinguish tampering from expiry.
	ErrInvalidState = errors.New("calendar: invalid oauth state")
)

// OAuthStateSigner signs and verifies the OAuth `state` parameter, which binds a
// callback to the member who started the flow and protects the round trip from
// CSRF. The state is stateless: it carries the member id and an expiry, signed
// with an HMAC key (the session secret), so no server-side storage is needed.
type OAuthStateSigner struct {
	key []byte
}

// NewOAuthStateSigner constructs a signer from a non-empty HMAC key.
func NewOAuthStateSigner(key []byte) (*OAuthStateSigner, error) {
	if len(key) == 0 {
		return nil, errors.New("calendar: oauth state signer requires a non-empty key")
	}
	return &OAuthStateSigner{key: key}, nil
}

// Sign returns a signed state binding memberID, valid until now+stateTTL. The
// format is base64url(payload) "." base64url(hmac), where payload is
// "memberID|expiryUnix".
func (s *OAuthStateSigner) Sign(memberID string, now time.Time) string {
	payload := []byte(memberID + "|" + strconv.FormatInt(now.Add(stateTTL).Unix(), 10))
	return encode(payload) + "." + encode(s.mac(payload))
}

// Verify checks the signature and expiry of state as of now and returns the
// member id it carries, or ErrInvalidState.
func (s *OAuthStateSigner) Verify(state string, now time.Time) (string, error) {
	encPayload, encMAC, ok := strings.Cut(state, ".")
	if !ok {
		return "", ErrInvalidState
	}
	payload, err := decode(encPayload)
	if err != nil {
		return "", ErrInvalidState
	}
	gotMAC, err := decode(encMAC)
	if err != nil {
		return "", ErrInvalidState
	}
	if !hmac.Equal(gotMAC, s.mac(payload)) {
		return "", ErrInvalidState
	}
	memberID, expiryStr, ok := strings.Cut(string(payload), "|")
	if !ok {
		return "", ErrInvalidState
	}
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return "", fmt.Errorf("%w: bad expiry", ErrInvalidState)
	}
	if now.Unix() > expiry {
		return "", fmt.Errorf("%w: expired", ErrInvalidState)
	}
	return memberID, nil
}

func (s *OAuthStateSigner) mac(payload []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write(payload)
	return m.Sum(nil)
}

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
