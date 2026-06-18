package crypto_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/crypto"
)

func TestHashNotEqualToPassword(t *testing.T) {
	t.Parallel()
	h, err := crypto.Hash("hunter2")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if h == "hunter2" {
		t.Error("Hash returned the plaintext password unchanged")
	}
}

func TestHashProducesPHCFormat(t *testing.T) {
	t.Parallel()
	h, err := crypto.Hash("mysecret")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Errorf("Hash does not start with $argon2id$v=19$: %q", h)
	}
}

func TestVerifyMatchesCorrectPassword(t *testing.T) {
	t.Parallel()
	const password = "correct-horse-battery-staple"
	h, err := crypto.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	ok, err := crypto.Verify(password, h)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Error("Verify returned false for correct password")
	}
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	t.Parallel()
	h, err := crypto.Hash("rightpassword")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	ok, err := crypto.Verify("wrongpassword", h)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Error("Verify returned true for wrong password")
	}
}

func TestVerifyErrorOnMalformedEncoding(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"notaphcstring",
		// wrong algorithm:
		"$argon2i$v=19$m=65536,t=1,p=4$salt$hash",
		// wrong version:
		"$argon2id$v=18$m=65536,t=1,p=4$salt$hash",
		// non-numeric memory:
		"$argon2id$v=19$m=abc,t=1,p=4$salt$hash",
		// invalid base64 in the salt field:
		"$argon2id$v=19$m=65536,t=1,p=4$inva!id$aGFzaA",
		// invalid base64 in the hash field:
		"$argon2id$v=19$m=65536,t=1,p=4$AAAA$inva!id",
		// valid base64 but the hash is not the expected key length (DoS guard):
		"$argon2id$v=19$m=65536,t=1,p=4$AAAA$aGFzaA",
	}
	for _, encoded := range cases {
		ok, err := crypto.Verify("anything", encoded)
		if !errors.Is(err, crypto.ErrMalformedHash) {
			t.Errorf("Verify(%q) error = %v, want ErrMalformedHash", encoded, err)
		}
		if ok {
			t.Errorf("Verify(%q) returned ok=true on malformed encoding", encoded)
		}
	}
}

func TestTwoHashesOfSamePasswordDiffer(t *testing.T) {
	t.Parallel()
	const password = "samepassword"
	h1, err := crypto.Hash(password)
	if err != nil {
		t.Fatalf("Hash(1): %v", err)
	}
	h2, err := crypto.Hash(password)
	if err != nil {
		t.Fatalf("Hash(2): %v", err)
	}
	if h1 == h2 {
		t.Error("two Hash calls for the same password returned identical strings (salt is not random)")
	}
}
