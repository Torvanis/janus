package httpapi

import (
	"context"
	"github.com/torvanis/janus/internal/store"
	"net/http"
	"testing"
)

func TestTeamCalendarQuotaRetentionValidation(t *testing.T) {
	h := newHarness(t)
	team, err := h.store.CreateTeam(context.Background(), "Short retention", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateTeam(context.Background(), team.ID, team.Name, h.user.ID, true); err != nil {
		t.Fatal(err)
	}
	h.server.Config.UsageRetentionDays = 7
	session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"/api/v1/admin/quotas", "/api/v1/lead/teams/" + team.ID + "/quotas"} {
		body := map[string]any{"subject_type": "team", "subject_id": team.ID, "metric": "tokens_out", "limit_value": 100, "window": "monthly"}
		resp := h.doAsSession(session, http.MethodPost, prefix, body)
		status := resp.Code
		if status != 400 {
			t.Fatalf("%s allowed monthly team quota with 7-day retention: %d", prefix, status)
		}
	}
	// User calendar quota retains durable accounting and remains supported.
	resp := h.doAsSession(session, http.MethodPost, "/api/v1/admin/quotas", map[string]any{"subject_type": "user", "subject_id": h.user.ID, "metric": "tokens_out", "limit_value": 100, "window": "monthly"})
	status := resp.Code
	if status != 201 {
		t.Fatalf("user calendar quota regressed: %d", status)
	}
}

func TestTeamCalendarQuotaRetentionEditValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Previously supported", h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	q := &store.Quota{SubjectType: "team", SubjectID: team.ID, Metric: store.MetricTokensOut, Limit: 100, Window: store.WindowMonthly, BreachBehavior: store.BreachLetFinish}
	if err := h.store.CreateQuota(ctx, q); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateTeam(context.Background(), team.ID, team.Name, h.user.ID, true); err != nil {
		t.Fatal(err)
	}
	h.server.Config.UsageRetentionDays = 7
	session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"/api/v1/admin/quotas/" + q.ID, "/api/v1/lead/teams/" + team.ID + "/quotas/" + q.ID} {
		resp := h.doAsSession(session, http.MethodPut, prefix, map[string]any{"limit_value": 200})
		status := resp.Code
		if status != 400 {
			t.Fatalf("%s allowed unsupported existing window: %d", prefix, status)
		}
	}
}
