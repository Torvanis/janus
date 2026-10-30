package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// seedNamedPool is seedPool with a distinct catalog name per member, so a
// rule can name one member: members[i] is served by engine names[i] and
// listed as model models[i]. Failover order follows the list.
func seedNamedPool(t *testing.T, h *harness, names, models []string) (*store.ManagedModel, []*fakeEngine, []*store.Model) {
	t.Helper()
	ctx := context.Background()
	var engines []*fakeEngine
	var rows []*store.Model
	var members []store.PoolMemberInput
	for i, n := range names {
		f := newFakeEngine(t, n)
		up, err := h.store.CreateUpstream(ctx, "engine-"+n, "vlm", f.srv.URL+"/v1", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, models[i], []string{"chat"}); err != nil {
			t.Fatal(err)
		}
		all, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: up.ID})
		if err := h.store.SetModelStatus(ctx, all[0].ID, store.ModelEnabled); err != nil {
			t.Fatal(err)
		}
		engines = append(engines, f)
		rows = append(rows, all[0])
		members = append(members, store.PoolMemberInput{ModelID: all[0].ID, Priority: i})
	}
	mm, err := h.store.CreateManagedModel(ctx, "qwen-pool", "", members[0].ModelID, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := h.store.NormalizePool(ctx, store.PoolInput{Policy: store.PoolPolicyFailover, Affinity: store.PoolAffinityOff, Members: members}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetManagedModelPool(ctx, mm.ID, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateGrant(ctx, mm.ID, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	h.server.InvalidateResolvedModelCache(mm.Name)
	mm, _ = h.store.ManagedModelByID(ctx, mm.ID)
	return mm, engines, rows
}

// A retry onto another pool member must not bypass a Security Gateway
// policy bound to that member: the request with a secret never reaches it.
func TestPoolRetrySkipsMemberWithStricterSecgwBinding(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, engines, rows := seedNamedPool(t, h, []string{"a", "b"}, []string{"qwen", "qwen"})
	p := &store.SecgwPolicy{Name: "strict b", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeBlock, Fail: store.SecgwFailClosed, Direction: store.SecgwDirectionIngress},
	}}
	if err := h.store.CreateSecgwPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateSecgwBinding(ctx, &store.SecgwBinding{PolicyID: p.ID, ScopeType: store.SecgwScopeModel, ScopeID: rows[1].ID}); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	engines[0].down.Store(true)
	rec := h.chatPool(t, "my key is "+testAWSKey, nil)
	if rec.Code == http.StatusOK || engines[1].count() != 0 {
		t.Fatalf("secret forwarded to the member whose policy blocks secrets: %d %s (b served %d)", rec.Code, rec.Body.String(), engines[1].count())
	}
	// b's checks differ from the ones this body passed, so b is skipped:
	// the request fails the way an exhausted pool does.
	if rec.Code != http.StatusBadGateway && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want the exhausted-pool failure, got %d %s", rec.Code, rec.Body.String())
	}
	// A member whose checks match the first's still takes the retry.
	engines[0].down.Store(false)
	h2 := newHarness(t)
	_, engines2, _ := seedNamedPool(t, h2, []string{"a", "b"}, []string{"qwen", "qwen"})
	engines2[0].down.Store(true)
	if rec := h2.chatPool(t, "hello", nil); rec.Code != http.StatusOK || servedBy(rec) != "b" {
		t.Fatalf("plain retry broke: %d %s", rec.Code, rec.Body.String())
	}
}

// A retry never lands on a member the caller's quota on it has used up.
func TestPoolRetrySkipsMemberOverQuota(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, engines, rows := seedNamedPool(t, h, []string{"a", "b"}, []string{"qwen", "qwen"})
	q := &store.Quota{SubjectType: "user", SubjectID: h.user.ID, ModelID: rows[1].ID, Metric: store.MetricRequests, Limit: 0, Window: store.WindowDaily, BreachBehavior: store.BreachLetFinish}
	if err := h.store.CreateQuota(ctx, q); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	h.server.invalidateQuotaRules()
	engines[0].down.Store(true)
	rec := h.chatPool(t, "hello", nil)
	if rec.Code == http.StatusOK || engines[1].count() != 0 {
		t.Fatalf("retry served by a member over quota: %d %s (b served %d)", rec.Code, rec.Body.String(), engines[1].count())
	}
}

// A retry never lands on a member the covered child's age range blocks.
func TestPoolRetrySkipsMemberChildSafetyBlocks(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	_, engines, _ := seedNamedPool(t, h, []string{"a", "b"}, []string{"qwen", "qwen-b"})
	m := h.familyModel(t, newFamilyDecider(t))
	profiles := strictKids()
	profiles[1]["blocked_models"] = []string{"qwen-b"}
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": profiles,
		"assignments": []map[string]string{{"scope_type": "user", "profile_id": store.ChildProfileKids, "scope_id": h.user.ID}}})
	engines[0].down.Store(true)
	rec := h.chatPool(t, "hello", nil)
	if rec.Code == http.StatusOK || engines[1].count() != 0 {
		t.Fatalf("retry served by a member Child Safety blocks: %d %s (b served %d)", rec.Code, rec.Body.String(), engines[1].count())
	}
}

// A pool member that became a classifier after joining never serves chat,
// and an alias whose target became one is refused at runtime.
func TestClassifierNeverServesThroughAliasOrPool(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, engines, rows := seedNamedPool(t, h, []string{"a", "b"}, []string{"qwen", "qwen"})
	// The store now refuses this; older data may still hold it.
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE model SET classifier_role = ? WHERE id = ?`, store.ClassifierRoleGenerativeGuard, rows[1].ID); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	h.server.InvalidateResolvedModelCache("qwen-pool")
	engines[0].down.Store(true)
	rec := h.chatPool(t, "hello", nil)
	if rec.Code == http.StatusOK || engines[1].count() != 0 {
		t.Fatalf("a guard classifier served chat as a pool member: %d %s", rec.Code, rec.Body.String())
	}
	engines[0].down.Store(false)
	for i := 0; i < 4; i++ {
		if rec := h.chatPool(t, fmt.Sprint("q", i), nil); rec.Code != http.StatusOK || servedBy(rec) != "a" {
			t.Fatalf("healthy member: %d %s", rec.Code, rec.Body.String())
		}
	}

	// Alias straight at a (now) classifier.
	mm, err := h.store.CreateManagedModel(ctx, "innocent", "", rows[0].ID, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateGrant(ctx, mm.ID, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE model SET classifier_role = ? WHERE id = ?`, store.ClassifierRoleTextClassification, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	h.server.InvalidateResolvedModelCache("innocent")
	rec = h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "innocent", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "classifier") {
		t.Fatalf("alias on a classifier served chat: %d %s", rec.Code, rec.Body.String())
	}
}

// A pool of decision models called on the chat path is refused with the
// decision message, not reported as out of service.
func TestDecisionPoolOnChatPathSaysWhy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, _, rows := seedNamedPool(t, h, []string{"a", "b"}, []string{"qwen", "qwen"})
	for _, m := range rows {
		if _, err := h.store.DB().ExecContext(ctx, `UPDATE model SET classifier_role = ? WHERE id = ?`, store.ClassifierRoleDecision, m.ID); err != nil {
			t.Fatal(err)
		}
	}
	h.server.InvalidateConfigCache()
	h.server.InvalidateResolvedModelCache("qwen-pool")
	rec := h.chatPool(t, "hello", nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "decision model") {
		t.Fatalf("decision pool on chat: %d %s", rec.Code, rec.Body.String())
	}
}
