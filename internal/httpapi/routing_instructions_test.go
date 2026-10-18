package httpapi

// Router instructions: router-wide, per-route and default text added to the
// system prompt of the requests a router serves.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/routing"
	"github.com/torvanis/janus/internal/store"
)

func TestInjectInstructionsShapes(t *testing.T) {
	const ins = "Be kind."
	cases := []struct {
		name, path, body string
		want             string // JSON of the field checked
		field            func(m map[string]any) any
		applied          bool
	}{
		{"chat system string", "/v1/chat/completions",
			`{"messages":[{"role":"system","content":"You are a tutor."},{"role":"user","content":"hi"}]}`,
			`{"content":"Be kind.\n\nYou are a tutor.","role":"system"}`, firstMessage, true},
		{"chat developer string", "/v1/chat/completions",
			`{"messages":[{"role":"developer","content":"Use metric."},{"role":"user","content":"hi"}]}`,
			`{"content":"Be kind.\n\nUse metric.","role":"developer"}`, firstMessage, true},
		{"chat system parts", "/v1/chat/completions",
			`{"messages":[{"role":"system","content":[{"type":"text","text":"You are a tutor."}]},{"role":"user","content":"hi"}]}`,
			`{"content":[{"text":"Be kind.","type":"text"},{"text":"You are a tutor.","type":"text"}],"role":"system"}`, firstMessage, true},
		{"chat no system message", "/v1/chat/completions",
			`{"messages":[{"role":"user","content":"hi"}]}`,
			`[{"content":"Be kind.","role":"system"},{"content":"hi","role":"user"}]`, func(m map[string]any) any { return m["messages"] }, true},
		{"chat system not first", "/v1/chat/completions",
			`{"messages":[{"role":"user","content":"hi"},{"role":"system","content":"late"}]}`,
			`{"content":"Be kind.","role":"system"}`, firstMessage, true},
		{"anthropic string", "/v1/messages",
			`{"system":"You are a tutor.","messages":[{"role":"user","content":"hi"}]}`,
			`"Be kind.\n\nYou are a tutor."`, func(m map[string]any) any { return m["system"] }, true},
		{"anthropic blocks", "/v1/messages",
			`{"system":[{"type":"text","text":"You are a tutor.","cache_control":{"type":"ephemeral"}}],"messages":[]}`,
			`[{"text":"Be kind.","type":"text"},{"cache_control":{"type":"ephemeral"},"text":"You are a tutor.","type":"text"}]`, func(m map[string]any) any { return m["system"] }, true},
		{"anthropic absent", "/v1/messages",
			`{"messages":[{"role":"user","content":"hi"}]}`,
			`"Be kind."`, func(m map[string]any) any { return m["system"] }, true},
		{"responses present", "/v1/responses",
			`{"instructions":"Answer briefly.","input":"hi"}`,
			`"Be kind.\n\nAnswer briefly."`, func(m map[string]any) any { return m["instructions"] }, true},
		{"responses absent", "/v1/responses",
			`{"input":"hi"}`,
			`"Be kind."`, func(m map[string]any) any { return m["instructions"] }, true},
		{"completions not applied", "/v1/completions", `{"prompt":"hi"}`, "", nil, false},
		{"embeddings not applied", "/v1/embeddings", `{"input":"hi"}`, "", nil, false},
		{"chat without messages", "/v1/chat/completions", `{"model":"x"}`, "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, ok := injectInstructions(c.path, []byte(c.body), ins)
			if ok != c.applied {
				t.Fatalf("applied=%v, want %v (%s)", ok, c.applied, out)
			}
			if !ok {
				if string(out) != c.body {
					t.Fatalf("a body that is not applied must be unchanged: %s", out)
				}
				return
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(c.field(m))
			if string(got) != c.want {
				t.Fatalf("got  %s\nwant %s", got, c.want)
			}
			if n := strings.Count(string(out), ins); n != 1 {
				t.Fatalf("instructions must appear exactly once, found %d: %s", n, out)
			}
		})
	}
}

func firstMessage(m map[string]any) any {
	msgs, _ := m["messages"].([]any)
	if len(msgs) == 0 {
		return nil
	}
	return msgs[0]
}

func TestRouteInstructionsJoinRouterAndRoute(t *testing.T) {
	cfg := store.RouterConfig{Instructions: "ROUTER", Default: store.RouterDefault{Instructions: "DEFAULT"},
		Routes: []store.RouterRoute{{ID: "a", Instructions: "ROUTE A"}, {ID: "b"}}}
	def := routing.Default(cfg, 0, store.RouteReasonNoMatch, nil)
	for _, c := range []struct {
		cfg         store.RouterConfig
		d           routing.Decision
		text, label string
	}{
		{cfg, routing.Decision{RouteID: "a", Index: 0}, "ROUTER\n\nROUTE A", "router+route"},
		{cfg, routing.Decision{RouteID: "b", Index: 1}, "ROUTER", "router"},
		{cfg, def, "ROUTER\n\nDEFAULT", "router+route"},
		{store.RouterConfig{Routes: cfg.Routes}, routing.Decision{RouteID: "a", Index: 0}, "ROUTE A", "route"},
		{store.RouterConfig{Routes: cfg.Routes}, routing.Decision{RouteID: "b", Index: 1}, "", ""},
	} {
		text, label := routeInstructions(c.cfg, c.d)
		if text != c.text || label != c.label {
			t.Fatalf("%+v: got %q %q, want %q %q", c.d, text, label, c.text, c.label)
		}
	}
}

func TestRouterInstructionsAreLimitedTo8000Characters(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	update := func(mutate func(*store.RouterConfig)) (*store.Router, error) {
		cfg := rig.router.Config
		cfg.Routes = append([]store.RouterRoute(nil), cfg.Routes...)
		mutate(&cfg)
		return rig.h.store.UpdateRouter(ctx, rig.router.ID, store.RouterInput{Name: rig.router.Name, Status: store.RouterStatusLive, Config: cfg})
	}
	// 8,000 characters (multi-byte ones too) is the limit, after trimming.
	ok := "  " + strings.Repeat("é", 8000) + "\n"
	r, err := update(func(c *store.RouterConfig) { c.Instructions = ok })
	if err != nil {
		t.Fatalf("8,000 characters must be accepted: %v", err)
	}
	if r.Config.Instructions != strings.Repeat("é", 8000) {
		t.Fatal("instructions must be stored trimmed")
	}
	long := strings.Repeat("a", 8001)
	for _, c := range []struct {
		mutate func(*store.RouterConfig)
		want   string
	}{
		{func(c *store.RouterConfig) { c.Instructions = long }, "Router instructions are limited to 8,000 characters (they have 8,001)"},
		{func(c *store.RouterConfig) { c.Routes[1].Instructions = long }, "Route 2's instructions are limited to 8,000 characters (they have 8,001)"},
		{func(c *store.RouterConfig) { c.Default.Instructions = long }, "The default's instructions are limited to 8,000 characters (they have 8,001)"},
	} {
		_, err := update(c.mutate)
		var ve *store.ValidationError
		if !errors.As(err, &ve) || ve.Message != c.want {
			t.Fatalf("got %v, want %q", err, c.want)
		}
	}
}

// instructionsRig is the routing rig with router-wide, legal-route and
// default instructions, disclose on.
func instructionsRig(t *testing.T, mode string) *routingRig {
	t.Helper()
	rig := newRoutingRig(t, mode)
	cfg := rig.router.Config
	cfg.Routes = append([]store.RouterRoute(nil), cfg.Routes...)
	cfg.Instructions = "ROUTER-WIDE"
	cfg.Routes[0].Instructions = "LEGAL-ROUTE"
	cfg.Default.Instructions = "DEFAULT-ROUTE"
	r, err := rig.h.store.UpdateRouter(context.Background(), rig.router.ID, store.RouterInput{Name: rig.router.Name, Status: mode, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	rig.router = r
	rig.h.server.InvalidateConfigCache()
	return rig
}

func messagesOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body["messages"])
	var msgs []map[string]any
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatalf("messages: %v", err)
	}
	return msgs
}

// countIn counts occurrences of s anywhere in a received body.
func countIn(body map[string]any, s string) int {
	raw, _ := json.Marshal(body)
	return strings.Count(string(raw), s)
}

func TestRouterInstructionsReachTheServingModel(t *testing.T) {
	rig := instructionsRig(t, store.RouterStatusLive)
	msgs := []map[string]string{{"role": "system", "content": "You are a paralegal."}, {"role": "user", "content": "Review this contract clause"}}
	rec := rig.h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "auto/test", "messages": msgs})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from onprem") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := messagesOf(t, rig.onprem.last())
	if len(got) != 2 || got[0]["role"] != "system" || got[0]["content"] != "ROUTER-WIDE\n\nLEGAL-ROUTE\n\nYou are a paralegal." {
		t.Fatalf("router and route instructions must be prepended to the caller's system prompt: %v", got)
	}
	if h := rec.Header().Get(headerRouteInstructions); h != "router+route" {
		t.Fatalf("disclose header %q", h)
	}
	if ev := rig.h.requestEvent(t); ev.RouteReason != "matched:legal;instructions=router+route" {
		t.Fatalf("the request log must record the instructions: %q", ev.RouteReason)
	}
	// The classifier judged the caller's text only.
	rig.decider.mu.Lock()
	states := append([]string(nil), rig.decider.states...)
	rig.decider.mu.Unlock()
	if len(states) == 0 {
		t.Fatal("the classifier was never asked")
	}
	for _, st := range states {
		if strings.Contains(st, "ROUTER-WIDE") || strings.Contains(st, "LEGAL-ROUTE") {
			t.Fatalf("the classifier must read the caller's text, not the instructions: %q", st)
		}
	}

	// The default: a new system message carrying router-wide and default
	// instructions, when the caller sent none.
	rec = rig.chat(t, "hello there", nil)
	if !strings.Contains(rec.Body.String(), "answer from frontier") {
		t.Fatalf("%s", rec.Body.String())
	}
	got = messagesOf(t, rig.frontier.last())
	if len(got) != 2 || got[0]["role"] != "system" || got[0]["content"] != "ROUTER-WIDE\n\nDEFAULT-ROUTE" || got[1]["content"] != "hello there" {
		t.Fatalf("the default's instructions must be a new first system message: %v", got)
	}
}

func TestRouterInstructionsDiscloseOffHasNoHeader(t *testing.T) {
	rig := instructionsRig(t, store.RouterStatusLive)
	rig.setRoutes(t, func(c *store.RouterConfig) { c.Disclose = false })
	rec := rig.chat(t, "Review this contract", nil)
	if rec.Header().Get(headerRouteInstructions) != "" || rec.Header().Get(headerRoute) != "" {
		t.Fatalf("no route headers without disclose: %v", rec.Header())
	}
	if n := countIn(rig.onprem.last(), "LEGAL-ROUTE"); n != 1 {
		t.Fatalf("instructions apply whether or not they are disclosed: %d", n)
	}
}

func TestRouterInstructionsReappliedOnStickyTurns(t *testing.T) {
	rig := instructionsRig(t, store.RouterStatusLive)
	first := []map[string]string{{"role": "system", "content": "You are a paralegal."}, {"role": "user", "content": "Review this contract"}}
	if b := bodyOf(t, rig.turn(t, first)); !strings.Contains(b, "answer from onprem") {
		t.Fatalf("first turn: %s", b)
	}
	// The caller resends its history without the instructions.
	second := append(first, map[string]string{"role": "assistant", "content": "ok"}, map[string]string{"role": "user", "content": "what is the capital of France"})
	resp := rig.turn(t, second)
	if b := bodyOf(t, resp); !strings.Contains(b, "answer from onprem") || resp.Header.Get(headerRouteReason) != store.RouteReasonSticky {
		t.Fatalf("second turn must stay on its route: %s %v", b, resp.Header)
	}
	got := messagesOf(t, rig.onprem.last())
	if got[0]["content"] != "ROUTER-WIDE\n\nLEGAL-ROUTE\n\nYou are a paralegal." || len(got) != 4 {
		t.Fatalf("a sticky turn must carry the route's instructions again, once: %v", got)
	}
	if resp.Header.Get(headerRouteInstructions) != "router+route" {
		t.Fatalf("header %q", resp.Header.Get(headerRouteInstructions))
	}
}

func TestShadowRouterAppliesRouterAndDefaultInstructions(t *testing.T) {
	rig := instructionsRig(t, store.RouterStatusShadow)
	rec := rig.chat(t, "Review this contract", nil)
	if !strings.Contains(rec.Body.String(), "answer from frontier") {
		t.Fatalf("shadow must serve the default: %s", rec.Body.String())
	}
	got := messagesOf(t, rig.frontier.last())
	if got[0]["content"] != "ROUTER-WIDE\n\nDEFAULT-ROUTE" || countIn(rig.frontier.last(), "LEGAL-ROUTE") != 0 {
		t.Fatalf("shadow serves the default, so the router-wide and default instructions apply: %v", got)
	}
	if rec.Header().Get(headerRouteInstructions) != "router+route" {
		t.Fatalf("header %q", rec.Header().Get(headerRouteInstructions))
	}
}

// The pool-exhaustion fall back re-applies the route from the caller's
// body: the default's instructions must land once, the route's never.
func TestRouterInstructionsAppliedOnceAfterPoolFallback(t *testing.T) {
	rig := instructionsRig(t, store.RouterStatusLive)
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
		t.Fatalf("an exhausted pool route must serve the default: %d %s", rec.Code, rec.Body.String())
	}
	body := rig.frontier.last()
	if countIn(body, "ROUTER-WIDE") != 1 || countIn(body, "DEFAULT-ROUTE") != 1 || countIn(body, "LEGAL-ROUTE") != 0 {
		t.Fatalf("the default's instructions exactly once, the route's not at all: %v", body)
	}
	if ev := rig.h.requestEvent(t); ev.RouteReason != store.RouteReasonUnavailable+";instructions=router+route" {
		t.Fatalf("reason %q", ev.RouteReason)
	}
}

func TestRouteEvalReplayCarriesRouterAndDefaultInstructions(t *testing.T) {
	rig := instructionsRig(t, store.RouterStatusLive)
	_, jm := newEvalJudge(t, rig.h, 0)
	setRouteEval(t, rig, 0, store.RouteEval{Enabled: true, SamplePct: 100, MaxPerDay: 100, DailyBudgetUSD: 5, JudgeModelID: jm.ID}, nil)
	msgs := []map[string]string{{"role": "system", "content": "You are a paralegal."}, {"role": "user", "content": "Review this contract clause"}}
	rec := rig.h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "auto/test", "messages": msgs})
	if !strings.Contains(rec.Body.String(), "answer from onprem") {
		t.Fatalf("%s", rec.Body.String())
	}
	if evs := finishedEvals(t, rig, "legal", 1, 10*time.Second); len(evs) != 1 || evs[0].Status != "ok" {
		t.Fatalf("evaluation: %+v", evs)
	}
	replay := rig.frontier.last()
	if replay == nil {
		t.Fatal("the default was never asked")
	}
	got := messagesOf(t, replay)
	if got[0]["content"] != "ROUTER-WIDE\n\nDEFAULT-ROUTE\n\nYou are a paralegal." || countIn(replay, "LEGAL-ROUTE") != 0 {
		t.Fatalf("the default's replay must carry router-wide and default instructions, not the route's: %v", got)
	}
	if countIn(rig.onprem.last(), "LEGAL-ROUTE") != 1 {
		t.Fatal("the routed answer was served with the route's instructions")
	}
}

func TestRouterInstructionsOnOtherEndpoints(t *testing.T) {
	rig := instructionsRig(t, store.RouterStatusLive)
	// Completions has no system prompt: not applied, recorded as such.
	rec := rig.h.do(http.MethodPost, "/v1/completions", map[string]any{"model": "auto/test", "prompt": "hello"})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	body := rig.frontier.last()
	if body["prompt"] != "hello" || countIn(body, "ROUTER-WIDE") != 0 {
		t.Fatalf("completions must be forwarded unchanged: %v", body)
	}
	if rec.Header().Get(headerRouteInstructions) != "" {
		t.Fatalf("nothing was added, so nothing is disclosed: %q", rec.Header().Get(headerRouteInstructions))
	}
	if ev := rig.h.requestEvent(t); !strings.HasSuffix(ev.RouteReason, ";instructions=not_applied") {
		t.Fatalf("reason %q", ev.RouteReason)
	}

	// Responses: instructions prepended.
	rec = rig.h.do(http.MethodPost, "/v1/responses", map[string]any{"model": "auto/test", "instructions": "Answer briefly.", "input": "hello"})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := rig.frontier.last()["instructions"]; got != "ROUTER-WIDE\n\nDEFAULT-ROUTE\n\nAnswer briefly." {
		t.Fatalf("responses instructions: %v", got)
	}
	// Anthropic Messages: system set when absent.
	rec = rig.h.do(http.MethodPost, "/v1/messages", map[string]any{"model": "auto/test", "max_tokens": 50,
		"messages": []map[string]string{{"role": "user", "content": "hello"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := rig.frontier.last()["system"]; got != "ROUTER-WIDE\n\nDEFAULT-ROUTE" {
		t.Fatalf("anthropic system: %v", got)
	}
}

// A route on the default's model and thinking changes nothing unless it has
// its own instructions or a name (Family's "Homework learning" route labels
// its requests even on the default's model).
func TestRouteLikeTheDefaultNeedsInstructionsOrAName(t *testing.T) {
	rig := newRoutingRig(t, store.RouterStatusLive)
	ctx := context.Background()
	def := rig.router.Config.Default.ModelID
	try := func(r store.RouterRoute) error {
		cfg := rig.router.Config
		cfg.Routes = []store.RouterRoute{r}
		_, err := rig.h.store.UpdateRouter(ctx, rig.router.ID, store.RouterInput{Name: rig.router.Name, Status: store.RouterStatusLive, Config: cfg})
		return err
	}
	var ve *store.ValidationError
	if err := try(store.RouterRoute{ModelID: def, Categories: []string{"legal"}}); !errors.As(err, &ve) || !strings.Contains(ve.Message, "changes nothing") {
		t.Fatalf("an unnamed route identical to the default must be refused: %v", err)
	}
	if err := try(store.RouterRoute{ModelID: def, Categories: []string{"legal"}, Instructions: "Cite the clause."}); err != nil {
		t.Fatalf("a route that only adds instructions is a real route: %v", err)
	}
	if err := try(store.RouterRoute{Name: "Homework learning", ModelID: def, Categories: []string{"homework_learning"}}); err != nil {
		t.Fatalf("a named route labels its requests: %v", err)
	}
}
