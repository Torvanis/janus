package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// routeDecider is a System One server that answers the routing questions
// from keywords in the text, and the guard questions benignly.
type routeDecider struct {
	*httptest.Server
	mu      sync.Mutex
	batches [][]string
	delay   time.Duration
	fail    bool
}

func newRouteDecider(t *testing.T) *routeDecider {
	f := &routeDecider{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State     string `json:"state"`
			Questions map[string]struct {
				Type     string            `json:"type"`
				Criteria map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		var ids []string
		for id := range req.Questions {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		f.mu.Lock()
		f.batches = append(f.batches, ids)
		delay, fail := f.delay, f.fail
		f.mu.Unlock()
		time.Sleep(delay)
		if fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		state := strings.ToLower(req.State)
		answers := map[string]any{}
		for id, q := range req.Questions {
			switch {
			case q.Type == "noul":
				answers[id] = map[string]any{"type": "noul", "noul": 0.01}
			case id == "content_safety":
				answers[id] = map[string]any{"type": "choice", "choice": "safe", "probabilities": map[string]float64{"safe": 1}}
			case id == "routing_category":
				choice, p := "none", 0.9
				switch {
				case strings.Contains(state, "contract"):
					choice = "legal"
				case strings.Contains(state, "maybe legal"):
					choice, p = "legal", 0.3
				case strings.Contains(state, "python"):
					choice = "coding"
				case strings.Contains(state, "capital"):
					choice = "quick_facts"
				}
				answers[id] = map[string]any{"type": "choice", "choice": choice, "probabilities": map[string]float64{choice: p, "none": 1 - p}}
			case strings.HasPrefix(id, "routing_difficulty"):
				lvl := "l2"
				if strings.Contains(state, "lock-free") {
					lvl = "l5"
				}
				answers[id] = map[string]any{"type": "choice", "choice": lvl, "probabilities": map[string]float64{lvl: 1}}
			default:
				answers[id] = map[string]any{"type": "choice", "choice": "other", "probabilities": map[string]float64{"other": 1}}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake-router", "answers": answers, "usage": map[string]any{"input_tokens": 200, "output_tokens": 2}})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *routeDecider) seen() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string{}, f.batches...)
}

func (f *routeDecider) reset() {
	f.mu.Lock()
	f.batches = nil
	f.mu.Unlock()
}

// routedEngine is an upstream that answers with its own name and records
// the bodies it received.
type routedEngine struct {
	name   string
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	// queries are the raw query strings of the chat calls received.
	queries []string
	// efforts, when set, is the only reasoning_effort values it accepts;
	// others get vLLM's 400 listing them.
	efforts []string
}

func newRoutedEngine(t *testing.T, name string) *routedEngine {
	e := &routedEngine{name: name}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = fmt.Fprintf(w, `{"data":[{"id":%q}]}`, name)
			return
		}
		var b map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		e.mu.Lock()
		e.bodies = append(e.bodies, b)
		e.queries = append(e.queries, r.URL.RawQuery)
		efforts := e.efforts
		e.mu.Unlock()
		if effort, ok := b["reasoning_effort"].(string); ok && len(efforts) > 0 && !slices.Contains(efforts, effort) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"Unexpected reasoning effort %s. Supported types are %s.","type":"BadRequestError","code":400}}`, effort, strings.Join(efforts, ", "))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"answer from %s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, name)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *routedEngine) last() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.bodies) == 0 {
		return nil
	}
	return e.bodies[len(e.bodies)-1]
}

func (e *routedEngine) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.bodies)
}

// addModel registers an engine as an enabled catalog model.
func (h *harness) addModel(t *testing.T, e *routedEngine, contextWindow int64) *store.Model {
	t.Helper()
	ctx := context.Background()
	up, err := h.store.CreateUpstream(ctx, "up-"+e.name, "openai_compatible", e.srv.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, e.name, []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	all, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: up.ID})
	m := all[0]
	if err := h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled); err != nil {
		t.Fatal(err)
	}
	if contextWindow > 0 {
		_ = h.store.SetModelContextWindow(ctx, m.ID, contextWindow)
	}
	m, _ = h.store.ModelByID(ctx, m.ID)
	return m
}

// routingRig is a router "auto/test" with:
//
//	route legal    -> onprem          (category legal)
//	route hardcode -> opus, thinking high (coding, difficulty >= 4)
//	default        -> frontier
type routingRig struct {
	h          *harness
	decider    *routeDecider
	classifier *store.Model
	onprem     *routedEngine
	opus       *routedEngine
	frontier   *routedEngine
	router     *store.Router
}

func newRoutingRig(t *testing.T, mode string) *routingRig {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()
	rig := &routingRig{h: h, decider: newRouteDecider(t),
		onprem: newRoutedEngine(t, "onprem"), opus: newRoutedEngine(t, "opus"), frontier: newRoutedEngine(t, "frontier")}
	up, err := h.store.CreateUpstream(ctx, "router-decider", "llama_cpp", rig.decider.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "fake-router", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	all, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: up.ID})
	rig.classifier = all[0]
	_ = h.store.SetModelStatus(ctx, rig.classifier.ID, store.ModelEnabled)
	if err := h.store.SetModelClassifierRole(ctx, rig.classifier.ID, store.ClassifierRoleDecision); err != nil {
		t.Fatal(err)
	}
	onprem := h.addModel(t, rig.onprem, 8000)
	opus := h.addModel(t, rig.opus, 0)
	frontier := h.addModel(t, rig.frontier, 0)
	settings := store.DefaultRoutingSettings()
	settings.ClassifierModelID = rig.classifier.ID
	if err := h.store.SetRoutingSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	r, err := h.store.CreateRouter(ctx, store.RouterInput{Name: "auto/test", Status: mode, Config: store.RouterConfig{
		Default: store.RouterDefault{ModelID: frontier.ID},
		Routes: []store.RouterRoute{
			{ID: "legal", Name: "Legal on-prem", ModelID: onprem.ID, Categories: []string{"legal"}},
			{ID: "hardcode", Name: "Hard coding", ModelID: opus.ID, Categories: []string{"coding"}, DifficultyMin: 4, Thinking: "high"},
		},
		BudgetMs: 2000, Sticky: true, Disclose: true,
	}}, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	rig.router = r
	if _, err := h.store.CreateGrant(ctx, r.ID, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	return rig
}

func (rig *routingRig) chat(t *testing.T, text string, extra map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"model": "auto/test", "messages": []map[string]string{{"role": "user", "content": text}}}
	for k, v := range extra {
		body[k] = v
	}
	return rig.h.do(http.MethodPost, "/v1/chat/completions", body)
}

func (rig *routingRig) decisions(t *testing.T, want int) []store.RoutingDecision {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ds, err := rig.h.store.ListRoutingDecisions(context.Background(), store.RoutingDecisionFilter{RouterID: rig.router.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(ds) >= want {
			return ds
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d routing decisions", want)
	return nil
}

func TestRouterRoutesByCategoryAndDifficulty(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)

	rec := rig.chat(t, "Review this contract clause for risk", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from onprem") {
		t.Fatalf("legal request must go on-prem: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerRoute); got != "Legal on-prem" {
		t.Fatalf("route header %q", got)
	}
	if got := rec.Header().Get(headerRouteReason); got != "matched:legal" {
		t.Fatalf("reason header %q", got)
	}
	ev := rig.h.requestEvent(t)
	if ev.ModelName != "onprem" || ev.RequestedModelName != "auto/test" || ev.ManagedModelID != rig.router.ID || ev.RouteReason != "matched:legal" {
		t.Fatalf("usage event: model=%s requested=%s managed=%s reason=%s", ev.ModelName, ev.RequestedModelName, ev.ManagedModelID, ev.RouteReason)
	}
	// llama.cpp decision models take one question per request (answers to
	// related questions in one request shift each other), so category and
	// difficulty are two parallel calls.
	if ev.ClassifierCalls != 2 {
		t.Fatalf("routing calls must be counted on the request: calls=%d", ev.ClassifierCalls)
	}
	if body := rig.onprem.last(); body["model"] != "onprem" {
		t.Fatalf("body model must be rewritten to the served model, got %v", body["model"])
	}

	rec = rig.chat(t, "Write a lock-free python queue", nil)
	if !strings.Contains(rec.Body.String(), "answer from opus") {
		t.Fatalf("hard coding must go to opus: %s", rec.Body.String())
	}
	if body := rig.opus.last(); body["reasoning_effort"] != "high" {
		t.Fatalf("route thinking must be applied, got %v", body["reasoning_effort"])
	}

	rec = rig.chat(t, "Write a python hello world", nil)
	if !strings.Contains(rec.Body.String(), "answer from frontier") || rec.Header().Get(headerRouteReason) != store.RouteReasonNoMatch {
		t.Fatalf("easy coding must fall to the default: %s %v", rec.Body.String(), rec.Header())
	}

	rec = rig.chat(t, "this is maybe legal", nil)
	if rec.Header().Get(headerRouteReason) != store.RouteReasonLowConfidence {
		t.Fatalf("a category below its minimum confidence must be named: %v", rec.Header())
	}

	ds := rig.decisions(t, 4)
	reasons := map[string]bool{}
	for _, d := range ds {
		reasons[d.Reason] = true
		if d.Mode != store.RouterModeLive {
			t.Fatalf("decision mode %q", d.Mode)
		}
	}
	if !reasons[store.RouteReasonMatched] || !reasons[store.RouteReasonNoMatch] || !reasons[store.RouteReasonLowConfidence] {
		t.Fatalf("decisions %v", reasons)
	}
}

// A forced thinking level the model refuses (vLLM lists the values it takes)
// is resent once at the nearest accepted level, and later requests are
// fitted up front, so routing never turns a working model into a 400.
func TestRouterThinkingRefusalIsRefitted(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	rig.opus.mu.Lock()
	rig.opus.efforts = []string{"xhigh", "medium", "low"}
	rig.opus.mu.Unlock()

	rec := rig.chat(t, "Write a lock-free python queue", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from opus") {
		t.Fatalf("a refused thinking level must be refitted, not relayed: %d %s", rec.Code, rec.Body.String())
	}
	if got := rig.opus.last()["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("high must be resent as the nearest accepted level (xhigh), got %v", got)
	}
	if n := rig.opus.count(); n != 2 {
		t.Fatalf("one refusal then one resend, got %d calls", n)
	}
	if ev := rig.h.requestEvent(t); ev.ReasoningAdjustment != "unset->xhigh (auto-routing)" {
		t.Fatalf("the record must name the level actually sent, got %q", ev.ReasoningAdjustment)
	}

	rec = rig.chat(t, "Write a lock-free python stack", nil)
	if rec.Code != http.StatusOK || rig.opus.count() != 3 || rig.opus.last()["reasoning_effort"] != "xhigh" {
		t.Fatalf("the learned levels must fit the next request up front: %d calls=%d effort=%v", rec.Code, rig.opus.count(), rig.opus.last()["reasoning_effort"])
	}
}

func TestRouterClassifierFailureAndTimeoutServeTheDefault(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	rig.decider.mu.Lock()
	rig.decider.fail = true
	rig.decider.mu.Unlock()
	rec := rig.chat(t, "Review this contract", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from frontier") || rec.Header().Get(headerRouteReason) != store.RouteReasonError {
		t.Fatalf("classifier failure must serve the default: %d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}

	rig.decider.mu.Lock()
	rig.decider.fail, rig.decider.delay = false, 600*time.Millisecond
	rig.decider.mu.Unlock()
	ctx := context.Background()
	cfg := rig.router.Config
	cfg.BudgetMs = 100
	if _, err := rig.h.store.UpdateRouter(ctx, rig.router.ID, store.RouterInput{Name: rig.router.Name, Status: store.RouterStatusLive, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	started := time.Now()
	rec = rig.chat(t, "Review this other contract", nil)
	if took := time.Since(started); took > 500*time.Millisecond {
		t.Fatalf("a slow classifier must not hold the request past its budget: %v", took)
	}
	if rec.Header().Get(headerRouteReason) != store.RouteReasonTimeout || !strings.Contains(rec.Body.String(), "answer from frontier") {
		t.Fatalf("timeout must serve the default: %v %s", rec.Header(), rec.Body.String())
	}
}

func TestRouterStickinessKeepsAConversationOnItsRoute(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	first := []map[string]string{{"role": "system", "content": "You are a paralegal."}, {"role": "user", "content": "Review this contract"}}
	rec := rig.h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "auto/test", "messages": first})
	if !strings.Contains(rec.Body.String(), "onprem") {
		t.Fatalf("first turn: %s", rec.Body.String())
	}
	rig.decider.reset()
	// Second turn would classify as quick facts, but it is the same
	// conversation: it must stay on-prem, without a classifier call.
	second := append(first, map[string]string{"role": "assistant", "content": "ok"}, map[string]string{"role": "user", "content": "what is the capital of France"})
	rec = rig.h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "auto/test", "messages": second})
	if !strings.Contains(rec.Body.String(), "onprem") || rec.Header().Get(headerRouteReason) != store.RouteReasonSticky {
		t.Fatalf("second turn must stay on its route: %s %v", rec.Body.String(), rec.Header())
	}
	if n := len(rig.decider.seen()); n != 0 {
		t.Fatalf("a sticky turn must not call the classifier, made %d calls", n)
	}
}

func TestShadowRouterServesDefaultAndRecordsWouldBe(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusShadow)
	rec := rig.chat(t, "Review this contract", nil)
	if !strings.Contains(rec.Body.String(), "answer from frontier") {
		t.Fatalf("shadow must serve the default: %s", rec.Body.String())
	}
	if rig.onprem.count() != 0 {
		t.Fatal("shadow must never send traffic to a route")
	}
	ds := rig.decisions(t, 1)
	var shadow *store.RoutingDecision
	for i := range ds {
		if ds[i].Mode == store.RouterModeShadow && ds[i].Reason == store.RouteReasonMatched {
			shadow = &ds[i]
		}
	}
	if shadow == nil || shadow.RouteID != "legal" || shadow.ModelName != "onprem" || shadow.Category != "legal" {
		t.Fatalf("shadow decision must record what would have served: %+v", ds)
	}
}

func TestRouterRidesOnTheGuardCall(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	rig.h.secgwPolicy(t, store.SecgwPolicy{Name: "guards", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckPromptInjection, Enabled: true, Mode: store.SecgwModeBlock, Fail: store.SecgwFailOpen, Direction: store.SecgwDirectionIngress,
			ClassifierModelID: rig.classifier.ID, Options: json.RawMessage(`{"timeout_ms":3000}`)},
	}})
	if err := rig.h.store.SetClassifierProfile(context.Background(), store.ClassifierProfile{UpstreamID: rig.classifier.UpstreamID, Model: rig.classifier.Name, CallStyle: store.ClassifierStyleCombined}); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	rig.decider.reset()
	rec := rig.chat(t, "Review this contract", nil)
	if !strings.Contains(rec.Body.String(), "onprem") {
		t.Fatalf("%s", rec.Body.String())
	}
	got := rig.decider.seen()
	if len(got) != 1 || strings.Join(got[0], ",") != "prompt_injection,routing_category,routing_difficulty" {
		t.Fatalf("routing questions must share the guard call, got %v", got)
	}
}

func TestRouterGrantsQuotasAndRouteRestrictions(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	// A route restricted to callers granted its model: the user holds only
	// the router grant, so the legal route is skipped.
	cfg := rig.router.Config
	cfg.Routes[0].RestrictToGranted = true
	if _, err := rig.h.store.UpdateRouter(ctx, rig.router.ID, store.RouterInput{Name: rig.router.Name, Status: store.RouterStatusLive, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	rec := rig.chat(t, "Review this contract", nil)
	if !strings.Contains(rec.Body.String(), "frontier") {
		t.Fatalf("a restricted route must be skipped for an ungranted caller: %s", rec.Body.String())
	}

	// A quota scoped to the router counts the traffic addressed to it,
	// whichever model served.
	q := &store.Quota{SubjectType: "user", SubjectID: rig.h.user.ID, ModelID: rig.router.ID, Metric: store.MetricRequests, Limit: 2,
		Window: store.WindowDaily, BreachBehavior: store.BreachLetFinish}
	if err := rig.h.store.CreateQuota(ctx, q); err != nil {
		t.Fatal(err)
	}
	rig.h.server.invalidateQuotaRules()
	rig.h.waitForUsageEvents(1)
	_ = rig.chat(t, "what is the capital of France", nil)
	time.Sleep(200 * time.Millisecond)
	rec = rig.chat(t, "what is the capital of Spain", nil)
	if rec.Code != http.StatusTooManyRequests && rec.Code != http.StatusForbidden && rec.Code != http.StatusPaymentRequired {
		t.Fatalf("a router-scoped quota must apply to the router's traffic: %d %s", rec.Code, rec.Body.String())
	}

	// Without a router grant, nothing is served.
	grants, _ := rig.h.store.ListGrants(ctx, rig.router.ID)
	for _, g := range grants {
		_ = rig.h.store.DeleteGrant(ctx, g.ID)
	}
	rig.h.server.InvalidateConfigCache()
	rec = rig.chat(t, "Review this contract", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a router needs its own grant: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRouterSecurityBindingAppliesToEveryRoute(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	p := &store.SecgwPolicy{Name: "router secrets", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeBlock, Fail: store.SecgwFailClosed, Direction: store.SecgwDirectionIngress},
	}}
	if err := rig.h.store.CreateSecgwPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := rig.h.store.CreateSecgwBinding(context.Background(), &store.SecgwBinding{PolicyID: p.ID, ScopeType: store.SecgwScopeManagedModel, ScopeID: rig.router.ID}); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	rec := rig.chat(t, "Review this contract, my key is AKIAIOSFODNN7EXAMPLE", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a policy bound to the router must apply to the routed request: %d %s", rec.Code, rec.Body.String())
	}
	if rig.onprem.count() != 0 {
		t.Fatal("blocked request reached the route's model")
	}
}

func TestRoutingAdminAPIAndTryBox(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusDraft)
	sess := adminSession(t, rig.h)
	rec := rig.h.doAsSession(sess, http.MethodGet, "/api/v1/admin/routing", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"running":true`) {
		t.Fatalf("doc: %d %s", rec.Code, rec.Body.String())
	}
	rec = rig.h.doAsSession(sess, http.MethodGet, "/api/v1/admin/routing/categories", nil)
	if !strings.Contains(rec.Body.String(), `"slug":"legal"`) || !strings.Contains(rec.Body.String(), `"used_by":["auto/test"]`) {
		t.Fatalf("categories: %s", rec.Body.String())
	}
	rec = rig.h.doAsSession(sess, http.MethodPost, "/api/v1/admin/routing/test", map[string]any{"text": "Review this contract", "router_id": rig.router.ID})
	var res routingTestResult
	decodeInto(t, rec, &res)
	if res.Status != "ok" || res.Category != "legal" || len(res.Routers) != 1 || res.Routers[0].RouteID != "legal" || res.Routers[0].ModelName != "onprem" {
		t.Fatalf("try: %s", rec.Body.String())
	}
	// A draft router is not callable.
	if rec := rig.chat(t, "Review this contract", nil); rec.Code == http.StatusOK {
		t.Fatalf("a draft router must not serve: %d", rec.Code)
	}
	// Disabling a category a router uses is refused, naming the router.
	cats, _ := rig.h.store.ListRoutingCategories(context.Background())
	var legalID string
	for _, c := range cats {
		if c.Slug == "legal" {
			legalID = c.ID
		}
	}
	rec = rig.h.doAsSession(sess, http.MethodPatch, "/api/v1/admin/routing/categories/"+legalID, map[string]any{"enabled": false})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "auto/test") {
		t.Fatalf("disable used category: %d %s", rec.Code, rec.Body.String())
	}
	// A route pointing at the default with the default thinking is refused.
	bad := rig.router.Config
	bad.Routes = append(bad.Routes, store.RouterRoute{ModelID: bad.Default.ModelID})
	rec = rig.h.doAsSession(sess, http.MethodPut, "/api/v1/admin/routing/routers/"+rig.router.ID, map[string]any{"name": "auto/test", "status": "live", "config": bad})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no-op route must be refused: %d %s", rec.Code, rec.Body.String())
	}
	// Routers are managed models: the alias endpoints refuse to repoint one.
	rec = rig.h.doAsSession(sess, http.MethodPatch, "/api/v1/admin/managed-models/"+rig.router.ID, map[string]any{"target_model_id": rig.classifier.ID})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("repointing a router via the alias API must be refused: %d", rec.Code)
	}
	// Batch test runs the built-in examples.
	rec = rig.h.doAsSession(sess, http.MethodPost, "/api/v1/admin/routing/test-batch", map[string]any{})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"categories"`) {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRouterEvaluationJudgesAgainstTheDefault(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	judge := newRoutedEngine(t, "judge")
	// The judge always prefers whichever answer mentions "onprem".
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
		reply := fmt.Sprintf(`{"winner":"%s","score_a":4,"score_b":3,"reason":"more specific"}`, winner)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": reply}}},
			"usage": map[string]any{"prompt_tokens": 50, "completion_tokens": 10}})
	})
	jm := rig.h.addModel(t, judge, 0)
	cfg := rig.router.Config
	cfg.Routes[0].Eval = store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 10, DailyBudgetUSD: 5, JudgeModelID: jm.ID}
	if _, err := rig.h.store.UpdateRouter(ctx, rig.router.ID, store.RouterInput{Name: rig.router.Name, Status: store.RouterStatusLive, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	rig.h.server.InvalidateConfigCache()
	if rec := rig.chat(t, "Review this contract", nil); !strings.Contains(rec.Body.String(), "onprem") {
		t.Fatalf("%s", rec.Body.String())
	}
	var evals []store.RoutingEvalResult
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(evals) == 0 {
		evals, _ = rig.h.store.ListRoutingEvals(ctx, rig.router.ID, "legal", 10)
		time.Sleep(50 * time.Millisecond)
	}
	if len(evals) != 1 {
		t.Fatal("no evaluation recorded")
	}
	e := evals[0]
	if e.Status != "ok" || e.Verdict != store.EvalVerdictBetter || !e.Consistent || e.DefaultModelID != rig.router.Config.Default.ModelID {
		t.Fatalf("evaluation: %+v", e)
	}
	if rig.frontier.count() != 1 || judge.count() != 2 {
		t.Fatalf("default must answer once and the judge twice (order swapped): default=%d judge=%d", rig.frontier.count(), judge.count())
	}
	// Evaluation calls are internal: hidden from the request log.
	evs := rig.h.waitForUsageEventsNoWait()
	for _, ev := range evs {
		if ev.ClientApp == store.InternalRoutingClientApp {
			t.Fatal("evaluation calls must be hidden from the request log")
		}
	}
}
