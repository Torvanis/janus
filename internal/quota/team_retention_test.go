package quota

import (
	"context"
	"github.com/torvanis/janus/internal/store"
	"strings"
	"testing"
	"time"
)

func TestTeamQuotaCannotRegainAllowanceAfterPurge(t *testing.T) {
	s := newEngineStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	user, _, err := s.UpsertUserFromIdentity(ctx, "retention", "retention@example.test", "Retention", false, false)
	if err != nil {
		t.Fatal(err)
	}
	team, err := s.CreateTeam(ctx, "Retention team", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	q := &store.Quota{SubjectType: "team", SubjectID: team.ID, Metric: store.MetricTokensOut, Window: store.WindowMonthly, Limit: 1, BreachBehavior: store.BreachLetFinish}
	if err := s.CreateQuota(ctx, q); err != nil {
		t.Fatal(err)
	}
	start, _, err := WindowBounds(q.Window, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertUsageEvent(ctx, &store.UsageEvent{UserID: user.ID, TeamIDs: team.ID, TokensOut: 5, CreatedAt: start}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*Engine{NewEngine(s), NewEngine(s)} {
		status, err := e.StatusFor(ctx, q)
		if err != nil || status.Current != 5 {
			t.Fatalf("before purge: %+v %v", status, err)
		}
	}
	if _, err := s.PurgeUsageEvents(ctx, start.Add(time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*Engine{NewEngine(s), NewEngine(s)} {
		decision, err := e.Check(ctx, UserSubject(user.ID, []string{team.ID}), "")
		if err == nil || !strings.Contains(err.Error(), "quota") {
			t.Fatalf("purged history granted new allowance: %+v %v", decision, err)
		}
	}
}
