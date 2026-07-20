// Package crypto provides password hashing and verification using the argon2id
// algorithm. The PHC string format is used so parameters are self-describing and
// future algorithm changes do not require a separate migration column.
package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id tuning parameters. Values follow the OWASP password storage
// recommendations: memory=64 MiB, time=1 iteration, parallelism=4 threads,
// output key length=32 bytes. These satisfy interactive-login latency budgets
// while providing strong resistance to GPU-based dictionary attacks.
const (
	// defaultArgonTime is the number of passes over the memory (time cost).
	defaultArgonTime = uint32(1)
	// defaultArgonMemory is the memory cost in KiB (64 MiB).
	defaultArgonMemory = uint32(64 * 1024)
	// defaultArgonThreads is the degree of parallelism.
	defaultArgonThreads = uint8(4)
	// argonKeyLen is the output hash length in bytes. Deliberately NOT tunable:
	// Verify rejects any hash whose length differs, so varying it per-Hasher
	// would make hashes unverifiable across differently-configured hashers.
	argonKeyLen = uint32(32)
	// argonSaltLen is the random salt length in bytes. Fixed for the same
	// reason as argonKeyLen.
	argonSaltLen = 16
)

// Params holds the tunable argon2id cost parameters. Only the cost knobs are
// configurable; the salt and key lengths are fixed package-wide (see above).
//
// Production code must use DefaultParams. The type is exported so that tests,
// which would otherwise pay a 64 MiB memory-hard derivation per hash, can
// construct a Hasher with cheap parameters: recovery-code flows hash ten codes
// per enrollment, which dominated the test suite's runtime.
//
// Lowering these values weakens resistance to offline dictionary attacks and
// is only ever safe for throwaway test fixtures.
type Params struct {
	// Time is the number of passes over the memory.
	Time uint32
	// Memory is the memory cost in KiB.
	Memory uint32
	// Threads is the degree of parallelism.
	Threads uint8
}

// DefaultParams returns the OWASP-recommended production cost parameters.
func DefaultParams() Params {
	return Params{
		Time:    defaultArgonTime,
		Memory:  defaultArgonMemory,
		Threads: defaultArgonThreads,
	}
}

// Hasher derives and verifies argon2id hashes at a fixed cost. Construct one
// with NewHasher and inject it, rather than calling the package-level Hash and
// Verify, wherever the cost needs to be selectable (see Params).
type Hasher struct {
	params Params
}

// NewHasher constructs a Hasher that derives new hashes at the supplied cost.
func NewHasher(params Params) *Hasher {
	return &Hasher{params: params}
}

// defaultHasher backs the package-level Hash and Verify functions, which
// remain the right choice for call sites with no need to vary the cost.
var defaultHasher = NewHasher(DefaultParams())

// ErrMalformedHash is returned by Verify when the encoded string is not a
// valid PHC-format argon2id hash produced by Hash.
var ErrMalformedHash = errors.New("crypto: malformed argon2id hash")

// Hash derives a new argon2id hash from password using a freshly generated
// random salt and returns the PHC-encoded string. Each call produces a
// different output even for the same password (due to the random salt), so
// the returned string must be stored and compared with Verify, not compared
// directly.
//
// Hash returns an error only when the system's random source is unavailable,
// which is a fatal condition.
func Hash(password string) (string, error) { return defaultHasher.Hash(password) }

// Hash derives a new argon2id hash at h's configured cost. See the
// package-level Hash for the full contract.
func (h *Hasher) Hash(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("crypto: generate salt: %w", err)
	}

	hash := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.Memory, h.params.Threads, argonKeyLen)

	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Hash := base64.RawStdEncoding.EncodeToString(hash)

	// The cost parameters are recorded in the PHC string, so Verify reproduces
	// them from the stored hash rather than from h — which is what lets a
	// cheaply-hashed test fixture and a production hash coexist.
	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.params.Memory, h.params.Time, h.params.Threads, b64Salt, b64Hash,
	)
	return encoded, nil
}

// Verify checks whether password matches the PHC-encoded argon2id hash
// produced by Hash. It returns (true, nil) on a match, (false, nil) on a
// mismatch, and (false, ErrMalformedHash) when encoded is not a valid PHC
// argon2id string. The comparison is constant-time to resist timing attacks.
func Verify(password, encoded string) (bool, error) { return defaultHasher.Verify(password, encoded) }

// Verify checks password against the PHC-encoded hash. See the package-level
// Verify for the full contract.
//
// The receiver's Params are deliberately unused: the cost parameters come from
// the encoded hash itself, so a Hasher verifies hashes produced at ANY cost,
// including ones written before it was configured. Verify is a method purely so
// that Hash and Verify can be injected together as one seam.
func (h *Hasher) Verify(password, encoded string) (bool, error) {
	salt, hash, params, err := parsePHC(encoded)
	if err != nil {
		return false, err
	}
	// Our hashes are always argonKeyLen bytes; reject anything else so a
	// malformed/oversized encoded hash cannot drive an excessive key derivation.
	if len(hash) != int(argonKeyLen) {
		return false, ErrMalformedHash
	}

	candidate := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, argonKeyLen)

	if subtle.ConstantTimeCompare(hash, candidate) == 1 {
		return true, nil
	}
	return false, nil
}

// argon2Params holds the decoded parameters from a PHC string.
type argon2Params struct {
	time    uint32
	memory  uint32
	threads uint8
}

// parsePHC decodes a PHC-format argon2id string of the form:
//
//	$argon2id$v=19$m=65536,t=1,p=4$<b64salt>$<b64hash>
//
// It returns the raw salt, raw hash, and parameters, or ErrMalformedHash if
// the string does not conform.
func parsePHC(encoded string) (salt, hash []byte, params argon2Params, err error) {
	parts := strings.Split(encoded, "$")
	// Expected: ["", "argon2id", "v=19", "m=65536,t=1,p=4", "<salt>", "<hash>"]
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, nil, argon2Params{}, ErrMalformedHash
	}

	var version int
	if _, scanErr := fmt.Sscanf(parts[2], "v=%d", &version); scanErr != nil {
		return nil, nil, argon2Params{}, ErrMalformedHash
	}
	if version != argon2.Version {
		return nil, nil, argon2Params{}, ErrMalformedHash
	}

	var p argon2Params
	if _, scanErr := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); scanErr != nil {
		return nil, nil, argon2Params{}, ErrMalformedHash
	}

	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, argon2Params{}, ErrMalformedHash
	}

	hash, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, argon2Params{}, ErrMalformedHash
	}

	return salt, hash, p, nil
}
