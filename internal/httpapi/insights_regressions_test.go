package httpapi

// Usage insights regressions found in the release review (area A).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

func reviewARideAlong(t *testing.T, mode string, feeNano int64) (*harness, *http.Response, []store.ClassificationResult, string) {
	t.Helper()
	h := newHarness(t)
	f := newFakeDecider(t)
	m := h.decisionGuard(t, f, "decider", "openai_compatible")
	ctx := context.Background()
	if err := h.store.SetClassifierProfile(ctx, store.ClassifierProfile{UpstreamID: m.UpstreamID, Model: m.Name, CallStyle: store.ClassifierStyleCombined}); err != nil {
		t.Fatal(err)
	}
	if feeNano > 0 {
		if err := h.store.SetDecisionSetting(ctx, store.DecisionSetting{ModelID: m.ID, RequestFeeNano: feeNano}); err != nil {
			t.Fatal(err)
		}
	}
	sess := adminSession(t, h)
	settings := store.DefaultClassificationSettings()
	settings.Enabled, settings.Mode, settings.UpstreamID, settings.Model, settings.TimeoutMs = true, mode, m.UpstreamID, m.Name, 2000
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/classification", settings); rec.Code != http.StatusOK {
		t.Fatalf("save settings: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()
	rec := h.do(http.MethodPost, "/v1/chat/completions", chatBody("why does the borrow checker reject this", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := len(f.seen()); n != 1 {
		t.Fatalf("expected the insights question to ride on the single guard call, got %d calls", n)
	}
	rows := h.waitForClassifications(t, 1)
	return h, rec.Result(), rows, rec.Header().Get(headerCategory)
}

// A3: a classification answered on the guard's call made no call of its
// own ("usage and cost are the guard's, metered there"), yet it is priced
// through the decision cost function with zero tokens, which still adds the
// model's per-call request fee. The insights "Classifier cost" double counts.
func TestInsightsRideAlongClassificationIsNotChargedTheRequestFee(t *testing.T) {
	_, _, rows, _ := reviewARideAlong(t, store.ClassificationModeInline, 5_000_000) // $0.005 per call
	if rows[0].Calls != 0 {
		t.Fatalf("precondition: ride-along row should have calls=0, got %d", rows[0].Calls)
	}
	if rows[0].CostNano != 0 {
		t.Fatalf("a ride-along classification made no call but was charged %d nano-USD (the request fee)", rows[0].CostNano)
	}
}

// A6: with Background (async) mode chosen, a classification that rides on a
// guard call is recorded as inline and the label is sent to the caller in
// X-Janus-Category, which the docs/UI promise only for Inline mode.
func TestInsightsAsyncModeDoesNotEmitCategoryHeaderOrInlineRows(t *testing.T) {
	_, _, rows, hdr := reviewARideAlong(t, store.ClassificationModeAsync, 0)
	if hdr != "" {
		t.Fatalf("background mode must not add X-Janus-Category, got %q", hdr)
	}
	if rows[0].Status != store.ClassificationStatusOK || rows[0].Calls != 0 || rows[0].CostNano != 0 {
		t.Fatalf("ride-along row %+v", rows[0])
	}
}

// A2: per-model call settings (dialect, call style, option limit) are saved
// under the catalog name; Usage insights names its classifier by the wire
// name ("gpt-6-luna" for the "gpt-6-luna:decisions" row) and must still
// find them. Here an OpenAI decision model sits behind a private proxy URL,
// so only the saved dialect makes Janus speak /v1/decisions to it.
func TestInsightsUsesProfileOfDecisionsSuffixedModel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/v1/decisions" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "gpt-6-luna", "answers": []any{
			map[string]any{"type": "choice", "name": "category", "choice": "coding", "probabilities": []any{map[string]any{"value": "coding", "probability": 0.9}}, "confidence": 0.9},
		}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 0}})
	}))
	t.Cleanup(srv.Close)
	up, err := h.store.CreateUpstream(ctx, "luna-proxy", "openai_compatible", srv.URL+"/v1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "gpt-6-luna:decisions", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	models, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: up.ID})
	if err := h.store.SetModelClassifierRole(ctx, models[0].ID, store.ClassifierRoleDecision); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetClassifierProfile(ctx, store.ClassifierProfile{UpstreamID: up.ID, Model: "gpt-6-luna:decisions", Dialect: store.DecisionDialectOpenAI}); err != nil {
		t.Fatal(err)
	}
	sess := adminSession(t, h)
	settings := store.DefaultClassificationSettings()
	settings.Enabled, settings.Mode, settings.UpstreamID, settings.Model, settings.TimeoutMs = true, store.ClassificationModeInline, up.ID, "gpt-6-luna", 2000
	settings.Subcategories = false
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/classification", settings); rec.Code != http.StatusOK {
		t.Fatalf("save settings: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()
	rec := h.do(http.MethodPost, "/v1/chat/completions", chatBody("why does the borrow checker reject this", false))
	if rec.Code != http.StatusOK || rec.Header().Get(headerCategory) != "coding" {
		t.Fatalf("%d header=%q paths=%v", rec.Code, rec.Header().Get(headerCategory), paths)
	}
}

// A0: the Usage insights question rides on a BLOCKING guard's call. If the
// classifier answers the guards correctly but gives an invalid answer to the
// insights question (a choice not offered, a refusal, a missing key), the
// whole combined call is an error, so the guard "fails" and a fail-closed
// guard blocks the request. A reporting-only classifier failure must never
// fail a request.
func TestInsightsBadAnswerNeverBlocksTheRequest(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]struct {
				Type string `json:"type"`
			} `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		answers := map[string]any{}
		for id, q := range req.Questions {
			switch {
			case q.Type == "noul":
				answers[id] = map[string]any{"type": "noul", "noul": 0.01}
			case id == "content_safety":
				answers[id] = map[string]any{"type": "choice", "choice": "safe", "probabilities": map[string]float64{"safe": 0.99}}
			default: // the insights question: an option that was not offered
				answers[id] = map[string]any{"type": "choice", "choice": "not-a-category", "probabilities": map[string]float64{"not-a-category": 1}}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "d", "answers": answers})
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	up, err := h.store.CreateUpstream(ctx, "decider", "openai_compatible", srv.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "fake-decider", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	models, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: up.ID})
	m := models[0]
	_ = h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled)
	if err := h.store.SetModelClassifierRole(ctx, m.ID, store.ClassifierRoleDecision); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetClassifierProfile(ctx, store.ClassifierProfile{UpstreamID: up.ID, Model: m.Name, CallStyle: store.ClassifierStyleCombined}); err != nil {
		t.Fatal(err)
	}
	h.secgwPolicy(t, store.SecgwPolicy{Name: "closed guard", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckPromptInjection, Enabled: true, Mode: store.SecgwModeBlock, Fail: store.SecgwFailClosed, Direction: store.SecgwDirectionIngress,
			ClassifierModelID: m.ID, Options: json.RawMessage(`{"timeout_ms":3000}`)},
	}})
	// Baseline: with insights off the benign request passes the closed guard.
	if rec := h.do(http.MethodPost, "/v1/chat/completions", chatBody("summarise this meeting", false)); rec.Code != http.StatusOK {
		t.Fatalf("baseline without insights: %d %s", rec.Code, rec.Body.String())
	}
	sess := adminSession(t, h)
	settings := store.DefaultClassificationSettings()
	settings.Enabled, settings.UpstreamID, settings.Model = true, up.ID, m.Name // background mode
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/classification", settings); rec.Code != http.StatusOK {
		t.Fatalf("save settings: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()
	rec := h.do(http.MethodPost, "/v1/chat/completions", chatBody("summarise this meeting", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("turning on Usage insights (background) made a benign request fail: %d %s", rec.Code, rec.Body.String())
	}
}

// A8: an inline classification that times out made the request wait the
// whole timeout, but classifier_ms records 0 (finishClassification only
// counts the wait when the outcome is ok or Calls > 0, and a failed call
// has Calls == 0). The request log's "Classifier wait" hides exactly the
// slow cases.
func TestInsightsInlineTimeoutIsCountedInClassifierWait(t *testing.T) {
	h := newHarness(t)
	f := newFakeDecider(t)
	f.delay = 800 * time.Millisecond
	ctx := context.Background()
	up, err := h.store.CreateUpstream(ctx, "slow-decider", "openai_compatible", f.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	sess := adminSession(t, h)
	settings := store.DefaultClassificationSettings()
	settings.Enabled, settings.Mode, settings.UpstreamID, settings.Model, settings.TimeoutMs = true, store.ClassificationModeInline, up.ID, "fake-decider", 300
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/classification", settings); rec.Code != http.StatusOK {
		t.Fatalf("save settings: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()
	start := time.Now()
	rec := h.do(http.MethodPost, "/v1/chat/completions", chatBody("why does the borrow checker reject this", false))
	took := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rows := h.waitForClassifications(t, 1)
	if rows[0].Status != store.ClassificationStatusTimeout {
		t.Fatalf("precondition: want a timeout row, got %+v", rows[0])
	}
	ev := h.requestEvent(t)
	if ev.ClassifierMs < 250 {
		t.Fatalf("request waited %v on an inline classifier timeout but classifier_ms=%d", took, ev.ClassifierMs)
	}
}

// A14: a two-stage plan (taxonomy above the classifier's option limit) must
// not ride on the guard call: the guard call carries only the category
// question, so the subcategory would never be asked.
func TestInsightsTwoStagePlanDoesNotLoseItsSubcategory(t *testing.T) {
	h := newHarness(t)
	f := newFakeDecider(t)
	m := h.decisionGuard(t, f, "decider", "openai_compatible")
	ctx := context.Background()
	// 22 built-in roots + Rust = 23 flat options; a limit of 22 forces
	// category first, subcategory second.
	if err := h.store.SetClassifierProfile(ctx, store.ClassifierProfile{UpstreamID: m.UpstreamID, Model: m.Name, CallStyle: store.ClassifierStyleCombined, MaxOptions: 22}); err != nil {
		t.Fatal(err)
	}
	sess := adminSession(t, h)
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/classification/categories",
		map[string]any{"name": "Rust", "description": "Rust code.", "parent_slug": "coding"}); rec.Code != http.StatusCreated {
		t.Fatalf("create subcategory: %d %s", rec.Code, rec.Body.String())
	}
	settings := store.DefaultClassificationSettings()
	settings.Enabled, settings.Mode, settings.UpstreamID, settings.Model, settings.TimeoutMs = true, store.ClassificationModeInline, m.UpstreamID, m.Name, 2000
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/classification", settings); rec.Code != http.StatusOK {
		t.Fatalf("save settings: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()
	if rec := h.do(http.MethodPost, "/v1/chat/completions", chatBody("why does the borrow checker reject this", false)); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rows := h.waitForClassifications(t, 1)
	if rows[0].Calls < 2 {
		t.Fatalf("two-stage classification asked only the category (calls=%d, batches=%v): the subcategory is never asked", rows[0].Calls, f.seen())
	}
}

// A15: more enabled top-level categories than the classifier accepts in one
// question used to save as "Running" and then fail every classification
// with "too many options"; the admin page now says why it is not running.
func TestInsightsTooManyRootsForClassifierIsReportedNotRunning(t *testing.T) {
	h := newHarness(t)
	f := newFakeDecider(t)
	m := h.decisionGuard(t, f, "decider", "openai_compatible")
	if err := h.store.SetClassifierProfile(context.Background(), store.ClassifierProfile{UpstreamID: m.UpstreamID, Model: m.Name, MaxOptions: 10}); err != nil {
		t.Fatal(err)
	}
	sess := adminSession(t, h)
	settings := store.DefaultClassificationSettings()
	settings.Enabled, settings.UpstreamID, settings.Model = true, m.UpstreamID, m.Name
	rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/classification", settings)
	var doc classificationDocument
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	if rec.Code != http.StatusOK || doc.Running || doc.Problem == "" {
		t.Fatalf("22 roots on a 10-option classifier: running=%v problem=%q", doc.Running, doc.Problem)
	}
}
