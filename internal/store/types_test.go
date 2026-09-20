package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestTokenMarshalJSONRevokedAt locks the API wire contract for token
// revocation status: active tokens must serialize revoked_at as an empty
// string (falsy for JS clients), and revoked tokens as an RFC 3339 timestamp.
// Regression: the raw time.Time zero value encoded to "0001-01-01T00:00:00Z",
// a truthy string the UI badged as "revoked" on every active token.
func TestTokenMarshalJSONRevokedAt(t *testing.T) {
	active := Token{
		ID: "tok-active", UserID: "user-1", Prefix: "janus_abc123",
		Description: "laptop", CreatedAt: time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC),
	}
	raw, err := json.Marshal(active)
	if err != nil {
		t.Fatalf("marshal active token: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode active token JSON: %v", err)
	}
	if got := decoded["revoked_at"]; got != "" {
		t.Errorf("active token revoked_at = %q, want empty string", got)
	}
	// The remaining fields must survive the custom marshaller unchanged.
	if got := decoded["id"]; got != "tok-active" {
		t.Errorf("id = %q, want %q", got, "tok-active")
	}
	if got := decoded["created_at"]; got != "2025-01-02T10:00:00Z" {
		t.Errorf("created_at = %q, want 2025-01-02T10:00:00Z", got)
	}

	revokedAt := time.Date(2025, 2, 1, 12, 30, 0, 0, time.UTC)
	revoked := active
	revoked.ID = "tok-revoked"
	revoked.RevokedAt = revokedAt
	raw, err = json.Marshal(revoked)
	if err != nil {
		t.Fatalf("marshal revoked token: %v", err)
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode revoked token JSON: %v", err)
	}
	got, ok := decoded["revoked_at"].(string)
	if !ok || got == "" {
		t.Fatalf("revoked token revoked_at = %v, want RFC 3339 string", decoded["revoked_at"])
	}
	parsed, err := time.Parse(time.RFC3339Nano, got)
	if err != nil {
		t.Fatalf("revoked_at %q is not RFC 3339: %v", got, err)
	}
	if !parsed.Equal(revokedAt) {
		t.Errorf("revoked_at = %s, want %s", parsed, revokedAt)
	}
	if strings.HasPrefix(got, "0001-01-01") {
		t.Errorf("revoked_at %q is the zero time — must never leak to clients", got)
	}

	// Pointer values take the same path (handlers encode []*Token).
	raw, err = json.Marshal([]*Token{&active})
	if err != nil {
		t.Fatalf("marshal token slice: %v", err)
	}
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode token slice JSON: %v", err)
	}
	if got := list[0]["revoked_at"]; got != "" {
		t.Errorf("slice-encoded active token revoked_at = %q, want empty string", got)
	}
}
