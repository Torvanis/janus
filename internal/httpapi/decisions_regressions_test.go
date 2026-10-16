package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/store"
)

// Regression tests for the Decisions API release review (area E).

func (u *dialectUpstream) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.paths)
}

// E1: a BLOCK secrets check must see the question text (instructions,
// choice and level descriptions, criteria), not only the evidence.
func TestDecisionQuestionTextIsInspectedBySecurityGateway(t *testing.T) {
	h := newHarness(t)
	s1 := newDialectUpstream(t, "systemone")
	jev := h.decisionModel(t, s1, "Openrouter", "typesafe/jev-router", 0.042, store.DecisionDialectSystemOne)
	h.secgwPolicy(t, store.SecgwPolicy{Name: "No secrets", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeBlock},
	}})
	ctrl := `{"model":"` + jev.Name + `","state":"key ` + testAWSKey + `","questions":{"a":{"type":"noul","instructions":"Is this fine?"}}}`
	if rec := h.doRaw(http.MethodPost, "/v1/systemone", "application/json", []byte(ctrl)); rec.Code != http.StatusForbidden {
		t.Fatalf("control: a secret in the state must be blocked: %d", rec.Code)
	}
	for _, c := range []struct{ name, path, body string }{
		{"s1 instructions", "/v1/systemone", `{"model":"` + jev.Name + `","state":"hello","questions":{"a":{"type":"noul","instructions":"Does it contain ` + testAWSKey + `?"}}}`},
		{"s1 counts_as_yes", "/api/alpha/decisions", `{"model":"` + jev.Name + `","state":"hello","questions":{"a":{"type":"noul","instructions":{"question":"ok?","counts_as_yes":"` + testAWSKey + `","counts_as_no":"no"}}}}`},
		{"s1 choice description", "/v1/systemone", `{"model":"` + jev.Name + `","state":"hello","questions":{"a":{"type":"choice","instructions":"pick","criteria":{"x":"` + testAWSKey + `","y":"other"}}}}`},
		{"s1 score label", "/v1/systemone", `{"model":"` + jev.Name + `","state":"hello","questions":{"a":{"type":"score","instructions":"rate","criteria":["low","` + testAWSKey + `"]}}}`},
		{"openai choice description", "/v1/decisions", `{"model":"` + jev.Name + `","input":"hello","questions":[{"type":"choice","name":"a","instructions":"pick","choices":[{"value":"x","description":"` + testAWSKey + `"},{"value":"y"}]}]}`},
		{"openai level description", "/v1/decisions", `{"model":"` + jev.Name + `","input":"hello","questions":[{"type":"score","name":"a","instructions":"rate","levels":[{"label":"low"},{"label":"high","description":"` + testAWSKey + `"}]}]}`},
		{"openai question name", "/v1/decisions", `{"model":"` + jev.Name + `","input":"hello","questions":[{"type":"predicate","name":"` + testAWSKey + `","instructions":"ok?"}]}`},
	} {
		before := s1.calls()
		rec := h.doRaw(http.MethodPost, c.path, "application/json", []byte(c.body))
		if rec.Code != http.StatusForbidden || s1.calls() != before {
			t.Errorf("%s: a secret in the question text reached the provider past a BLOCK secrets check (status %d)", c.name, rec.Code)
		}
	}
}

// E1: REDACT rewrites the question text in the field it was found in, in
// either dialect, same-dialect (forwarded) or translated.
func TestDecisionQuestionTextIsRedactedInEitherDialect(t *testing.T) {
	h := newHarness(t)
	s1 := newDialectUpstream(t, "systemone")
	oai := newDialectUpstream(t, "openai")
	jev := h.decisionModel(t, s1, "Openrouter", "typesafe/jev-router", 0.042, store.DecisionDialectSystemOne)
	luna := h.decisionModel(t, oai, "OpenAI", "gpt-6-luna:decisions", 0.10, store.DecisionDialectOpenAI)
	h.secgwPolicy(t, store.SecgwPolicy{Name: "Redact PII", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckPII, Enabled: true, Mode: store.SecgwModeRedact},
	}})
	const email = "bob@example.com"
	type sent struct {
		u     *dialectUpstream
		check func(map[string]any) string
	}
	s1Instr := func(b map[string]any) string {
		return b["questions"].(map[string]any)["a"].(map[string]any)["instructions"].(string)
	}
	oaiDesc := func(b map[string]any) string {
		return b["questions"].([]any)[0].(map[string]any)["choices"].([]any)[0].(map[string]any)["description"].(string)
	}
	s1Desc := func(b map[string]any) string {
		return b["questions"].(map[string]any)["a"].(map[string]any)["criteria"].(map[string]any)["x"].(string)
	}
	for _, c := range []struct {
		name, path, body string
		to               sent
	}{
		{"s1 -> s1 (forwarded)", "/v1/systemone", `{"model":"` + jev.Name + `","state":"hello","questions":{"a":{"type":"noul","instructions":"Is this from ` + email + `?"}}}`, sent{s1, s1Instr}},
		{"openai -> openai (forwarded)", "/v1/decisions", `{"model":"` + luna.Name + `","input":"hello","questions":[{"type":"choice","name":"a","instructions":"pick","choices":[{"value":"x","description":"mail ` + email + `"},{"value":"y"}]}]}`, sent{oai, oaiDesc}},
		{"openai -> s1 (translated)", "/v1/decisions", `{"model":"` + jev.Name + `","input":"hello","questions":[{"type":"choice","name":"a","instructions":"pick","choices":[{"value":"x","description":"mail ` + email + `"},{"value":"y"}]}]}`, sent{s1, s1Desc}},
	} {
		rec := h.doRaw(http.MethodPost, c.path, "application/json", []byte(c.body))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: redaction must pass: %d %s", c.name, rec.Code, rec.Body.String())
		}
		_, body := c.to.u.last()
		got := c.to.check(body)
		if strings.Contains(got, email) || !strings.Contains(got, "[REDACTED:email]") {
			t.Errorf("%s: the field the address was in reached the provider as %q", c.name, got)
		}
		if rec.Header().Get(headerSecgwAction) != store.SecgwActionRedacted {
			t.Errorf("%s: action header %q", c.name, rec.Header().Get(headerSecgwAction))
		}
	}
}

// E1: a redaction that falls on text that cannot be rewritten without
// changing the answers (a choice value, a question name) blocks.
func TestDecisionRedactionInAChoiceValueBlocks(t *testing.T) {
	h := newHarness(t)
	s1 := newDialectUpstream(t, "systemone")
	jev := h.decisionModel(t, s1, "Openrouter", "typesafe/jev-router", 0.042, store.DecisionDialectSystemOne)
	h.secgwPolicy(t, store.SecgwPolicy{Name: "Redact PII", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckPII, Enabled: true, Mode: store.SecgwModeRedact},
	}})
	for _, body := range []string{
		`{"model":"` + jev.Name + `","state":"hello","questions":{"a":{"type":"choice","instructions":"pick","criteria":{"bob@example.com":"him","y":"other"}}}}`,
		`{"model":"` + jev.Name + `","state":"hello","questions":{"bob@example.com":{"type":"noul","instructions":"ok?"}}}`,
	} {
		before := s1.calls()
		rec := h.doRaw(http.MethodPost, "/v1/systemone", "application/json", []byte(body))
		if rec.Code != http.StatusForbidden || s1.calls() != before {
			t.Errorf("an unredactable field with PII must block: %d %s", rec.Code, rec.Body.String())
		}
	}
}

// E2: a group policy naming model B in observe mode on top of a mandatory
// org floor that blocks the same kind makes B the effective blocking judge
// for the group; B must not be grantable.
func TestDecisionGuardingSeesMandatoryFloorTightening(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s1 := newDialectUpstream(t, "systemone")
	a := h.decisionModel(t, s1, "UpA", "judge-a", 0.042, store.DecisionDialectSystemOne)
	s2 := newDialectUpstream(t, "systemone")
	b := h.decisionModel(t, s2, "UpB", "judge-b", 0.042, store.DecisionDialectSystemOne)
	h.secgwPolicy(t, store.SecgwPolicy{Name: "Org floor", Enabled: true, Mandatory: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckPromptInjection, Enabled: true, Mode: store.SecgwModeBlock, Fail: store.SecgwFailOpen, Direction: store.SecgwDirectionIngress, ClassifierModelID: a.ID},
	}})
	g, err := h.store.CreateGroup(ctx, "eng")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetGroupMembers(ctx, g.ID, []string{h.user.ID}); err != nil {
		t.Fatal(err)
	}
	p := store.SecgwPolicy{Name: "Eng (observe)", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckPromptInjection, Enabled: true, Mode: store.SecgwModeObserve, Fail: store.SecgwFailOpen, Direction: store.SecgwDirectionIngress, ClassifierModelID: b.ID},
	}}
	if err := h.store.CreateSecgwPolicy(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateSecgwBinding(ctx, &store.SecgwBinding{PolicyID: p.ID, ScopeType: store.SecgwScopeGroup, ScopeID: g.ID}); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	eng, err := h.server.secgwEngine(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eff := eng.Resolve(secgwSubject(&store.UsageEvent{UserID: h.user.ID}, []string{g.ID}, store.ResolvedModel{Model: h.model}))
	if c := eff.Checks[store.SecgwCheckPromptInjection]; c.Mode != store.SecgwModeBlock || c.ClassifierModelID != b.ID {
		t.Fatalf("setup: effective check is %s with %s, want block with B", c.Mode, c.ClassifierModelID)
	}
	if ok, why, _ := h.store.DecisionGrantable(ctx, b); ok {
		t.Fatal("B is the effective blocking judge for group eng, yet it may be granted")
	} else if !strings.Contains(why, "Org floor") {
		t.Errorf("the admin reason should name the floor: %q", why)
	}
	if _, err := h.store.CreateGrant(ctx, b.ID, store.ModelKindModel, store.GranteeUser, h.user.ID); err == nil {
		t.Error("a grant of the effective blocking judge must be refused")
	}
	rec := h.doRaw(http.MethodPost, "/v1/systemone", "application/json", []byte(sprintf(s1Body, b.Name)))
	if rec.Code != http.StatusForbidden {
		t.Errorf("an existing grant must stop while B is the effective judge: %d", rec.Code)
	}
}

// E6: the caller's 403 for a reserved model names no policy.
func TestDecisionReservedRefusalNamesNoPolicy(t *testing.T) {
	h := newHarness(t)
	s1 := newDialectUpstream(t, "systemone")
	jev := h.decisionModel(t, s1, "Openrouter", "typesafe/jev-router", 0.042, store.DecisionDialectSystemOne)
	h.secgwPolicy(t, store.SecgwPolicy{Name: "Secret Org Injection Policy", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckPromptInjection, Enabled: true, Mode: store.SecgwModeBlock, Fail: store.SecgwFailOpen, Direction: store.SecgwDirectionIngress, ClassifierModelID: jev.ID},
	}})
	rec := h.doRaw(http.MethodPost, "/v1/systemone", "application/json", []byte(sprintf(s1Body, jev.Name)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "Secret Org Injection Policy") || !strings.Contains(body, "reserved for security checks") {
		t.Errorf("caller-facing 403 must not name the policy: %s", body)
	}
}

// E7: a decision model nobody priced is logged unpriced, as chat is.
func TestUnpricedDecisionModelIsLoggedUnpriced(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s1 := newDialectUpstream(t, "systemone")
	key, _ := h.server.Cipher.Encrypt("up-key")
	up, err := h.store.CreateUpstream(ctx, "llama", "llama_cpp", s1.URL, key, "up-k…")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "nimble", []string{"decision"}); err != nil {
		t.Fatal(err)
	}
	m, _ := h.store.ModelByUpstreamAndName(ctx, up.ID, "nimble")
	_ = h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled)
	if err := h.store.SetModelClassifierRole(ctx, m.ID, store.ClassifierRoleDecision); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateGrant(ctx, m.ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetDecisionSetting(ctx, store.DecisionSetting{ModelID: m.ID, CostSource: store.CostSourceCard}); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	status := func() string {
		t.Helper()
		ev := h.requestEvent(t)
		var s string
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && s == ""; time.Sleep(20 * time.Millisecond) {
			_ = h.store.DB().QueryRowContext(ctx, `SELECT cost_status FROM reporting_usage_snapshot WHERE usage_id = ?`, ev.ID).Scan(&s)
		}
		return s
	}
	if rec := h.doRaw(http.MethodPost, "/v1/systemone", "application/json", []byte(sprintf(s1Body, "nimble"))); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if s := status(); s != "unpriced" {
		t.Errorf("never-priced decision model logged as %q, want unpriced", s)
	}
}

// E8: Janus's own decision calls are priced from the same inputs as a
// served decision: cached tokens and an explicit reported $0.
func TestInternalDecisionCallsPriceLikeServedOnes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	oai := newDialectUpstream(t, "openai")
	luna := h.decisionModel(t, oai, "OpenAI", "gpt-6-luna:decisions", 0.10, store.DecisionDialectOpenAI)
	served := decisionCall{TokensIn: 1000, TokensCached: 800}
	internal := decisionCallFromUsage(classify.Usage{TokensIn: 1000, TokensCached: 800}, 1)
	a, _ := h.server.decisionCost(ctx, luna, served)
	b, _ := h.server.decisionCost(ctx, luna, internal)
	if a != b {
		t.Errorf("cached tokens: served %d nano, internal %d nano", a, b)
	}
	zero := decisionCallFromUsage(classify.Usage{TokensIn: 1000, CostReported: true}, 1)
	if c, _ := h.server.decisionCost(ctx, luna, zero); c != 0 {
		t.Errorf("a provider-reported $0 must price at $0, got %d nano", c)
	}
}

// E9: /api/alpha/decisions is outside /v1/ but serves the same models: a
// rate limit an admin scoped to /v1/* applies to it.
func TestDecisionAlphaPathFollowsV1Rules(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s1 := newDialectUpstream(t, "systemone")
	jev := h.decisionModel(t, s1, "Openrouter", "typesafe/jev-router", 0.042, store.DecisionDialectSystemOne)
	if err := h.store.CreateRateLimitRule(ctx, &store.RateLimitRule{Endpoint: "/v1/*", RequestsPerMinute: 1}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetFeatureFlag(ctx, "per_user_rate_limits_enabled", true); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	codes := []int{}
	for i := 0; i < 2; i++ {
		codes = append(codes, h.doRaw(http.MethodPost, "/api/alpha/decisions", "application/json", []byte(sprintf(s1Body, jev.Name))).Code)
	}
	if codes[1] != http.StatusTooManyRequests {
		t.Errorf("a /v1/* limit of 1/min must also bound /api/alpha/decisions: %v", codes)
	}
}

// E9: a pooled alias over decision models picks its member per request.
func TestDecisionPoolAliasPicksAMember(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s1 := newDialectUpstream(t, "systemone")
	s2 := newDialectUpstream(t, "systemone")
	a := h.decisionModel(t, s1, "UpA", "typesafe/jev-router", 0.042, store.DecisionDialectSystemOne)
	b := h.decisionModel(t, s2, "UpB", "typesafe/jev-router", 0.042, store.DecisionDialectSystemOne)
	mm, err := h.store.CreateManagedModel(ctx, "decide", "", a.ID, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := h.store.NormalizePool(ctx, store.PoolInput{Policy: store.PoolPolicyFailover, Affinity: store.PoolAffinityOff,
		Members: []store.PoolMemberInput{{ModelID: a.ID}, {ModelID: b.ID}}}, "")
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
	rec := h.doRaw(http.MethodPost, "/v1/systemone", "application/json", []byte(sprintf(s1Body, "decide")))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(headerPoolMember) == "" || rec.Header().Get(headerPoolReason) == "" {
		t.Errorf("a pooled decision alias must go through the pool: %v", rec.Header())
	}
}

// E10: /v1/models marks aliases of decision models as decision, and hides
// decision models (and their aliases) reserved for security checks.
func TestModelsListMarksDecisionAliasesAndHidesReservedModels(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s1 := newDialectUpstream(t, "systemone")
	jev := h.decisionModel(t, s1, "Openrouter", "typesafe/jev-router", 0.042, store.DecisionDialectSystemOne)
	mm, err := h.store.CreateManagedModel(ctx, "decide", "", jev.ID, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateGrant(ctx, mm.ID, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	kinds := func() map[string]string {
		rec := h.do(http.MethodGet, "/v1/models", nil)
		var list struct {
			Data []struct {
				ID    string         `json:"id"`
				Janus map[string]any `json:"janus"`
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &list)
		out := map[string]string{}
		for _, m := range list.Data {
			k, _ := m.Janus["kind"].(string)
			out[m.ID] = k
		}
		return out
	}
	got := kinds()
	if got["decide"] != "decision" || got[jev.Name] != "decision" {
		t.Fatalf("kinds: %v", got)
	}
	h.secgwPolicy(t, store.SecgwPolicy{Name: "Org guard", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckPromptInjection, Enabled: true, Mode: store.SecgwModeBlock, Fail: store.SecgwFailOpen, Direction: store.SecgwDirectionIngress, ClassifierModelID: jev.ID},
	}})
	got = kinds()
	if _, ok := got[jev.Name]; ok {
		t.Errorf("a decision model reserved for security checks is still listed: %v", got)
	}
	if _, ok := got["decide"]; ok {
		t.Errorf("an alias of a reserved decision model is still listed: %v", got)
	}
}
