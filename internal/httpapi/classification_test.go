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
)

// fakeDecisionServer is a System One endpoint that labels by keyword and
// records every state it was shown. Its wire shape matches what Ollama and
// OpenRouter returned in live probes.
type fakeDecisionServer struct {
	*httptest.Server
	mu     sync.Mutex
	states []string
	delay  time.Duration
}

func newFakeDecisionServer(t *testing.T) *fakeDecisionServer {
	f := &fakeDecisionServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State     string `json:"state"`
			Questions map[string]struct {
				Criteria map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.states = append(f.states, req.State)
		delay := f.delay
		f.mu.Unlock()
		time.Sleep(delay)
		answers := map[string]any{}
		for id, q := range req.Questions {
			choice := "other"
			switch {
			case id == "subcategory" && q.Criteria["rust"] != "":
				choice = "rust"
			case id == "subcategory":
				choice = "none"
			case strings.Contains(req.State, "borrow") && q.Criteria["coding|rust"] != "":
				// One flat question: category and subcategory together.
				choice = "coding|rust"
			case strings.Contains(req.State, "borrow"):
				choice = "coding"
			case strings.Contains(req.State, "landlord"):
				choice = "legal"
			}
			answers[id] = map[string]any{"type": "choice", "choice": choice, "probabilities": map[string]float64{choice: 0.97}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake-decider", "answers": answers, "usage": map[string]any{"input_tokens": 400, "output_tokens": 3}})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeDecisionServer) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.states...)
}

func (h *harness) enableClassification(t *testing.T, f *fakeDecisionServer, mode string, mutate func(*store.ClassificationSettings)) {
	t.Helper()
	ctx := context.Background()
	up, err := h.store.CreateUpstream(ctx, "decider-"+mode, "openai_compatible", f.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	sess := adminSession(t, h)
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/classification/categories",
		map[string]any{"name": "Rust", "description": "Rust code.", "parent_slug": "coding"}); rec.Code != http.StatusCreated {
		t.Fatalf("create subcategory: %d %s", rec.Code, rec.Body.String())
	}
	settings := store.DefaultClassificationSettings()
	settings.Enabled, settings.Mode, settings.UpstreamID, settings.Model, settings.TimeoutMs = true, mode, up.ID, "fake-decider", 2000
	if mutate != nil {
		mutate(&settings)
	}
	rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/classification", settings)
	if rec.Code != http.StatusOK {
		t.Fatalf("save settings: %d %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Running bool   `json:"running"`
		Problem string `json:"problem"`
	}
	decodeInto(t, rec, &doc)
	if !doc.Running {
		t.Fatalf("classification not running: %q", doc.Problem)
	}
}

func (h *harness) waitForClassifications(t *testing.T, want int) []store.ClassificationResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := h.store.ListClassificationResults(context.Background(), store.ClassificationResultFilter{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) >= want {
			return rows
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d classification results", want)
	return nil
}

func convoBody(model, text string) map[string]any {
	return map[string]any{"model": model, "messages": []map[string]any{
		{"role": "system", "content": "SYSTEM-SECRET"}, {"role": "user", "content": "earlier turn"}, {"role": "assistant", "content": "ok"},
		{"role": "user", "content": text}}}
}

func TestAsyncClassificationAddsNoLatencyAndStoresLabelsOnly(t *testing.T) {
	h := newHarness(t)
	f := newFakeDecisionServer(t)
	f.delay = 400 * time.Millisecond // a slow classifier must not slow requests
	h.enableClassification(t, f, store.ClassificationModeAsync, nil)

	start := time.Now()
	rec := h.do(http.MethodPost, "/v1/chat/completions", convoBody(h.model.Name, "why does the borrow checker reject this"))
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy: %d %s", rec.Code, rec.Body.String())
	}
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Fatalf("async classification delayed the request by %v", took)
	}
	if rec.Header().Get(headerCategory) != "" {
		t.Fatal("async mode must not set the category header")
	}
	rows := h.waitForClassifications(t, 1)
	r := rows[0]
	if r.Status != "ok" || r.Category != "coding" || r.Subcategory != "rust" || r.Mode != "async" || r.UserID != h.user.ID || r.ModelName != h.model.Name {
		t.Fatalf("result = %+v", r)
	}
	// The taxonomy fits one question, so category and subcategory come
	// from ONE call, timed and metered.
	if r.LatencyMs < 350 || r.Protocol != "systemone" || r.TokensIn != 400 || r.Calls != 1 || !strings.HasPrefix(r.Classifier, "decider-async / ") {
		t.Fatalf("one 400 ms call must be timed and metered: %+v", r)
	}
	seen := f.seen()
	if len(seen) != 1 || seen[0] != "why does the borrow checker reject this" {
		t.Fatalf("default scope is the last user turn without the system prompt, saw %q", seen)
	}
	for _, s := range seen {
		if strings.Contains(s, "SYSTEM-SECRET") {
			t.Fatal("system prompt sent although include_system_prompt is off")
		}
	}
	// Prompt text is never persisted.
	var stored int
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM classification_result WHERE probabilities LIKE '%borrow%' OR error LIKE '%borrow%'`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("prompt text leaked into storage (%d rows, %v)", stored, err)
	}
}

func TestInlineClassificationLabelsTheResponseWithinBudget(t *testing.T) {
	h := newHarness(t)
	f := newFakeDecisionServer(t)
	h.enableClassification(t, f, store.ClassificationModeInline, func(s *store.ClassificationSettings) {
		s.IncludeSystemPrompt, s.TextScope = true, store.ClassificationScopeAllUser
	})
	rec := h.do(http.MethodPost, "/v1/chat/completions", convoBody(h.model.Name, "my landlord kept the deposit"))
	if rec.Code != http.StatusOK || rec.Header().Get(headerCategory) != "legal" {
		t.Fatalf("inline: %d header %q", rec.Code, rec.Header().Get(headerCategory))
	}
	if seen := f.seen(); !strings.Contains(seen[0], "SYSTEM-SECRET") || !strings.Contains(seen[0], "earlier turn") {
		t.Fatalf("all-user scope with system prompt, saw %q", seen[0])
	}
	if rows := h.waitForClassifications(t, 1); rows[0].Mode != "inline" || rows[0].Category != "legal" {
		t.Fatalf("result = %+v", rows[0])
	}

	// A classifier slower than the budget: the request proceeds unlabelled
	// after the timeout, and the miss is recorded.
	f.mu.Lock()
	f.delay = 2 * time.Second
	f.mu.Unlock()
	settings, _ := h.store.ClassificationSettings(context.Background())
	settings.TimeoutMs = 150
	if rec := h.doAsSession(adminSession(t, h), http.MethodPut, "/api/v1/admin/classification", settings); rec.Code != http.StatusOK {
		t.Fatalf("save: %s", rec.Body.String())
	}
	start := time.Now()
	rec = h.do(http.MethodPost, "/v1/chat/completions", convoBody(h.model.Name, "my landlord again"))
	if rec.Code != http.StatusOK || rec.Header().Get(headerCategory) != "" || time.Since(start) > time.Second {
		t.Fatalf("timed-out inline classification must not block or fail: %d %q after %v", rec.Code, rec.Header().Get(headerCategory), time.Since(start))
	}
	rows := h.waitForClassifications(t, 2)
	if rows[0].Status != store.ClassificationStatusTimeout {
		t.Fatalf("latest result = %+v, want a recorded timeout", rows[0])
	}
}

func TestClassificationSkipsWhatItShould(t *testing.T) {
	h := newHarness(t)
	f := newFakeDecisionServer(t)
	h.enableClassification(t, f, store.ClassificationModeInline, func(s *store.ClassificationSettings) { s.IncludeServiceTokens = false })

	// Embeddings are not classified.
	h.do(http.MethodPost, "/v1/embeddings", map[string]any{"model": h.model.Name, "input": "the borrow checker"})
	// Service-token traffic is excluded by this setting.
	_, plaintext := h.newServiceToken(t, "svc", true)
	raw, _ := json.Marshal(convoBody(h.model.Name, "borrow checker"))
	if rec := h.doAsServiceToken(plaintext, http.MethodPost, "/v1/chat/completions", raw); rec.Code != http.StatusOK {
		t.Fatalf("service token chat: %d %s", rec.Code, rec.Body.String())
	}
	// A request refused by quota is never classified.
	q := &store.Quota{SubjectType: "user", SubjectID: h.user.ID, Metric: store.MetricRequests, Limit: 0, Window: store.WindowDaily, BreachBehavior: store.BreachLetFinish}
	if err := h.store.CreateQuota(context.Background(), q); err != nil {
		t.Fatalf("create quota: %v", err)
	}
	h.server.InvalidateConfigCache()
	h.server.invalidateQuotaRules()
	if rec := h.do(http.MethodPost, "/v1/chat/completions", convoBody(h.model.Name, "borrow checker")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("quota-refused request: %d, want 429", rec.Code)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(f.seen()); n != 0 {
		t.Fatalf("classifier was called %d times for requests it must skip", n)
	}
}

func TestClassificationDisabledOrBrokenNeverAffectsRequests(t *testing.T) {
	h := newHarness(t)
	f := newFakeDecisionServer(t)
	h.enableClassification(t, f, store.ClassificationModeInline, nil)
	f.Close() // classifier gone
	rec := h.do(http.MethodPost, "/v1/chat/completions", convoBody(h.model.Name, "borrow checker"))
	if rec.Code != http.StatusOK || rec.Header().Get(headerCategory) != "" {
		t.Fatalf("a dead classifier must not fail the request: %d", rec.Code)
	}
	if rows := h.waitForClassifications(t, 1); rows[0].Status != store.ClassificationStatusError || rows[0].Error == "" {
		t.Fatalf("result = %+v, want a recorded error", rows[0])
	}

	sess := adminSession(t, h)
	settings, _ := h.store.ClassificationSettings(context.Background())
	settings.Enabled = false
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/classification", settings); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	h.do(http.MethodPost, "/v1/chat/completions", convoBody(h.model.Name, "borrow checker"))
	time.Sleep(150 * time.Millisecond)
	if rows, _ := h.store.ListClassificationResults(context.Background(), store.ClassificationResultFilter{}); len(rows) != 1 {
		t.Fatalf("disabled classification still recorded results: %d", len(rows))
	}
}

func TestClassificationAdminAPI(t *testing.T) {
	h := newHarness(t)
	f := newFakeDecisionServer(t)
	sess := adminSession(t, h)
	up, err := h.store.CreateUpstream(context.Background(), "OpenRouter", "openai_compatible", "https://openrouter.ai/api", "", "")
	if err != nil {
		t.Fatal(err)
	}

	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/classification", nil)
	var doc classificationDocument
	decodeInto(t, rec, &doc)
	var or *classifierUpstream
	for i := range doc.Upstreams {
		if doc.Upstreams[i].ID == up.ID {
			or = &doc.Upstreams[i]
		}
	}
	if or == nil || or.SystemOneURL != "https://openrouter.ai/api/alpha/decisions" || len(or.DecisionModels) == 0 || or.DecisionModels[0] != "~typesafe/jev-latest" {
		t.Fatalf("OpenRouter must be offered with its decisions endpoint and Jev: %+v", or)
	}

	// Enabling without a model is refused with the field named.
	bad := doc.Settings
	bad.Enabled = true
	rec = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/classification", bad)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"param":"model"`) {
		t.Fatalf("enable without model: %d %s", rec.Code, rec.Body.String())
	}

	// Test endpoint: classifies with unsaved settings and stores nothing.
	decider, _ := h.store.CreateUpstream(context.Background(), "decider", "openai_compatible", f.URL, "", "")
	trial := doc.Settings
	trial.UpstreamID, trial.Model = decider.ID, "fake-decider"
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/classification/test", map[string]any{"text": "borrow checker", "settings": trial})
	var tr map[string]any
	decodeInto(t, rec, &tr)
	if rec.Code != http.StatusOK || tr["category"] != "coding" || tr["status"] != "ok" {
		t.Fatalf("test: %d %v", rec.Code, tr)
	}
	if rows, _ := h.store.ListClassificationResults(context.Background(), store.ClassificationResultFilter{}); len(rows) != 0 {
		t.Fatal("the test endpoint must not store results")
	}

	// Category CRUD and audit.
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/classification/categories", map[string]any{"name": "Reverse engineering", "description": "Binaries.", "parent_slug": "security"})
	var created struct {
		Category store.ClassificationCategory `json:"category"`
	}
	decodeInto(t, rec, &created)
	if rec.Code != http.StatusCreated || created.Category.Slug != "reverse_engineering" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsSession(sess, http.MethodPatch, "/api/v1/admin/classification/categories/"+created.Category.ID, map[string]any{"description": "Disassembly and firmware."})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Disassembly and firmware.") {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if rec = h.doAsSession(sess, http.MethodDelete, "/api/v1/admin/classification/categories/"+created.Category.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec = h.doAsSession(sess, http.MethodDelete, "/api/v1/admin/classification/categories/cat_coding", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("deleting a built-in: %d", rec.Code)
	}
	entries, _, err := h.store.ListAudit(context.Background(), store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, e := range entries {
		actions[e.Action] = true
	}
	for _, a := range []string{"classification_category_created", "classification_category_updated", "classification_category_deleted"} {
		if !actions[a] {
			t.Errorf("missing audit entry %s", a)
		}
	}

	// Non-admins are refused.
	member, _, _ := h.store.UpsertUserFromIdentity(context.Background(), "sub-member", "m@example.com", "Member", false, false)
	ms, _ := h.server.Sessions.Create(context.Background(), member.ID)
	if rec := h.doAsSession(ms, http.MethodGet, "/api/v1/admin/classification/report", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("member got %d from the admin report", rec.Code)
	}
}
