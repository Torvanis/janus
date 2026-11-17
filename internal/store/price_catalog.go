package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Price catalog state (internal/pricecatalog), kept in system_setting so
// every replica uses the same catalog and settings:
//
//	price_catalog_settings  what an administrator chose in the UI
//	price_catalog_active    the last good downloaded catalog (bytes + signature)
//	price_catalog_status    the outcome of the last check
const (
	SettingPriceCatalogSettings = "price_catalog_settings"
	SettingPriceCatalogActive   = "price_catalog_active"
	SettingPriceCatalogStatus   = "price_catalog_status"
)

// PriceCatalogSettings is the administrator's choice. Environment variables
// (JANUS_PRICE_CATALOG_REFRESH, JANUS_PRICE_CATALOG_URL) win over it.
type PriceCatalogSettings struct {
	// RefreshEnabled turns the daily refresh on. Off until an
	// administrator turns it on.
	RefreshEnabled bool `json:"refresh_enabled"`
	// URL is where the catalog is fetched from; empty means the default.
	URL string `json:"url,omitempty"`
}

// PriceCatalogActive is the downloaded catalog in use. Body and Signature
// are the exact bytes received; they are verified again when loaded.
type PriceCatalogActive struct {
	Version   string    `json:"version"`
	Body      string    `json:"body"`
	Signature string    `json:"signature,omitempty"`
	URL       string    `json:"url"`
	ETag      string    `json:"etag,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
}

// PriceCatalogStatus is the outcome of the last check.
type PriceCatalogStatus struct {
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	// Result: "updated", "not_modified", "up_to_date" (same or older
	// version), "error".
	Result  string `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
	URL     string `json:"url,omitempty"`
	Version string `json:"version,omitempty"`
	// Last applied update.
	AppliedAt      *time.Time `json:"applied_at,omitempty"`
	AppliedVersion string     `json:"applied_version,omitempty"`
	ModelsChanged  int        `json:"models_changed,omitempty"`
	LinesChanged   int        `json:"lines_changed,omitempty"`
}

func (s *Store) readJSONSetting(ctx context.Context, key string, into any) (bool, error) {
	raw, _, err := s.systemSetting(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		return false, fmt.Errorf("decode %s setting: %w", key, err)
	}
	return true, nil
}

func (s *Store) writeJSONSetting(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.putSystemSetting(ctx, key, string(raw))
}

// PriceCatalogSettings returns the stored settings (refresh off by default).
func (s *Store) PriceCatalogSettings(ctx context.Context) (PriceCatalogSettings, error) {
	var v PriceCatalogSettings
	_, err := s.readJSONSetting(ctx, SettingPriceCatalogSettings, &v)
	return v, err
}

// SetPriceCatalogSettings stores the settings.
func (s *Store) SetPriceCatalogSettings(ctx context.Context, v PriceCatalogSettings) error {
	return s.writeJSONSetting(ctx, SettingPriceCatalogSettings, v)
}

// PriceCatalogActive returns the stored downloaded catalog, nil when none.
func (s *Store) PriceCatalogActive(ctx context.Context) (*PriceCatalogActive, error) {
	var v PriceCatalogActive
	ok, err := s.readJSONSetting(ctx, SettingPriceCatalogActive, &v)
	if err != nil || !ok {
		return nil, err
	}
	return &v, nil
}

// SetPriceCatalogActive stores a downloaded catalog.
func (s *Store) SetPriceCatalogActive(ctx context.Context, v PriceCatalogActive) error {
	return s.writeJSONSetting(ctx, SettingPriceCatalogActive, v)
}

// PriceCatalogStatus returns the last check's outcome.
func (s *Store) PriceCatalogStatus(ctx context.Context) (PriceCatalogStatus, error) {
	var v PriceCatalogStatus
	_, err := s.readJSONSetting(ctx, SettingPriceCatalogStatus, &v)
	return v, err
}

// SetPriceCatalogStatus stores the last check's outcome.
func (s *Store) SetPriceCatalogStatus(ctx context.Context, v PriceCatalogStatus) error {
	return s.writeJSONSetting(ctx, SettingPriceCatalogStatus, v)
}
