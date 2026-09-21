package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const settingLicenseSync = "license_sync"

// LicenseSyncSnapshot reads the credential/configuration and installed key in
// one statement. The JSON is private engine state, never an API projection.
func (s *Store) LicenseSyncSnapshot(ctx context.Context) (state, key string, err error) {
	err = s.exec(ctx, `INSERT INTO system_setting (key,value,updated_at) VALUES (?,?,?) ON CONFLICT (key) DO NOTHING`, settingLicenseSync, "{}", FormatTime(time.Now()))
	if err != nil {
		return
	}
	err = s.queryRow(ctx, `SELECT a.value, COALESCE(b.value,'') FROM system_setting a LEFT JOIN system_setting b ON b.key = ? WHERE a.key = ?`, SettingLicenseKey, settingLicenseSync).Scan(&state, &key)
	return
}

var errLicenseSyncConflict = errors.New("license sync conflict")

// CompareAndSwapLicenseSync atomically commits a lease/observation and optional
// verified renewal, only if both config and key still match. Updating the sync
// row first serializes all manual installs and replica completions on Postgres.
func (s *Store) CompareAndSwapLicenseSync(ctx context.Context, expected, key, next string, renewed *string) (bool, error) {
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, s.rebind(`UPDATE system_setting SET value=?, updated_at=? WHERE key=? AND value=?`), next, FormatTime(time.Now()), settingLicenseSync, expected)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errLicenseSyncConflict
		}
		var actual string
		err = tx.QueryRowContext(ctx, s.rebind(`SELECT value FROM system_setting WHERE key=?`), SettingLicenseKey).Scan(&actual)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if actual != key {
			return errLicenseSyncConflict
		}
		if renewed != nil {
			_, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO system_setting (key,value,updated_at) VALUES (?,?,?) ON CONFLICT (key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`), SettingLicenseKey, *renewed, FormatTime(time.Now()))
			return err
		}
		return nil
	})
	if errors.Is(err, errLicenseSyncConflict) {
		return false, nil
	}
	return err == nil, err
}

// replaceManagedLicense invalidates credentials, leases and observations even
// for same-string reinstall (no ABA), in the same transaction as manual change.
func (s *Store) replaceManagedLicense(ctx context.Context, key *string) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		reset, _ := json.Marshal(map[string]string{"generation": NewID()})
		_, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO system_setting (key,value,updated_at) VALUES (?,?,?) ON CONFLICT (key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`), settingLicenseSync, string(reset), FormatTime(time.Now()))
		if err != nil {
			return err
		}
		if key == nil {
			_, err = tx.ExecContext(ctx, s.rebind(`DELETE FROM system_setting WHERE key=?`), SettingLicenseKey)
		} else {
			_, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO system_setting (key,value,updated_at) VALUES (?,?,?) ON CONFLICT (key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`), SettingLicenseKey, *key, FormatTime(time.Now()))
		}
		return err
	})
}
