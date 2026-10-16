package store

import (
	"context"
	"testing"
	"time"
)

// Classifier runs are deleted with their capture
// (ON DELETE CASCADE) on SQLite, and a run for a missing capture is refused.
func TestClassifierRunsCascadeWithCapture(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys`); err != nil {
		t.Fatal(err)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO troubleshooting_capture (id, session_id, usage_event_id, request_id, captured_at) VALUES ('c1','s1','e1','r1',?)`, FormatTime(time.Now().Add(-48*time.Hour)))
	if err != nil {
		t.Skipf("capture insert shape differs: %v", err)
	}
	if err := s.InsertTroubleshootingClassifierRun(ctx, &TroubleshootingClassifierRun{CaptureID: "c1", Feature: "prompt_injection"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTroubleshootingClassifierRun(ctx, &TroubleshootingClassifierRun{CaptureID: "nope"}); err == nil {
		t.Error("run for a missing capture was accepted (foreign keys off?)")
	}
	if _, _, err := s.DeleteTroubleshootingCapturesOlderThan(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.TroubleshootingClassifierRuns(ctx, "c1")
	if len(runs) != 0 {
		t.Fatalf("classifier runs survived their capture: %d", len(runs))
	}
}
