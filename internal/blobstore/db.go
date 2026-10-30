package blobstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/torvanis/janus/internal/store"
)

// DB is the database backend: objects are rows of blob_object.
type DB struct {
	store *store.Store
}

// NewDB builds the database backend over s.
func NewDB(s *store.Store) *DB { return &DB{store: s} }

// Kind implements Store.
func (d *DB) Kind() string { return KindDatabase }

// Put implements Store.
func (d *DB) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	return d.store.PutBlob(ctx, key, data, contentType)
}

// Get implements Store.
func (d *DB) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	obj, err := d.store.GetBlob(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return obj.Data, nil
}

// Delete implements Store.
func (d *DB) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	return d.store.DeleteBlob(ctx, key)
}

// Check implements Store.
func (d *DB) Check(ctx context.Context) error {
	return roundTrip(ctx, d)
}

// roundTrip is the shared Check body: put, get back, compare, delete.
func roundTrip(ctx context.Context, s Store) error {
	key := probeKey(store.NewID())
	payload := []byte("janus storage probe " + key)
	if err := s.Put(ctx, key, payload, "text/plain"); err != nil {
		return fmt.Errorf("write test object: %w", err)
	}
	got, err := s.Get(ctx, key)
	if err != nil {
		_ = s.Delete(ctx, key)
		return fmt.Errorf("read test object back: %w", err)
	}
	if !bytes.Equal(got, payload) {
		_ = s.Delete(ctx, key)
		return errors.New("read test object back: content differs from what was written")
	}
	if err := s.Delete(ctx, key); err != nil {
		return fmt.Errorf("delete test object: %w", err)
	}
	return nil
}
