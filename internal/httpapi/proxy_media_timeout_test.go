package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Image generation answers only when the image is ready, so its first byte
// IS the whole generation. The TTFB limit that catches a hung text upstream
// must not cut it off; the total timeout still bounds it. A chat request to
// the same slow upstream must still be cut off by the TTFB limit.
func TestProxyImageGenerationOutlivesTTFBTimeout(t *testing.T) {
	h := newHarness(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(700 * time.Millisecond) // "generating"
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/images/generations" {
			_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGk="}],"usage":{"input_tokens":10,"output_tokens":1000}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"late"}}]}`))
	}))
	t.Cleanup(up.Close)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, up.URL, true, "", ""); err != nil {
		t.Fatal(err)
	}
	h.server.Config.UpstreamTTFBTimeout = 200 * time.Millisecond
	h.server.Config.UpstreamTotalTimeout = 5 * time.Second

	rec := h.do(http.MethodPost, "/v1/images/generations", map[string]any{"model": "test-model", "prompt": "a cat"})
	if rec.Code != http.StatusOK {
		t.Fatalf("image generation cut off by the TTFB limit: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	if rec.Code == http.StatusOK {
		t.Fatalf("a chat request outlived the TTFB limit: the limit no longer applies to text")
	}

	// The total timeout still bounds image generation.
	h.server.Config.UpstreamTotalTimeout = 300 * time.Millisecond
	rec = h.do(http.MethodPost, "/v1/images/generations", map[string]any{"model": "test-model", "prompt": "a cat"})
	if rec.Code == http.StatusOK {
		t.Fatalf("image generation outlived the total timeout")
	}
}
