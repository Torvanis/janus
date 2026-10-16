package store

import (
	"context"
	"testing"
)

// Migration 0052 and the stale bookkeeping work on both databases (run with
// JANUS_ROLLUP_TEST_DATABASE_URL for Postgres): a decision model kept out of
// a stale sweep stays as it is, one that goes stale and comes back resumes
// its state, and a chat model back from stale waits for approval.
func TestDecisionModelStaleRoundTrip(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	up, err := s.CreateUpstream(ctx, "or", "openai_compatible", "https://openrouter.ai/api/v1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"chat", "jev"} {
		if _, err := s.UpsertDiscoveredModel(ctx, up.ID, n, []string{"chat"}); err != nil {
			t.Fatal(err)
		}
	}
	chat, _ := s.ModelByUpstreamAndName(ctx, up.ID, "chat")
	jev, _ := s.ModelByUpstreamAndName(ctx, up.ID, "jev")
	_ = s.SetModelStatus(ctx, chat.ID, ModelEnabled)
	_ = s.SetModelStatus(ctx, jev.ID, ModelEnabled)
	if err := s.SetModelClassifierRole(ctx, jev.ID, ClassifierRoleDecision); err != nil {
		t.Fatal(err)
	}
	status := func(id string) string {
		m, err := s.ModelByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return m.Status
	}
	if _, err := s.MarkStaleModelsKeeping(ctx, up.ID, []string{"chat"}, []string{ClassifierRoleDecision}); err != nil {
		t.Fatal(err)
	}
	if got := status(jev.ID); got != ModelEnabled {
		t.Fatalf("kept decision model is %q", got)
	}
	if _, err := s.MarkStaleModels(ctx, up.ID, nil); err != nil {
		t.Fatal(err)
	}
	if status(jev.ID) != ModelStale || status(chat.ID) != ModelStale {
		t.Fatal("both should be stale")
	}
	for _, n := range []string{"chat", "jev"} {
		if _, err := s.UpsertDiscoveredModel(ctx, up.ID, n, []string{"chat"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := status(jev.ID); got != ModelEnabled {
		t.Errorf("decision model back from stale is %q, want enabled", got)
	}
	if got := status(chat.ID); got != ModelPending {
		t.Errorf("chat model back from stale is %q, want pending_approval", got)
	}
}
