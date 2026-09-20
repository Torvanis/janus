package config

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// decodeKey accepts a 32-byte AES key expressed as 64 hex characters, standard
// base64, or 32 raw bytes. Anything else is rejected so a truncated or mangled
// key fails at startup instead of at first upstream decrypt.
func decodeKey(raw string) ([]byte, error) {
	if len(raw) == 64 {
		if b, err := hex.DecodeString(raw); err == nil {
			return b, nil
		}
	}
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	if len(raw) == 32 {
		return []byte(raw), nil
	}
	return nil, fmt.Errorf("expected 32 bytes (64 hex characters, base64, or 32 raw characters), got %d characters", len(raw))
}
