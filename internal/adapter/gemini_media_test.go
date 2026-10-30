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
	if !strings.Contains(p.URL, ":streamGenerateContent") {
		t.Fatalf("not native: %s", p.URL)
	}
	body := string(p.Body)
	if !strings.Contains(body, `"inlineData":{"data":"SUQz","mimeType":"audio/mp3"}`) ||
		!strings.Contains(body, `"inlineData":{"data":"AAAAGGZ0eXA=","mimeType":"video/mp4"}`) {
		t.Fatalf("native body = %s", body)
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
