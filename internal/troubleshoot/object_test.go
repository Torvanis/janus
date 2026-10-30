package troubleshoot

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/blobstore"
	"github.com/torvanis/janus/internal/blobstore/blobstoretest"
	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/store"
)

// TestCaptureObjectBackend: with file storage on S3 the "object" backend
// writes bodies as encrypted objects under troubleshoot/<day>/<id>/, reads
// them back, keeps reading them after the mode returns to database, and
// purging deletes the objects.
func TestCaptureObjectBackend(t *testing.T) {
	var capturedIDs []string
	ctx := context.Background()
	s := newStore(t)
	// JANUS_LIVE_S3_* runs the same test against a real S3-compatible
	// server (see internal/blobstore/live_test.go); otherwise a fake.
	endpoint, bucket, ak, sk := os.Getenv("JANUS_LIVE_S3_ENDPOINT"), "janus-files", "ak", "sk"
	var objects func() map[string][]byte
	if endpoint == "" {
		fake := blobstoretest.NewFakeS3(t)
		endpoint = fake.URL
		objects = func() map[string][]byte { return fake.Objects(bucket) }
	} else {
		bucket = "janus-capture-" + strings.ToLower(store.NewID()[:8])
		ak, sk = os.Getenv("JANUS_LIVE_S3_ACCESS_KEY_ID"), os.Getenv("JANUS_LIVE_S3_SECRET_ACCESS_KEY")
		raw, err := blobstore.NewS3(blobstore.S3Config{Endpoint: endpoint, Bucket: bucket, PathStyle: true, AccessKeyID: ak, SecretAccessKey: sk})
		if err != nil {
			t.Fatal(err)
		}
		objects = func() map[string][]byte {
			out := map[string][]byte{}
			for _, part := range []string{"request", "response"} {
				for _, c := range capturedIDs {
					k := "troubleshoot/" + time.Now().UTC().Format("2006-01-02") + "/" + c + "/" + part
					if b, err := raw.Get(context.Background(), k); err == nil {
						out[k] = b
					}
				}
			}
			return out
		}
		t.Logf("live S3: %s bucket %s", endpoint, bucket)
	}
	key, err := crypto.New(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	rec := New(s, fakeCipher{}, "", nil)

	cfg := store.TroubleshootingConfig{Storage: store.TroubleshootStorageObject, CaptureRequestBody: true, CaptureResponseBody: true,
		Retention: store.TroubleshootingRetention{MaxCount: 10}}
	if err := cfg.ValidateBackends(false, rec.ObjectAvailable(ctx)); err == nil {
		t.Fatal("object backend accepted without file storage")
	}
	rec.Blobs = blobstore.NewManager(s, key, blobstore.EnvConfig{}, 0)
	if rec.ObjectAvailable(ctx) {
		t.Fatal("object backend offered while file storage is database")
	}
	secret, _ := key.Encrypt(sk)
	if err := s.PutFileStorageSetting(ctx, store.FileStorageSetting{Mode: store.FileStorageS3, S3: store.FileStorageS3Config{
		Endpoint: endpoint, Bucket: bucket, PathStyle: true, AccessKeyID: ak, SecretAccessKeyEnc: secret,
	}}); err != nil {
		t.Fatal(err)
	}
	rec.Blobs.Invalidate()
	if !rec.ObjectAvailable(ctx) {
		t.Fatal("object backend not offered with S3 file storage")
	}
	if err := cfg.ValidateBackends(false, true); err != nil {
		t.Fatal(err)
	}
	session, err := s.CreateTroubleshootingSession(ctx, cfg, time.Now().Add(time.Hour), "admin")
	if err != nil {
		t.Fatal(err)
	}
	ev := &store.UsageEvent{ID: store.NewID(), RequestID: "req-obj", ModelName: "m", UserID: "u", HTTPStatus: 200}
	id := rec.Capture(ctx, session, ev, Payload{RequestHeaders: http.Header{}, RequestBody: []byte(`{"prompt":"PLAINTEXT-MARKER"}`),
		ResponseHeaders: http.Header{}, ResponseBody: []byte(`{"ok":true}`)})
	if id == "" {
		t.Fatal("capture not stored")
	}
	c, err := s.TroubleshootingCaptureByEvent(ctx, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	capturedIDs = append(capturedIDs, c.ID)
	day := time.Now().UTC().Format("2006-01-02")
	if c.StorageBackend != store.TroubleshootStorageObject || c.StorageRef != "s3:troubleshoot/"+day+"/"+c.ID || c.RequestBody != "" {
		t.Fatalf("capture row = %+v", c)
	}
	objs := objects()
	if len(objs) != 2 {
		t.Fatalf("objects = %v", len(objs))
	}
	for k, v := range objs {
		if !strings.HasPrefix(k, "troubleshoot/"+day+"/"+c.ID+"/") || bytes.Contains(v, []byte("PLAINTEXT")) {
			t.Fatalf("object %s holds plaintext or is misplaced", k)
		}
	}
	// Switch file storage back to database: the capture is still read
	// from S3, where it lives.
	if err := s.PutFileStorageSetting(ctx, store.FileStorageSetting{Mode: store.FileStorageDatabase, S3: store.FileStorageS3Config{
		Endpoint: endpoint, Bucket: bucket, PathStyle: true, AccessKeyID: ak, SecretAccessKeyEnc: secret,
	}}); err != nil {
		t.Fatal(err)
	}
	rec.Blobs.Invalidate()
	req, resp, err := rec.Bodies(ctx, c)
	if err != nil || string(req) != `{"prompt":"PLAINTEXT-MARKER"}` || string(resp) != `{"ok":true}` {
		t.Fatalf("bodies = %q %q %v", req, resp, err)
	}
	if n, err := rec.PurgeAll(ctx); err != nil || n != 1 {
		t.Fatalf("purge = %d %v", n, err)
	}
	t.Logf("capture %s: 2 encrypted objects in %s, read back after switching to database, purge removed them", c.ID, bucket)
	if left := objects(); len(left) != 0 {
		t.Fatalf("purge left %d objects", len(left))
	}
}
