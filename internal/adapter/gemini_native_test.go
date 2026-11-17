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

// "No thinking" on Gemini's native API is a thinking budget of 0: off where
// the model can turn it off, least thinking where it cannot. It used to
// fall through to the model's default thinking.
func TestGeminiNativeNoThinking(t *testing.T) {
	g := &Gemini{openai: &OpenAICompatible{name: "gemini"}}
	config := func(model, effort string) string {
		t.Helper()
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search"}]` + effort + `}`
		p, err := g.Prepare(Upstream{BaseURL: "https://generativelanguage.googleapis.com", APIKey: "k"}, &Request{Path: "/v1/chat/completions", Model: model, Body: []byte(body)})
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Gen struct {
				Thinking json.RawMessage `json:"thinkingConfig"`
			} `json:"generationConfig"`
		}
		_ = json.Unmarshal(p.Body, &out)
		return string(out.Gen.Thinking)
	}
	for _, c := range []struct{ model, effort, want string }{
		{"gemini-2.5-flash", `,"reasoning_effort":"none"`, `{"includeThoughts":true,"thinkingBudget":0}`},
		{"gemini-3.8-flash", `,"reasoning_effort":"none"`, `{"includeThoughts":true,"thinkingBudget":0}`},
		{"gemini-2.5-flash", ``, `{"includeThoughts":true}`},
		// 2.x refuses any thinkingLevel on the native API (live: 400
		// "Thinking level is not supported for this model."): each level is
		// the budget Google's OpenAI-compatible route maps it to.
		{"gemini-2.5-flash", `,"reasoning_effort":"minimal"`, `{"includeThoughts":true,"thinkingBudget":1024}`},
		{"gemini-2.5-flash", `,"reasoning_effort":"low"`, `{"includeThoughts":true,"thinkingBudget":1024}`},
		{"gemini-2.5-pro", `,"reasoning_effort":"medium"`, `{"includeThoughts":true,"thinkingBudget":8192}`},
		{"models/gemini-2.5-flash-lite", `,"reasoning_effort":"high"`, `{"includeThoughts":true,"thinkingBudget":24576}`},
		{"gemini-2.5-flash", `,"reasoning_effort":"xhigh"`, `{"includeThoughts":true,"thinkingBudget":24576}`},
		// Later families take the level itself.
		{"gemini-3.8-flash", `,"reasoning_effort":"low"`, `{"includeThoughts":true,"thinkingLevel":"low"}`},
		{"gemini-4-argon", `,"reasoning_effort":"high"`, `{"includeThoughts":true,"thinkingLevel":"high"}`},
		{"gemini-4-argon", `,"reasoning_effort":"xhigh"`, `{"includeThoughts":true}`},
	} {
		if got := config(c.model, c.effort); got != c.want {
			t.Errorf("%s%s: %s, want %s", c.model, c.effort, got, c.want)
		}
	}
}

func TestCarriesThinkingOff(t *testing.T) {
	for _, c := range []struct {
		adapterType, off string
		want             bool
	}{
		{"anthropic", ThinkingOffDisabled, true},
		{"anthropic", ThinkingOffEffortNone, false},
		{"gemini", ThinkingOffBudgetZero, true},
		{"openai_compatible", ThinkingOffDisabled, false},
		{"ollama", ThinkingOffThink, false},
	} {
		if got := CarriesThinkingOff(c.adapterType, c.off); got != c.want {
			t.Errorf("%s %s: %v", c.adapterType, c.off, got)
		}
	}
}

// The adapters CarriesSoundAndVideo rules out are the ones whose request
// mapping cannot carry the parts: Anthropic and the ChatGPT-plan
// translator refuse them, Bedrock's Converse mapping turns them into text.
func TestCarriesSoundAndVideo(t *testing.T) {
	for _, part := range []string{
		`{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}}`,
		`{"type":"video_url","video_url":{"url":"data:video/mp4;base64,AAAAGGZ0eXA="}}`,
	} {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"what is in it?"},` + part + `]}]}`)
		if _, err := chatToMessages(body); err == nil {
			t.Errorf("anthropic carried %s", part)
		}
		if _, _, err := chatToResponses(body, "m"); err == nil {
			t.Errorf("codex carried %s", part)
		}
		out, err := chatToConverse(body)
		if err != nil || !strings.Contains(string(out), `\"type\"`) {
			t.Errorf("bedrock should flatten the part into text: %s %v", out, err)
		}
	}
	for adapterType, want := range map[string]bool{"anthropic": false, CodexAdapterType: false, "bedrock": false, "tei": false,
		"gemini": true, "openai_compatible": true, "vlm": true, "llama_cpp": true, "ollama": true, "vertex": true, CopilotAdapterType: true} {
		if got := CarriesSoundAndVideo(adapterType); got != want {
			t.Errorf("%s: %v, want %v", adapterType, got, want)
		}
	}
}
