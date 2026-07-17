package domain

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

// recoveryCodeAlphabet is Crockford's Base32 alphabet: it excludes the
// visually confusable 0/O, 1/I/L pairs so a code copied down by hand is
// unambiguous. Mirrors internal/kiosk/domain's activation-code alphabet
// choice, kept as its own copy here rather than a shared import: the two
// contexts' codes are unrelated concepts (a short-lived kiosk pairing code
// vs. a long-lived MFA recovery code) that happen to share a generation
// technique, not a reason to couple auth and kiosk together.
const recoveryCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// recoveryCodeEncoding is standard Base32 (RFC 4648, 5 bits per symbol) over
// the Crockford alphabet, unpadded. Using encoding/base32 (rather than
// mapping one alphabet symbol per input byte) is what makes
// recoveryCodeBytes' 80 bits of randomness actually become 16 output symbols
// (80 / 5 = 16, evenly divisible — 10 bytes is exactly two 5-byte Base32
// groups, so NoPadding never needs to trim anything): a byte-per-symbol
// scheme would waste 3 of every 8 random bits per symbol and only produce
// one symbol per byte (10 symbols / 50 bits from the same 10 random bytes).
var recoveryCodeEncoding = base32.NewEncoding(recoveryCodeAlphabet).WithPadding(base32.NoPadding)

// recoveryCodeBytes is the raw random byte length per generated code. At 10
// bytes (80 bits) this comfortably resists offline guessing even though each
// code is checked against a slow argon2id hash (crypto.Verify), matching the
// same KDF used for member passwords.
const recoveryCodeBytes = 10

// recoveryCodeGroupSize is how many Base32 symbols appear between the
// dashes GenerateRecoveryCode inserts for readability (e.g. "ABCD-EFGH-...").
const recoveryCodeGroupSize = 4

// GenerateRecoveryCode returns one raw, human-typable recovery code (16
// Crockford-Base32 characters — recoveryCodeBytes' full 80 bits, see
// recoveryCodeEncoding's doc — split into four dash-separated groups of
// four, e.g. "ABCD-EFGH-JKMN-PQRS"). The raw code is returned to the caller
// exactly once — only its hash (via crypto.Hash) is ever persisted.
func GenerateRecoveryCode() (string, error) {
	b := make([]byte, recoveryCodeBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate recovery code: %w", err)
	}
	encoded := recoveryCodeEncoding.EncodeToString(b)

	var sb strings.Builder
	for i, r := range encoded {
		if i > 0 && i%recoveryCodeGroupSize == 0 {
			sb.WriteByte('-')
		}
		sb.WriteRune(r)
	}
	return sb.String(), nil
}

// NormalizeRecoveryCode uppercases, strips whitespace/dashes, and folds
// visually-confusable characters a member might type (O→0, I/L→1) before
// hashing or comparison, mirroring the kiosk activation code's normalization
// so a code entered with or without dashes, in either case, verifies
// identically.
func NormalizeRecoveryCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	s = recoveryCodeAliases.Replace(s)
	return s
}

// recoveryCodeAliases folds characters a member might type that are not in
// recoveryCodeAlphabet but are visually or phonetically equivalent to one
// that is.
var recoveryCodeAliases = strings.NewReplacer("O", "0", "I", "1", "L", "1")
