package blobstore

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/torvanis/janus/internal/store"
)

// TestLiveS3 runs the S3 backend against a real S3-compatible server. It is
// skipped unless JANUS_LIVE_S3_ENDPOINT is set, e.g. for a local SeaweedFS:
//
//	JANUS_LIVE_S3_ENDPOINT=http://127.0.0.1:58333 JANUS_LIVE_S3_BUCKET=janus-live \
//	JANUS_LIVE_S3_ACCESS_KEY_ID=… JANUS_LIVE_S3_SECRET_ACCESS_KEY=… \
//	go test -run TestLiveS3 -v ./internal/blobstore
func TestLiveS3(t *testing.T) {
	endpoint := os.Getenv("JANUS_LIVE_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("JANUS_LIVE_S3_ENDPOINT not set")
	}
	cfg := S3Config{
		Endpoint:        endpoint,
		Bucket:          envOrDefault("JANUS_LIVE_S3_BUCKET", "janus-live-test"),
		Region:          os.Getenv("JANUS_LIVE_S3_REGION"),
		PathStyle:       os.Getenv("JANUS_LIVE_S3_PATH_STYLE") != "false",
		Prefix:          "live-" + time.Now().UTC().Format("20060102T150405"),
		AccessKeyID:     os.Getenv("JANUS_LIVE_S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("JANUS_LIVE_S3_SECRET_ACCESS_KEY"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	raw, err := NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("CreateBucketIfMissing", func(t *testing.T) {
		_, headErr := raw.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(cfg.Bucket)})
		t.Logf("bucket existed before: %v", headErr == nil)
		if err := raw.EnsureBucket(ctx); err != nil {
			t.Fatal(err)
		}
		// A second, fresh client sees the bucket as already present (owned).
		again, _ := NewS3(cfg)
		if err := again.EnsureBucket(ctx); err != nil {
			t.Fatalf("second EnsureBucket: %v", err)
		}
		t.Logf("bucket %q ready at %s", cfg.Bucket, cfg.Endpoint)
	})
	t.Run("RawRoundTrip", func(t *testing.T) {
		data := bytes.Repeat([]byte{0, 1, 2, 3, 0xfe, 0xff}, 50_000) // 300 kB binary
		if err := raw.Put(ctx, "raw/object.bin", data, "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
		got, err := raw.Get(ctx, "raw/object.bin")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("get: len=%d err=%v", len(got), err)
		}
		if err := raw.Delete(ctx, "raw/object.bin"); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Get(ctx, "raw/object.bin"); err != ErrNotFound {
			t.Fatalf("after delete: %v", err)
		}
		if err := raw.Delete(ctx, "raw/object.bin"); err != nil {
			t.Fatalf("delete missing: %v", err)
		}
		t.Logf("put/get/delete %d bytes OK", len(data))
	})
	t.Run("EncryptedWrapper", func(t *testing.T) {
		enc, err := Encrypted(raw, testCipher(t))
		if err != nil {
			t.Fatal(err)
		}
		plain := []byte("CONFIDENTIAL chat attachment: quarterly numbers")
		key := "troubleshoot/" + time.Now().UTC().Format("2006-01-02") + "/live/request"
		if err := enc.Put(ctx, key, plain, "text/plain"); err != nil {
			t.Fatal(err)
		}
		stored, err := raw.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(stored, []byte("CONFIDENTIAL")) || bytes.Equal(stored, plain) {
			t.Fatal("S3 holds plaintext")
		}
		if !bytes.HasPrefix(stored, []byte("JB\x01")) {
			t.Fatalf("stored object lacks the encryption header: % x", stored[:3])
		}
		got, err := enc.Get(ctx, key)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("decrypt: %q %v", got, err)
		}
		_ = enc.Delete(ctx, key)
		t.Logf("plaintext %d bytes -> stored ciphertext %d bytes (header % x), decrypts back OK", len(plain), len(stored), stored[:3])
	})
	t.Run("Check", func(t *testing.T) {
		if err := raw.Check(ctx); err != nil {
			t.Fatal(err)
		}
		m := NewManager(testStore(t), testCipher(t), EnvConfig{}, 0)
		if err := m.Probe(ctx, store.FileStorageS3, cfg); err != nil {
			t.Fatalf("manager probe: %v", err)
		}
		t.Log("Check() and Manager.Probe() OK")
	})
	t.Run("WrongSecretIsClear", func(t *testing.T) {
		bad := cfg
		bad.SecretAccessKey = "definitely-not-the-secret"
		s, err := NewS3(bad)
		if err != nil {
			t.Fatal(err)
		}
		err = s.Check(ctx)
		if err == nil {
			t.Fatal("wrong secret passed Check")
		}
		if strings.Contains(err.Error(), bad.SecretAccessKey) {
			t.Fatal("error leaks the secret")
		}
		if !strings.Contains(err.Error(), "SignatureDoesNotMatch") && !strings.Contains(err.Error(), "AccessDenied") {
			t.Fatalf("unclear error: %v", err)
		}
		t.Logf("wrong secret -> %v", err)
	})
	t.Run("WrongAccessKeyIsClear", func(t *testing.T) {
		bad := cfg
		bad.AccessKeyID = "NOSUCHKEY"
		s, _ := NewS3(bad)
		err := s.Check(ctx)
		if err == nil {
			t.Fatal("unknown access key passed Check")
		}
		t.Logf("unknown access key -> %v", err)
	})
}

func envOrDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
