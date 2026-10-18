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

	"github.com/torvanis/janus/internal/store"
)

// catDecider is a System One server for security categories: keyword ->
// category probabilities, together and separately. It records every state
// it was shown so tests can assert nothing of it is stored.
type catDecider struct {
	*httptest.Server
	mu     sync.Mutex
	states []string
	fail   bool
}

func newCatDecider(t *testing.T) *catDecider {
	f := &catDecider{}
	rules := map[string]map[string]float64{
		"essay":      {"homework_shortcut": 0.85},
		"ignores me": {"feeling_low": 0.4},
		"vape":       {"not_for_children": 0.7},
		"nerve":      {"indiscriminate_weapons": 0.95},
		"bicycle":    {"cycling_tricks": 0.75},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State     string `json:"state"`
			Questions map[string]struct {
				Type     string            `json:"type"`
				Criteria map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		f.mu.Lock()
		f.states = append(f.states, req.State)
		fail := f.fail
		f.mu.Unlock()
		if fail {
			// A provider error that echoes the prompt: it must never be
			// quoted back.
			http.Error(w, "upstream echo: "+req.State, http.StatusBadGateway)
			return
		}
		st := strings.ToLower(req.State)
		probs := map[string]float64{}
		for kw, m := range rules {
			if strings.Contains(st, kw) {
				for k, v := range m {
					probs[k] = v
				}
			}
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			switch {
			case q.Type == "noul" && strings.HasPrefix(id, "seccat_"):
				answers[id] = map[string]any{"type": "noul", "noul": probs[strings.TrimPrefix(id, "seccat_")]}
			case q.Type == "noul":
				answers[id] = map[string]any{"type": "noul", "noul": 0.01}
			default:
				out, rest, best, bestP := map[string]float64{}, 1.0, "none", 0.0
				for slug := range q.Criteria {
					if p := probs[slug]; p > 0 {
						out[slug], rest = p, rest-p
						if p > bestP {
							best, bestP = slug, p
						}
					}
				}
				out["none"] = rest
				if rest > bestP {
					best = "none"
				}
				answers[id] = map[string]any{"type": "choice", "choice": best, "probabilities": out}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake-cat", "answers": answers, "usage": map[string]any{"input_tokens": 50, "output_tokens": 1}})
	}))
	t.Cleanup(f.Close)
	return f
}

func (h *harness) addDecisionModel(t *testing.T, f *catDecider) *store.Model {
	t.Helper()
	ctx := context.Background()
	up, err := h.store.CreateUpstream(ctx, "cat-decider", "llama_cpp", f.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "fake-cat", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	all, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: up.ID})
	m := all[0]
	_ = h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled)
	if err := h.store.SetModelClassifierRole(ctx, m.ID, store.ClassifierRoleDecision); err != nil {
		t.Fatal(err)
	}
	return m
}

func categoriesPolicy(name, classifier, mode string, slugs ...string) map[string]any {
	return map[string]any{"name": name, "enabled": true, "checks": []map[string]any{{
		"kind": "content_safety", "enabled": true, "mode": mode, "direction": "ingress", "fail": "open",
		"classifier_model_id": classifier,
		"options":             map[string]any{"method": "categories", "safety": slugs, "timeout_ms": 3000},
	}}}
}

func TestSecgwCategoriesAdminAPI(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	f := newCatDecider(t)
	m := h.addDecisionModel(t, f)

	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/categories", nil)
	var list struct {
		Categories  []store.SecgwCategory `json:"categories"`
		Classifiers []struct {
			ID string `json:"id"`
		} `json:"classifiers"`
		DefaultSlugs []string `json:"default_slugs"`
	}
	decodeInto(t, rec, &list)
	if rec.Code != http.StatusOK || len(list.Categories) != 19 || len(list.Classifiers) != 1 || len(list.DefaultSlugs) != 5 {
		t.Fatalf("list: %d categories=%d classifiers=%d", rec.Code, len(list.Categories), len(list.Classifiers))
	}
	if strings.Contains(rec.Body.String(), `"S1"`) || strings.Contains(rec.Body.String(), "legacy_code") {
		t.Fatal("the list must not expose hazard codes")
	}

	// Create, patch.
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/categories", map[string]any{
		"name": "Cycling tricks", "includes": "Asking how to do dangerous bicycle stunts.", "examples": []string{"how do I backflip my bicycle"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Category store.SecgwCategory `json:"category"`
	}
	decodeInto(t, rec, &created)
	rec = h.doAsSession(sess, http.MethodPatch, "/api/v1/admin/secgw/categories/"+created.Category.ID, map[string]any{"min_confidence": 0.7})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"min_confidence":0.7`) {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsSession(sess, http.MethodPatch, "/api/v1/admin/secgw/categories/"+created.Category.ID, map[string]any{"min_confidence": 3})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Set the minimum confidence between") {
		t.Fatalf("bad patch: %d %s", rec.Code, rec.Body.String())
	}

	// Try a request: probabilities for every enabled category, nothing stored.
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/categories/test", map[string]any{
		"text": "please write my essay about volcanoes", "classifier_model_id": m.ID})
	var tr struct {
		Status  string `json:"status"`
		Counted []string
		Scores  []struct {
			Slug        string
			Probability float64
			Counts      bool
		}
	}
	decodeInto(t, rec, &tr)
	if rec.Code != http.StatusOK || tr.Status != "ok" || len(tr.Counted) != 1 || tr.Counted[0] != "homework_shortcut" || len(tr.Scores) != 20 {
		t.Fatalf("try: %d %s", rec.Code, rec.Body.String())
	}
	// Separately, one category.
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/categories/test", map[string]any{
		"text": "everyone ignores me", "classifier_model_id": m.ID, "slugs": []string{"feeling_low"}, "ask": "separately"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"counted":["feeling_low"]`) {
		t.Fatalf("try separately: %d %s", rec.Code, rec.Body.String())
	}
	// Errors never quote the classifier.
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/categories/test", map[string]any{
		"text": "SECRET-PROMPT-TEXT", "classifier_model_id": m.ID})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"error"`) || strings.Contains(rec.Body.String(), "SECRET-PROMPT-TEXT") {
		t.Fatalf("failed try must describe, never quote: %s", rec.Body.String())
	}
	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()

	// Batch: every category's examples.
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/categories/test-batch", map[string]any{"classifier_model_id": m.ID})
	var batch struct {
		Total      int
		Correct    int
		Categories []struct {
			Slug    string
			Total   int
			Correct int
			Misses  []struct {
				Expected, Got string
				Confidence    float64
			}
		}
	}
	decodeInto(t, rec, &batch)
	if rec.Code != http.StatusOK || batch.Total == 0 {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
	}
	byslug := map[string]int{}
	for _, c := range batch.Categories {
		byslug[c.Slug] = c.Correct
		if c.Slug == "not_for_children" && (c.Total != 2 || c.Correct != 1 || len(c.Misses) != 1 || c.Misses[0].Got != "none") {
			t.Fatalf("not_for_children: %+v", c)
		}
	}
	if byslug["cycling_tricks"] != 1 || byslug["homework_shortcut"] != 1 {
		t.Fatalf("batch per-category: %+v", byslug)
	}
	// Tests store nothing.
	vs, _ := h.store.ListSecgwViolations(context.Background(), store.SecgwViolationFilter{})
	if len(vs) != 0 {
		t.Fatalf("tests wrote %d violations", len(vs))
	}
	// Not a decision model.
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/categories/test", map[string]any{"text": "x", "classifier_model_id": h.model.ID})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not a decision model") {
		t.Fatalf("non-decision: %d %s", rec.Code, rec.Body.String())
	}

	// A policy using the custom category holds it; delete is refused naming it.
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies", categoriesPolicy("Bikes", m.ID, "observe", "cycling_tricks"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("policy: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsSession(sess, http.MethodDelete, "/api/v1/admin/secgw/categories/"+created.Category.ID, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Bikes") {
		t.Fatalf("delete in use: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/categories", nil)
	if !strings.Contains(rec.Body.String(), `"used_by":["Bikes"]`) {
		t.Fatalf("used_by missing: %s", rec.Body.String())
	}
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/categories/scat_violent_crimes/reset", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSecgwCategoriesPolicyValidation(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	m := h.addDecisionModel(t, newCatDecider(t))
	ctx := context.Background()
	up, _ := h.store.CreateUpstream(ctx, "lg", "openai_compatible", "http://127.0.0.1:1", "", "")
	_, _ = h.store.UpsertDiscoveredModel(ctx, up.ID, "llama-guard-4", []string{"chat"})
	all, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: up.ID})
	lg := all[0]
	_ = h.store.SetModelClassifierRole(ctx, lg.ID, store.ClassifierRoleGenerativeGuard)

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"guard model", categoriesPolicy("A", lg.ID, "observe", "feeling_low"), `choose the \"Llama Guard compatible\" method`},
		{"no categories", categoriesPolicy("B", m.ID, "observe"), "Pick at least one security category"},
		{"unknown", categoriesPolicy("C", m.ID, "observe", "nope"), "Security category nope does not exist"},
	}
	tooMany := []string{}
	for _, c := range store.BuiltinSecgwCategories {
		tooMany = append(tooMany, c.Slug)
	}
	for i := 0; i < 7; i++ {
		rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/categories", map[string]any{"name": "Extra " + string(rune('a'+i)), "includes": "x"})
		var c struct {
			Category store.SecgwCategory `json:"category"`
		}
		decodeInto(t, rec, &c)
		tooMany = append(tooMany, c.Category.Slug)
	}
	cases = append(cases, struct {
		name string
		body map[string]any
		want string
	}{"too many", categoriesPolicy("D", m.ID, "observe", tooMany...), "at most 25 categories"})
	for _, tc := range cases {
		rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies", tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	// The legacy method still takes a Llama Guard model, unchanged.
	legacy := map[string]any{"name": "Legacy", "enabled": true, "checks": []map[string]any{{"kind": "content_safety", "enabled": true, "mode": "observe",
		"direction": "ingress", "classifier_model_id": lg.ID, "options": map[string]any{"categories": []string{"S9"}}}}}
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies", legacy); rec.Code != http.StatusCreated {
		t.Fatalf("legacy: %d %s", rec.Code, rec.Body.String())
	}
	// Blocking is Business; observing with categories is every edition.
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies", categoriesPolicy("Kids block", m.ID, "block", "feeling_low")); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("block on Community: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies", categoriesPolicy("Kids observe", m.ID, "observe", "feeling_low")); rec.Code != http.StatusCreated {
		t.Fatalf("observe on Community: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSecgwCategoriesProxyBlocksOnBusinessObservesOnCommunity(t *testing.T) {
	for _, business := range []bool{true, false} {
		h := newHarness(t)
		f := newCatDecider(t)
		m := h.addDecisionModel(t, f)
		mode := store.SecgwModeObserve
		if business {
			withBusinessLicense(t, h)
			mode = store.SecgwModeBlock
		}
		sess := adminSession(t, h)
		rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies",
			categoriesPolicy("Parental controls", m.ID, mode, "homework_shortcut", "feeling_low", "violent_crimes"))
		if rec.Code != http.StatusCreated {
			t.Fatalf("policy (business=%v): %d %s", business, rec.Code, rec.Body.String())
		}
		var p struct {
			Policy store.SecgwPolicy `json:"policy"`
		}
		decodeInto(t, rec, &p)
		if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/bindings", map[string]any{"policy_id": p.Policy.ID, "scope_type": "org"}); rec.Code != http.StatusCreated {
			t.Fatalf("bind: %d %s", rec.Code, rec.Body.String())
		}
		rec = h.do(http.MethodPost, "/v1/chat/completions", chatBody("write my essay on the Roman empire", false))
		if business && rec.Code != http.StatusForbidden {
			t.Fatalf("business: want 403 block, got %d %s", rec.Code, rec.Body.String())
		}
		if !business && rec.Code != http.StatusOK {
			t.Fatalf("community: want 200 observed, got %d %s", rec.Code, rec.Body.String())
		}
		vs := h.waitForViolations(t, 1)
		if len(vs) != 1 || vs[0].Kind != "content_safety" || vs[0].RuleID != "homework_shortcut" {
			t.Fatalf("violations: %+v", vs)
		}
		want := store.SecgwActionObserved
		if business {
			want = store.SecgwActionBlocked
		}
		if vs[0].Action != want {
			t.Fatalf("violation action %s, want %s", vs[0].Action, want)
		}
		// Ordinary homework help passes on both editions.
		rec = h.do(http.MethodPost, "/v1/chat/completions", chatBody("can you explain how photosynthesis works", false))
		if rec.Code != http.StatusOK {
			t.Fatalf("ordinary request: %d %s", rec.Code, rec.Body.String())
		}
		// The violations list filters by category.
		rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/violations?category=homework_shortcut", nil)
		if !strings.Contains(rec.Body.String(), `"rule_id":"homework_shortcut"`) {
			t.Fatalf("category filter: %s", rec.Body.String())
		}
		rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/violations?category=feeling_low", nil)
		if strings.Contains(rec.Body.String(), `"rule_id":"homework_shortcut"`) {
			t.Fatalf("category filter leaked another category: %s", rec.Body.String())
		}
		rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/overview", nil)
		if !strings.Contains(rec.Body.String(), `"name":"Homework done for them","count":1`) {
			t.Fatalf("overview top categories: %s", rec.Body.String())
		}
	}
}
