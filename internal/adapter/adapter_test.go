package adapter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestRegistryHasEveryShippedAdapter(t *testing.T) {
	for _, want := range []string{"anthropic", "bedrock", "llama_cpp", "ollama", "openai_compatible", "vertex", "vlm"} {
		a, err := Get(want)
		if err != nil {
			t.Fatalf("adapter %q not registered: %v", want, err)
		}
		if a.Type() != want {
			t.Fatalf("adapter %q reports type %q", want, a.Type())
		}
	}
	if _, err := Get("nope"); err == nil {
		t.Fatal("unknown adapter types must be rejected")
	}
}

func TestJoinURL(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"http://host:8000", "/v1/chat/completions", "http://host:8000/v1/chat/completions"},
		{"http://host:8000/", "/v1/models", "http://host:8000/v1/models"},
		// A base URL that already carries /v1 must not double it.
		{"http://host:8000/v1", "/v1/chat/completions", "http://host:8000/v1/chat/completions"},
		{"http://host", "v1/models", "http://host/v1/models"},
	}
	for _, c := range cases {
		if got := joinURL(c.base, c.path); got != c.want {
			t.Fatalf("joinURL(%q, %q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}

func TestSSEData(t *testing.T) {
	if payload, ok := sseData([]byte(`data: {"a":1}`)); !ok || string(payload) != `{"a":1}` {
		t.Fatalf("payload = %q ok=%v", payload, ok)
	}
	if _, ok := sseData([]byte("data: [DONE]")); ok {
		t.Fatal("[DONE] must not be parsed as a payload")
	}
	if _, ok := sseData([]byte("event: message_start")); ok {
		t.Fatal("non-data lines must be ignored")
	}
	if _, ok := sseData([]byte("data:")); ok {
		t.Fatal("empty data lines must be ignored")
	}
}

func TestCloneHeaderDropsHopAndCredentialHeaders(t *testing.T) {
	in := http.Header{}
	in.Set("Authorization", "Bearer secret")
	in.Set("Cookie", "janus_session=abc")
	in.Set("X-Janus-Csrf", "csrf")
	in.Set("Host", "gateway.local")
	in.Set("Content-Length", "42")
	in.Set("Accept-Encoding", "gzip")
	in.Set("Content-Type", "application/json")
	in.Set("X-Custom", "kept")

	out := cloneHeader(in)
	for _, dropped := range []string{"Authorization", "Cookie", "X-Janus-Csrf", "Host", "Content-Length", "Accept-Encoding"} {
		if got := out.Get(dropped); got != "" {
			t.Fatalf("%s must not be forwarded upstream, got %q", dropped, got)
		}
	}
	if out.Get("Content-Type") != "application/json" || out.Get("X-Custom") != "kept" {
		t.Fatal("benign headers must be forwarded")
	}
}

func TestEnsureStreamUsage(t *testing.T) {
	out := ensureStreamUsage([]byte(`{"stream":true,"model":"m"}`))
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	opts, ok := payload["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream_options.include_usage not injected: %s", out)
	}

	// Non-streaming bodies, bodies with explicit stream_options, and invalid
	// JSON all pass through untouched.
	for _, body := range []string{
		`{"stream":false,"model":"m"}`,
		`{"stream":true,"stream_options":{"include_usage":false}}`,
		`not json`,
	} {
		if got := ensureStreamUsage([]byte(body)); !bytes.Equal(got, []byte(body)) {
			t.Fatalf("body %q must pass through, got %q", body, got)
		}
	}
}

func TestParseOpenAIUsage(t *testing.T) {
	u, ok := parseOpenAIUsage([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}},"choices":[{"finish_reason":"stop"}]}`))
	if !ok || u.TokensIn != 10 || u.TokensOut != 5 || u.TokensCached != 4 || u.FinishReason != "stop" {
		t.Fatalf("usage = %+v ok=%v", u, ok)
	}

	// Responses-API field names.
	u, ok = parseOpenAIUsage([]byte(`{"usage":{"input_tokens":8,"output_tokens":2}}`))
	if !ok || u.TokensIn != 8 || u.TokensOut != 2 {
		t.Fatalf("responses usage = %+v ok=%v", u, ok)
	}

	if _, ok := parseOpenAIUsage([]byte(`{"usage":{}}`)); ok {
		t.Fatal("zero usage must not be reported")
	}
}

func TestGuessModalities(t *testing.T) {
	cases := map[string]string{
		"text-embedding-3-small": ModalityEmbedding,
		"whisper-large-v3":       ModalitySTT,
		"tts-1-hd":               ModalityTTS,
		"dall-e-3":               ModalityImage,
		"omni-moderation":        ModalityModeration,
		"llama-3.1-8b":           ModalityChat,
		// Video generators must not fall through to chat (or image).
		"sora":                   ModalityVideo,
		"sora-2":                 ModalityVideo,
		"sora-2-pro":             ModalityVideo,
		"grok-imagine-video":     ModalityVideo,
		"grok-imagine-video-1.5": ModalityVideo,
		"veo-3.1-generate":       ModalityVideo,
		"veo3":                   ModalityVideo,
		// …while the still-image sibling stays an image model, and a name
		// that merely starts with "veo" is not a video model.
		"grok-imagine":  ModalityImage,
		"grok-2-image":  ModalityImage,
		"verbose-llama": ModalityChat,
	}
	for name, want := range cases {
		got := guessModalities(name)
		if len(got) != 1 || got[0] != want {
			t.Fatalf("guessModalities(%q) = %v, want [%s]", name, got, want)
		}
	}
}
