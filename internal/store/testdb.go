package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Test support. Opening a fresh SQLite store runs every migration, which
// under the race detector costs a couple of seconds and is paid by every
// test that wants an empty database (hundreds per package). OpenMigratedSQLite
// migrates one database per process (per distinct migration list), keeps its
// bytes, and gives each caller its own copy, so each test still gets a
// private database migrated exactly as Open would have migrated it.

var (
	migratedMu sync.Mutex
	// One template per distinct migration list: a few tests run against a
	// truncated list to exercise a migration on legacy data.
	migratedTemplates = map[string][]byte{}
)

func migrationListKey() string {
	var b strings.Builder
	for _, m := range migrations {
		b.WriteString(m.name)
		b.WriteByte(0)
	}
	return b.String()
}

// OpenMigratedSQLite opens a private SQLite database at path, seeded from a
// once-per-process migrated template. Intended for tests.
func OpenMigratedSQLite(ctx context.Context, path string) (*Store, error) {
	migratedMu.Lock()
	key := migrationListKey()
	tpl, ok := migratedTemplates[key]
	if !ok {
		var err error
		if tpl, err = buildMigratedTemplate(ctx); err != nil {
			migratedMu.Unlock()
			return nil, fmt.Errorf("migrated template: %w", err)
		}
		migratedTemplates[key] = tpl
	}
	migratedMu.Unlock()
	if err := os.WriteFile(path, tpl, 0o600); err != nil {
		return nil, err
	}
	return Open(ctx, "sqlite://"+path)
}

func buildMigratedTemplate(ctx context.Context) ([]byte, error) {
	dir, err := os.MkdirTemp("", "janus-template-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "template.db")
	s, err := Open(ctx, "sqlite://"+path)
	if err != nil {
		return nil, err
	}
	// Fold the WAL into the main file so its bytes are the whole database.
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("checkpoint template: %w", err)
	}
	if err := s.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}
