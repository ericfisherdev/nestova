package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/ericfisherdev/nestova/internal/deeplink/domain"
)

// LinkTTL bounds how long a signed QR deep link is accepted after the kiosk
// renders it. Kept short (NES-129's ~10 minute target) because the kiosk
// re-signs every QR on each render (see internal/kiosk/adapter), so a live
// display always shows a fresh, short-lived link; a code that outlives this
// window was either scanned long after it was displayed (the kiosk has since
// re-rendered a new one) or was never legitimately displayed at all.
const LinkTTL = 10 * time.Minute

// Signer signs and verifies a deep link's path + expiry with an HMAC-SHA256
// key, so a link can be authenticated statelessly (no server-side storage) —
// mirroring internal/calendar/app's OAuthStateSigner. The signature is NOT an
// authorization grant: it only proves the path+expiry pair was minted by this
// server and has not been tampered with. Every action the path names is
// re-authorized independently by the member's own session and the target
// bounded context's domain rules (see internal/deeplink/adapter).
type Signer struct {
	key []byte
}

// NewSigner constructs a Signer from a non-empty HMAC key.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) == 0 {
		return nil, errors.New("deeplink: signer requires a non-empty key")
	}
	return &Signer{key: key}, nil
}

// NewSignerFromSecret derives a purpose-scoped HMAC key from secret via
// HMAC-SHA256(secret, purpose) and constructs a Signer from it.
//
// Deep links intentionally do NOT sign with secret directly: secret is
// shared with other consumers (e.g. session cookie infrastructure), and
// reusing a raw secret across independent purposes means a key compromise —
// or even an unexpected cryptographic interaction — in one consumer can leak
// into another. Deriving a distinct subkey per purpose (this function's
// purpose argument, e.g. "nestova:deeplink:v1") keeps every consumer's key
// cryptographically independent even though they trace back to one root
// secret, at the cost of zero additional configuration (no separate secret to
// provision, rotate, or keep in sync).
func NewSignerFromSecret(secret []byte, purpose string) (*Signer, error) {
	if len(secret) == 0 {
		return nil, errors.New("deeplink: signer requires a non-empty secret")
	}
	if purpose == "" {
		return nil, errors.New("deeplink: signer requires a non-empty purpose label")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(purpose))
	return NewSigner(mac.Sum(nil))
}

// Sign returns the expiry (now+LinkTTL, as a Unix timestamp) and the
// base64url-encoded signature authorizing path until that expiry. The caller
// embeds both as the "exp" and "sig" query parameters on the deep-link URL.
func (s *Signer) Sign(path string, now time.Time) (expiry int64, signature string) {
	expiry = now.Add(LinkTTL).Unix()
	return expiry, encode(s.mac(macPayload(path, expiry)))
}

// Verify checks path's signature against expiry as of now, and on success
// returns the signature's raw decoded bytes — the canonical, unambiguous
// form a caller should use as an idempotency key (see
// internal/deeplink/adapter's redemption-replay guard), rather than the
// presented signature string itself (see decode's doc for why the string is
// not safe to use as a key directly).
//
// Error contracts:
//   - Returns [domain.ErrLinkInvalidSignature] when sig is malformed (not
//     valid canonical base64url — see decode) or does not match path+expiry.
//   - Returns [domain.ErrLinkExpired] when the signature is valid but expiry
//     has passed as of now.
//
// The signature is checked BEFORE expiry (matching OAuthStateSigner's order)
// so an attacker cannot distinguish "this expiry was never validly signed"
// from "this was signed, but for a different expiry" — both are tamper
// attempts and both fail the same way.
func (s *Signer) Verify(path string, expiry int64, signature string, now time.Time) ([]byte, error) {
	gotMAC, err := decode(signature)
	if err != nil {
		return nil, domain.ErrLinkInvalidSignature
	}
	if !hmac.Equal(gotMAC, s.mac(macPayload(path, expiry))) {
		return nil, domain.ErrLinkInvalidSignature
	}
	if now.Unix() >= expiry {
		return nil, domain.ErrLinkExpired
	}
	return gotMAC, nil
}

// HashSignature returns the SHA-256 hash (hex-encoded) of a signature's
// canonical decoded bytes — i.e. Verify's own second return value — for use
// as a durable, database-storable idempotency key by an action that must
// never be repeated for the same signed link (NES-129: see
// internal/deeplink/adapter's redemption-replay guard, and
// domain.RewardRedemption.DeepLinkSignatureHash). Hashing (rather than
// storing the decoded signature itself) keeps the persisted key a fixed,
// short, opaque value regardless of the underlying HMAC's own size, and
// avoids persisting anything resembling a live credential — the hash alone
// can never be used to forge or replay a NEW link, only to recognize a
// repeat of one already spent.
func HashSignature(decodedSignature []byte) string {
	sum := sha256.Sum256(decodedSignature)
	return hex.EncodeToString(sum[:])
}

// macPayload builds the exact byte sequence signed/verified for path+expiry:
// "path|expiryUnix". path never legitimately contains "|" (see the deeplink
// domain's Action.Path, which builds it from a fixed "/go/<action>/<id>"
// shape), so no delimiter collision is possible in practice; even if one
// occurred, it would only ever cause Verify to (safely) reject a signature,
// never to accept a forged one.
func macPayload(path string, expiry int64) []byte {
	return []byte(path + "|" + strconv.FormatInt(expiry, 10))
}

func (s *Signer) mac(payload []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write(payload)
	return m.Sum(nil)
}

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// errNonCanonicalSignature is decode's internal sentinel for a signature that
// decodes successfully but is not the unique canonical encoding of its own
// bytes. It never escapes this package — Verify maps every decode failure to
// [domain.ErrLinkInvalidSignature] uniformly, so a caller can never
// distinguish "malformed" from "technically valid but non-canonical" (the
// same anti-oracle reasoning as every other signature failure mode here).
var errNonCanonicalSignature = errors.New("deeplink: signature is not canonically encoded")

// decode strictly parses s as canonical, unpadded base64url — exactly the
// form encode produces, and nothing else. Two independent checks enforce
// this:
//
//  1. base64.RawURLEncoding.Strict() itself rejects a string whose unused
//     low bits, in a partial final 6-bit group, are non-zero — Go's base64
//     package's own definition of "non-canonical" input.
//  2. decode ALSO re-encodes the successfully-decoded bytes and requires the
//     result to be byte-for-byte identical to s, rejecting otherwise.
//
// Both matter: a raw HMAC-SHA256 output (32 bytes = 256 bits) does not
// evenly fill its base64 encoding's 6-bit groups (43 characters = 258 bits),
// leaving 2 "don't care" bits in the final character that an ORDINARY
// (non-strict) decoder ignores — meaning multiple DISTINCT strings can all
// decode to the IDENTICAL signature bytes and all verify successfully,
// while remaining distinct as strings. That distinction matters here
// specifically because deeplink/adapter's redemption-replay guard is keyed
// off a signature — Verify returns the decoded bytes precisely so that key
// is built from the canonical form, never the presented string, as a second
// independent layer of defense even if a non-canonical variant somehow
// survived this function (belt and suspenders).
//
// TrimSpace-then-decode (the previous behavior) is deliberately NOT done
// here: silently stripping whitespace before decoding would let a
// whitespace-padded variant decode successfully too, for the identical
// reason a non-canonical variant would — it is simply a different string
// that happens to decode to the same bytes.
func decode(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, err
	}
	if encode(b) != s {
		return nil, errNonCanonicalSignature
	}
	return b, nil
}
