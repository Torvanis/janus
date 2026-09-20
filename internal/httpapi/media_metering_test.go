package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/torvanis/janus/internal/usage"
)

// TestProxyDoesNotByteMeterBinaryTTS is the end-to-end regression test for the
// byte-count metering bug.
//
// /v1/audio/speech returns raw MP3 bytes and no usage block. The old fallback
// estimated tokens as responseBytes/4, turning a 30 KB clip into 7,680 phantom
// "output tokens" against a real 48 — a measured 159x overcharge that also fed
// the quota engine. A media response with no upstream usage must now be
// recorded unmetered rather than billed on a guess.
func TestProxyDoesNotByteMeterBinaryTTS(t *testing.T) {
	h := newHarness(t)

	// 30 KB of non-JSON bytes, exactly the shape of a real TTS response.
	audio := make([]byte, 30720)
	for i := range audio {
		audio[i] = byte(i % 251)
	}
	ttsUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(audio)
	}))
	t.Cleanup(ttsUp.Close)

	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, ttsUp.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}

	rec := h.do(http.MethodPost, "/v1/audio/speech", map[string]any{
		"model": "test-model", "input": "Hello from Janus.", "voice": "alloy",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("tts proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Len(); got != len(audio) {
		t.Fatalf("client received %d bytes, want the full %d-byte audio body", got, len(audio))
	}

	event := h.waitForUsageEvents(1)[0]

	if event.Modality != "tts" {
		t.Fatalf("modality = %q, want tts", event.Modality)
	}
	if event.AccountingMode != usage.AccountingUnmetered {
		t.Errorf("accounting mode = %q, want %q: a binary body must never be byte-estimated, and the gap must be distinguishable from an error response",
			event.AccountingMode, usage.AccountingUnmetered)
	}
	if event.TokensOut != 0 || event.TokensIn != 0 {
		t.Errorf("token counts = (%d, %d), want (0, 0): %d phantom tokens would be invented by bytes/4",
			event.TokensIn, event.TokensOut, usage.EstimateTokensFromBytes(int64(len(audio))))
	}
	if event.CostNano != 0 {
		t.Errorf("cost = %d nano-USD, want 0 for an unmetered modality", event.CostNano)
	}
	// The response bytes are still recorded — observability is unaffected,
	// only the fabricated billing is removed.
	if event.ResponseBytes != int64(len(audio)) {
		t.Errorf("response bytes = %d, want %d (traffic accounting must survive)",
			event.ResponseBytes, len(audio))
	}
}

// TestProxyStillByteMetersTextWithoutUsage pins the other half of the
// contract: text-shaped responses with no usage block keep the byte fallback,
// so unknown text endpoints do not silently stop metering.
func TestProxyStillByteMetersTextWithoutUsage(t *testing.T) {
	h := newHarness(t)
	noUsage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-x","choices":[{"message":{"role":"assistant","content":"Hello there"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(noUsage.Close)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, noUsage.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}

	event := h.waitForUsageEvents(1)[0]
	if event.AccountingMode != usage.AccountingBytes {
		t.Errorf("accounting mode = %q, want %q for a text body", event.AccountingMode, usage.AccountingBytes)
	}
	if event.TokensIn <= 0 || event.TokensOut <= 0 {
		t.Errorf("token counts = (%d, %d), want both > 0", event.TokensIn, event.TokensOut)
	}
}
