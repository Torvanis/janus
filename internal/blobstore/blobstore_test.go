package blobstore

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/blobstore/blobstoretest"
	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/store"
)

func testCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	c, err := crypto.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.OpenMigratedSQLite(context.Background(), filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestValidateKey(t *testing.T) {
	for _, ok := range []string{"a", "troubleshoot/2026-10-19/abc/request", "chat/u1/f.bin"} {
		if err := ValidateKey(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/abs", "a//b", "a/../b", "./a", "a\\b", "a\nb", strings.Repeat("x", 513)} {
		if err := ValidateKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRefRoundTrip(t *testing.T) {
	kind, key, ok := ParseRef(Ref(KindS3, "troubleshoot/x/request"))
	if !ok || kind != KindS3 || key != "troubleshoot/x/request" {
		t.Fatalf("got %q %q %v", kind, key, ok)
	}
	if _, _, ok := ParseRef("2026-10-19/abc"); ok {
		t.Fatal("a disk path parsed as a ref")
	}
}

func TestDBBackendEncryptedRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	raw := NewDB(s)
	enc, err := Encrypted(raw, testCipher(t))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("the quick brown fox: secret attachment body")
	if err := enc.Put(ctx, "chat/u1/f1", plain, "text/plain"); err != nil {
		t.Fatal(err)
	}
	got, err := enc.Get(ctx, "chat/u1/f1")
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("get: %q %v", got, err)
	}
	stored, err := raw.Get(ctx, "chat/u1/f1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("secret attachment")) {
		t.Fatal("database holds plaintext")
	}
	obj, err := s.GetBlob(ctx, "chat/u1/f1")
	if err != nil || obj.ContentType != "application/octet-stream" || obj.SizeBytes != int64(len(stored)) {
		t.Fatalf("row: %+v %v", obj, err)
	}
	usage, err := s.BlobUsage(ctx)
	if err != nil || usage.Objects != 1 || usage.Bytes != int64(len(stored)) {
		t.Fatalf("usage: %+v %v", usage, err)
	}
	if err := enc.Delete(ctx, "chat/u1/f1"); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Get(ctx, "chat/u1/f1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := enc.Delete(ctx, "chat/u1/f1"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if err := enc.Check(ctx); err != nil {
		t.Fatalf("check: %v", err)
	}
	if usage, _ := s.BlobUsage(ctx); usage.Objects != 0 {
		t.Fatalf("probe object left behind: %+v", usage)
	}
}

func TestEncryptedRefusesNilCipher(t *testing.T) {
	if _, err := Encrypted(NewDB(nil), nil); err == nil {
		t.Fatal("nil cipher accepted")
	}
}

func TestManagerSettingAndLimit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c := testCipher(t)
	m := NewManager(s, c, EnvConfig{}, 16)
	if m.EnvManaged() {
		t.Fatal("env managed without env")
	}
	st, err := m.Active(ctx)
	if err != nil || st.Kind() != KindDatabase {
		t.Fatalf("default: %v %v", st, err)
	}
	if err := st.Put(ctx, "a/b", bytes.Repeat([]byte("x"), 17), ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over limit: %v", err)
	}
	if err := st.Put(ctx, "a/b", []byte("ok"), ""); err != nil {
		t.Fatal(err)
	}

	enc, _ := c.Encrypt("topsecret")
	if err := s.PutFileStorageSetting(ctx, store.FileStorageSetting{Mode: store.FileStorageS3, S3: store.FileStorageS3Config{
		Endpoint: "http://127.0.0.1:1", Bucket: "janus-files", AccessKeyID: "AK", SecretAccessKeyEnc: enc,
	}}); err != nil {
		t.Fatal(err)
	}
	// Cached until invalidated.
	if k := m.ActiveKind(ctx); k != KindDatabase {
		t.Fatalf("cache ignored: %s", k)
	}
	m.Invalidate()
	r, err := m.Resolve(ctx)
	if err != nil || r.Mode != store.FileStorageS3 || r.S3.SecretAccessKey != "topsecret" || r.S3.Region != DefaultRegion {
		t.Fatalf("resolve: %+v %v", r, err)
	}
	st, err = m.Active(ctx)
	if err != nil || st.Kind() != KindS3 {
		t.Fatalf("active after switch: %v %v", st, err)
	}
	// Objects written before the switch stay readable from their backend.
	old, err := m.For(ctx, KindDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := old.Get(ctx, "a/b"); err != nil || string(got) != "ok" {
		t.Fatalf("old object: %q %v", got, err)
	}
}

func TestManagerEnvWins(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	m := NewManager(s, testCipher(t), EnvConfig{Mode: "S3", S3: S3Config{Endpoint: "http://seaweed:8333", Bucket: "janus", AccessKeyID: "a", SecretAccessKey: "fake-s3-secret-access-key", PathStyle: true}}, 0)
	if !m.EnvManaged() || m.MaxObjectBytes() != DefaultMaxObjectBytes {
		t.Fatal("env config not applied")
	}
	r, err := m.Resolve(ctx)
	if err != nil || r.Mode != "s3" || !r.EnvManaged || r.S3.Region != DefaultRegion {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestS3ConfigValidate(t *testing.T) {
	good := S3Config{Endpoint: "https://s3.example.com", Bucket: "janus-files", AccessKeyID: "a", SecretAccessKey: "fake-s3-secret-access-key"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*S3Config){
		"endpoint":   func(c *S3Config) { c.Endpoint = "" },
		"scheme":     func(c *S3Config) { c.Endpoint = "ftp://x" },
		"userinfo":   func(c *S3Config) { c.Endpoint = "https://a:b@x" },
		"bucket":     func(c *S3Config) { c.Bucket = "Bad_Bucket" },
		"bucket len": func(c *S3Config) { c.Bucket = "ab" },
		"ak":         func(c *S3Config) { c.AccessKeyID = "" },
		"sk":         func(c *S3Config) { c.SecretAccessKey = "" },
		"prefix":     func(c *S3Config) { c.Prefix = "a/../b" },
		"ca":         func(c *S3Config) { c.CABundle = "not a pem" },
	}
	for name, mut := range cases {
		c := good
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestProbeDatabase(t *testing.T) {
	m := NewManager(testStore(t), testCipher(t), EnvConfig{}, 0)
	if err := m.Probe(context.Background(), "database", S3Config{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Probe(context.Background(), "s3", S3Config{Endpoint: "http://x"}); err == nil {
		t.Fatal("incomplete s3 accepted")
	}
}

func TestS3BackendAgainstFake(t *testing.T) {
	ctx := context.Background()
	fake := blobstoretest.NewFakeS3(t)
	raw, err := NewS3(S3Config{Endpoint: fake.URL, Bucket: "janus-files", PathStyle: true, Prefix: "/tenant-a/", AccessKeyID: "a", SecretAccessKey: "fake-s3-secret-access-key"})
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Check(ctx); err != nil {
		t.Fatalf("check (creates bucket): %v", err)
	}
	enc, _ := Encrypted(raw, testCipher(t))
	if err := enc.Put(ctx, "chat/f1", []byte("hello world"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	objs := fake.Objects("janus-files")
	stored, ok := objs["tenant-a/chat/f1"]
	if !ok || len(objs) != 1 {
		t.Fatalf("prefix not applied: %v", objs)
	}
	if bytes.Contains(stored, []byte("hello")) {
		t.Fatal("S3 holds plaintext")
	}
	if got, err := enc.Get(ctx, "chat/f1"); err != nil || string(got) != "hello world" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := enc.Get(ctx, "chat/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := enc.Delete(ctx, "chat/f1"); err != nil {
		t.Fatal(err)
	}
}
