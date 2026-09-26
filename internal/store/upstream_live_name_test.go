package store

import (
	"context"
	"os"
	"testing"
)

// A deleted upstream's name must be reusable: the row is kept for usage
// history, but it no longer occupies the name. Two LIVE upstreams still may
// not share one. Before 0039 the second create failed with
// `UNIQUE constraint failed: upstream.name`.
func TestDeletedUpstreamNameCanBeReused(t *testing.T) {
	assertDeletedUpstreamNameReusable(t, newTestStore(t))
}

// Same contract on PostgreSQL, where the table rebuild in 0039 must also run
// cleanly (set JANUS_UPGRADE_TEST_DATABASE_URL to a disposable database).
func TestDeletedUpstreamNameCanBeReusedPostgres(t *testing.T) {
	dsn := os.Getenv("JANUS_UPGRADE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("JANUS_UPGRADE_TEST_DATABASE_URL not set")
	}
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	assertDeletedUpstreamNameReusable(t, s)
}

func assertDeletedUpstreamNameReusable(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	name := "vLLM on K8s " + NewID()
	first, err := s.CreateUpstream(ctx, name, "openai_compatible", "http://old-node:8000/v1", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateUpstream(ctx, name, "openai_compatible", "http://other:8000/v1", "", ""); err == nil {
		t.Fatal("two live upstreams must not share a name")
	}
	if err := s.SoftDeleteUpstream(ctx, first.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	second, err := s.CreateUpstream(ctx, name, "anthropic", "http://new-node:8000/v1", "", "")
	if err != nil {
		t.Fatalf("recreating a deleted upstream's name failed: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("recreate must be a new upstream, not the deleted row")
	}
	// The deleted row is still there for history, just hidden.
	var deleted string
	if err := s.queryRow(ctx, `SELECT deleted_at FROM upstream WHERE id = ?`, first.ID).Scan(&deleted); err != nil || deleted == "" {
		t.Fatalf("deleted row = %q (%v), want it retained with deleted_at set", deleted, err)
	}
	// Renaming a live upstream onto another live name is still refused.
	third, err := s.CreateUpstream(ctx, name+" b", "openai_compatible", "http://x:8000/v1", "", "")
	if err != nil {
		t.Fatalf("create third: %v", err)
	}
	if err := s.RenameUpstream(ctx, third.ID, name); err == nil {
		t.Fatal("rename onto a live upstream's name must fail")
	}
}
