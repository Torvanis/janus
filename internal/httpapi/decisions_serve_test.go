package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// dialectUpstream is a fake decision server speaking one dialect. It records
// the bodies it received and answers like the real providers did on
// 2026-10-08 (OpenAI gpt-6-luna; OpenRouter Jev with usage.cost).
type dialectUpstream struct {
	*httptest.Server
	dialect string
	mu      sync.Mutex
	paths   []string
	bodies  []map[string]any
}

func newDialectUpstream(t *testing.T, dialect string) *dialectUpstream {
	u := &dialectUpstream{dialect: dialect}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		u.bodies = append(u.bodies, body)
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer up-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			return
		}
		if dialect == "openai" {
			qs, _ := body["questions"].([]any)
			answers := []map[string]any{}
			for _, q := range qs {
				qm := q.(map[string]any)
				switch qm["type"] {
				case "predicate":
					answers = append(answers, map[string]any{"type": "predicate", "name": qm["name"], "probability": 0.25})
				case "choice":
					first := qm["choices"].([]any)[0].(map[string]any)["value"]
					answers = append(answers, map[string]any{"type": "choice", "name": qm["name"], "choice": first, "confidence": 0.9,
						"probabilities": []map[string]any{{"value": first, "probability": 0.9}}})
				case "score":
					answers = append(answers, map[string]any{"type": "score", "name": qm["name"], "score": 0.9, "confidence": 0.8,
						"probabilities": []map[string]any{{"value": 0, "label": "low", "probability": 0.1}, {"value": 1, "label": "high", "probability": 0.9}}})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "gpt-6-luna", "answers": answers,
				"usage": map[string]any{"input_tokens": 1000, "output_tokens": 0, "input_tokens_details": map[string]any{"cached_tokens": 0}}})
			return
		}
		qs, _ := body["questions"].(map[string]any)
		answers := map[string]any{}
		for id, q := range qs {
			qm := q.(map[string]any)
			switch qm["type"] {
			case "noul":
				p := 0.67
				if id == "prompt_injection" {
					p = 0.01 // a clean request, as far as the guard is concerned
				}
				answers[id] = map[string]any{"type": "noul", "noul": p}
			case "choice":
				var first string
				for k := range qm["criteria"].(map[string]any) {
					if first == "" || k < first {
						first = k
					}
				}
				answers[id] = map[string]any{"type": "choice", "choice": first, "probabilities": map[string]float64{first: 1}, "confidence": 1}
			case "score":
				answers[id] = map[string]any{"type": "score", "score": 1, "probabilities": map[string]float64{"0": 0, "1": 1}, "confidence": 0.9}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "typesafe/jev-1.13", "answers": answers,
			"usage": map[string]any{"input_tokens": 300, "output_tokens": 40, "cost": 0.0000126}, "provider": "TypeSafe"})
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *dialectUpstream) last() (string, map[string]any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		return "", nil
	}
	return u.paths[len(u.paths)-1], u.bodies[len(u.bodies)-1]
}

// decisionModel registers a decision catalog model on a fake upstream,
// priced and granted to everyone.
func (h *harness) decisionModel(t *testing.T, u *dialectUpstream, upName, model string, inUSD float64, dialect string) *store.Model {
	t.Helper()
	ctx := context.Background()
	key, _ := h.server.Cipher.Encrypt("up-key")
	up, err := h.store.CreateUpstream(ctx, upName, "openai_compatible", u.URL, key, "up-k…")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, model, []string{"decision"}); err != nil {
		t.Fatal(err)
	}
	m, err := h.store.ModelByUpstreamAndName(ctx, up.ID, model)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelRates(ctx, m.ID, store.RateCard{RateInNano: usage.NanoFromUSD(inUSD), EffectiveFrom: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_ = h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled)
	if err := h.store.SetModelClassifierRole(ctx, m.ID, store.ClassifierRoleDecision); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetClassifierProfile(ctx, store.ClassifierProfile{UpstreamID: up.ID, Model: model, Dialect: dialect}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateGrant(ctx, m.ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	m, _ = h.store.ModelByID(ctx, m.ID)
	return m
}

const s1Body = `{"model":"%s","state":"I was charged twice.","questions":{
 "department":{"type":"choice","instructions":"Which department?","criteria":{"billing":"Payments","technical":"Bugs"}},
 "urgent":{"type":"noul","instructions":"Is this urgent?","criteria":{"true":"Explicitly time-sensitive","false":"No urgency"}}}}`

const oaiBody = `{"model":"%s","input":"I was charged twice.","questions":[
 {"type":"choice","name":"department","instructions":"Which department?","choices":[{"value":"billing","description":"Payments"},{"value":"technical","description":"Bugs"}]},
 {"type":"predicate","name":"urgent","instructions":"Is this urgent?"},
 {"type":"score","name":"anger","instructions":"How angry?","levels":[{"label":"low"},{"label":"high"}]}]}`

func sprintf(f, a string) string { return strings.Replace(f, "%s", a, 1) }

func TestServedDecisionsAllFourDirections(t *testing.T) {
	h := newHarness(t)
	oai := newDialectUpstream(t, "openai")
	s1 := newDialectUpstream(t, "systemone")
	luna := h.decisionModel(t, oai, "OpenAI", "gpt-6-luna:decisions", 0.10, store.DecisionDialectOpenAI)
	jev := h.decisionModel(t, s1, "Openrouter", "typesafe/jev-router", 0.042, store.DecisionDialectSystemOne)

	// 1. OpenAI SDK -> OpenAI model: passed through, model name mapped.
	rec := h.doRaw(http.MethodPost, "/v1/decisions", "application/json", []byte(sprintf(oaiBody, luna.Name)))
	if rec.Code != http.StatusOK || rec.Header().Get(headerDecisionTranslated) != "" {
		t.Fatalf("openai->openai: %d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}
	path, sent := oai.last()
	if path != "/v1/decisions" || sent["model"] != "gpt-6-luna" {
		t.Fatalf("Luna's catalog row is gpt-6-luna:decisions; the wire name is gpt-6-luna: %s %v", path, sent["model"])
	}
	if !strings.Contains(rec.Body.String(), `"probability":0.25`) {
		t.Fatalf("the provider's answer is relayed: %s", rec.Body.String())
	}

	// 2. TypeSafe SDK -> OpenAI model: translated both ways.
	rec = h.doRaw(http.MethodPost, "/v1/systemone", "application/json", []byte(sprintf(s1Body, luna.Name)))
	if rec.Code != http.StatusOK || rec.Header().Get(headerDecisionTranslated) != "systemone->openai" {
		t.Fatalf("systemone->openai: %d %s", rec.Code, rec.Body.String())
	}
	_, sent = oai.last()
	qs := sent["questions"].([]any)
	if len(qs) != 2 || qs[1].(map[string]any)["type"] != "predicate" || !strings.Contains(qs[1].(map[string]any)["instructions"].(string), "Counts as yes") {
		t.Fatalf("noul must reach OpenAI as a predicate with its boundary in words: %v", sent)
	}
	var s1Resp struct {
		Answers map[string]map[string]any `json:"answers"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &s1Resp)
	if s1Resp.Answers["urgent"]["type"] != "noul" || s1Resp.Answers["urgent"]["noul"] != 0.25 || s1Resp.Answers["department"]["choice"] != "billing" {
		t.Fatalf("the caller gets System One answers back: %s", rec.Body.String())
	}

	// 3. OpenAI SDK -> System One model (Jev), with a score question.
	rec = h.doRaw(http.MethodPost, "/v1/decisions", "application/json", []byte(sprintf(oaiBody, "typesafe/jev-router")))
	if rec.Code != http.StatusOK || rec.Header().Get(headerDecisionTranslated) != "openai->systemone" {
		t.Fatalf("openai->systemone: %d %s", rec.Code, rec.Body.String())
	}
	path, sent = s1.last()
	// (the ~typesafe/jev-latest wire name applies on openrouter.ai only;
	// see decisions.WireModel)
	if path != "/v1/systemone" || sent["model"] != "typesafe/jev-router" || sent["state"] != "I was charged twice." {
		t.Fatalf("System One body: %s %v", path, sent)
	}
	var oaiResp struct {
		Answers []map[string]any `json:"answers"`
		Usage   map[string]any   `json:"usage"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &oaiResp)
	if len(oaiResp.Answers) != 3 || oaiResp.Answers[1]["type"] != "predicate" || oaiResp.Answers[1]["probability"] != 0.67 || oaiResp.Answers[2]["type"] != "score" {
		t.Fatalf("the caller gets OpenAI answers back in question order: %s", rec.Body.String())
	}

	// 4. OpenRouter SDK path -> System One model: passed through.
	rec = h.doRaw(http.MethodPost, "/api/alpha/decisions", "application/json", []byte(sprintf(s1Body, jev.Name)))
	if rec.Code != http.StatusOK || rec.Header().Get(headerDecisionTranslated) != "" || !strings.Contains(rec.Body.String(), `"provider":"TypeSafe"`) {
		t.Fatalf("systemone passthrough: %d %s", rec.Code, rec.Body.String())
	}

	// Every call is in the request log as a decision, priced.
	var decs []*store.UsageEvent
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && len(decs) < 4; time.Sleep(25 * time.Millisecond) {
		decs = nil
		for _, e := range h.waitForUsageEventsNoWait() {
			if e.Modality == "decision" {
				decs = append(decs, e)
			}
		}
	}
	if len(decs) != 4 {
		t.Fatalf("4 decision events, got %d", len(decs))
	}
	for _, e := range decs {
		switch e.ModelID {
		case luna.ID:
			// $0.10/M x 1000 input tokens = $0.0001 = 100,000 nano.
			if e.CostNano != 100_000 || e.TokensIn != 1000 || e.AccountingMode != usage.AccountingUpstream {
				t.Fatalf("Luna is priced from its rate card (input only): %+v", e)
			}
		case jev.ID:
			// OpenRouter's reported charge wins in automatic mode.
			if e.CostNano != 12_600 || e.AccountingMode != usage.AccountingUpstreamCost {
				t.Fatalf("Jev uses the provider-reported cost: cost=%d mode=%s", e.CostNano, e.AccountingMode)
			}
		default:
			t.Fatalf("unexpected model %s", e.ModelName)
		}
		if e.HTTPStatus != 200 || e.UserID != h.user.ID {
			t.Fatalf("attribution: %+v", e)
		}
	}
}

func TestServedDecisionsPricingOptions(t *testing.T) {
	h := newHarness(t)
	s1 := newDialectUpstream(t, "systemone")
	jev := h.decisionModel(t, s1, "Openrouter", "typesafe/jev-router", 0.042, store.DecisionDialectSystemOne)
	ctx := context.Background()
	call := func() *store.UsageEvent {
		t.Helper()
		before := len(h.waitForUsageEventsNoWait())
		rec := h.doRaw(http.MethodPost, "/v1/systemone", "application/json", []byte(sprintf(s1Body, jev.Name)))
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if evs := h.waitForUsageEventsNoWait(); len(evs) > before {
				return evs[0]
			}
		}
		t.Fatal("no usage event")
		return nil
	}
	set := func(d store.DecisionSetting) {
		d.ModelID = jev.ID
		if err := h.store.SetDecisionSetting(ctx, d); err != nil {
			t.Fatal(err)
		}
		h.server.InvalidateConfigCache()
	}
	// card ignores the reported charge: 300 input tokens x $0.042/M = 12,600 nano-USD.
	set(store.DecisionSetting{CostSource: store.CostSourceCard, RequestFeeNano: 1_000})
	if e := call(); e.CostNano != 12_600+1_000 || e.AccountingMode != usage.AccountingUpstream {
		t.Fatalf("card + fee: %d %s", e.CostNano, e.AccountingMode)
	}
	set(store.DecisionSetting{CostSource: store.CostSourceNone, RequestFeeNano: 1_000})
	if e := call(); e.CostNano != 0 {
		t.Fatalf("none is free: %d", e.CostNano)
	}
	set(store.DecisionSetting{CostSource: store.CostSourceProvider})
	if e := call(); e.CostNano != 12_600 || e.AccountingMode != usage.AccountingUpstreamCost {
		t.Fatalf("provider: %d %s", e.CostNano, e.AccountingMode)
	}
}

func TestServedDecisionsGrantsAndGuards(t *testing.T) {
	h := newHarness(t)
	s1 := newDialectUpstream(t, "systemone")
	jev := h.decisionModel(t, s1, "Openrouter", "typesafe/jev-router", 0.042, store.DecisionDialectSystemOne)
	ctx := context.Background()

	// Decision models are not chat models, and vice versa.
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": jev.Name, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "/v1/decisions") {
		t.Fatalf("chat to a decision model must say where to go: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doRaw(http.MethodPost, "/v1/systemone", "application/json", []byte(sprintf(s1Body, "test-model")))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "chat model") {
		t.Fatalf("a decision call to a chat model must say so: %d %s", rec.Code, rec.Body.String())
	}

	// /v1/models lists it as a decision model.
	rec = h.do(http.MethodGet, "/v1/models", nil)
	if !strings.Contains(rec.Body.String(), `"kind":"decision"`) || !strings.Contains(rec.Body.String(), `"/v1/decisions"`) {
		t.Fatalf("/v1/models must list decision models: %s", rec.Body.String())
	}

	// Once it backs a blocking guard, existing grants stop working and new
	// grants are refused, until the admin allows it.
	h.secgwPolicy(t, store.SecgwPolicy{Name: "Org guard", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckPromptInjection, Enabled: true, Mode: store.SecgwModeBlock, Fail: store.SecgwFailOpen, Direction: store.SecgwDirectionIngress,
			ClassifierModelID: jev.ID, Options: json.RawMessage(`{"timeout_ms":3000}`)},
	}})
	h.server.InvalidateConfigCache()
	if _, err := h.store.CreateGrant(ctx, jev.ID, store.ModelKindModel, store.GranteeUser, h.user.ID); err == nil || !strings.Contains(err.Error(), "Org guard") {
		t.Fatalf("granting a blocking guard must be refused with the policy named: %v", err)
	}
	// Through the admin API it is the admin's mistake to fix: 400 with the reason.
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec = h.doAsSession(session, http.MethodPost, "/api/v1/admin/grants", map[string]any{"model_id": jev.ID, "model_kind": "model", "grantee_type": "user", "grantee_id": h.user.ID})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Org guard") {
		t.Fatalf("admin grant of a guarding model: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doRaw(http.MethodPost, "/v1/systemone", "application/json", []byte(sprintf(s1Body, jev.Name)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an existing grant must not let callers probe a blocking guard: %d %s", rec.Code, rec.Body.String())
	}
	if err := h.store.SetDecisionSetting(ctx, store.DecisionSetting{ModelID: jev.ID, GrantWhenGuarding: true}); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	rec = h.doRaw(http.MethodPost, "/v1/systemone", "application/json", []byte(sprintf(s1Body, jev.Name)))
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed by the admin: %d %s", rec.Code, rec.Body.String())
	}
	// The guard itself ran on the decision's evidence (one guard call +
	// the served decision on the same fake).
	guarded := false
	s1.mu.Lock()
	for _, b := range s1.bodies {
		if qs, ok := b["questions"].(map[string]any); ok && qs["prompt_injection"] != nil && strings.Contains(b["state"].(string), "charged twice") {
			guarded = true
		}
	}
	s1.mu.Unlock()
	if !guarded {
		t.Fatal("the Security Gateway must inspect the served decision's evidence")
	}
}

func TestServedDecisionsRefuseImagesForTextOnlyModels(t *testing.T) {
	h := newHarness(t)
	s1 := newDialectUpstream(t, "systemone")
	h.decisionModel(t, s1, "Openrouter", "typesafe/jev-router", 0.042, store.DecisionDialectSystemOne)
	body := `{"model":"typesafe/jev-router","input":[{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}],"questions":[{"type":"predicate","name":"d","instructions":"Damaged?"}]}`
	rec := h.doRaw(http.MethodPost, "/v1/decisions", "application/json", []byte(body))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "reads text only") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if len(s1.paths) != 0 {
		t.Fatal("nothing may reach the provider")
	}
}
