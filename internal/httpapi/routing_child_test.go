package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// childOnRig covers the rig's caller (h.user) by Child Safety with the Kids
// range blocking the given model references (ids or names).
func (rig *routingRig) childOnRig(t *testing.T, blocked ...string) {
	t.Helper()
	sess := adminSession(t, rig.h)
	m, ok := rigFamilyModels.Load(rig)
	if !ok {
		m = rig.h.familyModel(t, newFamilyDecider(t))
		rigFamilyModels.Store(rig, m)
	}
	profiles := strictKids()
	profiles[1]["blocked_models"] = blocked
	rig.h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.(*store.Model).ID, "profiles": profiles,
		"assignments": []map[string]string{{"scope_type": "user", "profile_id": store.ChildProfileKids, "scope_id": rig.h.user.ID}}})
}

// rigFamilyModels is each rig's Child Safety classifier (made once).
var rigFamilyModels sync.Map

func (rig *routingRig) modelID(t *testing.T, name string) string {
	t.Helper()
	all, err := rig.h.store.ListModels(context.Background(), store.ModelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Name == name {
			return m.ID
		}
	}
	t.Fatalf("no model %s", name)
	return ""
}

// A route whose model the child's age range blocks is skipped like an
// ungranted one: the request falls to the default instead of being refused.
func TestRouterSkipsAChildsBlockedRoute(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	rig.childOnRig(t, "opus")
	rec := rig.chat(t, "Write a lock-free python queue", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from frontier") {
		t.Fatalf("a blocked route must fall to the default, not refuse the child: %d %s", rec.Code, rec.Body.String())
	}
	if rig.opus.count() != 0 {
		t.Fatal("the blocked model was called")
	}
}

// A sticky conversation does not stay on a route the child is blocked from.
func TestRouterStickyRouteBlockedForChild(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	first := []map[string]string{{"role": "system", "content": "You are a paralegal."}, {"role": "user", "content": "Review this contract"}}
	if rec := rig.h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "auto/test", "messages": first}); !strings.Contains(rec.Body.String(), "onprem") {
		t.Fatalf("first turn: %s", rec.Body.String())
	}
	rig.childOnRig(t, "onprem")
	second := append(first, map[string]string{"role": "assistant", "content": "ok"}, map[string]string{"role": "user", "content": "and this other contract"})
	rec := rig.h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "auto/test", "messages": second})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from frontier") {
		t.Fatalf("a sticky turn must not stay on a blocked route: %d %s", rec.Code, rec.Body.String())
	}
}

// A blocked default does not refuse the child up front: a route they may
// use serves, and when none matches the first allowed route does. With
// every model blocked the child gets the model notice.
func TestRouterBlockedDefaultForChild(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	rig.childOnRig(t, "frontier")
	rec := rig.chat(t, "Review this contract", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from onprem") {
		t.Fatalf("a matching allowed route must serve when the default is blocked: %d %s", rec.Code, rec.Body.String())
	}
	rec = rig.chat(t, "what is the capital of France", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from onprem") {
		t.Fatalf("no match with a blocked default must use the next allowed route: %d %s", rec.Code, rec.Body.String())
	}
	if rig.frontier.count() != 0 {
		t.Fatal("the blocked default was called")
	}
	rig.childOnRig(t, "frontier", "onprem", "opus")
	rec = rig.chat(t, "Review this contract", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), familyModelNotice[:20]) {
		t.Fatalf("every model blocked must give the model notice: %d %s", rec.Code, rec.Body.String())
	}
}

// A route that serves through an alias the child's range blocks is
// refused (by name or id), however the alias's target is named.
func TestRouterRouteThroughBlockedAliasForChild(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	alias, err := rig.h.store.CreateManagedModel(ctx, "lawyer", "", rig.modelID(t, "onprem"), rig.h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := rig.router.Config
	cfg.Routes[0].ModelID = alias.ID
	if _, err := rig.h.store.UpdateRouter(ctx, rig.router.ID, store.RouterInput{Name: rig.router.Name, Status: store.RouterStatusLive, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	rig.childOnRig(t, "lawyer")
	rec := rig.chat(t, "Review this contract", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from frontier") {
		t.Fatalf("a route through a blocked alias must fall to the default: %d %s", rec.Code, rec.Body.String())
	}
	if rig.onprem.count() != 0 {
		t.Fatal("the blocked alias's model was called")
	}
}

// A blocked model is stored by id, so renaming the alias (or a model's
// display name) does not lift the block.
func TestChildBlockedModelsFollowRenames(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	onprem := rig.modelID(t, "onprem")
	alias, err := rig.h.store.CreateManagedModel(ctx, "uncensored", "", onprem, rig.h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.h.store.CreateGrant(ctx, alias.ID, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	opus := rig.modelID(t, "opus")
	if _, err := rig.h.store.CreateGrant(ctx, opus, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	if err := rig.h.store.SetModelDisplayName(ctx, opus, "big-brain"); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	rig.childOnRig(t, "uncensored", "big-brain")
	fam, err := rig.h.store.SecgwFamily(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range fam.Profiles {
		if p.ID == store.ChildProfileKids && strings.Join(p.BlockedModels, ",") != strings.Join(sortedPair(alias.ID, opus), ",") {
			t.Fatalf("blocked models must be stored by id: %v", p.BlockedModels)
		}
	}
	if err := rig.h.store.UpdateManagedModel(ctx, alias.ID, "creative", ""); err != nil {
		t.Fatal(err)
	}
	if err := rig.h.store.SetModelDisplayName(ctx, opus, "thinker"); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	for _, name := range []string{"creative", "thinker"} {
		body := map[string]any{"model": name, "messages": []map[string]string{{"role": "user", "content": "hello"}}}
		if rec := rig.h.do(http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), familyModelNotice[:20]) {
			t.Fatalf("a renamed blocked model %s must stay blocked: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

// Names saved by an earlier version resolve to ids when read, and are saved
// back as ids.
func TestChildBlockedModelNamesMigrateToIDs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	all, _ := h.store.ListModels(ctx, store.ModelFilter{})
	var id string
	for _, m := range all {
		if m.Name == "test-model" {
			id = m.ID
		}
	}
	if id == "" {
		t.Fatal("no test-model")
	}
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE child_safety_profile SET config = '{"blocked_models":["Test-Model","my/xai/grok-4.7","no-such-model"]}' WHERE id = 'cs_kids'`); err != nil {
		t.Fatal(err)
	}
	want := []string{id, "my/xai/grok-4.7", "no-such-model"}
	for pass := 0; pass < 2; pass++ {
		ps, err := h.store.ChildProfiles(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if p.ID != store.ChildProfileKids {
				continue
			}
			got := map[string]bool{}
			for _, m := range p.BlockedModels {
				got[m] = true
			}
			for _, w := range want {
				if !got[w] || len(got) != len(want) {
					t.Fatalf("pass %d: blocked models %v, want %v", pass, p.BlockedModels, want)
				}
			}
		}
	}
	var cfg string
	_ = h.store.DB().QueryRowContext(ctx, `SELECT config FROM child_safety_profile WHERE id = 'cs_kids'`).Scan(&cfg)
	if !strings.Contains(cfg, id) || strings.Contains(cfg, "Test-Model") {
		t.Fatalf("the migrated ids must be saved: %s", cfg)
	}
}

func sortedPair(a, b string) []string {
	if a < b {
		return []string{a, b}
	}
	return []string{b, a}
}

// A child's routed conversation is never replayed for evaluation.
func TestRouterEvaluationSkipsChildren(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	judge := newRoutedEngine(t, "judge")
	judge.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"judge"}]}`))
			return
		}
		_, _ = io.ReadAll(r.Body)
		judge.mu.Lock()
		judge.bodies = append(judge.bodies, map[string]any{})
		judge.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"winner\":\"A\",\"score_a\":4,\"score_b\":3,\"reason\":\"x\"}"}}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`)
	})
	jm := rig.h.addModel(t, judge, 0)
	cfg := rig.router.Config
	cfg.Routes[0].Eval = store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 10, DailyBudgetUSD: 5, JudgeModelID: jm.ID}
	if _, err := rig.h.store.UpdateRouter(ctx, rig.router.ID, store.RouterInput{Name: rig.router.Name, Status: store.RouterStatusLive, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	rig.childOnRig(t)
	if rec := rig.chat(t, "Review this contract", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "onprem") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	time.Sleep(500 * time.Millisecond)
	if n := rig.frontier.count(); n != 0 {
		t.Fatalf("a child's prompt was replayed to the default %d times", n)
	}
	if n := judge.count(); n != 0 {
		t.Fatalf("a child's prompt was sent to the judge %d times", n)
	}
	if evals, _ := rig.h.store.ListRoutingEvals(ctx, rig.router.ID, "legal", 10); len(evals) != 0 {
		t.Fatalf("an evaluation was recorded for a child: %+v", evals)
	}
}
