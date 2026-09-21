package store

import (
	"context"
	"database/sql"
)

// SettingLicenseKey holds the pasted license key (Admin → System). A key file
// on disk (JANUS_LICENSE_FILE) takes precedence over this row.
const SettingLicenseKey = "license_key"

// SettingInstanceID is the stable identifier this gateway minted at first boot.
// It is shown in Admin → System next to the key's site label and sent (opt-in)
// with update checks so a customer can tell instances apart in the portal.
const SettingInstanceID = "instance_id"

// LicenseKey returns the stored key or "" when none is set.
func (s *Store) LicenseKey(ctx context.Context) (string, error) {
	v, _, err := s.systemSetting(ctx, SettingLicenseKey)
	if err == ErrNotFound {
		return "", nil
	}
	return v, err
}

// PutLicenseKey stores a key that the license manager has already verified.
func (s *Store) PutLicenseKey(ctx context.Context, key string) error {
	return s.replaceManagedLicense(ctx, &key)
}

// DeleteLicenseKey removes the stored key.
func (s *Store) DeleteLicenseKey(ctx context.Context) error {
	return s.replaceManagedLicense(ctx, nil)
}

// InstanceID returns the stable instance identifier, minting one on first call.
func (s *Store) InstanceID(ctx context.Context) (string, error) {
	v, _, err := s.systemSetting(ctx, SettingInstanceID)
	if err == nil && v != "" {
		return v, nil
	}
	if err != nil && err != ErrNotFound {
		return "", err
	}
	id := NewID()
	if err := s.putSystemSetting(ctx, SettingInstanceID, id); err != nil {
		return "", err
	}
	// Two replicas may race on first boot; the upsert means the last writer
	// wins and both re-read the same value from here on.
	v, _, err = s.systemSetting(ctx, SettingInstanceID)
	return v, err
}

// PeekLicenseKey reads the stored key WITHOUT opening a Store or running
// migrations. It exists for the perpetual-maintenance check, which must refuse
// a too-new build before any schema change so the previous version restarts
// cleanly. Missing table (fresh database) or missing row returns "".
func PeekLicenseKey(ctx context.Context, url string) (string, error) {
	driver, dsn, dialect, err := resolveDSN(url)
	if err != nil {
		return "", err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	var v string
	q := `SELECT value FROM system_setting WHERE key = ?`
	if dialect == DialectPostgres {
		q = `SELECT value FROM system_setting WHERE key = $1`
	}
	err = db.QueryRowContext(ctx, q, SettingLicenseKey).Scan(&v)
	if err != nil {
		// No table yet, no row, or unreachable: all mean "nothing to check
		// here"; a real connectivity problem surfaces moments later in Open.
		return "", nil
	}
	return v, nil
}
