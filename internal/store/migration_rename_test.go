package store

import (
	"context"
	"testing"
)

// A database migrated by 1.2.0 has the money widening recorded as
// 0011_bigint_money_cache_write_rates (later renumbered 0013, with 0011
// reused for admin_via_group). Migrate must treat it as 0013 — not re-run it
// (its ADD COLUMNs would fail) — and still run the new 0011.
func TestMigrateRenamesThe120MoneyMigration(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	db := s.DB()
	// Make the ledger look like 1.2.0's: the money migration under its old
	// name, and admin_via_group not yet applied (its column dropped).
	for _, q := range []string{
		`UPDATE schema_migration SET name = '0011_bigint_money_cache_write_rates' WHERE name = '0013_bigint_money_cache_write_rates'`,
		`DELETE FROM schema_migration WHERE name = '0011_admin_via_group'`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var adminViaGroup string
	for _, m := range migrations {
		if m.name == "0011_admin_via_group" {
			for _, st := range m.stmt {
				adminViaGroup += st + ";"
			}
		}
	}
	if adminViaGroup == "" {
		t.Fatal("0011_admin_via_group not found")
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE app_user DROP COLUMN admin_via_group`); err != nil {
		t.Skipf("cannot reshape the test schema (%v): %s", err, adminViaGroup)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate a 1.2.0 ledger: %v", err)
	}
	names := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_migration`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names[n] = true
	}
	if names["0011_bigint_money_cache_write_rates"] || !names["0013_bigint_money_cache_write_rates"] || !names["0011_admin_via_group"] {
		t.Fatalf("ledger after migrate: %v", names)
	}
	if _, err := db.ExecContext(ctx, `SELECT admin_via_group FROM app_user LIMIT 1`); err != nil {
		t.Fatalf("0011_admin_via_group did not run: %v", err)
	}
	// Running again is a no-op.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}
