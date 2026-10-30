package blobstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// DefaultMaxObjectBytes is the per-object ceiling when none is configured
// (JANUS_STORAGE_MAX_OBJECT_BYTES).
const DefaultMaxObjectBytes int64 = 25 << 20

// managerCacheTTL bounds how long a replica keeps serving a backend after
// an admin changed the setting on another replica.
const managerCacheTTL = 15 * time.Second

// ManagerCipher is the key material the Manager needs: byte encryption for
// objects and string decryption for the stored S3 secret.
type ManagerCipher interface {
	ByteCipher
	Decrypt(envelope string) (string, error)
}

// EnvConfig is file storage configured by the deployment
// (JANUS_STORAGE_MODE and JANUS_S3_*). An empty Mode means "not set by the
// deployment": the admin setting decides.
type EnvConfig struct {
	Mode string
	S3   S3Config
}

// Resolved is the effective file storage configuration.
type Resolved struct {
	Mode       string
	S3         S3Config
	EnvManaged bool
}

// Manager resolves the active backend from the environment or the admin
// setting, caches it briefly, and hands out encrypting, size-limited
// stores. It is safe for concurrent use.
type Manager struct {
	store    *store.Store
	cipher   ManagerCipher
	env      EnvConfig
	maxBytes int64

	mu       sync.Mutex
	cachedAt time.Time
	resolved *Resolved
	backends map[string]Store // kind -> raw backend for the resolved config
	fp       string           // fingerprint of the S3 config the s3 backend was built from
}

// NewManager builds a manager. maxBytes <= 0 applies DefaultMaxObjectBytes.
func NewManager(s *store.Store, cipher ManagerCipher, env EnvConfig, maxBytes int64) *Manager {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxObjectBytes
	}
	env.Mode = strings.TrimSpace(strings.ToLower(env.Mode))
	if env.S3.Region == "" {
		env.S3.Region = DefaultRegion
	}
	return &Manager{store: s, cipher: cipher, env: env, maxBytes: maxBytes}
}

// MaxObjectBytes is the per-object size ceiling.
func (m *Manager) MaxObjectBytes() int64 { return m.maxBytes }

// EnvManaged reports whether the deployment sets the storage mode.
func (m *Manager) EnvManaged() bool { return m.env.Mode != "" }

// Env returns the deployment-provided configuration.
func (m *Manager) Env() EnvConfig { return m.env }

// Invalidate drops the cached configuration; the next call re-reads it.
func (m *Manager) Invalidate() {
	m.mu.Lock()
	m.cachedAt = time.Time{}
	m.mu.Unlock()
}

// Resolve returns the effective configuration (secret decrypted).
func (m *Manager) Resolve(ctx context.Context) (Resolved, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refreshLocked(ctx); err != nil {
		return Resolved{}, err
	}
	return *m.resolved, nil
}

func (m *Manager) refreshLocked(ctx context.Context) error {
	if m.resolved != nil && time.Since(m.cachedAt) < managerCacheTTL {
		return nil
	}
	var r Resolved
	if m.env.Mode != "" {
		r = Resolved{Mode: m.env.Mode, S3: m.env.S3, EnvManaged: true}
	} else {
		setting, _, err := m.store.FileStorageSetting(ctx)
		if err != nil {
			return err
		}
		r = Resolved{Mode: setting.Mode, S3: S3Config{
			Endpoint: setting.S3.Endpoint, Bucket: setting.S3.Bucket, Region: setting.S3.Region,
			PathStyle: setting.S3.PathStyle, Prefix: setting.S3.Prefix, AccessKeyID: setting.S3.AccessKeyID,
			CABundle: setting.S3.CABundle,
		}}
		if setting.S3.SecretAccessKeyEnc != "" {
			if m.cipher == nil {
				return errors.New("file storage: the S3 secret is stored encrypted but no encryption key is configured")
			}
			secret, err := m.cipher.Decrypt(setting.S3.SecretAccessKeyEnc)
			if err != nil {
				return fmt.Errorf("file storage: decrypt S3 secret: %w", err)
			}
			r.S3.SecretAccessKey = secret
		}
	}
	if r.Mode == "" {
		r.Mode = store.FileStorageDatabase
	}
	if r.S3.Region == "" {
		r.S3.Region = DefaultRegion
	}
	m.resolved = &r
	m.cachedAt = time.Now()
	if fp := fingerprint(r.S3); fp != m.fp {
		// The S3 settings changed: drop the old client (and its
		// "bucket exists" memo) so the next call builds a fresh one.
		delete(m.backends, KindS3)
		m.fp = fp
	}
	return nil
}

func fingerprint(c S3Config) string {
	return strings.Join([]string{c.Endpoint, c.Bucket, c.Region, fmt.Sprint(c.PathStyle), c.Prefix, c.AccessKeyID, c.SecretAccessKey, c.CABundle}, "\x00")
}

// rawLocked returns the unencrypted backend of one kind for the current
// configuration.
func (m *Manager) rawLocked(kind string) (Store, error) {
	if b, ok := m.backends[kind]; ok {
		return b, nil
	}
	var b Store
	switch kind {
	case KindDatabase:
		b = NewDB(m.store)
	case KindS3:
		if m.resolved.S3.Endpoint == "" {
			return nil, errors.New("file storage: S3 is not configured")
		}
		s, err := NewS3(m.resolved.S3)
		if err != nil {
			return nil, fmt.Errorf("file storage: %w", err)
		}
		b = s
	default:
		return nil, fmt.Errorf("file storage: unknown backend %q", kind)
	}
	if m.backends == nil {
		m.backends = map[string]Store{}
	}
	m.backends[kind] = b
	return b, nil
}

func (m *Manager) wrap(b Store) (Store, error) {
	enc, err := Encrypted(b, m.cipher)
	if err != nil {
		return nil, err
	}
	return limited{Store: enc, max: m.maxBytes}, nil
}

// Active returns the encrypting store for the configured mode. New objects
// go here; record Store.Kind() next to each object so it can be read back
// with For after the mode changes.
func (m *Manager) Active(ctx context.Context) (Store, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refreshLocked(ctx); err != nil {
		return nil, err
	}
	kind := KindDatabase
	if m.resolved.Mode == store.FileStorageS3 {
		kind = KindS3
	}
	b, err := m.rawLocked(kind)
	if err != nil {
		return nil, err
	}
	return m.wrap(b)
}

// ActiveKind names the configured backend without building it.
func (m *Manager) ActiveKind(ctx context.Context) string {
	r, err := m.Resolve(ctx)
	if err != nil || r.Mode != store.FileStorageS3 {
		return KindDatabase
	}
	return KindS3
}

// For returns the encrypting store of a specific kind, for reading or
// deleting an object written before the mode changed. The S3 backend uses
// the saved S3 settings even while the active mode is database.
func (m *Manager) For(ctx context.Context, kind string) (Store, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refreshLocked(ctx); err != nil {
		return nil, err
	}
	b, err := m.rawLocked(kind)
	if err != nil {
		return nil, err
	}
	return m.wrap(b)
}

// Probe builds a one-off encrypting store from an unsaved configuration and
// runs Check against it. The returned error is safe to show an admin.
func (m *Manager) Probe(ctx context.Context, mode string, cfg S3Config) error {
	var b Store
	switch mode {
	case store.FileStorageDatabase, "":
		b = NewDB(m.store)
	case store.FileStorageS3:
		if cfg.Region == "" {
			cfg.Region = DefaultRegion
		}
		s, err := NewS3(cfg)
		if err != nil {
			return err
		}
		b = s
	default:
		return fmt.Errorf("unknown storage mode %q", mode)
	}
	enc, err := m.wrap(b)
	if err != nil {
		return err
	}
	err = enc.Check(ctx)
	if err != nil && cfg.SecretAccessKey != "" && strings.Contains(err.Error(), cfg.SecretAccessKey) {
		// Defensive: no provider echoes the secret, but never let one
		// reach an API response or a log line if it did.
		return errors.New(strings.ReplaceAll(err.Error(), cfg.SecretAccessKey, "[redacted]"))
	}
	return err
}
