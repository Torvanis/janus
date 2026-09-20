package store

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
)

// This opt-in upgrade check only accepts a loopback disposable database.
// Restore a pre-teams snapshot with pg_restore --no-owner --no-privileges first.
func TestTeamUpgradeSnapshot(t *testing.T) {
	dsn := os.Getenv("JANUS_UPGRADE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires an isolated pre-teams PostgreSQL snapshot")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.Path != "/janus_upgrade_check" {
		t.Fatal("only loopback janus_upgrade_check is permitted")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var beforeCount, beforeCost, teams int64
	if err = db.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(cost_nanousd),0) FROM usage_event`).Scan(&beforeCount, &beforeCost); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM team`).Scan(&teams); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var afterCount, afterCost, listed, missingSources int64
	if err = s.db.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(cost_nanousd),0) FROM usage_event`).Scan(&afterCount, &afterCost); err != nil {
		t.Fatal(err)
	}
	if afterCount != beforeCount || afterCost != beforeCost {
		t.Fatal("migration changed historical usage or cost")
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM team WHERE listed=1`).Scan(&listed); err != nil {
		t.Fatal(err)
	}
	if listed != teams {
		t.Fatalf("existing teams should be searchable by default: listed=%d teams=%d", listed, teams)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM team_member m WHERE NOT EXISTS (SELECT 1 FROM team_membership_source x WHERE x.team_id=m.team_id AND x.user_id=m.user_id AND x.source_type='manual')`).Scan(&missingSources); err != nil {
		t.Fatal(err)
	}
	if missingSources != 0 {
		t.Fatal("legacy membership was not retained as a direct source")
	}
	t.Logf("Snapshot upgraded: %d teams retained; %d historical calls and their total cost unchanged", teams, afterCount)
}
