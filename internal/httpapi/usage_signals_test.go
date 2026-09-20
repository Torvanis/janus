package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/torvanis/janus/internal/usage"
)

// An X.ai-style image response carries no token counts, only
// cost_in_usd_ticks. That exact figure must be the recorded cost — not a
// byte-count guess, and not zero.
func TestProxyBillsUpstreamReportedCost(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 7e8 ticks × 1e-10 USD/tick = $0.07.
		_, _ = w.Write([]byte(`{"created":1,"data":[{"url":"https://img.example/1.png"}],"usage":{"cost_in_usd_ticks":700000000}}`))
	}))
	t.Cleanup(up.Close)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, up.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}

	rec := h.do(http.MethodPost, "/v1/images/generations", map[string]any{"model": "test-model", "prompt": "a cat"})
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Janus-Cost-USD"); got != "0.07" {
		t.Errorf("X-Janus-Cost-USD = %q, want 0.07", got)
	}
	if got := rec.Header().Get("X-Janus-Token-Accounting"); got != usage.AccountingUpstreamCost {
		t.Errorf("X-Janus-Token-Accounting = %q, want %q", got, usage.AccountingUpstreamCost)
	}

	event := h.waitForUsageEvents(1)[0]
	if event.Modality != "image" {
		t.Fatalf("modality = %q, want image", event.Modality)
	}
	if event.AccountingMode != usage.AccountingUpstreamCost {
		t.Errorf("accounting mode = %q, want %q", event.AccountingMode, usage.AccountingUpstreamCost)
	}
	if event.CostNano != 70_000_000 {
		t.Errorf("cost = %d nano-USD, want 70000000 ($0.07)", event.CostNano)
	}
	if event.TokensIn != 0 || event.TokensOut != 0 {
		t.Errorf("tokens = (%d,%d), want (0,0): no counts were reported and none may be invented", event.TokensIn, event.TokensOut)
	}
}

// Streamed TTS reports exact usage on speech.audio.done; the same model that
// is unmetered on the binary path must be metered exactly here.
func TestProxyMetersStreamedTTSFromDoneFrame(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		for _, frame := range []string{
			`data: {"type":"speech.audio.delta","audio":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`,
			`data: {"type":"speech.audio.done","usage":{"input_tokens":12,"output_tokens":48,"total_tokens":60}}`,
		} {
			_, _ = w.Write([]byte(frame + "\n\n"))
			f.Flush()
		}
	}))
	t.Cleanup(up.Close)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, up.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}

	rec := h.do(http.MethodPost, "/v1/audio/speech", map[string]any{
		"model": "test-model", "input": "Hello from Janus.", "voice": "alloy", "stream_format": "sse",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	event := h.waitForUsageEvents(1)[0]
	if event.Modality != "tts" || !event.Streaming {
		t.Fatalf("modality=%q streaming=%v, want tts stream", event.Modality, event.Streaming)
	}
	if event.AccountingMode != usage.AccountingUpstream {
		t.Errorf("accounting mode = %q, want %q", event.AccountingMode, usage.AccountingUpstream)
	}
	if event.TokensIn != 12 || event.TokensOut != 48 {
		t.Errorf("tokens = (%d,%d), want (12,48)", event.TokensIn, event.TokensOut)
	}
}

// Without a provider measurement the gateway derives throughput from its own
// clock and says so: the header names carry "Calculated" and the source
// header reads "calculated".
func TestProxyReturnsCalculatedThroughputHeaders(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerThroughputSource); got != usage.ThroughputCalculated {
		t.Fatalf("%s = %q, want calculated", headerThroughputSource, got)
	}
	out, err := strconv.ParseFloat(rec.Header().Get(headerCalcTokensOutPerSecond), 64)
	if err != nil || out <= 0 {
		t.Errorf("%s = %q, want a positive number", headerCalcTokensOutPerSecond, rec.Header().Get(headerCalcTokensOutPerSecond))
	}
	if rec.Header().Get(headerTokensOutPerSecond) != "" {
		t.Errorf("%s must be absent when the figure is calculated, not reported", headerTokensOutPerSecond)
	}
	event := h.waitForUsageEvents(1)[0]
	if event.ThroughputSource != usage.ThroughputCalculated || event.TokensOutPerSecond <= 0 {
		t.Errorf("event throughput = %+v/%q, want calculated > 0", event.TokensOutPerSecond, event.ThroughputSource)
	}
}

// Provider-measured throughput (Groq-style prompt_time/completion_time) is
// forwarded under the un-prefixed header names and recorded as upstream.
func TestProxyForwardsReportedThroughput(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_time":0.01,"completion_time":0.1}}`))
	}))
	t.Cleanup(up.Close)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, up.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerThroughputSource); got != usage.ThroughputUpstream {
		t.Fatalf("%s = %q, want upstream", headerThroughputSource, got)
	}
	if got := rec.Header().Get(headerTokensInPerSecond); got != "10000.00" {
		t.Errorf("%s = %q, want 10000.00", headerTokensInPerSecond, got)
	}
	if got := rec.Header().Get(headerTokensOutPerSecond); got != "500.00" {
		t.Errorf("%s = %q, want 500.00", headerTokensOutPerSecond, got)
	}
	if rec.Header().Get(headerCalcTokensOutPerSecond) != "" {
		t.Errorf("calculated header must be absent when the provider reported throughput")
	}
	event := h.waitForUsageEvents(1)[0]
	if event.ThroughputSource != usage.ThroughputUpstream || event.TokensOutPerSecond != 500 {
		t.Errorf("event throughput = %v/%q, want 500/upstream", event.TokensOutPerSecond, event.ThroughputSource)
	}
}

// Streams cannot carry headers after the first byte, so throughput arrives as
// HTTP trailers at the end of the stream.
func TestProxyStreamCarriesThroughputTrailers(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	trailer := rec.Result().Trailer
	if got := trailer.Get(headerThroughputSource); got != usage.ThroughputCalculated {
		t.Fatalf("trailer %s = %q, want calculated (trailers: %v)", headerThroughputSource, got, trailer)
	}
	if v, err := strconv.ParseFloat(trailer.Get(headerCalcTokensOutPerSecond), 64); err != nil || v <= 0 {
		t.Errorf("trailer %s = %q, want positive", headerCalcTokensOutPerSecond, trailer.Get(headerCalcTokensOutPerSecond))
	}
}
