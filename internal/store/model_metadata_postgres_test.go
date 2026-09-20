package store

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
)

// Set JANUS_TEST_METADATA_POSTGRES to a disposable loopback PostgreSQL server.
// Each test creates and drops its own database; no existing database is migrated.
func newMetadataTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("JANUS_TEST_METADATA_POSTGRES")
	if dsn == "" {
		return newTestStore(t)
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatal("metadata PostgreSQL tests require a loopback test server")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := "janus_metadata_" + strings.ReplaceAll(NewID(), "-", "")
	if _, err = admin.ExecContext(context.Background(), `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
			t.Errorf("drop disposable database: %v", err)
		}
	})
	u.Path = "/" + name
	s, err := Open(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
