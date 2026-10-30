package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/blobstore"
	"github.com/torvanis/janus/internal/blobstore/blobstoretest"
	"github.com/torvanis/janus/internal/store"
)

// TestAdminStorageEndpoints covers file storage administration: the default
// database document, the test endpoint against an unsaved S3 config, saving
// it (secret encrypted, never returned, kept when omitted), audit entries,
// usage counts, the troubleshooting "object" backend appearing, the env
// lock, and admin-only access.
func TestAdminStorageEndpoints(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	fake := blobstoretest.NewFakeS3(t)
	const path = "/api/v1/admin/storage"
	decode := func(code int, body []byte) map[string]any {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("status %d: %s", code, body)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}

	rec := h.doAsSession(session, http.MethodGet, path, nil)
	doc := decode(rec.Code, rec.Body.Bytes())
	if doc["mode"] != "database" || len(doc["env_managed"].([]any)) != 0 || doc["max_object_bytes"].(float64) != float64(blobstore.DefaultMaxObjectBytes) {
		t.Fatalf("default doc = %v", doc)
	}

	s3 := map[string]any{"endpoint": fake.URL, "bucket": "janus-files", "path_style": true, "access_key_id": "AKID", "secret_access_key": "s3cr3t-value"}
	rec = h.doAsSession(session, http.MethodPost, path+"/test", map[string]any{"mode": "s3", "s3": s3})
	res := decode(rec.Code, rec.Body.Bytes())
	if res["ok"] != true {
		t.Fatalf("test = %v", res)
	}
	if got, _, _ := h.store.FileStorageSetting(ctx); got.Mode != "database" {
		t.Fatal("test endpoint must not save")
	}
	rec = h.doAsSession(session, http.MethodPost, path+"/test", map[string]any{"mode": "s3", "s3": map[string]any{"endpoint": "http://127.0.0.1:1", "bucket": "janus-files", "access_key_id": "a", "secret_access_key": "s3cr3t-value"}})
	res = decode(rec.Code, rec.Body.Bytes())
	if res["ok"] != false || res["error"] == "" || strings.Contains(rec.Body.String(), "s3cr3t-value") {
		t.Fatalf("unreachable test = %s", rec.Body.String())
	}
	rec = h.doAsSession(session, http.MethodPost, path+"/test", map[string]any{"mode": "s3", "s3": map[string]any{"endpoint": fake.URL}})
	if res = decode(rec.Code, rec.Body.Bytes()); res["ok"] != false || !strings.Contains(res["error"].(string), "bucket") {
		t.Fatalf("incomplete test = %v", res)
	}

	rec = h.doAsSession(session, http.MethodPut, path, map[string]any{"mode": "s3", "s3": s3})
	doc = decode(rec.Code, rec.Body.Bytes())
	if strings.Contains(rec.Body.String(), "s3cr3t-value") {
		t.Fatal("secret returned")
	}
	view := doc["s3"].(map[string]any)
	if doc["mode"] != "s3" || view["secret_set"] != true || view["region"] != "us-east-1" || view["bucket"] != "janus-files" {
		t.Fatalf("saved doc = %v", doc)
	}
	saved, _, _ := h.store.FileStorageSetting(ctx)
	if saved.S3.SecretAccessKeyEnc == "" || strings.Contains(saved.S3.SecretAccessKeyEnc, "s3cr3t") {
		t.Fatalf("secret not encrypted at rest: %q", saved.S3.SecretAccessKeyEnc)
	}
	// Saving without a secret keeps the stored one, and a test with the
	// blank secret uses it.
	delete(s3, "secret_access_key")
	s3["prefix"] = "gw1"
	rec = h.doAsSession(session, http.MethodPut, path, map[string]any{"mode": "s3", "s3": s3})
	decode(rec.Code, rec.Body.Bytes())
	if again, _, _ := h.store.FileStorageSetting(ctx); again.S3.SecretAccessKeyEnc != saved.S3.SecretAccessKeyEnc || again.S3.Prefix != "gw1" {
		t.Fatalf("secret not kept: %+v", again)
	}
	rec = h.doAsSession(session, http.MethodPost, path+"/test", map[string]any{"mode": "s3", "s3": s3})
	if res = decode(rec.Code, rec.Body.Bytes()); res["ok"] != true {
		t.Fatalf("test with saved secret = %v", res)
	}
	entries, _, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "storage.configure"})
	if err != nil || len(entries) != 2 || strings.Contains(entries[0].NewValue+entries[1].NewValue, "s3cr3t") {
		t.Fatalf("audit = %d %v", len(entries), err)
	}

	// The troubleshooting status now offers the object backend.
	rec = h.doAsSession(session, http.MethodGet, "/api/v1/admin/troubleshooting", nil)
	if !strings.Contains(rec.Body.String(), `"object"`) {
		t.Fatalf("object backend not offered: %s", rec.Body.String())
	}

	// Bad input is refused.
	rec = h.doAsSession(session, http.MethodPut, path, map[string]any{"mode": "minio"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode = %d", rec.Code)
	}
	rec = h.doAsSession(session, http.MethodPut, path, map[string]any{"mode": "s3", "s3": map[string]any{"endpoint": "ftp://x", "bucket": "b-b-b", "access_key_id": "a"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad endpoint = %d", rec.Code)
	}

	// Usage counts the database backend.
	st, err := h.server.blobs().For(ctx, blobstore.KindDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "chat/x", []byte("hello"), ""); err != nil {
		t.Fatal(err)
	}
	rec = h.doAsSession(session, http.MethodGet, path, nil)
	doc = decode(rec.Code, rec.Body.Bytes())
	if u := doc["usage"].(map[string]any)["database"].(map[string]any); u["objects"].(float64) != 1 || u["bytes"].(float64) <= 5 {
		t.Fatalf("usage = %v", u)
	}

	// Non-admins are refused.
	plain, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-storage-plain", "plain-storage@example.com", "Plain", false, false)
	if err != nil {
		t.Fatal(err)
	}
	plainSession, _ := h.server.Sessions.Create(ctx, plain.ID)
	if rec = h.doAsSession(plainSession, http.MethodGet, path, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET = %d", rec.Code)
	}
}

// TestAdminStorageEnvManaged: JANUS_STORAGE_MODE locks every field.
func TestAdminStorageEnvManaged(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	fake := blobstoretest.NewFakeS3(t)
	h.server.Config.StorageMode = "s3"
	h.server.Config.S3Endpoint = fake.URL
	h.server.Config.S3Bucket = "env-bucket"
	h.server.Config.S3PathStyle = true
	h.server.Config.S3AccessKeyID = "envak"
	h.server.Config.S3SecretAccessKey = "env-secret-value"
	session, _ := h.server.Sessions.Create(ctx, h.user.ID)

	rec := h.doAsSession(session, http.MethodGet, "/api/v1/admin/storage", nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "env-secret-value") {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	var doc storageDocument
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	if doc.Mode != "s3" || !doc.S3.SecretSet || len(doc.EnvManaged) != len(storageEnvFields) || doc.S3.Bucket != "env-bucket" {
		t.Fatalf("doc = %+v", doc)
	}
	if rec = h.doAsSession(session, http.MethodPut, "/api/v1/admin/storage", map[string]any{"mode": "database"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("put under env = %d", rec.Code)
	}
	rec = h.doAsSession(session, http.MethodPost, "/api/v1/admin/storage/test", map[string]any{})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("env test = %s", rec.Body.String())
	}
}
