package store

import (
	"context"
	"path/filepath"
	"testing"
)

// Measuring a model when it is enabled is on until an administrator turns
// it off, and the choice is kept.
func TestCapabilityProbeSettingDefault(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMigratedSQLite(ctx, filepath.Join(t.TempDir(), "janus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if v, err := s.CapabilityProbeSetting(ctx); err != nil || !v.OnEnable {
		t.Fatalf("default: %+v %v", v, err)
	}
	if err := s.SetCapabilityProbeSetting(ctx, CapabilityProbeSetting{OnEnable: false}); err != nil {
		t.Fatal(err)
	}
	if v, err := s.CapabilityProbeSetting(ctx); err != nil || v.OnEnable {
		t.Fatalf("after turning off: %+v %v", v, err)
	}
}
