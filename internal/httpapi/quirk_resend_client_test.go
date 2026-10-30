package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
)

const refuseTemperature = `{"error":{"message":"Unsupported value: 'temperature' does not support 0.7 with this model. Only the default (1) value is supported.","type":"invalid_request_error","param":"temperature","code":"unsupported_value"}}`

// quirkUpstream refuses any body carrying a temperature; the fitted resend
// (temperature dropped) is answered by serve.
func quirkUpstream(t *testing.T, refusals *atomic.Int32, serve func(w http.ResponseWriter, body map[string]any)) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		if _, ok := b["temperature"]; ok {
			refusals.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(refuseTemperature))
			return
		}
		serve(w, b)
	}))
	t.Cleanup(up.Close)
	return up
}

// A streamed chat that is refused once for a setting and resent fitted is
// bounded by silence on the resend too, exactly like the first send: a
// stream that keeps sending for longer than the total timeout finishes.
func TestQuirkResendOfAStreamKeepsTheIdleTimer(t *testing.T) {
	h := newHarness(t)
	var refusals atomic.Int32
	up := quirkUpstream(t, &refusals, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 0; i < 12; i++ {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
			f.Flush()
			time.Sleep(50 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, up.URL, true, "", ""); err != nil {
		t.Fatal(err)
	}
	h.server.Config.UpstreamTotalTimeout = 300 * time.Millisecond // the stream lasts ~600 ms

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "stream": true, "temperature": 0.7,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if refusals.Load() != 1 {
		t.Fatalf("upstream refused %d times, want exactly one refusal and a fitted resend", refusals.Load())
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("the fitted resend of a steadily streaming reply was cut off by the total timeout: %d %s", rec.Code, rec.Body.String())
	}
	if n := strings.Count(rec.Body.String(), `"content":"x"`); n != 12 {
		t.Fatalf("relayed %d of 12 chunks", n)
	}
}

// A non-streamed request with provider-run tools waits for its first byte
// like a media request; its fitted resend must wait just as long.
func TestQuirkResendOfAHostedToolRequestKeepsTheLongWait(t *testing.T) {
	h := newHarness(t)
	var refusals atomic.Int32
	up := quirkUpstream(t, &refusals, func(w http.ResponseWriter, _ map[string]any) {
		time.Sleep(600 * time.Millisecond) // "searching the web"
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[{"type":"web_search_call"},{"type":"message","content":[{"type":"output_text","text":"found it"}]}],"usage":{"input_tokens":10,"output_tokens":5}}`))
	})
	// The fake upstream stands in for a provider that runs web search.
	prev, had := adapter.HostedToolHosts["127.0.0.1"]
	adapter.HostedToolHosts["127.0.0.1"] = map[string]bool{"web_search": true}
	t.Cleanup(func() {
		if had {
			adapter.HostedToolHosts["127.0.0.1"] = prev
		} else {
			delete(adapter.HostedToolHosts, "127.0.0.1")
		}
	})
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, up.URL, true, "", ""); err != nil {
		t.Fatal(err)
	}
	h.server.Config.UpstreamTTFBTimeout = 200 * time.Millisecond
	h.server.Config.UpstreamTotalTimeout = 5 * time.Second

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "temperature": 0.7,
		"tools":    []map[string]any{{"type": "web_search"}},
		"messages": []map[string]string{{"role": "user", "content": "search"}},
	})
	if refusals.Load() != 1 {
		t.Fatalf("upstream refused %d times, want exactly one refusal and a fitted resend: %d %s", refusals.Load(), rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "found it") {
		t.Fatalf("the fitted resend of a hosted-tool request was cut off by the text TTFB limit: %d %s", rec.Code, rec.Body.String())
	}
}
