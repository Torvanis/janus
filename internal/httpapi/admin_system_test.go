package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// systemStatusBody fetches GET /api/v1/admin/system/status as the harness's
// admin user and returns the decoded document plus the raw body for
// leak assertions.
func systemStatusBody(t *testing.T, h *harness) (map[string]any, string) {
	t.Helper()
	session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
	if err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	rec := h.doAsSession(session, http.MethodGet, "/api/v1/admin/system/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("system status returned %d: %s", rec.Code, rec.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode status body: %v", err)
	}
	return doc, rec.Body.String()
}

func databaseBlock(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	db, ok := doc["database"].(map[string]any)
	if !ok {
		t.Fatalf("status document has no database object: %v", doc)
	}
	return db
}

// TestSystemStatusDatabaseSanitization locks the security contract: a postgres
// URL carrying credentials yields backend + host:port/dbname only — the
// username, password, full DSN, and query parameters never appear anywhere in
// the response body.
func TestSystemStatusDatabaseSanitization(t *testing.T) {
	h := newHarness(t)
	h.server.Config.DatabaseURL = "postgresql://dbadmin:sup3r-s3cret@db.internal.example.com:5432/janus_prod?sslmode=require"
	h.server.Config.DatabaseURLDefaulted = false

	doc, raw := systemStatusBody(t, h)
	db := databaseBlock(t, doc)

	if got := db["backend"]; got != "postgres" {
		t.Fatalf("database.backend = %v, want postgres", got)
	}
	if got := db["location"]; got != "db.internal.example.com:5432/janus_prod" {
		t.Fatalf("database.location = %v, want db.internal.example.com:5432/janus_prod", got)
	}
	if got := db["defaulted"]; got != false {
		t.Fatalf("database.defaulted = %v, want false for an explicit URL", got)
	}
	for _, leak := range []string{"dbadmin", "sup3r-s3cret", "sslmode", "postgresql://"} {
		if containsSubstring(raw, leak) {
			t.Fatalf("status body leaks %q: %s", leak, raw)
		}
	}
}

// TestSystemStatusDatabaseSQLite locks the sqlite presentation: the backend is
// named and the location is the bare file path (driver options stripped),
// with the defaulted flag surfaced so the UI can badge evaluation mode.
func TestSystemStatusDatabaseSQLite(t *testing.T) {
	h := newHarness(t)
	h.server.Config.DatabaseURL = "sqlite:///data/janus.db?cache=shared"
	h.server.Config.DatabaseURLDefaulted = true

	doc, _ := systemStatusBody(t, h)
	db := databaseBlock(t, doc)

	if got := db["backend"]; got != "sqlite" {
		t.Fatalf("database.backend = %v, want sqlite", got)
	}
	if got := db["location"]; got != "/data/janus.db" {
		t.Fatalf("database.location = %v, want /data/janus.db", got)
	}
	if got := db["defaulted"]; got != true {
		t.Fatalf("database.defaulted = %v, want true when the embedded default applied", got)
	}
}

// TestSystemStatusSanitizeDatabaseURL covers every URL form store.Open
// accepts, plus the failure modes: no input may ever echo credentials back.
func TestSystemStatusSanitizeDatabaseURL(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		wantBackend  string
		wantLocation string
	}{
		{"postgres with credentials and query", "postgres://admin:secret@db.example.com:5432/production?sslmode=require", "postgres", "db.example.com:5432/production"},
		{"postgresql scheme", "postgresql://u:p@localhost:5432/janus", "postgres", "localhost:5432/janus"},
		{"postgres without port", "postgres://u:p@dbhost/janus", "postgres", "dbhost/janus"},
		{"postgres without database", "postgres://u:p@dbhost:5432", "postgres", "dbhost:5432"},
		{"sqlite triple slash", "sqlite:///data/janus.db", "sqlite", "/data/janus.db"},
		{"sqlite with options", "sqlite:///data/janus.db?cache=shared&mode=rwc", "sqlite", "/data/janus.db"},
		{"file DSN", "file:janus.db?cache=shared", "sqlite", "janus.db"},
		{"bare path", "/var/lib/janus/janus.db", "sqlite", "/var/lib/janus/janus.db"},
		{"in-memory", ":memory:", "sqlite", ":memory:"},
		{"empty", "", "", ""},
		{"unparseable postgres never echoes the DSN", "postgres://bad:pass@[::1:5432/x", "postgres", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend, location := sanitizeDatabaseURL(tc.raw)
			if backend != tc.wantBackend || location != tc.wantLocation {
				t.Fatalf("sanitizeDatabaseURL(%q) = (%q, %q), want (%q, %q)",
					tc.raw, backend, location, tc.wantBackend, tc.wantLocation)
			}
			if containsSubstring(location, "secret") || containsSubstring(location, "pass") ||
				containsSubstring(location, "admin:") || containsSubstring(location, "u:p@") {
				t.Fatalf("sanitized location %q still carries credentials", location)
			}
		})
	}
}

// containsSubstring exists so leak assertions read as intent, not mechanics.
func containsSubstring(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
