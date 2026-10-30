package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestBlobRoundTrip runs on SQLite and, with JANUS_ROLLUP_TEST_DATABASE_URL,
// on PostgreSQL: blob_object must keep arbitrary binary bytes exactly
// (BLOB vs BYTEA), upsert on the same key, count usage and delete.
func TestBlobRoundTrip(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	data := make([]byte, 0, 1024)
	for i := 0; i < 1024; i++ {
		data = append(data, byte(i))
	}
	if err := s.PutBlob(ctx, "chat/a", data, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	obj, err := s.GetBlob(ctx, "chat/a")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(obj.Data, data) || obj.SizeBytes != 1024 || obj.ContentType != "application/octet-stream" || obj.CreatedAt.IsZero() {
		t.Fatalf("round trip: size=%d ct=%q equal=%v", obj.SizeBytes, obj.ContentType, bytes.Equal(obj.Data, data))
	}
	if err := s.PutBlob(ctx, "chat/a", []byte{0, 0, 0xff}, "x/y"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBlob(ctx, "chat/empty", nil, ""); err != nil {
		t.Fatal(err)
	}
	obj, _ = s.GetBlob(ctx, "chat/a")
	if !bytes.Equal(obj.Data, []byte{0, 0, 0xff}) || obj.SizeBytes != 3 {
		t.Fatalf("upsert: %v", obj.Data)
	}
	if empty, err := s.GetBlob(ctx, "chat/empty"); err != nil || empty.Data == nil || len(empty.Data) != 0 {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	u, err := s.BlobUsage(ctx)
	if err != nil || u.Objects != 2 || u.Bytes != 3 {
		t.Fatalf("usage: %+v %v", u, err)
	}
	if err := s.DeleteBlob(ctx, "chat/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBlob(ctx, "chat/a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestFileStorageSettingRoundTrip(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	got, updated, err := s.FileStorageSetting(ctx)
	if err != nil || got.Mode != FileStorageDatabase || !updated.IsZero() {
		t.Fatalf("default: %+v %v", got, err)
	}
	want := FileStorageSetting{Mode: FileStorageS3, S3: FileStorageS3Config{Endpoint: "http://x:8333", Bucket: "janus", PathStyle: true, SecretAccessKeyEnc: "janus.v1.abc"}}
	if err := s.PutFileStorageSetting(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, updated, err = s.FileStorageSetting(ctx)
	if err != nil || got != want || updated.IsZero() {
		t.Fatalf("saved: %+v %v", got, err)
	}
}
