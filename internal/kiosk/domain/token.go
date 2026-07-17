package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// tokenBytes is the raw entropy of a generated kiosk device token: 32 bytes
// (256 bits), encoded as a 64-character hex string. This matches the
// project's other high-entropy-token sizing (see auth/adapter.csrfTokenLen).
const tokenBytes = 32

// GenerateToken returns a new random, hex-encoded kiosk device token. It
// errors only when the system's random source is unavailable, which is a
// fatal condition (mirrors crypto.Hash's contract for the same failure).
func GenerateToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("kiosk: generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// HashToken returns the SHA-256 hex digest of a raw kiosk device token, the
// form persisted in kiosk_device.token_hash.
//
// This deliberately does not use the argon2id KDF that internal/platform/crypto
// applies to member passwords: a kiosk token is 256 bits of crypto/rand output,
// not a human-chosen password, so it already carries full entropy and there is
// no dictionary or rainbow-table attack to defend against by stretching it.
// SHA-256 is the right tool for comparing a high-entropy random bearer secret,
// and avoids paying argon2's deliberate CPU/memory cost on every kiosk request.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// TokensMatch reports whether raw hashes to hash, using a constant-time
// comparison of the computed digest so token verification does not leak
// timing information.
func TokensMatch(raw, hash string) bool {
	candidate := HashToken(raw)
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(hash)) == 1
}

// activationCodeAlphabet is Crockford's Base32 alphabet: it excludes the
// visually confusable 0/O, 1/I/L pairs so a code read off a screen and typed
// by hand on the kiosk device is unambiguous.
const activationCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// activationCodeLength is the number of alphabet characters in a generated
// activation code (hyphens added for readability are not counted): 10
// characters from a 32-symbol alphabet is ~50 bits of entropy. That is far
// less than tokenBytes' 256 bits, which is fine — see GenerateActivationCode's
// doc comment for why a hand-typeable code is safe at this length.
const activationCodeLength = 10

// GenerateActivationCode returns a new random, hyphen-grouped activation code
// (e.g. "ABCD-EFGH-JK") for the kiosk provisioning flow. Unlike
// GenerateToken's 256-bit device token, this is deliberately short enough to
// read off a screen and type by hand — safe specifically because an
// activation code is single-use and expires quickly (domain.ActivationCodeTTL),
// so the exposure window for a brute-force or leaked-hash attack against it is
// far too short to matter at this entropy. It errors only when the system's
// random source is unavailable.
func GenerateActivationCode() (string, error) {
	b := make([]byte, activationCodeLength)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("kiosk: generate activation code: %w", err)
	}
	var sb strings.Builder
	for i, v := range b {
		// 256 (len(byte range)) is an exact multiple of 32 (len(alphabet)), so
		// this modulo introduces no bias toward any symbol.
		sb.WriteByte(activationCodeAlphabet[int(v)%len(activationCodeAlphabet)])
		if i == 3 || i == 6 {
			sb.WriteByte('-')
		}
	}
	return sb.String(), nil
}

// NormalizeActivationCode uppercases and strips whitespace/hyphens from a
// user-typed activation code so "abcd-efgh-jk", "ABCD EFGH JK", and
// "ABCDEFGHJK" all hash identically to the code as generated.
func NormalizeActivationCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
}
