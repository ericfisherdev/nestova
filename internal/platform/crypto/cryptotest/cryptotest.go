// Package cryptotest provides test-only helpers for the crypto package,
// following the convention of the standard library's httptest.
//
// Nothing here may be used by production code. The composition root in
// cmd/server builds its hasher from crypto.DefaultParams; a build that reaches
// this package outside a test has selected deliberately breakable parameters.
package cryptotest

import "github.com/ericfisherdev/nestova/internal/platform/crypto"

// Cheap argon2id cost parameters: 64 KiB of memory, one pass, one thread —
// roughly a thousandth of the production memory cost.
//
// These are trivially brute-forceable and exist purely so tests do not pay a
// 64 MiB memory-hard derivation per hash. The MFA recovery-code flows hash ten
// codes per enrollment, which made argon2 the single largest contributor to the
// suite's runtime.
//
// Memory must stay at or above 8*Threads, which the argon2 implementation
// otherwise silently raises it to.
const (
	cheapTime    = uint32(1)
	cheapMemory  = uint32(64)
	cheapThreads = uint8(1)
)

// Hasher returns an argon2id hasher with cheap, test-only cost parameters.
//
// Hashes it produces verify correctly through the ordinary crypto.Verify path:
// the cost parameters are recorded in the PHC-encoded string and read back on
// verification, so cheaply-hashed fixtures and production hashes are handled by
// exactly the same code.
func Hasher() *crypto.Hasher {
	return crypto.NewHasher(crypto.Params{
		Time:    cheapTime,
		Memory:  cheapMemory,
		Threads: cheapThreads,
	})
}
