package blobstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/torvanis/janus/internal/crypto"
)

// ByteCipher is the subset of crypto.Cipher Encrypted needs.
type ByteCipher interface {
	EncryptBytes(plaintext []byte) ([]byte, error)
	DecryptBytes(envelope []byte) ([]byte, error)
}

var _ ByteCipher = (*crypto.Cipher)(nil)

// EncryptedStore encrypts on Put and decrypts on Get, so the wrapped
// backend only ever sees ciphertext. Obtain one through Encrypted.
type EncryptedStore struct {
	inner  Store
	cipher ByteCipher
}

// Encrypted wraps inner so every object is sealed with the gateway key
// (AES-256-GCM, crypto.Cipher.EncryptBytes) before it leaves the process.
// A nil cipher is refused outright rather than silently storing plaintext.
func Encrypted(inner Store, cipher ByteCipher) (*EncryptedStore, error) {
	if inner == nil {
		return nil, errors.New("blobstore: no backend")
	}
	if cipher == nil {
		return nil, errors.New("blobstore: no encryption key configured; refusing to store files unencrypted")
	}
	return &EncryptedStore{inner: inner, cipher: cipher}, nil
}

// Inner returns the wrapped backend (raw ciphertext access, for tests and
// diagnostics).
func (e *EncryptedStore) Inner() Store { return e.inner }

// Kind implements Store.
func (e *EncryptedStore) Kind() string { return e.inner.Kind() }

// Put implements Store.
func (e *EncryptedStore) Put(ctx context.Context, key string, data []byte, contentType string) error {
	sealed, err := e.cipher.EncryptBytes(data)
	if err != nil {
		return fmt.Errorf("encrypt object: %w", err)
	}
	// The real content type would leak what the object is; the backend
	// only ever holds opaque ciphertext.
	_ = contentType
	return e.inner.Put(ctx, key, sealed, "application/octet-stream")
}

// Get implements Store.
func (e *EncryptedStore) Get(ctx context.Context, key string) ([]byte, error) {
	sealed, err := e.inner.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	plain, err := e.cipher.DecryptBytes(sealed)
	if err != nil {
		return nil, fmt.Errorf("object %s: %w", key, err)
	}
	return plain, nil
}

// Delete implements Store.
func (e *EncryptedStore) Delete(ctx context.Context, key string) error {
	return e.inner.Delete(ctx, key)
}

// Check implements Store: the probe goes through encryption too, proving
// the key and the backend together.
func (e *EncryptedStore) Check(ctx context.Context) error { return roundTrip(ctx, e) }

// limited refuses objects above max bytes (plaintext size).
type limited struct {
	Store
	max int64
}

// Put implements Store.
func (l limited) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if l.max > 0 && int64(len(data)) > l.max {
		return fmt.Errorf("%w (%d bytes, limit %d)", ErrTooLarge, len(data), l.max)
	}
	return l.Store.Put(ctx, key, data, contentType)
}
