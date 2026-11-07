package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/blobstore"
	"github.com/torvanis/janus/internal/config"
	"github.com/torvanis/janus/internal/store"
)

// File storage administration: GET/PUT /api/v1/admin/storage and
// POST /api/v1/admin/storage/test. The backend itself lives in
// internal/blobstore; this file only exposes its configuration.

// blobs returns the file storage manager, building one lazily from the
// server's dependencies so any construction path gets a working one.
func (s *Server) blobs() *blobstore.Manager {
	s.blobsOnce.Do(func() {
		if s.Blobs != nil {
			return
		}
		var cipher blobstore.ManagerCipher
		if s.Cipher != nil {
			cipher = s.Cipher
		}
		s.Blobs = blobstore.NewManager(s.Store, cipher, StorageEnvConfig(s.Config), maxObjectBytes(s.Config))
	})
	return s.Blobs
}

func maxObjectBytes(c *config.Config) int64 {
	if c == nil {
		return 0
	}
	return c.StorageMaxObjectBytes
}

// StorageEnvConfig maps the deployment's JANUS_STORAGE_MODE / JANUS_S3_*
// settings onto the blobstore manager's environment configuration. An
// unset mode leaves file storage to the admin UI.
func StorageEnvConfig(c *config.Config) blobstore.EnvConfig {
	if c == nil || c.StorageMode == "" {
		return blobstore.EnvConfig{}
	}
	return blobstore.EnvConfig{Mode: c.StorageMode, S3: blobstore.S3Config{
		Endpoint: c.S3Endpoint, Bucket: c.S3Bucket, Region: c.S3Region, PathStyle: c.S3PathStyle, Prefix: c.S3Prefix,
		AccessKeyID: c.S3AccessKeyID, SecretAccessKey: c.S3SecretAccessKey, CABundle: c.S3CABundle,
	}}
}

// storageS3View is the S3 part of the storage document. The secret is
// never returned, only whether one is set.
type storageS3View struct {
	Endpoint    string `json:"endpoint"`
	Bucket      string `json:"bucket"`
	Region      string `json:"region"`
	PathStyle   bool   `json:"path_style"`
	Prefix      string `json:"prefix"`
	AccessKeyID string `json:"access_key_id"`
	SecretSet   bool   `json:"secret_set"`
	CABundle    string `json:"ca_bundle"`
}

// storageDocument is served by GET /admin/storage.
type storageDocument struct {
	Mode string        `json:"mode"`
	S3   storageS3View `json:"s3"`
	// EnvManaged lists the fields set by the deployment (JANUS_STORAGE_MODE
	// and JANUS_S3_*); the UI shows them read-only.
	EnvManaged     []string          `json:"env_managed"`
	MaxObjectBytes int64             `json:"max_object_bytes"`
	Usage          storageUsage      `json:"usage"`
	UpdatedAt      *time.Time        `json:"updated_at,omitempty"`
	Backends       []string          `json:"backends"`
	Notes          map[string]string `json:"notes"`
}

type storageUsage struct {
	// Database counts objects held in the database backend.
	Database store.BlobUsage `json:"database"`
}

var storageEnvFields = []string{"mode", "endpoint", "bucket", "region", "path_style", "prefix", "access_key_id", "secret_access_key", "ca_bundle"}

func (s *Server) storageDocument(ctx context.Context) (*storageDocument, error) {
	m := s.blobs()
	doc := &storageDocument{
		EnvManaged:     []string{},
		MaxObjectBytes: m.MaxObjectBytes(),
		Backends:       []string{store.FileStorageDatabase, store.FileStorageS3},
		Notes:          map[string]string{"migration": "Changing the storage mode does not move existing files: they stay where they are and remain readable."},
	}
	if m.EnvManaged() {
		env := m.Env()
		doc.Mode = env.Mode
		doc.S3 = storageS3View{Endpoint: env.S3.Endpoint, Bucket: env.S3.Bucket, Region: env.S3.Region, PathStyle: env.S3.PathStyle,
			Prefix: env.S3.Prefix, AccessKeyID: env.S3.AccessKeyID, SecretSet: env.S3.SecretAccessKey != "", CABundle: env.S3.CABundle}
		doc.EnvManaged = append(doc.EnvManaged, storageEnvFields...)
	} else {
		setting, updated, err := s.Store.FileStorageSetting(ctx)
		if err != nil {
			return nil, err
		}
		doc.Mode = setting.Mode
		region := setting.S3.Region
		if region == "" {
			region = blobstore.DefaultRegion
		}
		doc.S3 = storageS3View{Endpoint: setting.S3.Endpoint, Bucket: setting.S3.Bucket, Region: region, PathStyle: setting.S3.PathStyle,
			Prefix: setting.S3.Prefix, AccessKeyID: setting.S3.AccessKeyID, SecretSet: setting.S3.SecretAccessKeyEnc != "", CABundle: setting.S3.CABundle}
		if !updated.IsZero() {
			doc.UpdatedAt = &updated
		}
	}
	usage, err := s.Store.BlobUsage(ctx)
	if err != nil {
		return nil, err
	}
	doc.Usage.Database = usage
	return doc, nil
}

func (s *Server) handleGetStorage(w http.ResponseWriter, r *http.Request) {
	doc, err := s.storageDocument(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, doc)
}

// storageRequest is the body of PUT /admin/storage and POST
// /admin/storage/test. An empty secret_access_key keeps the saved one.
type storageRequest struct {
	Mode string `json:"mode"`
	S3   struct {
		Endpoint        string `json:"endpoint"`
		Bucket          string `json:"bucket"`
		Region          string `json:"region"`
		PathStyle       bool   `json:"path_style"`
		Prefix          string `json:"prefix"`
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		CABundle        string `json:"ca_bundle"`
	} `json:"s3"`
}

// resolveStorageRequest normalizes a request into a blobstore config,
// filling the secret from the saved setting when the request leaves it
// blank. It returns the stored setting for audit comparison.
func (s *Server) resolveStorageRequest(ctx context.Context, body *storageRequest) (string, blobstore.S3Config, store.FileStorageSetting, error) {
	saved, _, err := s.Store.FileStorageSetting(ctx)
	if err != nil {
		return "", blobstore.S3Config{}, saved, err
	}
	mode := strings.ToLower(strings.TrimSpace(body.Mode))
	if mode == "" {
		mode = store.FileStorageDatabase
	}
	if mode != store.FileStorageDatabase && mode != store.FileStorageS3 {
		return "", blobstore.S3Config{}, saved, ErrInvalidRequest(`mode must be "database" or "s3".`)
	}
	cfg := blobstore.S3Config{
		Endpoint: strings.TrimRight(strings.TrimSpace(body.S3.Endpoint), "/"), Bucket: strings.TrimSpace(body.S3.Bucket),
		Region: strings.TrimSpace(body.S3.Region), PathStyle: body.S3.PathStyle, Prefix: strings.Trim(strings.TrimSpace(body.S3.Prefix), "/"),
		AccessKeyID: strings.TrimSpace(body.S3.AccessKeyID), SecretAccessKey: body.S3.SecretAccessKey, CABundle: strings.TrimSpace(body.S3.CABundle),
	}
	if cfg.Region == "" {
		cfg.Region = blobstore.DefaultRegion
	}
	if cfg.SecretAccessKey == "" && saved.S3.SecretAccessKeyEnc != "" {
		if s.Cipher == nil {
			return "", cfg, saved, ErrInvalidRequest("The gateway has no encryption key configured.")
		}
		secret, err := s.Cipher.Decrypt(saved.S3.SecretAccessKeyEnc)
		if err != nil {
			return "", cfg, saved, err
		}
		cfg.SecretAccessKey = secret
	}
	if mode == store.FileStorageS3 || cfg.Endpoint != "" {
		if err := cfg.Validate(); err != nil {
			return "", cfg, saved, ErrInvalidRequest(capitalize(err.Error()) + ".")
		}
	}
	return mode, cfg, saved, nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (s *Server) handlePutStorage(w http.ResponseWriter, r *http.Request) {
	m := s.blobs()
	if m.EnvManaged() {
		WriteError(w, r, ErrInvalidRequest("File storage is set by the deployment (JANUS_STORAGE_MODE) and cannot be changed here."))
		return
	}
	if s.Cipher == nil {
		WriteError(w, r, ErrInvalidRequest("File storage cannot be configured: the gateway has no encryption key configured."))
		return
	}
	var body storageRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	mode, cfg, saved, err := s.resolveStorageRequest(r.Context(), &body)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	next := store.FileStorageSetting{Mode: mode, S3: store.FileStorageS3Config{
		Endpoint: cfg.Endpoint, Bucket: cfg.Bucket, Region: cfg.Region, PathStyle: cfg.PathStyle, Prefix: cfg.Prefix,
		AccessKeyID: cfg.AccessKeyID, CABundle: cfg.CABundle,
	}}
	secretChanged := false
	switch {
	case body.S3.SecretAccessKey != "":
		enc, err := s.Cipher.Encrypt(body.S3.SecretAccessKey)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		next.S3.SecretAccessKeyEnc = enc
		secretChanged = true
	default:
		next.S3.SecretAccessKeyEnc = saved.S3.SecretAccessKeyEnc
	}
	if err := s.Store.PutFileStorageSetting(r.Context(), next); err != nil {
		WriteError(w, r, err)
		return
	}
	m.Invalidate()
	s.audit(r, "storage.configure", "system_setting", store.SettingFileStorage, storageAuditView(saved, false), storageAuditView(next, secretChanged))
	s.handleGetStorage(w, r)
}

// storageAuditView is what the audit log records: everything but the secret.
func storageAuditView(v store.FileStorageSetting, secretChanged bool) map[string]any {
	out := map[string]any{
		"mode": v.Mode, "endpoint": v.S3.Endpoint, "bucket": v.S3.Bucket, "region": v.S3.Region,
		"path_style": v.S3.PathStyle, "prefix": v.S3.Prefix, "access_key_id": v.S3.AccessKeyID,
		"secret_set": v.S3.SecretAccessKeyEnc != "", "ca_bundle_set": v.S3.CABundle != "",
	}
	if secretChanged {
		out["secret_changed"] = true
	}
	return out
}

// handleTestStorage tries the posted (unsaved) configuration: write, read
// back and delete a probe object. Under JANUS_STORAGE_MODE the deployment's
// configuration is tested instead.
func (s *Server) handleTestStorage(w http.ResponseWriter, r *http.Request) {
	m := s.blobs()
	var mode string
	var cfg blobstore.S3Config
	if m.EnvManaged() {
		env := m.Env()
		mode, cfg = env.Mode, env.S3
	} else {
		var body storageRequest
		if err := decodeJSON(r, &body); err != nil {
			WriteError(w, r, err)
			return
		}
		var err error
		if mode, cfg, _, err = s.resolveStorageRequest(r.Context(), &body); err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				WriteJSON(w, http.StatusOK, map[string]any{"ok": false, "mode": body.Mode, "error": apiErr.Message})
				return
			}
			WriteError(w, r, err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	started := time.Now()
	err := m.Probe(ctx, mode, cfg)
	result := map[string]any{"ok": err == nil, "mode": mode, "latency_ms": time.Since(started).Milliseconds()}
	if err != nil {
		result["error"] = err.Error()
	}
	s.audit(r, "storage.test", "system_setting", store.SettingFileStorage, nil, map[string]any{"mode": mode, "endpoint": cfg.Endpoint, "bucket": cfg.Bucket, "ok": err == nil})
	WriteJSON(w, http.StatusOK, result)
}
