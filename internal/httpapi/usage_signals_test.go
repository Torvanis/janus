package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
	"math"
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

// A calculated input rate counts only the prompt the provider processed. With
// 1,000 prompt tokens of which 900 were cache hits, the reported rate must be
// the rate for 100 tokens — the old code divided all 1,000 by the same window
// and over-reported prefill speed 10x on a cache-heavy request.
func TestProxyCalculatedInputThroughputExcludesCachedTokens(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":900}}}`))
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
	in, err := strconv.ParseFloat(rec.Header().Get(headerCalcTokensInPerSecond), 64)
	if err != nil {
		t.Fatalf("%s = %q: %v", headerCalcTokensInPerSecond, rec.Header().Get(headerCalcTokensInPerSecond), err)
	}
	out, err := strconv.ParseFloat(rec.Header().Get(headerCalcTokensOutPerSecond), 64)
	if err != nil || out <= 0 {
		t.Fatalf("%s = %q", headerCalcTokensOutPerSecond, rec.Header().Get(headerCalcTokensOutPerSecond))
	}
	// A buffered response shares one window for both phases, so the ratio
	// of the two rates is exactly processed-input : output = 100 : 10.
	if ratio := in / out; ratio < 9.9 || ratio > 10.1 {
		t.Fatalf("input/output rate ratio = %.2f (in=%.2f out=%.2f), want 10 — cached tokens must not count as processed input", ratio, in, out)
	}
	event := h.waitForUsageEvents(1)[0]
	if event.TokensIn != 1000 || event.TokensCached != 900 {
		t.Fatalf("event tokens in=%d cached=%d, want 1000/900 (counts stay as reported)", event.TokensIn, event.TokensCached)
	}
	if r := event.TokensInPerSecond / event.TokensOutPerSecond; r < 9.9 || r > 10.1 {
		t.Fatalf("recorded input/output rate ratio = %.2f, want 10", r)
	}
}

// Gemini streams no thinking: on gemini-2.5-flash, 39 visible output tokens
// followed 640 ms of hidden thinking (12 visible + 39 thinking tokens metered
// as output; first byte at 642 ms of a 665 ms call). Splitting at TTFB
// credited the thinking to prompt processing and divided all 51 output tokens
// by the last 23 ms: 13,360 tokens/s recorded in testing. With hidden
// reasoning both phases share the whole call.
func TestCalculatedThroughputWithHiddenReasoningUsesWholeCall(t *testing.T) {
	elapsed, ttfb := 665*time.Millisecond, 642*time.Millisecond
	event := &store.UsageEvent{Streaming: true, TokensIn: 12, TokensOut: 51}
	applyThroughput(event, adapter.Usage{HiddenReasoning: true}, elapsed, ttfb)
	if want := 51 / elapsed.Seconds(); math.Abs(event.TokensOutPerSecond-want) > 0.01 {
		t.Fatalf("output rate = %.1f tok/s, want %.1f (51 tokens over the whole 665 ms)", event.TokensOutPerSecond, want)
	}
	if want := 12 / elapsed.Seconds(); math.Abs(event.TokensInPerSecond-want) > 0.01 {
		t.Fatalf("input rate = %.1f tok/s, want %.1f", event.TokensInPerSecond, want)
	}
	// Without hidden reasoning a stream still splits at the first byte.
	plain := &store.UsageEvent{Streaming: true, TokensIn: 12, TokensOut: 51}
	applyThroughput(plain, adapter.Usage{}, elapsed, ttfb)
	if want := 51 / (elapsed - ttfb).Seconds(); math.Abs(plain.TokensOutPerSecond-want) > 0.01 {
		t.Fatalf("plain stream output rate = %.1f, want %.1f", plain.TokensOutPerSecond, want)
	}
}
