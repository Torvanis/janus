package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// File storage (blobs).
//
// Janus keeps opaque binary objects (chat attachments, troubleshooting
// capture bodies) in one admin-chosen backend: the database (blob_object,
// below) or an S3-compatible bucket. internal/blobstore owns the backends
// and encrypts every object before it reaches either one, so the bytes in
// blob_object are ciphertext, never plaintext.

// blobStorageMigration adds the database backend's table. PostgreSQL needs
// BYTEA where SQLite says BLOB, so the PostgreSQL DDL runs first (pgStmt)
// and the shared CREATE TABLE IF NOT EXISTS is then a no-op there.
var blobStorageMigration = migration{
	name: "0056_blob_storage",
	pgStmt: []string{
		`CREATE TABLE IF NOT EXISTS blob_object (
			key TEXT PRIMARY KEY,
			data BYTEA NOT NULL,
			content_type TEXT NOT NULL DEFAULT '',
			size_bytes BIGINT NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		)`,
	},
	stmt: []string{
		`CREATE TABLE IF NOT EXISTS blob_object (
			key TEXT PRIMARY KEY,
			data BLOB NOT NULL,
			content_type TEXT NOT NULL DEFAULT '',
			size_bytes BIGINT NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		)`,
	},
	down: []string{`DROP TABLE IF EXISTS blob_object`},
}

// BlobObject is one row of the database blob backend. Data is the stored
// (already encrypted) bytes.
type BlobObject struct {
	Key         string
	Data        []byte
	ContentType string
	SizeBytes   int64
	CreatedAt   time.Time
}

// PutBlob stores or replaces one object in the database backend.
func (s *Store) PutBlob(ctx context.Context, key string, data []byte, contentType string) error {
	if data == nil {
		data = []byte{}
	}
	if err := s.exec(ctx, `INSERT INTO blob_object (key, data, content_type, size_bytes, created_at) VALUES (?,?,?,?,?)
		ON CONFLICT (key) DO UPDATE SET data = excluded.data, content_type = excluded.content_type,
		size_bytes = excluded.size_bytes, created_at = excluded.created_at`,
		key, data, contentType, int64(len(data)), FormatTime(nowUTC())); err != nil {
		return fmt.Errorf("put blob: %w", err)
	}
	return nil
}

// GetBlob reads one object; ErrNotFound when the key is absent.
func (s *Store) GetBlob(ctx context.Context, key string) (*BlobObject, error) {
	out := &BlobObject{Key: key}
	var created string
	err := s.queryRow(ctx, `SELECT data, content_type, size_bytes, created_at FROM blob_object WHERE key = ?`, key).
		Scan(&out.Data, &out.ContentType, &out.SizeBytes, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get blob: %w", err)
	}
	if out.Data == nil {
		out.Data = []byte{}
	}
	out.CreatedAt = ParseTime(created)
	return out, nil
}

// DeleteBlob removes one object. Deleting a missing key is not an error.
func (s *Store) DeleteBlob(ctx context.Context, key string) error {
	if err := s.exec(ctx, `DELETE FROM blob_object WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete blob: %w", err)
	}
	return nil
}

// BlobUsage is the size of the database blob backend.
type BlobUsage struct {
	Objects int64 `json:"objects"`
	Bytes   int64 `json:"bytes"`
}

// BlobUsage counts the objects and stored bytes in the database backend.
func (s *Store) BlobUsage(ctx context.Context) (BlobUsage, error) {
	var out BlobUsage
	var total sql.NullInt64
	if err := s.queryRow(ctx, `SELECT COUNT(*), SUM(size_bytes) FROM blob_object`).Scan(&out.Objects, &total); err != nil {
		return out, fmt.Errorf("blob usage: %w", err)
	}
	out.Bytes = total.Int64
	return out, nil
}

// SettingFileStorage holds the admin-configured file storage backend
// (FileStorageSetting as JSON). Ignored when JANUS_STORAGE_MODE is set.
const SettingFileStorage = "file_storage"

// File storage modes.
const (
	FileStorageDatabase = "database"
	FileStorageS3       = "s3"
)

// FileStorageSetting is the stored file storage configuration. The S3
// secret is kept only as a Cipher envelope (SecretAccessKeyEnc); the store
// never sees it in the clear.
type FileStorageSetting struct {
	Mode string              `json:"mode"`
	S3   FileStorageS3Config `json:"s3"`
}

// FileStorageS3Config is the S3-compatible part of FileStorageSetting.
type FileStorageS3Config struct {
	Endpoint           string `json:"endpoint"`
	Bucket             string `json:"bucket"`
	Region             string `json:"region"`
	PathStyle          bool   `json:"path_style"`
	Prefix             string `json:"prefix"`
	AccessKeyID        string `json:"access_key_id"`
	SecretAccessKeyEnc string `json:"secret_access_key_enc"`
	CABundle           string `json:"ca_bundle"`
}

// FileStorageSetting reads the stored configuration. With nothing saved it
// returns the database mode and a zero time.
func (s *Store) FileStorageSetting(ctx context.Context) (FileStorageSetting, time.Time, error) {
	raw, updated, err := s.systemSetting(ctx, SettingFileStorage)
	if errors.Is(err, ErrNotFound) {
		return FileStorageSetting{Mode: FileStorageDatabase}, time.Time{}, nil
	}
	if err != nil {
		return FileStorageSetting{}, time.Time{}, err
	}
	var out FileStorageSetting
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return FileStorageSetting{}, time.Time{}, fmt.Errorf("decode file storage setting: %w", err)
	}
	if out.Mode == "" {
		out.Mode = FileStorageDatabase
	}
	return out, updated, nil
}

// PutFileStorageSetting saves the configuration.
func (s *Store) PutFileStorageSetting(ctx context.Context, v FileStorageSetting) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.putSystemSetting(ctx, SettingFileStorage, string(encoded))
}
