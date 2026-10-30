package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

// Built-in tools go to Gemini's native API (the OpenAI-compatible endpoint
// refuses them); grounding comes back as activity and citations, and each
// search query is metered at Google's per-query price.
func TestGeminiNativeBuiltins(t *testing.T) {
	g := &Gemini{openai: &OpenAICompatible{name: "gemini"}}
	req := &Request{Path: "/v1/chat/completions", Model: "gemini-3.8-flash", Streaming: true, Header: map[string][]string{"Authorization": {"Bearer x"}},
		Body: []byte(`{"model":"gemini-3.8-flash","stream":true,"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"news"}],"tools":[{"type":"web_search"},{"type":"code_execution"}]}`)}
	p, err := g.Prepare(Upstream{BaseURL: "https://generativelanguage.googleapis.com", APIKey: "k"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse" || p.Header.Get("x-goog-api-key") != "k" || p.Header.Get("Authorization") != "" {
		t.Fatalf("prepared %s %v", p.URL, p.Header)
	}
	var body map[string]any
	_ = json.Unmarshal(p.Body, &body)
	if b, _ := json.Marshal(body["tools"]); string(b) != `[{"google_search":{}},{"code_execution":{}}]` || body["systemInstruction"] == nil {
		t.Fatalf("native body = %s", p.Body)
	}
	tr := g.NewStreamTransformer(req, nil)
	col := g.NewStreamCollector()
	var out strings.Builder
	for _, l := range []string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Talks resume."}]}}],"responseId":"r1","modelVersion":"gemini-3.8-flash"}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP","groundingMetadata":{"webSearchQueries":["news today","talks"],"groundingChunks":[{"web":{"uri":"https://vertexaisearch.cloud.google.com/x","title":"apnews.com"}}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":7}}`,
	} {
		col.Feed([]byte(l))
		b, err := tr.TransformStreamLine([]byte(l))
		if err != nil {
			t.Fatal(err)
		}
		out.Write(b)
	}
	out.Write(tr.(StreamFinisher).FinishStream())
	s := out.String()
	for _, want := range []string{`"content":"Talks resume."`, `"query":"news today · talks"`, `"title":"apnews.com"`, `"finish_reason":"stop"`, `"prompt_tokens":10`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("stream missing %s\n%s", want, s)
		}
	}
	if u := col.Usage(); u.TokensOut != 12 || u.ToolFeeNano != 2*geminiSearchFeeNano {
		t.Fatalf("usage = %+v", u)
	}
	// Without built-in tools: the OpenAI-compatible endpoint, unchanged.
	plain := &Request{Path: "/v1/chat/completions", Model: "gemini-3.8-flash", Body: []byte(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`)}
	if p, _ := g.Prepare(Upstream{BaseURL: "https://generativelanguage.googleapis.com", APIKey: "k"}, plain); !strings.HasSuffix(p.URL, "/v1beta/openai/chat/completions") {
		t.Fatalf("plain URL = %s", p.URL)
	}
}

// A refusal answering a streamed request is relayed as Google sent it (a
// multi-line JSON error), never turned into an empty "finished" stream.
func TestGeminiNativeStreamRelaysErrors(t *testing.T) {
	tr := &geminiNativeStream{model: "gemini-3.8-flash"}
	var out strings.Builder
	for _, l := range []string{"[{\n", "  \"error\": {\n", "    \"code\": 400,\n", "    \"message\": \"Bad tool\"\n", "  }\n", "}]\n"} {
		b, _ := tr.TransformStreamLine([]byte(l))
		out.Write(b)
	}
	out.Write(tr.FinishStream())
	if s := out.String(); !strings.Contains(s, `"message": "Bad tool"`) || strings.Contains(s, "[DONE]") || strings.Contains(s, "finish_reason") {
		t.Fatalf("relayed = %q", s)
	}
}
