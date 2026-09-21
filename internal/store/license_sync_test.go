package store

import (
	"context"
	"testing"
)

func TestLicenseSyncCASAndManualInvalidation(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutLicenseKey(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	state, key, err := s.LicenseSyncSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.CompareAndSwapLicenseSync(ctx, state, key, `{"enabled":true}`, nil)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	ok, err = s.CompareAndSwapLicenseSync(ctx, state, key, `{}`, nil)
	if err != nil || ok {
		t.Fatalf("duplicate lease %v %v", ok, err)
	}
	state, key, _ = s.LicenseSyncSnapshot(ctx)
	if err := s.PutLicenseKey(ctx, "manual"); err != nil {
		t.Fatal(err)
	}
	renewed := "renewed"
	ok, err = s.CompareAndSwapLicenseSync(ctx, state, key, `{}`, &renewed)
	if err != nil || ok {
		t.Fatalf("stale response %v %v", ok, err)
	}
	got, _ := s.LicenseKey(ctx)
	if got != "manual" {
		t.Fatal(got)
	}
	state, key, _ = s.LicenseSyncSnapshot(ctx)
	ok, err = s.CompareAndSwapLicenseSync(ctx, state, key, `{"success":true}`, &renewed)
	if err != nil || !ok {
		t.Fatalf("commit %v %v", ok, err)
	}
	state, key, err = s.LicenseSyncSnapshot(ctx)
	if err != nil || key != renewed || state != `{"success":true}` {
		t.Fatalf("atomic snapshot %q %q %v", state, key, err)
	}
}
