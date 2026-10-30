package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A streamed reply that keeps sending is never cut off by the total timeout;
// only silence that long ends it. (A Qwen reply streaming steadily at ~31
// tokens/s was cut off at exactly 600 s.)
func TestStreamedReplyTimesOutOnSilenceNotLength(t *testing.T) {
	h := newHarness(t)
	silentAfter := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		silent := strings.Contains(string(raw), "go silent")
		for i := 0; i < 12; i++ {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
			f.Flush()
			time.Sleep(50 * time.Millisecond)
			if silent && i == 2 {
				<-silentAfter
				return
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(silentAfter) })
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, up.URL, true, "", ""); err != nil {
		t.Fatal(err)
	}
	h.server.Config.UpstreamTotalTimeout = 300 * time.Millisecond // stream lasts ~600 ms

	body := map[string]any{"model": "test-model", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hi"}}}
	rec := h.do(http.MethodPost, "/v1/chat/completions", body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") || strings.Contains(rec.Body.String(), "incomplete") {
		t.Fatalf("a steadily streaming reply was cut off by the total timeout: %d %s", rec.Code, rec.Body.String())
	}
	if n := strings.Count(rec.Body.String(), `"content":"x"`); n != 12 {
		t.Fatalf("relayed %d of 12 chunks", n)
	}

	// Silence for longer than the limit still ends it, and says why.
	silentBody := map[string]any{"model": "test-model", "stream": true, "messages": []map[string]string{{"role": "user", "content": "go silent"}}}
	rec = h.do(http.MethodPost, "/v1/chat/completions", silentBody)
	if !strings.Contains(rec.Body.String(), "sent nothing for") {
		t.Fatalf("a silent stream was not ended by the timeout: %s", rec.Body.String())
	}

	// A non-streamed request keeps the total bound.
	delete(body, "stream")
	rec = h.do(http.MethodPost, "/v1/chat/completions", body)
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("a non-streamed request outlived the total timeout: %s", rec.Body.String())
	}
}
