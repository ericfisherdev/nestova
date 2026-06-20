package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// keyLen is the required symmetric key length: 32 bytes for AES-256.
const keyLen = 32

// Crypto errors for the symmetric cipher.
var (
	// ErrInvalidKey is returned by NewCipher when the key is not 32 bytes.
	ErrInvalidKey = errors.New("crypto: encryption key must be 32 bytes")
	// ErrMalformedCiphertext is returned by Decrypt when the input is too short
	// to contain a nonce or otherwise fails authentication.
	ErrMalformedCiphertext = errors.New("crypto: malformed ciphertext")
)

// Cipher encrypts and decrypts small secrets (e.g. OAuth tokens) at rest using
// AES-256-GCM, which provides confidentiality and authentication. The key is
// injected via the constructor (never a package global) so it can be sourced
// from configuration and kept out of logs.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher constructs a Cipher from a 32-byte key, returning ErrInvalidKey for
// any other length.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidKey, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: new aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals plaintext under a fresh random nonce and returns the nonce
// prepended to the ciphertext (nonce || ciphertext+tag). It errors only when the
// system random source is unavailable.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: read nonce: %w", err)
	}
	// Seal appends the ciphertext to its first argument; passing nonce makes the
	// result nonce||ciphertext so Decrypt can recover the nonce.
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt reverses Encrypt: it splits the nonce from the ciphertext and opens
// it, returning ErrMalformedCiphertext when the input is too short or fails the
// GCM authentication tag (tampering or a wrong key).
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, ErrMalformedCiphertext
	}
	nonce, sealed := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, ErrMalformedCiphertext
	}
	return plaintext, nil
}
