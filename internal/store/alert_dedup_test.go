package store

import (
	"context"
	"testing"
	"time"
)

func TestClaimAlertOncePerWindowAndRelease(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	if ok, err := s.ClaimAlert(ctx, "k", time.Hour); err != nil || !ok {
		t.Fatalf("first claim = %v, %v; want won", ok, err)
	}
	if ok, err := s.ClaimAlert(ctx, "k", time.Hour); err != nil || ok {
		t.Fatalf("second claim = %v, %v; want lost (another replica already sent it)", ok, err)
	}
	if ok, _ := s.ClaimAlert(ctx, "other", time.Hour); !ok {
		t.Fatal("a different key must be claimable")
	}
	if released, _ := s.ReleaseAlert(ctx, "k"); !released {
		t.Fatal("release of a held claim must report true")
	}
	if released, _ := s.ReleaseAlert(ctx, "k"); released {
		t.Fatal("a second release must report false (recovery is sent once)")
	}
	if ok, _ := s.ClaimAlert(ctx, "k", time.Hour); !ok {
		t.Fatal("after release the key must be claimable again")
	}
	// An expired claim can be claimed again.
	if ok, _ := s.ClaimAlert(ctx, "short", -time.Second); !ok {
		t.Fatal("claim short")
	}
	if ok, _ := s.ClaimAlert(ctx, "short", time.Hour); !ok {
		t.Fatal("an expired claim must be re-claimable")
	}
	if n, err := s.PurgeAlertClaims(ctx, time.Now().Add(2*time.Hour)); err != nil || n != 3 {
		t.Fatalf("purge = %d, %v; want 3", n, err)
	}
}
