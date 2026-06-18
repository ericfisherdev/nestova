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
	// argonTime is the number of passes over the memory (time cost).
	argonTime = uint32(1)
	// argonMemory is the memory cost in KiB (64 MiB).
	argonMemory = uint32(64 * 1024)
	// argonThreads is the degree of parallelism.
	argonThreads = uint8(4)
	// argonKeyLen is the output hash length in bytes.
	argonKeyLen = uint32(32)
	// argonSaltLen is the random salt length in bytes.
	argonSaltLen = 16
)

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
func Hash(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("crypto: generate salt: %w", err)
	}

	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Hash := base64.RawStdEncoding.EncodeToString(hash)

	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64Salt, b64Hash,
	)
	return encoded, nil
}

// Verify checks whether password matches the PHC-encoded argon2id hash
// produced by Hash. It returns (true, nil) on a match, (false, nil) on a
// mismatch, and (false, ErrMalformedHash) when encoded is not a valid PHC
// argon2id string. The comparison is constant-time to resist timing attacks.
func Verify(password, encoded string) (bool, error) {
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
