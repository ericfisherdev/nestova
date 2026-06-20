package crypto_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/crypto"
)

func testKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c, err := crypto.NewCipher(testKey())
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	plaintext := []byte("ya29.super-secret-access-token")
	ciphertext, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Contains(ciphertext, plaintext) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := c.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip = %q, want %q", got, plaintext)
	}
}

func TestEncryptUsesFreshNonce(t *testing.T) {
	c, _ := crypto.NewCipher(testKey())
	a, _ := c.Encrypt([]byte("same"))
	b, _ := c.Encrypt([]byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertext (nonce reuse)")
	}
}

func TestNewCipherRejectsBadKey(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := crypto.NewCipher(make([]byte, n)); !errors.Is(err, crypto.ErrInvalidKey) {
			t.Errorf("NewCipher(%d-byte key) error = %v, want ErrInvalidKey", n, err)
		}
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	c, _ := crypto.NewCipher(testKey())
	ciphertext, _ := c.Encrypt([]byte("secret"))
	ciphertext[len(ciphertext)-1] ^= 0xff // flip a tag bit
	if _, err := c.Decrypt(ciphertext); !errors.Is(err, crypto.ErrMalformedCiphertext) {
		t.Fatalf("Decrypt(tampered) error = %v, want ErrMalformedCiphertext", err)
	}
}

func TestDecryptRejectsShortInput(t *testing.T) {
	c, _ := crypto.NewCipher(testKey())
	if _, err := c.Decrypt([]byte{0x01, 0x02}); !errors.Is(err, crypto.ErrMalformedCiphertext) {
		t.Fatalf("Decrypt(short) error = %v, want ErrMalformedCiphertext", err)
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	enc, _ := crypto.NewCipher(testKey())
	ciphertext, _ := enc.Encrypt([]byte("secret"))
	otherKey := testKey()
	otherKey[0] ^= 0xff
	dec, _ := crypto.NewCipher(otherKey)
	if _, err := dec.Decrypt(ciphertext); !errors.Is(err, crypto.ErrMalformedCiphertext) {
		t.Fatalf("Decrypt(wrong key) error = %v, want ErrMalformedCiphertext", err)
	}
}
