package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Request transactions.
//
// The STIG profile makes administrative changes fail closed when their audit
// record cannot be written: the change and its audit row commit together or
// not at all. The HTTP layer opens one transaction per administrative
// mutation with BeginRequestTx and carries it in the request context; every
// statement this package runs with that context (exec, query, queryRow,
// InTx, and the direct ExecContext call sites through s.runner) then runs on
// that transaction, so the handler needs no changes.
//
// Semantics are kept as close to autocommit as possible:
//   - On PostgreSQL every exec-type statement runs inside its own SAVEPOINT,
//     and so does every InTx block. A statement that fails (a unique
//     violation a handler maps to 409, say) rolls back to its savepoint
//     instead of aborting the whole transaction, exactly as it would have
//     failed alone under autocommit. SQLite already rolls back only the
//     failing statement, so it needs no savepoint per statement.
//   - Once the request ends (Finish), the context no longer routes to the
//     transaction: a goroutine the handler started with a derived context
//     falls back to the connection pool instead of using a finished
//     transaction.
//
// A request transaction is used by one goroutine at a time; handlers that
// fan work out to concurrent goroutines are excluded by the HTTP layer.

type requestTxKey struct{}

type requestTx struct {
	tx      *sql.Tx
	dialect Dialect
	done    atomic.Bool
	seq     atomic.Uint64
	mu      sync.Mutex
	post    []func()
}

// onCommit runs fn once ctx's writes are committed: at once outside a
// request transaction, after its commit inside one, never on rollback.
func onCommit(ctx context.Context, fn func()) {
	if rt := requestTxFrom(ctx); rt != nil {
		rt.mu.Lock()
		rt.post = append(rt.post, fn)
		rt.mu.Unlock()
		return
	}
	fn()
}

// ErrRequestTxDone is returned by Finish when it is called twice.
var ErrRequestTxDone = errors.New("request transaction already finished")

// BeginRequestTx opens a transaction bound to the returned context. Call
// finish(true) to commit or finish(false) to roll back; finish is safe to
// call again (later calls return ErrRequestTxDone and do nothing).
func (s *Store) BeginRequestTx(ctx context.Context) (context.Context, func(commit bool) error, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ctx, func(bool) error { return nil }, fmt.Errorf("begin request transaction: %w", err)
	}
	rt := &requestTx{tx: tx, dialect: s.dialect}
	finish := func(commit bool) error {
		if !rt.done.CompareAndSwap(false, true) {
			return ErrRequestTxDone
		}
		if commit {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit request transaction: %w", err)
			}
			rt.mu.Lock()
			post := rt.post
			rt.mu.Unlock()
			for _, fn := range post {
				fn()
			}
			return nil
		}
		return tx.Rollback()
	}
	return context.WithValue(ctx, requestTxKey{}, rt), finish, nil
}

// InRequestTx reports whether ctx carries an open request transaction.
func InRequestTx(ctx context.Context) bool { return requestTxFrom(ctx) != nil }

func requestTxFrom(ctx context.Context) *requestTx {
	if ctx == nil {
		return nil
	}
	rt, _ := ctx.Value(requestTxKey{}).(*requestTx)
	if rt == nil || rt.done.Load() {
		return nil
	}
	return rt
}

// savepoint runs fn between SAVEPOINT and RELEASE, rolling back to the
// savepoint when fn fails so the enclosing transaction stays usable.
func (rt *requestTx) savepoint(ctx context.Context, fn func() error) error {
	name := fmt.Sprintf("janus_sp_%d", rt.seq.Add(1))
	if _, err := rt.tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}
	if err := fn(); err != nil {
		_, _ = rt.tx.ExecContext(context.WithoutCancel(ctx), "ROLLBACK TO SAVEPOINT "+name)
		return err
	}
	if _, err := rt.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+name); err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	return nil
}

// ExecContext runs one statement; on PostgreSQL inside its own savepoint.
func (rt *requestTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if rt.dialect != DialectPostgres {
		return rt.tx.ExecContext(ctx, query, args...)
	}
	var res sql.Result
	err := rt.savepoint(ctx, func() error {
		var err error
		res, err = rt.tx.ExecContext(ctx, query, args...)
		return err
	})
	return res, err
}

func (rt *requestTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return rt.tx.QueryContext(ctx, query, args...)
}

func (rt *requestTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return rt.tx.QueryRowContext(ctx, query, args...)
}

// rowRunner is what every statement site needs: a connection pool, a
// store-level transaction or a request transaction.
type rowRunner interface {
	sqlRunner
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// runner picks where a statement runs: the store's own transaction (a
// tx-bound copy inside InTx), else the request transaction in ctx, else the
// pool.
func (s *Store) runner(ctx context.Context) rowRunner {
	if s.tx != nil {
		return s.tx
	}
	if rt := requestTxFrom(ctx); rt != nil {
		return rt
	}
	return s.db
}
