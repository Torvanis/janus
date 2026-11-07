package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// Request transactions back the STIG profile's fail-closed audit: writes made
// with the request context commit or roll back together, a failing
// statement does not poison the rest (PostgreSQL savepoints), and InTx
// blocks nest. Runs on SQLite always and on PostgreSQL when
// JANUS_ROLLUP_TEST_DATABASE_URL names a disposable loopback database.
func TestRequestTx(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	if err := s.exec(ctx, `CREATE TABLE rtx_probe (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		var n int
		if err := s.queryRow(ctx, `SELECT COUNT(*) FROM rtx_probe`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Rollback discards every write, including those in a nested InTx.
	rctx, finish, err := s.BeginRequestTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !InRequestTx(rctx) {
		t.Fatal("context does not carry the transaction")
	}
	if err := s.exec(rctx, `INSERT INTO rtx_probe (id) VALUES ('a')`); err != nil {
		t.Fatal(err)
	}
	if err := s.InTx(rctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(rctx, s.rebind(`INSERT INTO rtx_probe (id) VALUES (?)`), "b")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAudit(rctx, &AuditEntry{Action: "probe", ResourceType: "probe"}); err != nil {
		t.Fatal(err)
	}
	if err := finish(false); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(finish(true), ErrRequestTxDone) || InRequestTx(rctx) {
		t.Fatal("finished transaction still routable")
	}
	if n := count(); n != 0 {
		t.Fatalf("rollback kept %d rows", n)
	}
	if _, n, _ := s.ListAudit(ctx, AuditFilter{Action: "probe", Limit: 10}); n != 0 {
		t.Fatalf("rollback kept %d audit rows", n)
	}
	// A context whose transaction finished falls back to the pool.
	if err := s.exec(rctx, `INSERT INTO rtx_probe (id) VALUES ('late')`); err != nil {
		t.Fatalf("write on a finished request context: %v", err)
	}

	// A failing statement (duplicate key) and a failing InTx block leave
	// the transaction usable; commit keeps everything else.
	rctx, finish, err = s.BeginRequestTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.exec(rctx, `INSERT INTO rtx_probe (id) VALUES ('c')`); err != nil {
		t.Fatal(err)
	}
	if err := s.exec(rctx, `INSERT INTO rtx_probe (id) VALUES ('c')`); err == nil {
		t.Fatal("duplicate accepted")
	}
	boom := errors.New("boom")
	if err := s.InTx(rctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(rctx, s.rebind(`INSERT INTO rtx_probe (id) VALUES (?)`), "d"); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("InTx error: %v", err)
	}
	if err := s.exec(rctx, `INSERT INTO rtx_probe (id) VALUES ('e')`); err != nil {
		t.Fatalf("transaction unusable after a failed statement: %v", err)
	}
	if err := finish(true); err != nil {
		t.Fatal(err)
	}
	var ids []string
	rows, err := s.query(ctx, `SELECT id FROM rtx_probe ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	_ = rows.Close()
	if got := len(ids); got != 3 || ids[0] != "c" || ids[1] != "e" || ids[2] != "late" {
		t.Fatalf("committed rows = %v, want [c e late] (d rolled back with its block)", ids)
	}
}
