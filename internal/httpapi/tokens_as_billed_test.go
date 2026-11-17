package httpapi

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// seedBilledModel registers an enabled, priced and granted model on a new
// upstream of the given adapter type.
func seedBilledModel(t *testing.T, h *harness, upstreamName, adapterType, baseURL, encKey, modelName string) {
	t.Helper()
	ctx := context.Background()
	up, err := h.store.CreateUpstream(ctx, upstreamName, adapterType, baseURL, encKey, "")
	if err != nil {
		t.Fatalf("create %s upstream: %v", adapterType, err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, modelName, []string{"chat"}); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	models, err := h.store.ListModels(ctx, store.ModelFilter{Search: modelName})
	if err != nil || len(models) != 1 {
		t.Fatalf("find model %s: %v (%d found)", modelName, err, len(models))
	}
	m := models[0]
	if err := h.store.SetModelRates(ctx, m.ID, store.RateCard{
		RateInNano: 3 * usage.NanoPerUSD, RateOutNano: 15 * usage.NanoPerUSD, RateCachedNano: usage.NanoFromUSD(0.30),
		EffectiveFrom: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("set rates: %v", err)
	}
	if err := h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled); err != nil {
		t.Fatalf("enable model: %v", err)
	}
	if _, err := h.store.CreateGrant(ctx, m.ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant model: %v", err)
	}
	h.server.InvalidateConfigCache()
}

// fakeAnthropic answers 50,000 fresh input + 40,000 cache-read tokens, 20
// out, buffered or streamed.
func fakeAnthropic(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			for _, frame := range []string{
				"event: message_start",
				`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":50000,"output_tokens":0,"cache_read_input_tokens":40000}}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`,
				`data: {"type":"message_stop"}`,
			} {
				_, _ = w.Write([]byte(frame + "\n\n"))
				flusher.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],` +
			`"usage":{"input_tokens":50000,"output_tokens":20,"cache_read_input_tokens":40000}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Input tokens include cached tokens, as providers bill them. The worked
// example — an Anthropic request of 50,000 fresh + 40,000 cache-read tokens —
// records tokens_in 90,000 (44% cached) at an unchanged cost, buffered and
// streamed; Bedrock likewise; the OpenAI-format response the client gets says
// prompt_tokens 90,000 with cached_tokens 40,000; the CSV export and the
// Prometheus input counter carry the same 90,000; the console totals do not
// add the cache part a second time.
func TestTokensInIsTheWholePromptAsBilled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	anthropicUp := fakeAnthropic(t)
	bedrockUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"stopReason":"end_turn",` +
			`"usage":{"inputTokens":2000,"outputTokens":20,"cacheReadInputTokens":90000}}`))
	}))
	t.Cleanup(bedrockUp.Close)
	seedBilledModel(t, h, "anthropic-billed", "anthropic", anthropicUp.URL, "", "claude-billed")
	encKey, err := h.server.Cipher.Encrypt("AKIDEXAMPLE:testsecretkey")
	if err != nil {
		t.Fatalf("encrypt bedrock credential: %v", err)
	}
	seedBilledModel(t, h, "bedrock-billed", "bedrock", bedrockUp.URL, encKey, "anthropic.claude-billed-v1")

	// $3/M in, $0.30/M cached, $15/M out — the cost each provider bills.
	costOf := func(fresh, cached, out int64) int64 {
		return (fresh*3*usage.NanoPerUSD + cached*usage.NanoFromUSD(0.30) + out*15*usage.NanoPerUSD) / usage.TokensPerRateUnit
	}
	type call struct {
		model           string
		stream          bool
		in, cached, out int64
		cost            int64 // 0 = the harness model's own rate card
	}
	calls := []call{
		// The harness upstream is OpenAI-compatible: 100 prompt tokens, 20 of them cached.
		{model: "test-model", in: 100, cached: 20, out: 50},
		{model: "claude-billed", in: 90_000, cached: 40_000, out: 20, cost: costOf(50_000, 40_000, 20)},
		{model: "claude-billed", stream: true, in: 90_000, cached: 40_000, out: 20, cost: costOf(50_000, 40_000, 20)},
		{model: "anthropic.claude-billed-v1", in: 92_000, cached: 90_000, out: 20, cost: costOf(2_000, 90_000, 20)},
	}
	for i, c := range calls {
		body := map[string]any{"model": c.model, "messages": []map[string]string{{"role": "user", "content": "hi"}}}
		if c.stream {
			body["stream"] = true
			body["stream_options"] = map[string]any{"include_usage": true}
		}
		rec := h.do(http.MethodPost, "/v1/chat/completions", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d (%s) returned %d: %s", i, c.model, rec.Code, rec.Body.String())
		}
		if c.model != "test-model" {
			// The OpenAI-format answer uses OpenAI's convention.
			u := lastUsage(t, rec.Body.String())
			details, _ := u["prompt_tokens_details"].(map[string]any)
			if u["prompt_tokens"] != float64(c.in) || u["total_tokens"] != float64(c.in+c.out) || details["cached_tokens"] != float64(c.cached) {
				t.Errorf("%s stream=%v: client usage = %v, want prompt_tokens %d, total %d, cached_tokens %d", c.model, c.stream, u, c.in, c.in+c.out, c.cached)
			}
		}
		events := h.waitForUsageEvents(i + 1)
		var got *store.UsageEvent
		for _, e := range events {
			if e.ModelName == c.model && e.Streaming == c.stream {
				got = e
				break
			}
		}
		if got == nil {
			t.Fatalf("call %d (%s): no usage event among %d", i, c.model, len(events))
		}
		if got.TokensIn != c.in || got.TokensCached != c.cached {
			t.Errorf("%s stream=%v: tokens_in=%d cached=%d, want %d/%d", c.model, c.stream, got.TokensIn, got.TokensCached, c.in, c.cached)
		}
		if c.cost != 0 && got.CostNano != c.cost {
			t.Errorf("%s stream=%v: cost = %d nano-USD, want %d (unchanged)", c.model, c.stream, got.CostNano, c.cost)
		}
	}

	wantIn := int64(100 + 90_000 + 90_000 + 92_000)
	totals, err := h.store.AggregateUsage(ctx, store.UsageScope{UserID: h.user.ID})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if totals.TokensIn != wantIn || totals.TokensCached != 20+40_000+40_000+90_000 {
		t.Errorf("totals: tokens_in=%d cached=%d, want %d (no part counted twice)", totals.TokensIn, totals.TokensCached, wantIn)
	}
	if got := testutil.ToFloat64(h.server.Metrics.TokensIn.WithLabelValues("claude-billed", "chat", "anthropic-billed")); got != 180_000 {
		t.Errorf("janus_tokens_in_total for Claude = %v, want 180000 (two 90k prompts)", got)
	}

	rec := h.do(http.MethodGet, "/api/v1/requests.csv?range=day", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export returned %d: %s", rec.Code, rec.Body.String())
	}
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil || len(records) < 2 {
		t.Fatalf("csv: %v (%d records)", err, len(records))
	}
	col := map[string]int{}
	for i, name := range records[0] {
		col[name] = i
	}
	if _, ok := col["tokens_prompt"]; ok {
		t.Error("the superseded tokens_prompt column is still exported")
	}
	var csvIn int
	for _, r := range records[1:] {
		if r[col["model"]] == "claude-billed" && r[col["tokens_in"]] != "90000" {
			t.Errorf("csv tokens_in for claude-billed = %s, want 90000", r[col["tokens_in"]])
		}
		if r[col["model"]] == "claude-billed" {
			csvIn++
		}
	}
	if csvIn != 2 {
		t.Errorf("csv has %d claude-billed rows, want 2", csvIn)
	}
}

// lastUsage returns the usage object of a chat.completion body or of the
// last chunk that carries one.
func lastUsage(t *testing.T, body string) map[string]any {
	t.Helper()
	var last map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		var v map[string]any
		if line != "" && json.Unmarshal([]byte(line), &v) == nil {
			if u, ok := v["usage"].(map[string]any); ok {
				last = u
			}
		}
	}
	if last == nil {
		t.Fatalf("no usage in %s", body)
	}
	return last
}

// A tokens_in quota counts the whole prompt: one Anthropic request of 50,000
// fresh + 40,000 cache-read tokens uses 90,000 of a 60,000 limit, so the next
// request is refused. (Counting only the fresh part, 50,000, it was not.)
func TestTokensInQuotaCountsTheWholePrompt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedBilledModel(t, h, "anthropic-quota", "anthropic", fakeAnthropic(t).URL, "", "claude-quota")
	if err := h.store.CreateQuota(ctx, &store.Quota{
		SubjectType: "user", SubjectID: h.user.ID, Metric: store.MetricTokensIn,
		Limit: 60_000, Window: store.WindowDaily, BreachBehavior: store.BreachLetFinish,
	}); err != nil {
		t.Fatalf("create quota: %v", err)
	}
	ask := func() *httptest.ResponseRecorder {
		return h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
			"model": "claude-quota", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		})
	}
	if first := ask(); first.Code != http.StatusOK {
		t.Fatalf("first request returned %d, want 200: %s", first.Code, first.Body.String())
	}
	h.waitForUsageEvents(1)
	var used int64
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && used == 0 {
		statuses, err := h.server.Quota.Status(ctx, h.user.ID, nil)
		if err != nil {
			t.Fatalf("quota status: %v", err)
		}
		for _, st := range statuses {
			if st.Quota.Metric == store.MetricTokensIn {
				used = st.Current
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if used != 90_000 {
		t.Fatalf("tokens_in quota used = %d after one request, want 90000 (50,000 fresh + 40,000 cache reads)", used)
	}
	second := ask()
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("the request after a 90,000-token prompt against a 60,000 tokens_in quota returned %d, want 429", second.Code)
	}
	if code := decodeBody(t, second)["error"].(map[string]any)["code"]; code != CodeQuotaExceeded {
		t.Errorf("error code = %v, want %s", code, CodeQuotaExceeded)
	}
}
