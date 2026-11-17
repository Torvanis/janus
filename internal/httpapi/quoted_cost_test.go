package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Owner ruling: whenever the upstream provider quotes the cost of a call,
// that exact quote is the call's cost, over the rate card AND over an
// administrator's price. These tests run each provider's documented
// response shape through the real proxy against a model whose rate card an
// administrator overrode to an absurd $1,000 per million tokens: any
// leakage of the rate card into a quoted call shows up as millions of
// nano-USD.

// quotedUpstream answers every request with body (and headers), as
// text/event-stream when stream is set (body is then the SSE frames).
func quotedUpstream(t *testing.T, h *harness, body string, stream bool, header map[string]string) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range header {
			w.Header().Set(k, v)
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for _, frame := range strings.Split(body, "\n") {
				_, _ = w.Write([]byte(frame + "\n\n"))
				w.(http.Flusher).Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(up.Close)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, up.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}
}

// adminOverpricedModel sets administrator rate overrides of $1,000 per
// million input and output tokens on the harness model. Unquoted calls are
// priced at them (10 in + 2 out = $0.012 = 12,000,000 nano-USD).
func adminOverpricedModel(t *testing.T, h *harness) {
	t.Helper()
	session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec := h.patchModel(session, h.model.ID, map[string]any{
		"rate_in_usd_per_mtok": 1000, "rate_out_usd_per_mtok": 1000, "rate_cached_usd_per_mtok": 1000,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin price override: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()
}

const adminCardNano = 12_000_000 // 10 in + 2 out at the admin's $1,000/M

func quotedChatBody(stream bool) map[string]any {
	b := map[string]any{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}}
	if stream {
		b["stream"] = true
	}
	return b
}

func TestProviderQuotedCostIsTheCost(t *testing.T) {
	msg := `"choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}]`
	for _, tc := range []struct {
		name     string
		path     string
		body     string
		stream   bool
		header   map[string]string
		wantNano int64
		wantMode string
	}{
		{name: "openrouter buffered, sub-nano digits", path: "/v1/chat/completions",
			body:     `{"id":"gen-1",` + msg + `,"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"cost":0.000123456789,"is_byok":false,"cost_details":{"upstream_inference_cost":null}}}`,
			wantNano: 123_457, wantMode: usage.AccountingUpstreamCost},
		{name: "openrouter byok: fee plus the provider's inference", path: "/v1/chat/completions",
			body:     `{"id":"gen-1",` + msg + `,"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.00001,"is_byok":true,"cost_details":{"upstream_inference_cost":0.0002}}}`,
			wantNano: 210_000, wantMode: usage.AccountingUpstreamCost},
		{name: "openrouter free model quotes $0", path: "/v1/chat/completions",
			body:     `{"id":"gen-1",` + msg + `,"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0}}`,
			wantNano: 0, wantMode: usage.AccountingUpstreamCost},
		{name: "requesty", path: "/v1/chat/completions",
			body:     `{"id":"r-1",` + msg + `,"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"cost":0.0000935}}`,
			wantNano: 93_500, wantMode: usage.AccountingUpstreamCost},
		{name: "perplexity cost object", path: "/v1/chat/completions",
			body:     `{"id":"p-1",` + msg + `,"usage":{"prompt_tokens":10,"completion_tokens":2,"search_context_size":"low","cost":{"input_tokens_cost":2.4e-05,"output_tokens_cost":0.006585,"request_cost":0.006,"total_cost":0.012609}}}`,
			wantNano: 12_609_000, wantMode: usage.AccountingUpstreamCost},
		{name: "deepinfra estimated_cost", path: "/v1/chat/completions",
			body:     `{"id":"d-1",` + msg + `,"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"estimated_cost":0.0000268}}`,
			wantNano: 26_800, wantMode: usage.AccountingUpstreamCost},
		{name: "xai ticks", path: "/v1/chat/completions",
			body:     `{"id":"x-1",` + msg + `,"usage":{"prompt_tokens":10,"completion_tokens":2,"cost_in_usd_ticks":158500}}`,
			wantNano: 15_850, wantMode: usage.AccountingUpstreamCost},
		{name: "litellm header", path: "/v1/chat/completions",
			body:     `{"id":"l-1",` + msg + `,"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
			header:   map[string]string{"x-litellm-response-cost": "0.00042"},
			wantNano: 420_000, wantMode: usage.AccountingUpstreamCost},
		{name: "openrouter embeddings", path: "/v1/embeddings",
			body:     `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"m","usage":{"prompt_tokens":10,"total_tokens":10,"cost":0.0000001}}`,
			wantNano: 100, wantMode: usage.AccountingUpstreamCost},
		{name: "openrouter stream: final usage chunk", path: "/v1/chat/completions", stream: true,
			body: `data: {"choices":[{"delta":{"content":"Hi"}}]}` + "\n" +
				`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n" +
				`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.0000123456}}` + "\n" +
				`data: [DONE]`,
			wantNano: 12_346, wantMode: usage.AccountingUpstreamCost},
		{name: "openrouter responses stream: response.completed", path: "/v1/responses", stream: true,
			body: `data: {"type":"response.output_text.delta","delta":"Hi"}` + "\n" +
				`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"cost":0.95}}}`,
			wantNano: 950_000_000, wantMode: usage.AccountingUpstreamCost},
		{name: "missing quote falls back to the (admin) rate card", path: "/v1/chat/completions",
			body:     `{"id":"o-1",` + msg + `,"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
			wantNano: adminCardNano, wantMode: usage.AccountingUpstream},
		{name: "missing quote on a stream falls back to the rate card", path: "/v1/chat/completions", stream: true,
			body: `data: {"choices":[{"delta":{"content":"Hi"},"finish_reason":"stop"}]}` + "\n" +
				`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}` + "\n" +
				`data: [DONE]`,
			wantNano: adminCardNano, wantMode: usage.AccountingUpstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			adminOverpricedModel(t, h)
			quotedUpstream(t, h, tc.body, tc.stream, tc.header)
			body := quotedChatBody(tc.stream)
			switch tc.path {
			case "/v1/embeddings":
				body = map[string]any{"model": "test-model", "input": "hi"}
			case "/v1/responses":
				body = map[string]any{"model": "test-model", "input": "hi", "stream": true}
			}
			rec := h.do(http.MethodPost, tc.path, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
			}
			if !tc.stream && strings.Contains(tc.body, `"cost"`) && !strings.Contains(rec.Body.String(), `"cost"`) {
				t.Errorf("the client's response lost the provider's cost field: %s", rec.Body.String())
			}
			event := h.waitForUsageEvents(1)[0]
			if event.CostNano != tc.wantNano || event.AccountingMode != tc.wantMode {
				t.Fatalf("cost = %d nano (%s), want %d (%s)", event.CostNano, event.AccountingMode, tc.wantNano, tc.wantMode)
			}
			if event.TokensIn != 10 {
				t.Errorf("tokens in = %d, want 10 (counts are kept beside a quote)", event.TokensIn)
			}
			if tc.wantMode != usage.AccountingUpstreamCost {
				if event.QuotedBy != "" {
					t.Errorf("cost_quoted_by = %q on an unquoted row", event.QuotedBy)
				}
				if event.EstimatedCostNano != nil {
					t.Errorf("an unquoted row carries an estimate (%d): its cost is Janus's price", *event.EstimatedCostNano)
				}
				return
			}
			// Janus's own price for the same call is kept beside the quote,
			// at the admin's card, and never replaces it (checked above).
			wantEstimate := int64(adminCardNano)
			if tc.path == "/v1/embeddings" {
				wantEstimate = 10_000_000 // 10 in, no output
			}
			if event.EstimatedCostNano == nil || *event.EstimatedCostNano != wantEstimate {
				t.Errorf("Janus's estimate = %v, want %d", event.EstimatedCostNano, wantEstimate)
			}
			var upstreamName string
			_ = h.store.DB().QueryRowContext(context.Background(), `SELECT name FROM upstream WHERE id = ?`, event.UpstreamID).Scan(&upstreamName)
			if event.QuotedBy == "" || event.QuotedBy != upstreamName {
				t.Errorf("cost_quoted_by = %q, want the upstream's name %q", event.QuotedBy, upstreamName)
			}
			wantStatus := "priced"
			if tc.wantNano == 0 {
				wantStatus = "known_free"
			}
			var status string
			_ = h.store.DB().QueryRowContext(context.Background(), `SELECT cost_status FROM reporting_usage_snapshot WHERE usage_id = ?`, event.ID).Scan(&status)
			if status != wantStatus {
				t.Errorf("cost_status = %q, want %q", status, wantStatus)
			}
			if !tc.stream {
				if got := rec.Header().Get("X-Janus-Token-Accounting"); got != usage.AccountingUpstreamCost {
					t.Errorf("X-Janus-Token-Accounting = %q", got)
				}
			}
		})
	}
}

// A model with NO rate card (an OpenRouter router priced "-1") is still
// priced when the provider quotes the call: reports must not call it
// unpriced.
func TestQuotedCostOnUnpricedModelIsPriced(t *testing.T) {
	h := newHarness(t)
	if err := h.store.SetModelRates(context.Background(), h.model.ID, store.RateCard{}); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	quotedUpstream(t, h, `{"id":"gen-1","choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.0004}}`, false, nil)
	if rec := h.do(http.MethodPost, "/v1/chat/completions", quotedChatBody(false)); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	event := h.waitForUsageEvents(1)[0]
	var status string
	_ = h.store.DB().QueryRowContext(context.Background(), `SELECT cost_status FROM reporting_usage_snapshot WHERE usage_id = ?`, event.ID).Scan(&status)
	if event.CostNano != 400_000 || status != "priced" {
		t.Fatalf("cost %d status %q, want 400000 priced", event.CostNano, status)
	}
}

// Spend quotas count the quoted cost, not the rate card. The admin's card
// would price each call at $0.012 and trip a $0.00015 limit on the first
// call; the quotes ($0.0000935 each) trip it only after the second.
func TestSpendQuotaCountsQuotedCost(t *testing.T) {
	h := newHarness(t)
	adminOverpricedModel(t, h)
	quotedUpstream(t, h, `{"id":"r-1","choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.0000935}}`, false, nil)
	if err := h.store.CreateQuota(context.Background(), &store.Quota{
		SubjectType: "user", SubjectID: h.user.ID, Metric: store.MetricCostUSD, Limit: 150_000,
		Window: store.WindowDaily, BreachBehavior: store.BreachHardKill,
	}); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests} {
		rec := h.do(http.MethodPost, "/v1/chat/completions", quotedChatBody(false))
		if rec.Code != want {
			t.Fatalf("call %d: %d %s, want %d", i+1, rec.Code, rec.Body.String(), want)
		}
		if err := h.server.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// A finished batch whose every result quotes its cost (xAI batch results
// carry cost_in_usd_ticks) is billed the sum of the quotes, not the rate
// card at the batch discount; one unquoted result sends the whole batch
// back to the card.
func TestFinishedBatchBillsQuotedCosts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		second   string
		wantNano int64
		wantMode string
	}{
		{"every result quoted", `"cost_in_usd_ticks":2500`, 500, usage.AccountingUpstreamCost},
		{"one result unquoted", `"x":0`, usage.ComputeCost(200, 100, 0, usage.Rates{InNano: 5 * usage.NanoPerUSD, OutNano: 15 * usage.NanoPerUSD}), usage.AccountingUpstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			h.uploadFile(t, h.token, "file-batchin001")
			line := batchLineJSON("1", "/v1/chat/completions", `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
			h.route("GET /v1/files/file-batchin001/content", line+"\n"+strings.Replace(line, `"1"`, `"2"`, 1)+"\n")
			h.route("POST /v1/batches", `{"id":"batch_good0001","object":"batch","status":"validating"}`)
			if rec := h.createBatch(h.token, "file-batchin001"); rec.Code != http.StatusOK {
				t.Fatalf("create batch: %d %s", rec.Code, rec.Body.String())
			}
			before := len(h.waitForUsageEvents(2))
			h.route("GET /v1/batches/batch_good0001", `{"id":"batch_good0001","status":"completed","output_file_id":"file-batchout01"}`)
			first := `{"custom_id":"1","response":{"status_code":200,"body":{"model":"test-model","usage":{"prompt_tokens":100,"completion_tokens":50,"cost_in_usd_ticks":2500}}}}`
			second := `{"custom_id":"2","response":{"status_code":200,"body":{"model":"test-model","usage":{"prompt_tokens":100,"completion_tokens":50,` + tc.second + `}}}}`
			h.route("GET /v1/files/file-batchout01/content", first+"\n"+second+"\n")
			jobs, err := h.store.ClaimObjectJobs(ctx, 10, 0)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("open jobs: %d %v", len(jobs), err)
			}
			if !h.server.settleJob(ctx, jobs[0]) {
				t.Fatal("the finished batch was not settled")
			}
			if err := h.server.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			var metered *store.UsageEvent
			for _, e := range h.waitForUsageEvents(before + 1) {
				if strings.HasPrefix(e.RequestID, "batch:") {
					metered = e
				}
			}
			if metered == nil {
				t.Fatal("no metered batch event")
			}
			if metered.CostNano != tc.wantNano || metered.AccountingMode != tc.wantMode || metered.TokensIn != 200 {
				t.Fatalf("batch: cost %d (%s) in %d, want %d (%s) in 200", metered.CostNano, metered.AccountingMode, metered.TokensIn, tc.wantNano, tc.wantMode)
			}
		})
	}
}
