package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

// Lyria makes music: a chat request for it goes to the native API (the
// OpenAI-compatible endpoint 400s on audio output), without thinking
// settings, and the audio comes back as OpenAI's message.audio.
func TestGeminiLyriaGoesNativeAndReturnsAudio(t *testing.T) {
	g := &Gemini{}
	req := &Request{Method: "POST", Path: "/v1/chat/completions", Model: "lyria-3-clip-preview",
		Body: []byte(`{"model":"lyria-3-clip-preview","stream":false,"messages":[{"role":"user","content":"calm piano"}]}`)}
	p, err := g.Prepare(Upstream{BaseURL: "https://generativelanguage.googleapis.com", APIKey: "k"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p.URL, "/v1beta/models/lyria-3-clip-preview:generateContent") || strings.Contains(string(p.Body), "thinkingConfig") ||
		!strings.Contains(string(p.Body), "calm piano") {
		t.Fatalf("prepared %s %s", p.URL, p.Body)
	}
	native := `{"candidates":[{"content":{"parts":[{"text":"<instrumental>"},{"inlineData":{"mimeType":"audio/mpeg","data":"SUQzAw=="}}],"role":"model"},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":16,"candidatesTokenCount":282},"modelVersion":"lyria-3-clip-preview"}`
	out, err := g.TransformResponse(req, []byte(native))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Audio   struct {
					Data, Format string
				} `json:"audio"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &r); err != nil || len(r.Choices) != 1 {
		t.Fatalf("out %s %v", out, err)
	}
	m := r.Choices[0].Message
	if m.Audio.Data != "SUQzAw==" || m.Audio.Format != "mp3" || m.Content != "<instrumental>" || r.Usage.CompletionTokens != 282 {
		t.Fatalf("message = %+v usage %+v", m, r.Usage)
	}
	// Other Gemini chat without built-in tools stays on the compatible endpoint.
	plain := &Request{Method: "POST", Path: "/v1/chat/completions", Model: "gemini-3.8-flash", Body: []byte(`{"model":"gemini-3.8-flash","messages":[]}`)}
	if geminiNativeRequest(plain) {
		t.Fatal("plain Gemini chat routed native")
	}
}
