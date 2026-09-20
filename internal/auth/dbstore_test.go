package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// openTestDB returns one shared database, as replicas would share Postgres.
func openTestDB(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "auth-test.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestDBSessionStoreIsSharedAcrossReplicas(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Two store instances over one database stand in for two replicas.
	replicaA := NewDBSessionStore(db, time.Hour, time.Hour)
	replicaB := NewDBSessionStore(db, time.Hour, time.Hour)

	created, err := replicaA.Create(ctx, "user-1")
	if err != nil {
		t.Fatalf("create session on replica A: %v", err)
	}
	if created.CSRFToken == "" {
		t.Fatal("session must carry a CSRF token")
	}

	// The session issued by replica A must be visible on replica B.
	got, err := replicaB.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("read session on replica B: %v", err)
	}
	if got.UserID != "user-1" || got.CSRFToken != created.CSRFToken {
		t.Fatalf("replica B sees %+v, want the session replica A created", got)
	}

	// Sign-out-everywhere on B must end the session for A too.
	if err := replicaB.DeleteForUser(ctx, "user-1"); err != nil {
		t.Fatalf("delete sessions for user: %v", err)
	}
	if _, err := replicaA.Get(ctx, created.ID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("session survived cross-replica sign-out: err=%v", err)
	}
}

func TestDBSessionStoreEnforcesTTLAndIdleTimeout(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	s := NewDBSessionStore(db, 24*time.Hour, 4*time.Hour)
	base := time.Now().UTC()
	s.now = func() time.Time { return base }

	sess, err := s.Create(ctx, "user-2")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Inside both windows: alive, and the idle window slides.
	s.now = func() time.Time { return base.Add(3 * time.Hour) }
	if _, err := s.Get(ctx, sess.ID); err != nil {
		t.Fatalf("session should be alive at +3h: %v", err)
	}
	// +6h is fine because the idle window slid to +3h.
	s.now = func() time.Time { return base.Add(6 * time.Hour) }
	if _, err := s.Get(ctx, sess.ID); err != nil {
		t.Fatalf("session should be alive at +6h after sliding: %v", err)
	}
	// Idle timeout: no activity for >4h ends the session.
	s.now = func() time.Time { return base.Add(11 * time.Hour) }
	if _, err := s.Get(ctx, sess.ID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("idle session should be expired: err=%v", err)
	}

	// Absolute TTL: even continuous activity cannot outlive it.
	s.now = func() time.Time { return base }
	sess2, err := s.Create(ctx, "user-2")
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}
	s.now = func() time.Time { return base.Add(25 * time.Hour) }
	if _, err := s.Get(ctx, sess2.ID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("session should be dead past absolute TTL: err=%v", err)
	}
	if n, err := s.Count(ctx, "user-2"); err != nil || n != 0 {
		t.Fatalf("count after expiry = %d (%v), want 0", n, err)
	}
}

func TestDBOIDCStateStoreIsSingleUseAcrossReplicas(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	issuer := NewDBOIDCStateStore(db)
	callback := NewDBOIDCStateStore(db) // the replica the callback lands on

	now := time.Now().UTC()
	st := &AuthState{
		State: "state-1", Verifier: "verifier-1", Nonce: "nonce-1",
		RedirectTo: "/dashboard", RedirectURI: "https://janus.example.com/auth/callback",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := issuer.Save(ctx, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	got, err := callback.Consume(ctx, "state-1")
	if err != nil {
		t.Fatalf("consume on the other replica: %v", err)
	}
	if got.Verifier != "verifier-1" || got.Nonce != "nonce-1" || got.RedirectTo != "/dashboard" {
		t.Fatalf("consumed state = %+v, want the saved values", got)
	}

	// Replay must fail on every replica.
	if _, err := issuer.Consume(ctx, "state-1"); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("replayed state should be rejected: err=%v", err)
	}
}

func TestDBOIDCStateStoreRejectsExpiredState(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	ss := NewDBOIDCStateStore(db)
	now := time.Now().UTC()
	if err := ss.Save(ctx, &AuthState{
		State: "state-old", Verifier: "v", Nonce: "n",
		CreatedAt: now.Add(-20 * time.Minute), ExpiresAt: now.Add(-10 * time.Minute),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := ss.Consume(ctx, "state-old"); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("expired state should be rejected: err=%v", err)
	}
}
