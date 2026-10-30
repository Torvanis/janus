package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

func repointUpstream(t *testing.T, h *harness, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, up.URL, true, "", ""); err != nil {
		t.Fatal(err)
	}
	return up
}

// Embeddings and moderation without a usage block are byte-estimated on
// INPUT only: their responses are vectors / verdicts, not generated text.
// Gemini's two-line embedding (190 KB of floats) was recorded as 47,500
// output tokens.
func TestByteFallbackHasNoOutputForVectorsAndVerdicts(t *testing.T) {
	h := newHarness(t)
	floats := strings.Repeat("0.0123456789,", 8000)
	repointUpstream(t, h, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/moderations") {
			_, _ = w.Write([]byte(`{"results":[{"flagged":false,"categories":{},"category_scores":{"violence":0.0001}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","embedding":[` + strings.TrimSuffix(floats, ",") + `]}]}`))
	})
	for i, path := range []string{"/v1/embeddings", "/v1/moderations"} {
		rec := h.do(http.MethodPost, path, map[string]any{"model": "test-model", "input": "two short lines of text"})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		ev := h.waitForUsageEvents(i + 1)[0]
		if ev.AccountingMode != usage.AccountingBytes {
			t.Fatalf("%s accounting = %s", path, ev.AccountingMode)
		}
		if ev.TokensOut != 0 || ev.TokensIn == 0 {
			t.Errorf("%s tokens = in %d / out %d, want input only", path, ev.TokensIn, ev.TokensOut)
		}
	}
}

// Whisper's duration usage meters seconds of audio, not ~bytes/4 of the
// upload, through the proxy (multipart request).
func TestProxyMetersWhisperDuration(t *testing.T) {
	h := newHarness(t)
	repointUpstream(t, h, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"The quick brown fox.","usage":{"type":"duration","seconds":4}}`))
	})
	audio := strings.Repeat("A", 180_000)
	body := "--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ntest-model\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\nContent-Type: audio/wav\r\n\r\n" +
		audio + "\r\n--b--\r\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("transcription: %d %s", rec.Code, rec.Body.String())
	}
	ev := h.waitForUsageEvents(1)[0]
	if ev.Modality != "stt" || ev.AccountingMode != usage.AccountingUpstream || ev.TokensIn != 4 || ev.TokensOut != 0 {
		t.Fatalf("event = %s %s in=%d out=%d, want stt upstream_reported 4/0", ev.Modality, ev.AccountingMode, ev.TokensIn, ev.TokensOut)
	}

}

// response_format=text|srt: no usage at all. The audio upload is never
// byte-counted as input (a 4-second WAV was 46,614 "tokens"); only the
// transcript is estimated.
func TestProxyPlainTextSTTDoesNotCountTheUpload(t *testing.T) {
	h := newHarness(t)
	repointUpstream(t, h, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("The quick brown fox jumps over the lazy dog.\n"))
	})
	audio := strings.Repeat("A", 180_000)
	body := "--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ntest-model\r\n--b\r\nContent-Disposition: form-data; name=\"response_format\"\r\n\r\ntext\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\nContent-Type: audio/wav\r\n\r\n" +
		audio + "\r\n--b--\r\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("text transcription: %d %s", rec.Code, rec.Body.String())
	}
	ev := h.waitForUsageEvents(1)[0]
	if ev.AccountingMode != usage.AccountingBytes || ev.TokensIn != 0 || ev.TokensOut == 0 || ev.TokensOut > 20 {
		t.Fatalf("plain-text STT = %s in %d / out %d, want byte_count_fallback 0 / ~11", ev.AccountingMode, ev.TokensIn, ev.TokensOut)
	}
}

// Streamed speech sends its first byte only when audio is ready (measured
// 59–82 s from OpenAI); the text TTFB limit must not cut it off.
func TestProxySpeechOutlivesTTFBTimeout(t *testing.T) {
	h := newHarness(t)
	repointUpstream(t, h, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"speech.audio.delta\",\"audio\":\"AAAA\"}\n\ndata: {\"type\":\"speech.audio.done\",\"usage\":{\"input_tokens\":3,\"output_tokens\":40}}\n\n"))
	})
	h.server.Config.UpstreamTTFBTimeout = 200 * time.Millisecond
	h.server.Config.UpstreamTotalTimeout = 5 * time.Second
	rec := h.do(http.MethodPost, "/v1/audio/speech", map[string]any{"model": "test-model", "input": "hi", "voice": "alloy", "stream_format": "sse"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "speech.audio.done") {
		t.Fatalf("speech cut off: %d %s", rec.Code, rec.Body.String())
	}
}

// Gemini's finished-video download redirects to a files endpoint that takes
// only x-goog-api-key; the proxy must follow it with that header (and never
// carry the credential to another host).
func TestProxyGeminiVideoDownloadRedirect(t *testing.T) {
	h := newHarness(t)
	var final *http.Request
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/content"):
			if r.Header.Get("Authorization") != "Bearer gk" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, srv.URL+"/v1beta/files/abc:download?alt=media", http.StatusFound)
		case strings.HasSuffix(r.URL.Path, ":download"):
			final = r.Clone(context.Background())
			if r.Header.Get("Authorization") != "" || r.Header.Get("x-goog-api-key") != "gk" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":401,"message":"API keys are not supported by this API."}}`))
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("\x00\x00\x00\x18ftypmp42"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	up, err := h.store.CreateUpstream(ctx, "gemini-fake", "gemini", srv.URL+"/v1beta/openai", "", "")
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := h.server.Cipher.Encrypt("gk")
	if err := h.store.UpdateUpstream(ctx, up.ID, srv.URL+"/v1beta/openai", true, enc, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "veo-test", []string{"video"}); err != nil {
		t.Fatal(err)
	}
	m, _ := h.store.ModelByUpstreamAndName(ctx, up.ID, "veo-test")
	_ = h.store.SetModelRates(ctx, m.ID, store.RateCard{RateInNano: 1, RateOutNano: 1, EffectiveFrom: time.Now().UTC().Add(-time.Hour)})
	_ = h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled)
	_, _ = h.store.CreateGrant(ctx, m.ID, store.ModelKindModel, store.GranteeAllUsers, "")

	req := httptest.NewRequest(http.MethodGet, "/v1/videos/models/veo-test/operations/x1/content", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("X-Janus-Model", "veo-test")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ftyp") {
		t.Fatalf("download: %d %q (final headers %v)", rec.Code, rec.Body.String(), final)
	}
}

// A non-streamed reply that uses provider-run tools (web search) sends
// nothing until the searches finish; the 30 s first-byte limit cut it off
// with a 503 (seen live: xAI grok with web_search + x_search at 30.5 s).
func TestProxyHostedToolsOutliveTTFBTimeout(t *testing.T) {
	h := newHarness(t)
	repointUpstream(t, h, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[{"type":"web_search_call"},{"type":"message","content":[{"type":"output_text","text":"Headlines"}]}],"usage":{"input_tokens":10,"output_tokens":5}}`))
	})
	h.server.Config.UpstreamTTFBTimeout = 200 * time.Millisecond
	h.server.Config.UpstreamTotalTimeout = 5 * time.Second
	rec := h.do(http.MethodPost, "/v1/responses", map[string]any{"model": "test-model", "input": "news", "tools": []any{map[string]any{"type": "web_search"}}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Headlines") {
		t.Fatalf("hosted-tool reply cut off: %d %s", rec.Code, rec.Body.String())
	}
}
