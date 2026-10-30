// Package blobstore is Janus's file storage: one small interface over the
// admin-chosen backend for opaque binary objects (chat attachments,
// troubleshooting capture bodies). Two backends exist: the database
// (store.blob_object) and any S3-compatible service (AWS S3, SeaweedFS,
// Ceph RGW, …).
//
// Every object is encrypted by Janus before it reaches a backend (see
// Encrypted), so neither the database nor the S3 provider ever holds
// plaintext. Callers obtain stores from a Manager, which resolves the
// active backend from the environment or the admin setting and always
// returns an encrypting store.
//
// Changing the backend does not migrate existing objects. Each caller
// records which backend holds an object (Ref / ParseRef) and reads it back
// from that backend (Manager.For), so files written before a switch stay
// readable where they are.
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Backend kinds, as returned by Store.Kind and recorded in refs.
const (
	KindDatabase = "database"
	KindS3       = "s3"
)

// ErrNotFound is returned by Get when the key does not exist.
var ErrNotFound = errors.New("blobstore: object not found")

// ErrTooLarge is returned by Put when an object exceeds the configured
// maximum object size.
var ErrTooLarge = errors.New("blobstore: object exceeds the maximum object size")

// Store is one storage backend.
type Store interface {
	// Put stores data under key, replacing any existing object.
	Put(ctx context.Context, key string, data []byte, contentType string) error
	// Get returns the object's bytes, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, error)
	// Delete removes the object. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// Kind names the backend (KindDatabase or KindS3).
	Kind() string
	// Check proves the backend works end to end by writing, reading back
	// and deleting a probe object. The error text is safe to show an
	// administrator (it never contains credentials).
	Check(ctx context.Context) error
}

// ValidateKey rejects keys that could escape a prefix or confuse a backend.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return errors.New("blobstore: empty key")
	case len(key) > 512:
		return errors.New("blobstore: key longer than 512 characters")
	case strings.HasPrefix(key, "/"):
		return errors.New("blobstore: key must not start with /")
	case strings.Contains(key, "\\") || strings.ContainsAny(key, "\x00\r\n"):
		return errors.New("blobstore: key contains a forbidden character")
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("blobstore: key %q has an empty or relative path segment", key)
		}
	}
	return nil
}

// Ref encodes where an object lives ("<kind>:<key>"), for callers that keep
// one reference column.
func Ref(kind, key string) string { return kind + ":" + key }

// ParseRef splits a Ref. ok is false for strings that are not refs (for
// example a troubleshooting disk path).
func ParseRef(ref string) (kind, key string, ok bool) {
	kind, key, found := strings.Cut(ref, ":")
	if !found || (kind != KindDatabase && kind != KindS3) || key == "" {
		return "", "", false
	}
	return kind, key, true
}

// probeKey is the object Check writes and removes.
func probeKey(suffix string) string { return ".janus-probe/" + suffix }
