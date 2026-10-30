package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/quota"
)

// The held-content purge (deleted people, deleted chats) runs with the
// daily retention job.
func TestPurgeRunsContentPurgeHook(t *testing.T) {
	db, metrics, logger := newPurgeFixture(t)
	hooked := false
	p := NewPurger(db, quota.NewEngine(db), metrics, logger, 365, 730, "02:00")
	p.ContentPurge = func(context.Context, time.Time) int64 { hooked = true; return 0 }
	p.RunOnce(context.Background())
	if !hooked {
		t.Error("held-content purge did not run with the retention job")
	}
}

func TestEffectiveClassificationRetention(t *testing.T) {
	for _, c := range []struct{ own, usage, want int }{
		{180, 365, 180}, {180, 14, 14}, {0, 14, 14}, {0, 0, 0}, {30, 0, 30},
	} {
		if got := EffectiveClassificationRetention(c.own, c.usage); got != c.want {
			t.Errorf("EffectiveClassificationRetention(%d,%d) = %d, want %d", c.own, c.usage, got, c.want)
		}
	}
}
