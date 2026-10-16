package httpapi

// Regression tests for the route evaluation fixes of the release review
// (area D): Security Gateway on the out-of-band calls, the default's real
// request, atomic daily limits and the concurrency cap, per-pairing
// tallies, judge status, admin read side effects, judge-reason naming and
// truncated streamed answers.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// evalJudge is a judge engine that says A wins after a delay and records
// how many calls it received and the most it had in flight at once.
type evalJudge struct {
	*routedEngine
	calls    atomic.Int64
	inFlight atomic.Int64
	peak     atomic.Int64
}

func newEvalJudge(t *testing.T, h *harness, delay time.Duration) (*evalJudge, *store.Model) {
	t.Helper()
	j := &evalJudge{routedEngine: newRoutedEngine(t, "judge")}
	j.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"judge"}]}`))
			return
		}
		_, _ = io.ReadAll(r.Body)
		j.calls.Add(1)
		now := j.inFlight.Add(1)
		for {
			p := j.peak.Load()
			if now <= p || j.peak.CompareAndSwap(p, now) {
				break
			}
		}
		time.Sleep(delay)
		j.inFlight.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant",
			"content": `{"winner":"A","score_a":4,"score_b":3,"reason":"A is more precise."}`}}}, "usage": map[string]any{"prompt_tokens": 50, "completion_tokens": 10}})
	})
	return j, h.addModel(t, j.routedEngine, 0)
}

func setRouteEval(t *testing.T, rig *routingRig, routeIdx int, ev store.RouteEval, edit func(*store.RouterConfig)) {
	t.Helper()
	cfg := rig.router.Config
	cfg.Routes = append([]store.RouterRoute(nil), cfg.Routes...)
	cfg.Routes[routeIdx].Eval = ev
	if edit != nil {
		edit(&cfg)
	}
	r, err := rig.h.store.UpdateRouter(context.Background(), rig.router.ID, store.RouterInput{Name: rig.router.Name, Status: store.RouterStatusLive, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	rig.router = r
	rig.h.server.InvalidateConfigCache()
}

// finishedEvals waits until want evaluations of a route have finished (any
// status but running) and returns them.
func finishedEvals(t *testing.T, rig *routingRig, routeID string, want int, within time.Duration) []store.RoutingEvalResult {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		evs, err := rig.h.store.ListRoutingEvals(context.Background(), rig.router.ID, routeID, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) >= want || time.Now().After(deadline) {
			return evs
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitEvalIdle waits for the replica's evaluation backlog to drain.
func waitEvalIdle(t *testing.T, rig *routingRig, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for rig.h.server.routingRT().eval.Pending() > 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
}

// D2: a block policy bound to the default model's upstream must stop the
// replay; nothing goes to the default or the judge, and the skip is
// recorded with its reason.
func TestRouteEvalSkipsWhenPolicyBlocksTheDefault(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	judge, jm := newEvalJudge(t, rig.h, 0)
	setRouteEval(t, rig, 0, store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 100, DailyBudgetUSD: 5, JudgeModelID: jm.ID}, nil)
	def, err := rig.h.store.ModelByID(ctx, rig.router.Config.Default.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	p := &store.SecgwPolicy{Name: "no secrets to the frontier", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeBlock, Fail: store.SecgwFailClosed, Direction: store.SecgwDirectionIngress},
	}}
	if err := rig.h.store.CreateSecgwPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := rig.h.store.CreateSecgwBinding(ctx, &store.SecgwBinding{PolicyID: p.ID, ScopeType: store.SecgwScopeUpstream, ScopeID: def.UpstreamID}); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	rec := rig.chat(t, "Review this contract, the key is AKIAIOSFODNN7EXAMPLE", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from onprem") {
		t.Fatalf("the on-prem route has no policy and must serve: %d %s", rec.Code, rec.Body.String())
	}
	evs := finishedEvals(t, rig, "legal", 1, 10*time.Second)
	waitEvalIdle(t, rig, 5*time.Second)
	if n := rig.frontier.count(); n != 0 {
		t.Fatalf("the default's upstream blocks this prompt; evaluation sent it there %d time(s)", n)
	}
	if n := judge.calls.Load(); n != 0 {
		t.Fatalf("a skipped sample must not reach the judge (%d calls)", n)
	}
	if len(evs) != 1 || evs[0].Status != store.EvalStatusSkipped || !strings.Contains(evs[0].Error, "default model") {
		t.Fatalf("the skip must be recorded with its reason: %+v", evs)
	}
	sums, err := rig.h.store.RoutingEvalSummaries(ctx, time.Now().Add(-time.Hour))
	if err != nil || len(sums) != 1 || sums[0].Skipped != 1 || sums[0].Evaluated != 0 || sums[0].Failed != 0 {
		t.Fatalf("summary must count the skip on its own: %+v %v", sums, err)
	}
}

// D2: a policy bound to the judge model keeps the prompt from the judge
// (and therefore from the default too: nothing is spent on a sample that
// cannot be judged).
func TestRouteEvalSkipsWhenPolicyBlocksTheJudge(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	judge, jm := newEvalJudge(t, rig.h, 0)
	setRouteEval(t, rig, 0, store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 100, DailyBudgetUSD: 5, JudgeModelID: jm.ID}, nil)
	p := &store.SecgwPolicy{Name: "redact secrets for the judge", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeRedact, Fail: store.SecgwFailOpen, Direction: store.SecgwDirectionIngress},
	}}
	if err := rig.h.store.CreateSecgwPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := rig.h.store.CreateSecgwBinding(ctx, &store.SecgwBinding{PolicyID: p.ID, ScopeType: store.SecgwScopeModel, ScopeID: jm.ID}); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	if rec := rig.chat(t, "Review this contract, the key is AKIAIOSFODNN7EXAMPLE", nil); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	evs := finishedEvals(t, rig, "legal", 1, 10*time.Second)
	waitEvalIdle(t, rig, 5*time.Second)
	if judge.calls.Load() != 0 || rig.frontier.count() != 0 {
		t.Fatalf("a policy that would redact the judge's copy must skip the sample: judge=%d default=%d", judge.calls.Load(), rig.frontier.count())
	}
	if len(evs) != 1 || evs[0].Status != store.EvalStatusSkipped || !strings.Contains(evs[0].Error, "redact") {
		t.Fatalf("skip not recorded: %+v", evs)
	}
	// Without a secret in the prompt the same route is evaluated normally.
	if rec := rig.chat(t, "Review this contract clause", nil); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	evs = finishedEvals(t, rig, "legal", 2, 10*time.Second)
	if len(evs) != 2 || evs[0].Status != "ok" {
		t.Fatalf("a clean prompt must still be evaluated: %+v", evs)
	}
}

// D3: the default's answer is generated from the caller's request with the
// default's own thinking setting, not the route's forced level.
func TestRouteEvalReplaysTheCallersRequestToTheDefault(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	_, jm := newEvalJudge(t, rig.h, 0)
	setRouteEval(t, rig, 1, store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 100, DailyBudgetUSD: 5, JudgeModelID: jm.ID}, nil)
	if rec := rig.chat(t, "Write a lock-free python queue", map[string]any{"temperature": 0.3}); !strings.Contains(rec.Body.String(), "answer from opus") {
		t.Fatalf("%s", rec.Body.String())
	}
	finishedEvals(t, rig, "hardcode", 1, 10*time.Second)
	body := rig.frontier.last()
	if body == nil {
		t.Fatal("the default model was never asked")
	}
	if v, ok := body["reasoning_effort"]; ok {
		t.Fatalf("the default's replay must be the caller's request, not the route's (got reasoning_effort=%v)", v)
	}
	if body["temperature"] != 0.3 {
		t.Fatalf("the caller's sampling must be kept: %v", body)
	}
	// A default with its own thinking setting answers with that setting.
	setRouteEval(t, rig, 1, store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 100, DailyBudgetUSD: 5, JudgeModelID: jm.ID},
		func(c *store.RouterConfig) { c.Default.Thinking = "low" })
	if rec := rig.chat(t, "Write a lock-free python stack", nil); !strings.Contains(rec.Body.String(), "answer from opus") {
		t.Fatalf("%s", rec.Body.String())
	}
	finishedEvals(t, rig, "hardcode", 2, 10*time.Second)
	if got := rig.frontier.last()["reasoning_effort"]; got != "low" {
		t.Fatalf("the default's own thinking setting must apply to its replay, got %v", got)
	}
}

// D4: max_per_day is reserved atomically: a burst runs exactly the limit.
func TestRouteEvalDailyCountHoldsUnderBurst(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	settings, _ := rig.h.store.RoutingSettings(ctx)
	settings.EvalMaxConcurrent = 4
	if err := rig.h.store.SetRoutingSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	judge, jm := newEvalJudge(t, rig.h, 400*time.Millisecond)
	setRouteEval(t, rig, 0, store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 1, DailyBudgetUSD: 5, JudgeModelID: jm.ID}, nil)
	for i := 0; i < 4; i++ {
		if rec := rig.chat(t, fmt.Sprintf("Review this contract %d", i), nil); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	}
	waitEvalIdle(t, rig, 15*time.Second)
	evs, _ := rig.h.store.ListRoutingEvals(ctx, rig.router.ID, "legal", 50)
	if len(evs) != 1 || rig.frontier.count() != 1 || judge.calls.Load() != 2 {
		t.Fatalf("max_per_day=1 but %d evaluations ran (default calls %d, judge calls %d)", len(evs), rig.frontier.count(), judge.calls.Load())
	}
}

// D4: the daily budget is reserved at an estimate before anything is spent,
// so a budget smaller than one evaluation's estimate runs none.
func TestRouteEvalBudgetIsReservedBeforeSpending(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	judge, jm := newEvalJudge(t, rig.h, 0)
	// $10 per million tokens in and out on the default.
	ten := usage.NanoFromUSD(10)
	if err := rig.h.store.SetModelRates(ctx, rig.router.Config.Default.ModelID, store.RateCard{RateInNano: ten, RateOutNano: ten,
		EffectiveFrom: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	// The default's full-cap answer alone (2048 tokens at $10/M = $0.02)
	// is over a one-cent budget.
	setRouteEval(t, rig, 0, store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 100, DailyBudgetUSD: 0.01, JudgeModelID: jm.ID}, nil)
	for i := 0; i < 3; i++ {
		if rec := rig.chat(t, fmt.Sprintf("Review this contract %d", i), nil); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	}
	waitEvalIdle(t, rig, 10*time.Second)
	if rig.frontier.count() != 0 || judge.calls.Load() != 0 {
		t.Fatalf("budget below one evaluation's estimate must not spend: default=%d judge=%d", rig.frontier.count(), judge.calls.Load())
	}
}

// D4: eval_max_concurrent bounds evaluations in progress on the replica.
func TestRouteEvalMaxConcurrentIsEnforced(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	settings, _ := rig.h.store.RoutingSettings(ctx)
	settings.EvalMaxConcurrent = 1
	if err := rig.h.store.SetRoutingSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	judge, jm := newEvalJudge(t, rig.h, 1100*time.Millisecond)
	setRouteEval(t, rig, 0, store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 100, DailyBudgetUSD: 5, JudgeModelID: jm.ID}, nil)
	for i := 0; i < 4; i++ {
		if rec := rig.chat(t, fmt.Sprintf("Review this contract %d", i), nil); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	}
	finishedEvals(t, rig, "legal", 4, 30*time.Second)
	waitEvalIdle(t, rig, 5*time.Second)
	if p := judge.peak.Load(); p != 1 {
		t.Fatalf("eval_max_concurrent=1 but %d evaluations ran at once", p)
	}
}

// D6: tallies are split by the pairing actually used, and the API flags
// the one the route is configured with now.
func TestRouteEvalTalliesArePerPairing(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	_, jm := newEvalJudge(t, rig.h, 0)
	setRouteEval(t, rig, 0, store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 100, DailyBudgetUSD: 5, JudgeModelID: jm.ID}, nil)
	route := rig.router.Config.Routes[0]
	now := time.Now().UTC()
	for i := 0; i < 12; i++ {
		// An older pairing the route no longer uses: all losses.
		old := &store.RoutingEvalResult{RouterID: rig.router.ID, RouteID: route.ID, RoutedModelID: "retired-model", DefaultModelID: rig.router.Config.Default.ModelID,
			JudgeModelID: jm.ID, Status: "ok", Verdict: store.EvalVerdictWorse, CreatedAt: now.Add(-time.Duration(48+i) * time.Hour)}
		if err := rig.h.store.InsertRoutingEval(ctx, old); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		cur := &store.RoutingEvalResult{RouterID: rig.router.ID, RouteID: route.ID, RoutedModelID: route.ModelID, DefaultModelID: rig.router.Config.Default.ModelID,
			JudgeModelID: jm.ID, Status: "ok", Verdict: store.EvalVerdictBetter, CreatedAt: now.Add(-time.Duration(i) * time.Hour)}
		if err := rig.h.store.InsertRoutingEval(ctx, cur); err != nil {
			t.Fatal(err)
		}
	}
	sess := adminSession(t, rig.h)
	rec := rig.h.doAsSession(sess, http.MethodGet, "/api/v1/admin/routing/evaluations?days=30", nil)
	var out struct {
		Evaluations []store.RouteEvalSummary `json:"evaluations"`
	}
	decodeInto(t, rec, &out)
	if len(out.Evaluations) != 2 {
		t.Fatalf("two pairings must give two tallies, got %d: %s", len(out.Evaluations), rec.Body.String())
	}
	first, second := out.Evaluations[0], out.Evaluations[1]
	if !first.Current || first.RoutedModelID != route.ModelID || first.Evaluated != 3 || first.Worse != 0 || first.Verdict != store.EvalRouteNeedsData {
		t.Fatalf("the current pairing must come first and hold only its own verdicts: %+v", first)
	}
	if second.Current || second.RoutedModelID != "retired-model" || second.Verdict != store.EvalRouteLosing {
		t.Fatalf("the old pairing must stay separate history: %+v", second)
	}
	// The detail list narrows to one pairing.
	rec = rig.h.doAsSession(sess, http.MethodGet, fmt.Sprintf("/api/v1/admin/routing/evaluations/%s?route_id=%s&routed_model_id=%s&default_model_id=%s&judge_model_id=%s",
		rig.router.ID, route.ID, route.ModelID, rig.router.Config.Default.ModelID, jm.ID), nil)
	var detail struct {
		Results []store.RoutingEvalResult `json:"results"`
	}
	decodeInto(t, rec, &detail)
	if len(detail.Results) != 3 {
		t.Fatalf("detail must list only the current pairing: %d", len(detail.Results))
	}
}

// D8: a judge that was disabled after it was chosen is never called.
func TestRouteEvalRefusesADisabledJudge(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	judge, jm := newEvalJudge(t, rig.h, 0)
	setRouteEval(t, rig, 0, store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 100, DailyBudgetUSD: 5, JudgeModelID: jm.ID}, nil)
	if err := rig.h.store.SetModelStatus(ctx, jm.ID, store.ModelDisabled); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	if rec := rig.chat(t, "Review this contract", nil); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	evs := finishedEvals(t, rig, "legal", 1, 10*time.Second)
	waitEvalIdle(t, rig, 5*time.Second)
	if judge.calls.Load() != 0 || rig.frontier.count() != 0 {
		t.Fatalf("a disabled judge must not be called (judge=%d default=%d)", judge.calls.Load(), rig.frontier.count())
	}
	if len(evs) != 1 || evs[0].Status != "error" || !strings.Contains(evs[0].Error, "not enabled") {
		t.Fatalf("the failure must say why: %+v", evs)
	}
	// And saving a disabled judge is refused.
	cfg := rig.router.Config
	cfg.Routes[0].Eval.JudgeModelID = jm.ID
	if _, err := rig.h.store.UpdateRouter(ctx, rig.router.ID, store.RouterInput{Name: rig.router.Name, Status: store.RouterStatusLive, Config: cfg}); err == nil {
		t.Fatal("a disabled judge must be refused on save")
	}
}

// D10: reading the routing page does not flush the hot-path config cache.
func TestRoutingDocumentReadKeepsTheConfigCache(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	rig.h.server.configCache.entries.Store("sentinel", configCacheEntry{value: 1, cachedAt: time.Now()})
	sess := adminSession(t, rig.h)
	if rec := rig.h.doAsSession(sess, http.MethodGet, "/api/v1/admin/routing", nil); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, ok := rig.h.server.configCache.entries.Load("sentinel"); !ok {
		t.Fatal("GET /admin/routing flushed the whole config cache")
	}
}

// D11: the English article "A" is not a reference to answer A.
func TestNameAnswersKeepsTheArticle(t *testing.T) {
	cases := map[string]string{
		"A more complete answer is B.":                      "A more complete answer is opus.",
		"B is better. A misses the edge case.":              "opus is better. sonnet misses the edge case.",
		"Answer A is shorter, and answer B's code is wrong": "sonnet is shorter, and opus's code is wrong",
		"A/B testing aside, (B) wins":                       "A/B testing aside, (opus) wins",
		"A tie: both A and B are fine.":                     "A tie: both sonnet and opus are fine.",
		"Both are fine; a tie.":                             "Both are fine; a tie.",
	}
	for in, want := range cases {
		if got := nameAnswers(in, "sonnet", "opus"); got != want {
			t.Errorf("nameAnswers(%q) = %q, want %q", in, got, want)
		}
	}
}

// D14: a streamed answer longer than the capture is not judged as if it
// stopped mid-sentence.
func TestRouteEvalSkipsATruncatedStreamedAnswer(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	judge, jm := newEvalJudge(t, rig.h, 0)
	var mu sync.Mutex
	rig.onprem.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"onprem"}]}`))
			return
		}
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := strings.Repeat("word ", 40)
		for i := 0; i < 2000; i++ { // ~400 KB of SSE
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", chunk)
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":16000}}\n\ndata: [DONE]\n\n")
	})
	setRouteEval(t, rig, 0, store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 100, DailyBudgetUSD: 5, JudgeModelID: jm.ID}, nil)
	rec := rig.chat(t, "Review this contract", map[string]any{"stream": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String()[:200])
	}
	time.Sleep(500 * time.Millisecond)
	waitEvalIdle(t, rig, 10*time.Second)
	if judge.calls.Load() != 0 || rig.frontier.count() != 0 {
		t.Fatalf("an answer cut off by the capture must not be judged (judge=%d default=%d)", judge.calls.Load(), rig.frontier.count())
	}
}
