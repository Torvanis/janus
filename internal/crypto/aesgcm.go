// Package crypto implements the AES-256-GCM envelope used for upstream provider
// credentials at rest. The key comes from JANUS_ENCRYPTION_KEY; a wrong key
// produces an explicit decryption error rather than silent corruption.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrDecrypt is returned whenever a ciphertext cannot be authenticated with the
// configured key. Callers surface it as an operator-facing "wrong
// JANUS_ENCRYPTION_KEY" message.
var ErrDecrypt = errors.New("decrypt upstream credential: authentication failed (is JANUS_ENCRYPTION_KEY correct?)")

const envelopePrefix = "janus.v1."

// Cipher encrypts and decrypts secrets with a single symmetric key.
type Cipher struct {
	aead cipher.AEAD
}

// New builds a Cipher from a 32-byte key.
func New(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("build cipher: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("build gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt returns a self-describing, versioned, base64 envelope. An empty
// plaintext encrypts to an empty string so "no credential" round-trips cleanly.
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return envelopePrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt.
func (c *Cipher) Decrypt(envelope string) (string, error) {
	if envelope == "" {
		return "", nil
	}
	if !strings.HasPrefix(envelope, envelopePrefix) {
		return "", fmt.Errorf("decrypt upstream credential: unrecognised envelope format")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(envelope, envelopePrefix))
	if err != nil {
		return "", fmt.Errorf("decrypt upstream credential: %w", err)
	}
	if len(raw) < c.aead.NonceSize() {
		return "", ErrDecrypt
	}
	nonce, body := raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plain), nil
}

// Mask renders a stored credential the way the admin UI is allowed to show it:
// first four and last four characters only.
func Mask(secret string) string {
	switch {
	case secret == "":
		return ""
	case len(secret) <= 8:
		return strings.Repeat("•", len(secret))
	default:
		return secret[:4] + "…" + secret[len(secret)-4:]
	}
}
