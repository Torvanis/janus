package httpapi

// Auto-routing request path: regressions from the release review (area C).
// Each test failed on the reviewed release candidate.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/balancer"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

func (rig *routingRig) setRoutes(t *testing.T, mutate func(cfg *store.RouterConfig)) {
	t.Helper()
	cfg := rig.router.Config
	cfg.Routes = append([]store.RouterRoute(nil), cfg.Routes...)
	mutate(&cfg)
	r, err := rig.h.store.UpdateRouter(context.Background(), rig.router.ID, store.RouterInput{Name: rig.router.Name, Status: store.RouterStatusLive, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	rig.router = r
	rig.h.server.InvalidateConfigCache()
}

func (rig *routingRig) userQuota(t *testing.T, modelID string, limit int64) *store.Quota {
	t.Helper()
	q := &store.Quota{SubjectType: "user", SubjectID: rig.h.user.ID, ModelID: modelID, Metric: store.MetricRequests, Limit: limit,
		Window: store.WindowDaily, BreachBehavior: store.BreachLetFinish}
	if err := rig.h.store.CreateQuota(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	rig.h.server.invalidateQuotaRules()
	return q
}

func (rig *routingRig) turn(t *testing.T, messages []map[string]string) *http.Response {
	t.Helper()
	rec := rig.h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "auto/test", "messages": messages})
	return rec.Result()
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// A route whose target is a pool with every member out of service hands
// the request to the router's default: routing never fails a request.
func TestRouterPoolRouteWithEveryMemberOutServesTheDefault(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	h := rig.h
	second := h.addModel(t, newRoutedEngine(t, "onprem2"), 8000)
	onpremID := rig.router.Config.Routes[0].ModelID
	mm, err := h.store.CreateManagedModel(ctx, "legal-pool", "", onpremID, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := h.store.NormalizePool(ctx, store.PoolInput{Policy: store.PoolPolicyFailover, Members: []store.PoolMemberInput{{ModelID: onpremID}, {ModelID: second.ID}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetManagedModelPool(ctx, mm.ID, pool); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	for _, id := range []string{onpremID, second.ID} {
		if err := h.store.EjectPoolMember(ctx, store.PoolMemberEjection{ManagedModelID: mm.ID, ModelID: id, Until: until, Reason: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	rig.setRoutes(t, func(cfg *store.RouterConfig) { cfg.Routes[0].ModelID = mm.ID })
	h.server.InvalidateResolvedModelCache("legal-pool")

	rec := rig.chat(t, "Review this contract clause", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from frontier") {
		t.Fatalf("an exhausted pool route must serve the default, got %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerRouteReason); got != store.RouteReasonUnavailable {
		t.Fatalf("reason %q, want %q", got, store.RouteReasonUnavailable)
	}
	if got := rec.Header().Get(headerRoute); got != "default" {
		t.Fatalf("route header %q, want default", got)
	}
	if ev := rig.h.requestEvent(t); ev.ModelName != "frontier" || ev.RouteReason != store.RouteReasonUnavailable {
		t.Fatalf("event: model=%s reason=%s", ev.ModelName, ev.RouteReason)
	}
}

// A sticky turn must still fit the remembered route's context window; a
// conversation that outgrew it is decided afresh.
func TestRouterStickyTurnRechecksContextWindow(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	first := []map[string]string{{"role": "system", "content": "You are a paralegal."}, {"role": "user", "content": "Review this contract"}}
	if b := bodyOf(t, rig.turn(t, first)); !strings.Contains(b, "answer from onprem") {
		t.Fatalf("first turn: %s", b)
	}
	big := strings.Repeat("lorem ipsum dolor ", 2500) // ~45 KB, about 11k tokens > 0.9 x 8000
	second := append(first, map[string]string{"role": "assistant", "content": big}, map[string]string{"role": "user", "content": "continue"})
	resp := rig.turn(t, second)
	if b := bodyOf(t, resp); resp.StatusCode != http.StatusOK || strings.Contains(b, "answer from onprem") {
		t.Fatalf("a turn too large for the 8000-token route went there anyway (reason %q): %d", resp.Header.Get(headerRouteReason), resp.StatusCode)
	}
	if got := resp.Header.Get(headerRouteReason); got == store.RouteReasonSticky {
		t.Fatalf("reason %q: the outgrown route must not be sticky", got)
	}
}

// Revoking a caller's grant on a restricted route's model takes effect on
// the next turn of a conversation already placed there.
func TestRouterStickyTurnHonoursGrantRevocation(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	onpremID := rig.router.Config.Routes[0].ModelID
	rig.setRoutes(t, func(cfg *store.RouterConfig) { cfg.Routes[0].RestrictToGranted = true })
	g, err := rig.h.store.CreateGrant(ctx, onpremID, store.ModelKindModel, store.GranteeAllUsers, "")
	if err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	first := []map[string]string{{"role": "user", "content": "Review this contract"}}
	if b := bodyOf(t, rig.turn(t, first)); !strings.Contains(b, "answer from onprem") {
		t.Fatalf("a granted caller gets the restricted route: %s", b)
	}
	if err := rig.h.store.DeleteGrant(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	second := append(first, map[string]string{"role": "assistant", "content": "ok"}, map[string]string{"role": "user", "content": "and clause 2?"})
	resp := rig.turn(t, second)
	if b := bodyOf(t, resp); strings.Contains(b, "answer from onprem") || !strings.Contains(b, "answer from frontier") {
		t.Fatalf("the grant was revoked but the conversation still reached the restricted model (reason %q): %s", resp.Header.Get(headerRouteReason), b)
	}
}

// "Skip this route when the caller is over a quota on its model" applies
// to every turn of a conversation, not only the first.
func TestRouterStickyTurnHonoursSkipWhenOverQuota(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	rig.setRoutes(t, func(cfg *store.RouterConfig) { cfg.Routes[0].SkipWhenOverQuota = true })
	rig.userQuota(t, rig.router.Config.Routes[0].ModelID, 1)
	first := []map[string]string{{"role": "user", "content": "Review this contract"}}
	if b := bodyOf(t, rig.turn(t, first)); !strings.Contains(b, "answer from onprem") {
		t.Fatalf("first turn: %s", b)
	}
	rig.h.waitForUsageEvents(1)
	time.Sleep(150 * time.Millisecond) // the quota ledger is written after the response
	second := append(first, map[string]string{"role": "assistant", "content": "ok"}, map[string]string{"role": "user", "content": "and clause 2?"})
	resp := rig.turn(t, second)
	if b := bodyOf(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(b, "answer from frontier") {
		t.Fatalf("a route that skips itself over quota must hand a sticky turn to the default, got %d %s", resp.StatusCode, b)
	}
}

// Without "skip when over quota" the documented default holds: a quota on
// the route's model still applies to traffic it serves.
func TestRouterQuotaOnRouteModelStillAppliesByDefault(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	rig.userQuota(t, rig.router.Config.Routes[0].ModelID, 1)
	if rec := rig.chat(t, "Review this contract", nil); rec.Code != http.StatusOK {
		t.Fatalf("first: %d", rec.Code)
	}
	rig.h.waitForUsageEvents(1)
	time.Sleep(150 * time.Millisecond)
	if rec := rig.chat(t, "Review this other contract", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("documented default: the route model's quota applies, got %d", rec.Code)
	}
}

// newJudge is a judge model that prefers whichever answer came from onprem.
func newJudge(t *testing.T, rig *routingRig) (*routedEngine, *store.Model) {
	t.Helper()
	judge := newRoutedEngine(t, "judge")
	judge.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"judge"}]}`))
			return
		}
		var b struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		judge.mu.Lock()
		judge.bodies = append(judge.bodies, map[string]any{})
		judge.mu.Unlock()
		user := b.Messages[len(b.Messages)-1].Content
		winner := "B"
		if strings.Index(user, "answer from onprem") < strings.Index(user, "ANSWER B") {
			winner = "A"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant",
			"content": fmt.Sprintf(`{"winner":"%s","score_a":4,"score_b":3,"reason":"more specific"}`, winner)}}},
			"usage": map[string]any{"prompt_tokens": 50, "completion_tokens": 10}})
	})
	return judge, rig.h.addModel(t, judge, 0)
}

func waitEvals(t *testing.T, rig *routingRig, want int) []store.RoutingEvalResult {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		evals, _ := rig.h.store.ListRoutingEvals(context.Background(), rig.router.ID, "legal", 200)
		if len(evals) >= want {
			return evals
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d evaluations", want)
	return nil
}

// A sampled request refused after the route was chosen (here by a quota on
// the route's model) releases its evaluation slot. 100 of them used to
// fill the 64-slot queue for good and evaluation stopped on the replica.
func TestRouterEvaluationSurvivesManyRejectedSampledRequests(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	_, jm := newJudge(t, rig)
	rig.setRoutes(t, func(cfg *store.RouterConfig) {
		cfg.Routes[0].Eval = store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 10, DailyBudgetUSD: 5, JudgeModelID: jm.ID}
	})
	q := rig.userQuota(t, rig.router.Config.Routes[0].ModelID, 1)
	if rec := rig.chat(t, "Review this contract", nil); rec.Code != http.StatusOK {
		t.Fatalf("first: %d", rec.Code)
	}
	waitEvals(t, rig, 1)
	rig.h.waitForUsageEvents(1)
	time.Sleep(150 * time.Millisecond)
	for i := 0; i < 100; i++ {
		if rec := rig.chat(t, fmt.Sprintf("Review contract %d", i), nil); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d: want a quota refusal, got %d", i, rec.Code)
		}
	}
	if p := rig.h.server.routingRT().eval.Pending(); p != 0 {
		t.Fatalf("%d evaluation slots leaked by refused requests", p)
	}
	if err := rig.h.store.DeleteQuota(ctx, q.ID); err != nil {
		t.Fatal(err)
	}
	rig.h.server.invalidateQuotaRules()
	if rec := rig.chat(t, "Review this last contract", nil); rec.Code != http.StatusOK {
		t.Fatalf("after the quota: %d %s", rec.Code, rec.Body.String())
	}
	if evals := waitEvals(t, rig, 2); len(evals) != 2 {
		t.Fatalf("%d evaluations", len(evals))
	}
}

// The first evaluation slot test from the review: an upstream that is
// unreachable after the route was chosen releases the slot too.
func TestRouterEvaluationSlotReleasedWhenUpstreamUnreachable(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	rig.setRoutes(t, func(cfg *store.RouterConfig) {
		cfg.Routes[0].Eval = store.RouteEval{Enabled: true, SamplePct: 100}
	})
	rig.onprem.srv.Close()
	if rec := rig.chat(t, "Review this contract", nil); rec.Code == http.StatusOK {
		t.Fatalf("expected the request to fail: %d", rec.Code)
	}
	if p := rig.h.server.routingRT().eval.Pending(); p != 0 {
		t.Fatalf("evaluation slot leaked: pending %d", p)
	}
}

// When the router's configuration cannot be read, no route is chosen and
// the default serves: policies bound to the default model (and its
// upstream) must still apply, including blocking ones.
func TestRouterUnreadableConfigStillAppliesServedModelPolicies(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	p := &store.SecgwPolicy{Name: "frontier secrets", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeBlock, Fail: store.SecgwFailClosed, Direction: store.SecgwDirectionIngress},
	}}
	if err := rig.h.store.CreateSecgwPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := rig.h.store.CreateSecgwBinding(ctx, &store.SecgwBinding{PolicyID: p.ID, ScopeType: store.SecgwScopeModel, ScopeID: rig.router.Config.Default.ModelID}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.h.store.DB().ExecContext(ctx, `DELETE FROM routing_router WHERE managed_model_id = ?`, rig.router.ID); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	rec := rig.chat(t, "hello, my key is AKIAIOSFODNN7EXAMPLE", nil)
	if rec.Code != http.StatusForbidden || rig.frontier.count() != 0 {
		t.Fatalf("a blocking policy bound to the model that serves was not applied: %d %s", rec.Code, rec.Body.String())
	}
}

// The served model's second ingress pass runs only the checks its
// bindings add: a secret is recorded once, not once per pass.
func TestRouterSecondIngressPassRunsOnlyAddedChecks(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	rig.h.secgwPolicy(t, store.SecgwPolicy{Name: "org observe", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeObserve, Fail: store.SecgwFailOpen, Direction: store.SecgwDirectionIngress},
	}})
	p := &store.SecgwPolicy{Name: "onprem shape", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckShape, Enabled: true, Mode: store.SecgwModeObserve, Fail: store.SecgwFailOpen, Direction: store.SecgwDirectionIngress},
	}}
	if err := rig.h.store.CreateSecgwPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := rig.h.store.CreateSecgwBinding(ctx, &store.SecgwBinding{PolicyID: p.ID, ScopeType: store.SecgwScopeModel, ScopeID: rig.router.Config.Routes[0].ModelID}); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	rec := rig.chat(t, "Review this contract, my key is AKIAIOSFODNN7EXAMPLE", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "onprem") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if ev := rig.h.requestEvent(t); ev.SecgwViolations != 1 {
		t.Fatalf("one secret in the prompt, but the request records %d violations", ev.SecgwViolations)
	}
}

// A quota on an alias a route serves through is consumed by routed
// traffic, not only checked.
func TestRouterQuotaOnAliasServedThroughIsConsumed(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	h := rig.h
	alias, err := h.store.CreateManagedModel(ctx, "legal-alias", "", rig.router.Config.Routes[0].ModelID, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	rig.setRoutes(t, func(cfg *store.RouterConfig) { cfg.Routes[0].ModelID = alias.ID })
	rig.userQuota(t, alias.ID, 1)
	if rec := rig.chat(t, "Review this contract", nil); rec.Code != http.StatusOK {
		t.Fatalf("first: %d %s", rec.Code, rec.Body.String())
	}
	if ev := rig.h.requestEvent(t); ev.ServedManagedModelID != alias.ID {
		t.Fatalf("event must record the alias it served through, got %q", ev.ServedManagedModelID)
	}
	time.Sleep(150 * time.Millisecond)
	if rec := rig.chat(t, "Review this other contract", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a 1-request/day quota on %s let a second routed request through: %d", alias.Name, rec.Code)
	}
	// A rebuild from the event log (cache loss, reconciliation) agrees.
	if err := h.server.Quota.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := rig.chat(t, "Review a third contract", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after a ledger rebuild the alias quota no longer counts routed traffic: %d", rec.Code)
	}
}

// A caller already refused by a router-scoped quota costs no classifier
// call: the request is refused whichever route would serve.
func TestRouterOverRouterQuotaMakesNoClassifierCall(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	rig.userQuota(t, rig.router.ID, 1)
	if rec := rig.chat(t, "Review this contract", nil); rec.Code != http.StatusOK {
		t.Fatalf("first: %d", rec.Code)
	}
	rig.h.waitForUsageEvents(1)
	time.Sleep(150 * time.Millisecond)
	rig.decider.reset()
	if rec := rig.chat(t, "Review this other contract", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("router quota: %d", rec.Code)
	}
	if n := len(rig.decider.seen()); n != 0 {
		t.Fatalf("an over-quota caller cost %d classifier calls", n)
	}
}

// The resend after a refused thinking level keeps the request's query
// string (Azure-style api-version, key parameters).
func TestRouterThinkingResendKeepsQueryString(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	rig.opus.mu.Lock()
	rig.opus.efforts = []string{"xhigh", "medium", "low"}
	rig.opus.mu.Unlock()
	rec := rig.h.do(http.MethodPost, "/v1/chat/completions?api-version=2024-10-21",
		map[string]any{"model": "auto/test", "messages": []map[string]string{{"role": "user", "content": "Write a lock-free python queue"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rig.opus.mu.Lock()
	queries := append([]string(nil), rig.opus.queries...)
	rig.opus.mu.Unlock()
	if len(queries) != 2 || queries[1] != "api-version=2024-10-21" {
		t.Fatalf("the resend must keep the query string, got %q", queries)
	}
}

// A first turn (no assistant message yet) does not read the shared sticky
// table: nothing can have placed a conversation that has not started.
func TestRouterFirstTurnSkipsStickyDatabaseRead(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	if hasAssistantTurn([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)) ||
		!hasAssistantTurn([]byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"}]}`)) {
		t.Fatal("hasAssistantTurn")
	}
	// Plant a row for this exact first turn as another replica would: a
	// read would make the turn sticky to the coding route.
	first := []map[string]string{{"role": "user", "content": "Review this contract"}}
	raw, _ := json.Marshal(map[string]any{"model": "auto/test", "messages": first})
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	key := rig.router.ID + "|" + mustSessionKey(t, req.Header, raw, rig.h.user.ID)
	if err := rig.h.store.PutStickyRoute(ctx, store.StickyRoute{Key: key, RouterID: rig.router.ID, RouteID: "hardcode", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	resp := rig.turn(t, first)
	if got := resp.Header.Get(headerRouteReason); got == store.RouteReasonSticky {
		t.Fatalf("a first turn read the sticky table (reason %q)", got)
	}
}

// Learned refusal facts belong to the server and expire, so an upgraded
// model is probed again instead of being down-fitted until restart.
func TestRouterLearnedReasoningIsPerServerAndExpires(t *testing.T) {
	a, b := newRoutingRig(t, store.RouterStatusLive), newRoutingRig(t, store.RouterStatusLive)
	a.h.server.rememberRoutedReasoning("m1", subscription.Reasoning{Levels: []string{"low"}})
	if a.h.server.routedReasoning("m1") == nil {
		t.Fatal("not remembered")
	}
	if b.h.server.routedReasoning("m1") != nil {
		t.Fatal("learned facts leak across servers")
	}
	a.h.server.routingRT().reasoning.Store("m1", learnedReasoning{facts: subscription.Reasoning{Levels: []string{"low"}}, learned: time.Now().Add(-learnedReasoningTTL - time.Minute)})
	if a.h.server.routedReasoning("m1") != nil {
		t.Fatal("an expired fact is still used")
	}
}

// Vertex is an OpenAI-compatible (Gemini) surface: a forced level is sent
// as reasoning_effort. Bedrock's Converse translation has no thinking
// field, so the body is left untouched and nothing is claimed.
func TestApplyThinkingDialectForVertexAndBedrock(t *testing.T) {
	in := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"temperature":0.2}`)
	out, _, ok := applyThinking(in, "high", "vertex")
	if !ok {
		t.Fatal("not applied")
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["reasoning_effort"] != "high" || m["thinking"] != nil || m["temperature"] != 0.2 || m["max_tokens"] != float64(100) {
		t.Fatalf("vertex body: %s", out)
	}

	out, _, ok = applyThinking(in, "high", "bedrock")
	if ok || string(out) != string(in) {
		t.Fatalf("bedrock: a level Converse cannot carry must not be applied (ok=%v): %s", ok, out)
	}
	a, _ := adapter.Get("bedrock")
	prep, err := a.Prepare(adapter.Upstream{BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com", APIKey: "AKIDEXAMPLE:fakesecret"}, &adapter.Request{Body: out, Model: "anthropic.claude"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prep.Body), `"maxTokens":100`) || !strings.Contains(string(prep.Body), `"temperature":0.2`) {
		t.Fatalf("bedrock keeps the caller's max_tokens and temperature: %s", prep.Body)
	}
}

func mustSessionKey(t *testing.T, h http.Header, body []byte, caller string) string {
	t.Helper()
	key, _ := balancer.SessionKey(h, body, caller)
	if key == "" {
		t.Fatal("no session key")
	}
	return key
}
