package adapter

import (
	"strings"
	"testing"
)

// Sound and video parts send a Gemini chat to the native API as inlineData
// (the OpenAI-compatible endpoint has no video part); a plain chat that
// merely mentions "video_url" in its text stays on the compatible endpoint.
func TestGeminiSoundAndVideoGoNative(t *testing.T) {
	g := &Gemini{openai: &OpenAICompatible{name: "gemini"}}
	up := Upstream{BaseURL: "https://generativelanguage.googleapis.com", APIKey: "k"}
	req := &Request{Path: "/v1/chat/completions", Model: "gemini-3.8-flash", Streaming: true, Header: map[string][]string{},
		Body: []byte(`{"model":"gemini-3.8-flash","stream":true,"messages":[{"role":"user","content":[` +
			`{"type":"text","text":"What is in these?"},` +
			`{"type":"input_audio","input_audio":{"data":"SUQz","format":"mp3"}},` +
			`{"type":"video_url","video_url":{"url":"data:video/mp4;base64,AAAAGGZ0eXA="}}]}]}`)}
	p, err := g.Prepare(up, req)
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse" {
		t.Fatalf("not native: %s", p.URL)
	}
	if want := `{"contents":[{"parts":[{"text":"What is in these?"},` +
		`{"inlineData":{"data":"SUQz","mimeType":"audio/mp3"}},` +
		`{"inlineData":{"data":"AAAAGGZ0eXA=","mimeType":"video/mp4"}}],"role":"user"}],` +
		`"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`; string(p.Body) != want {
		t.Fatalf("native body =\n%s\nwant\n%s", p.Body, want)
	}

	// WAV sound with a thinking level on a 2.x model (what Measure sends
	// when the model's off switch is unknown): audio/wav inlineData and a
	// thinkingBudget, never the thinkingLevel 2.x refuses.
	wav := &Request{Path: "/v1/chat/completions", Model: "gemini-2.5-flash", Header: map[string][]string{},
		Body: []byte(`{"model":"gemini-2.5-flash","max_tokens":256,"reasoning_effort":"minimal","messages":[{"role":"user","content":[` +
			`{"type":"text","text":"What number?"},{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}}]}]}`)}
	p, err = g.Prepare(up, wav)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"contents":[{"parts":[{"text":"What number?"},{"inlineData":{"data":"UklGRg==","mimeType":"audio/wav"}}],"role":"user"}],` +
		`"generationConfig":{"maxOutputTokens":256,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":1024}}}`; string(p.Body) != want ||
		p.URL != "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("wav: %s\n%s\nwant\n%s", p.URL, p.Body, want)
	}

	// Google refuses sound/video next to code execution: that tool is
	// dropped for such a request, the others stay.
	withTools := &Request{Path: "/v1/chat/completions", Model: "gemini-3.8-flash", Streaming: true, Header: map[string][]string{},
		Body: []byte(`{"model":"gemini-3.8-flash","stream":true,"messages":[{"role":"user","content":[` +
			`{"type":"input_audio","input_audio":{"data":"SUQz","format":"mp3"}}]}],` +
			`"tools":[{"type":"web_search"},{"type":"code_execution"}]}`)}
	p, err = g.Prepare(up, withTools)
	if err != nil {
		t.Fatal(err)
	}
	if b := string(p.Body); strings.Contains(b, "code_execution") || !strings.Contains(b, "google_search") {
		t.Fatalf("tools with media = %s", b)
	}

	plain := &Request{Path: "/v1/chat/completions", Model: "gemini-3.8-flash", Header: map[string][]string{},
		Body: []byte(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"explain video_url and input_audio parts"}]}`)}
	p, err = g.Prepare(up, plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.URL, "GenerateContent") {
		t.Fatalf("plain chat went native: %s", p.URL)
	}
}
