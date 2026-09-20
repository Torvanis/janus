package store

import (
	"context"
	"testing"
	"time"
)

func TestSCIMRotationRevokesOriginalAtomically(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	expires := nowUTC().Add(time.Hour)
	old, secret, err := s.CreateSCIMToken(ctx, "Directory", expires)
	if err != nil {
		t.Fatal(err)
	}
	rotate, ok := any(s).(interface {
		RotateSCIMToken(context.Context, string) (*SCIMToken, string, error)
	})
	if !ok {
		t.Fatal("atomic rotation is not implemented")
	}
	next, value, err := rotate.RotateSCIMToken(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == old.ID || value == secret || next.Name != old.Name || !next.ExpiresAt.Equal(expires) {
		t.Fatalf("rotation lost identity/lifetime: %+v", next)
	}
	if _, err = s.AuthenticateSCIMToken(ctx, secret); err != ErrNotFound {
		t.Fatalf("old secret still accepted: %v", err)
	}
	if _, err = s.AuthenticateSCIMToken(ctx, value); err != nil {
		t.Fatal(err)
	}
	if _, _, err = rotate.RotateSCIMToken(ctx, old.ID); err != ErrNotFound {
		t.Fatalf("revoked token rotated again: %v", err)
	}
	tokens, err := s.ListSCIMTokens(ctx)
	if err != nil || len(tokens) != 2 {
		t.Fatalf("rotation leaked replacements: %d %v", len(tokens), err)
	}
}
