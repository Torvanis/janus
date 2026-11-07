package store

import (
	"context"
	"errors"
)

// SettingListenerTLS holds the self-signed listener certificate
// (internal/listenertls): the certificate in PEM and its private key
// encrypted with the gateway key. Shared by every replica.
const SettingListenerTLS = "tls.listener_self_signed"

// ListenerTLS returns the stored value, or "" when none is stored.
func (s *Store) ListenerTLS(ctx context.Context) (string, error) {
	raw, _, err := s.systemSetting(ctx, SettingListenerTLS)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return raw, err
}

// SetListenerTLS stores the self-signed listener certificate.
func (s *Store) SetListenerTLS(ctx context.Context, value string) error {
	return s.putSystemSetting(ctx, SettingListenerTLS, value)
}
