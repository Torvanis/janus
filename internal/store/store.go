// Package store owns all persistence. It targets PostgreSQL 16 in production
// and an embedded SQLite file for single-host evaluation deployments and the
// test suite, behind one dialect-portable SQL surface.
//
// Portability rules that keep one schema working on both engines:
//   - identifiers are TEXT (UUIDv4 strings), never a database-specific uuid type
//   - timestamps are TEXT in a fixed UTC layout so lexicographic order == chronological order
//   - money is an integer count of nano-USD (1e-9 USD); never a float
//   - booleans are INTEGER 0/1
//   - statements are written with ? placeholders and rewritten to $n for Postgres
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	_ "modernc.org/sqlite"             // embedded sqlite driver (pure Go, no cgo)
)

// ErrNotFound is returned by every single-row lookup when nothing matches.
var ErrNotFound = errors.New("not found")

// Dialect identifies the SQL engine behind a Store.
type Dialect string

const (
	// DialectPostgres is the production target (PostgreSQL 16, optionally with TimescaleDB).
	DialectPostgres Dialect = "postgres"
	// DialectSQLite is the embedded target used for evaluation deployments and tests.
	DialectSQLite Dialect = "sqlite"
)

// TimeLayout is the fixed UTC timestamp encoding used for every stored instant.
const TimeLayout = "2006-01-02T15:04:05.000000000Z"

// FormatTime encodes an instant for storage.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// ParseTime decodes a stored instant. Unparseable values yield the zero time so
// a single malformed row can never take down a whole listing.
func ParseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{TimeLayout, time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// Store is a handle on the gateway database.
type Store struct {
	db            *sql.DB
	tx            *sql.Tx
	modelRevision *atomic.Uint64
	dialect       Dialect
	log           *slog.Logger
}

// SetLogger directs the store's diagnostic log output (e.g. invalid model
// names dropped from usage breakdowns) to a specific logger. When unset, the
// process-wide slog default is used.
func (s *Store) SetLogger(l *slog.Logger) { s.log = l }

// logger returns the configured diagnostic logger, falling back to the
// process default so a zero-value Store never panics on a log call.
func (s *Store) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}

// Open connects to the database named by url and applies all migrations.
// Accepted forms: postgres://…, postgresql://…, sqlite:///abs/path.db, file:…,
// or a bare filesystem path.
func Open(ctx context.Context, url string) (*Store, error) {
	driver, dsn, dialect, err := resolveDSN(url)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if dialect == DialectSQLite {
		// One writer avoids SQLITE_BUSY under concurrent proxy metering.
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(50)
		db.SetMaxIdleConns(10)
		db.SetConnMaxLifetime(time.Hour)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to database (%s): %w", dialect, err)
	}
	s := &Store{db: db, dialect: dialect, modelRevision: &atomic.Uint64{}}
	if dialect == DialectSQLite {
		for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
			if _, err := db.ExecContext(ctx, pragma); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("configure sqlite (%s): %w", pragma, err)
			}
		}
	}
	if err := s.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func resolveDSN(url string) (driver, dsn string, dialect Dialect, err error) {
	switch {
	case strings.HasPrefix(url, "postgres://"), strings.HasPrefix(url, "postgresql://"):
		return "pgx", url, DialectPostgres, nil
	case strings.HasPrefix(url, "sqlite://"):
		return "sqlite", strings.TrimPrefix(url, "sqlite://"), DialectSQLite, nil
	case strings.HasPrefix(url, "file:"), strings.HasSuffix(url, ".db"), url == ":memory:":
		return "sqlite", url, DialectSQLite, nil
	default:
		return "", "", "", fmt.Errorf("unsupported JANUS_DATABASE_URL %q: expected postgres://…, sqlite:///path.db, or a *.db file path", url)
	}
}

// Dialect reports which engine is in use.
func (s *Store) Dialect() Dialect { return s.dialect }

// DB exposes the underlying handle for health checks.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the connection pool.
func (s *Store) Close() error { return s.db.Close() }

// Ping verifies connectivity for /readyz.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// rebind converts ? placeholders to the dialect's native form.
func (s *Store) rebind(query string) string {
	if s.dialect != DialectPostgres {
		return query
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteString(fmt.Sprintf("$%d", n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

func (s *Store) exec(ctx context.Context, query string, args ...any) error {
	runner := sqlRunner(s.db)
	if s.tx != nil {
		runner = s.tx
	}
	if _, err := runner.ExecContext(ctx, s.rebind(query), args...); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

func (s *Store) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	runner := sqlRunner(s.db)
	if s.tx != nil {
		runner = s.tx
	}
	rows, err := runner.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	return rows, nil
}

func (s *Store) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	if s.tx != nil {
		return s.tx.QueryRowContext(ctx, s.rebind(query), args...)
	}
	return s.db.QueryRowContext(ctx, s.rebind(query), args...)
}

// InTx runs fn inside a transaction, rolling back on any error or panic.
func (s *Store) InTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// placeholders renders "?,?,?" for an IN clause of n items.
func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toArgs[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// sqlRunner lets model transactions reuse catalog validation on the same snapshot.
type sqlRunner interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) ModelRevision() uint64 {
	if s.modelRevision == nil {
		return 0
	}
	return s.modelRevision.Load()
}
func (s *Store) modelTx(ctx context.Context, fn func(*Store) error) error {
	if s.tx != nil {
		return fn(s)
	}
	err := s.InTx(ctx, func(tx *sql.Tx) error { copy := *s; copy.tx = tx; return fn(&copy) })
	if err == nil && s.modelRevision != nil {
		s.modelRevision.Add(1)
	}
	return err
}
