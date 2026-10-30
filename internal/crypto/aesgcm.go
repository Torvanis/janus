// Package crypto implements the AES-256-GCM envelope used for upstream provider
// credentials at rest. The key comes from JANUS_ENCRYPTION_KEY; a wrong key
// produces an explicit decryption error rather than silent corruption.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// ErrDecrypt is returned whenever a ciphertext cannot be authenticated with the
// configured key. Callers surface it as an operator-facing "wrong
// JANUS_ENCRYPTION_KEY" message.
var ErrDecrypt = errors.New("decrypt upstream credential: authentication failed (is JANUS_ENCRYPTION_KEY correct?)")

const envelopePrefix = "janus.v1."

// Cipher encrypts and decrypts secrets with a single symmetric key.
type Cipher struct {
	aead cipher.AEAD
	// key is the raw instance key, kept only so DeriveKey can derive
	// purpose-bound subkeys from it; it is never used directly elsewhere.
	key []byte
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
	return &Cipher{aead: aead, key: append([]byte(nil), key...)}, nil
}

// DeriveKey returns a 32-byte subkey bound to label, derived from the
// instance key with HKDF-SHA256 (no salt, info = "janus/" + label). The same
// key and label always give the same subkey; different labels give unrelated
// subkeys, so a subkey (e.g. the chat search HMAC key) never doubles as the
// encryption key.
func (c *Cipher) DeriveKey(label string) []byte {
	out := make([]byte, 32)
	r := hkdf.New(sha256.New, c.key, nil, []byte("janus/"+label))
	if _, err := io.ReadFull(r, out); err != nil {
		// HKDF-SHA256 can produce up to 255*32 bytes; 32 never fails.
		panic(fmt.Sprintf("derive key: %v", err))
	}
	return out
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

// bytesMagic opens every EncryptBytes output: "JB" plus a format version
// byte, so a reader can refuse foreign or future data instead of failing
// authentication with a misleading wrong-key error.
var bytesMagic = []byte{'J', 'B', 0x01}

// ErrBytesFormat reports data that does not carry the EncryptBytes header.
var ErrBytesFormat = errors.New("decrypt object: unrecognised encrypted object format")

// EncryptBytes seals arbitrary binary data (file contents, captures) into a
// raw byte envelope: a 3-byte versioned header, the nonce, then the GCM
// ciphertext and tag. Unlike Encrypt it never base64-encodes, so blobs keep
// their size (+31 bytes). An empty input still produces a full envelope so
// "empty file" and "no data" stay distinguishable.
func (c *Cipher) EncryptBytes(plaintext []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	out := make([]byte, len(bytesMagic)+nonceSize, len(bytesMagic)+nonceSize+len(plaintext)+c.aead.Overhead())
	copy(out, bytesMagic)
	nonce := out[len(bytesMagic):]
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	// The header is authenticated as additional data, so a flipped version
	// byte fails the tag check rather than being silently accepted.
	return c.aead.Seal(out, nonce, plaintext, bytesMagic), nil
}

// DecryptBytes reverses EncryptBytes.
func (c *Cipher) DecryptBytes(envelope []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(envelope) < len(bytesMagic) || string(envelope[:len(bytesMagic)]) != string(bytesMagic) {
		return nil, ErrBytesFormat
	}
	if len(envelope) < len(bytesMagic)+nonceSize+c.aead.Overhead() {
		return nil, ErrDecrypt
	}
	nonce := envelope[len(bytesMagic) : len(bytesMagic)+nonceSize]
	plain, err := c.aead.Open(nil, nonce, envelope[len(bytesMagic)+nonceSize:], bytesMagic)
	if err != nil {
		return nil, ErrDecrypt
	}
	if plain == nil {
		plain = []byte{}
	}
	return plain, nil
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
