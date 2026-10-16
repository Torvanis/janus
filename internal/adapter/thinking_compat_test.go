package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

// reasoning_effort reached Claude as nothing at all: the translation
// dropped it. Each Claude generation takes a different thinking form
// (probed against the live providers, 2026-10-15).
func TestAnthropicReasoningEffortTranslated(t *testing.T) {
	cases := []struct {
		name, model, extra string
		thinking           map[string]any
		effort             string
		maxTokens          float64
		noTemp             bool
	}{
		{"claude 5.5 takes adaptive + effort", "claude-opus-5-5", `"reasoning_effort":"high","temperature":0.7`,
			map[string]any{"type": "adaptive"}, "high", anthropicAdaptiveMaxTokens, true},
		{"claude 5.5 none: least effort, no thinking object", "claude-haiku-5-5", `"reasoning_effort":"none"`,
			nil, "low", anthropicDefaultMaxTokens, false},
		{"claude 5.5 minimal -> low", "claude-sonnet-5-5", `"reasoning_effort":"minimal"`,
			map[string]any{"type": "adaptive"}, "low", anthropicAdaptiveMaxTokens, true},
		{"reasoning.effort form", "claude-opus-5-5", `"reasoning":{"effort":"xhigh"}`,
			map[string]any{"type": "adaptive"}, "xhigh", anthropicAdaptiveMaxTokens, true},
		{"claude 4.6 is adaptive too", "claude-sonnet-4-6", `"reasoning_effort":"medium"`,
			map[string]any{"type": "adaptive"}, "medium", anthropicAdaptiveMaxTokens, true},
		{"claude 4.5 takes a budget", "claude-sonnet-4-5-20250929", `"reasoning_effort":"medium"`,
			map[string]any{"type": "enabled", "budget_tokens": float64(8192)}, "", float64(8192 + anthropicDefaultMaxTokens), true},
		{"claude 4.5 none: thinking stays off", "claude-sonnet-4-5-20250929", `"reasoning_effort":"none","temperature":0.7`,
			nil, "", anthropicDefaultMaxTokens, false},
		{"explicit cap kept on adaptive", "claude-opus-5-5", `"reasoning_effort":"low","max_tokens":1000`,
			map[string]any{"type": "adaptive"}, "low", 1000, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := chatToMessages([]byte(`{"model":"` + c.model + `","messages":[{"role":"user","content":"hi"}],` + c.extra + `}`))
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			_ = json.Unmarshal(out, &m)
			th, _ := m["thinking"].(map[string]any)
			if (th == nil) != (c.thinking == nil) || (th != nil && (th["type"] != c.thinking["type"] || th["budget_tokens"] != c.thinking["budget_tokens"])) ||
				(th != nil && th["type"] == "adaptive" && th["display"] != "summarized") {
				t.Fatalf("thinking = %v, want %v (%s)", m["thinking"], c.thinking, out)
			}
			oc, _ := m["output_config"].(map[string]any)
			if got, _ := oc["effort"].(string); got != c.effort {
				t.Fatalf("output_config.effort = %q, want %q (%s)", got, c.effort, out)
			}
			if m["max_tokens"] != c.maxTokens {
				t.Fatalf("max_tokens = %v, want %v", m["max_tokens"], c.maxTokens)
			}
			if _, has := m["temperature"]; has == c.noTemp && strings.Contains(c.extra, "temperature") {
				t.Fatalf("temperature present=%v, want %v (%s)", has, !c.noTemp, out)
			}
			if _, leaked := m["reasoning_effort"]; leaked {
				t.Fatal("reasoning_effort leaked into the Messages body")
			}
		})
	}
}

// Budget plus answer stays within the model's output limit: "max" on
// Sonnet 4.5 was refused ("max_tokens: 68095 > 64000").
func TestAnthropicBudgetFitsOutputLimit(t *testing.T) {
	for _, c := range []struct {
		model, extra string
		limit        float64
	}{
		{"claude-sonnet-4-5-20250929", `"reasoning_effort":"max"`, 64000},
		{"claude-sonnet-4-5-20250929", `"reasoning_effort":"xhigh","max_tokens":60000`, 64000},
		{"claude-opus-4-1-20250805", `"reasoning_effort":"xhigh"`, 32000},
		{"claude-opus-4-1-20250805", `"reasoning_effort":"max"`, 32000},
	} {
		out, err := chatToMessages([]byte(`{"model":"` + c.model + `","messages":[{"role":"user","content":"hi"}],` + c.extra + `}`))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(out, &m)
		mt := m["max_tokens"].(float64)
		b := m["thinking"].(map[string]any)["budget_tokens"].(float64)
		if mt > c.limit || b >= mt || mt-b < 1024 {
			t.Fatalf("%s %s: max_tokens=%v budget=%v (limit %v)", c.model, c.extra, mt, b, c.limit)
		}
	}
}

// A caller that speaks Anthropic's own thinking object keeps control.
func TestAnthropicNativeThinkingWins(t *testing.T) {
	out, err := chatToMessages([]byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"},"reasoning_effort":"low"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "output_config") {
		t.Fatalf("native thinking was rewritten: %s", out)
	}
}

func TestClaudeVersion(t *testing.T) {
	for id, want := range map[string]bool{"claude-opus-5-5": true, "claude-haiku-5-5": true, "claude-opus-5": true, "claude-sonnet-4-6": true,
		"claude-sonnet-4-5-20250929": false, "claude-opus-4-1-20250805": false, "claude-3-7-sonnet-20250219": false, "claude-fable-5-1": true,
		"anthropic.claude-sonnet-4-5-20250929-v1:0": false, "some-other-model": true} {
		if got := claudeAdaptive(id); got != want {
			t.Errorf("claudeAdaptive(%q) = %v, want %v", id, got, want)
		}
	}
}

// Gemini refused every tool round trip from Open WebUI: the client drops
// the thought signature from the replayed call and Gemini requires it.
func TestGeminiThoughtSignatureRestored(t *testing.T) {
	resp := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"extra_content":{"google":{"thought_signature":"SIG-abc"}},"id":"call_42","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}}]}`
	g := &Gemini{openai: &OpenAICompatible{name: "gemini"}}
	req := &Request{Path: "/v1/chat/completions", Model: "gemini-3.8-flash"}
	if _, err := g.TransformResponse(req, []byte(resp)); err != nil {
		t.Fatal(err)
	}
	replay := `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"weather?"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_42","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_unknown","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_42","content":"18C"}]}`
	p, err := g.Prepare(Upstream{BaseURL: "https://generativelanguage.googleapis.com", APIKey: "k"}, &Request{Path: "/v1/chat/completions", Body: []byte(replay), Header: map[string][]string{"Content-Type": {"application/json"}}})
	if err != nil {
		t.Fatal(err)
	}
	body := string(p.Body)
	if !strings.Contains(body, `"thought_signature":"SIG-abc"`) {
		t.Fatalf("remembered signature not restored: %s", body)
	}
	if !strings.Contains(body, `"thought_signature":"`+geminiSignatureSkip+`"`) {
		t.Fatalf("unknown call did not get the documented placeholder: %s", body)
	}
	// A body with signatures already present is left byte-for-byte.
	same := `{"model":"g","messages":[{"role":"assistant","tool_calls":[{"id":"x","extra_content":{"google":{"thought_signature":"S"}},"function":{"name":"f","arguments":"{}"}}]}]}`
	if got := restoreGeminiSignatures([]byte(same)); string(got) != same {
		t.Fatalf("body rewritten without need: %s", got)
	}
}

// Signatures arriving in a stream are remembered too.
func TestGeminiThoughtSignatureFromStream(t *testing.T) {
	g := &Gemini{openai: &OpenAICompatible{name: "gemini"}}
	tr := g.NewStreamTransformer(&Request{Path: "/v1/chat/completions"}, nil)
	line := `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"extra_content":{"google":{"thought_signature":"SIG-stream"}},"function":{"arguments":"{}","name":"f"},"id":"call_s1","type":"function"}]},"index":0}]}` + "\n"
	if out, err := tr.TransformStreamLine([]byte(line)); err != nil || string(out) != line {
		t.Fatalf("stream line changed: %q %v", out, err)
	}
	if sig, ok := geminiSignatures.Get("call_s1"); !ok || sig != "SIG-stream" {
		t.Fatalf("stream signature not remembered: %q %v", sig, ok)
	}
}

// The Responses fallback translates a streamed chat request (Open WebUI
// streams) and keeps the caller's cap and sampling.
func TestResponsesFallbackRequest(t *testing.T) {
	a := ResponsesFallback()
	p, err := a.Prepare(Upstream{BaseURL: "https://api.openai.com/v1", APIKey: "k"}, &Request{Path: "/v1/chat/completions", Model: "gpt-6.1-sol", Streaming: true,
		Body: []byte(`{"model":"gpt-6.1-sol","stream":true,"stream_options":{"include_usage":true},"reasoning_effort":"high","max_completion_tokens":900,"temperature":1,` +
			`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"hi"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != "https://api.openai.com/v1/responses" {
		t.Fatalf("url %s", p.URL)
	}
	var m map[string]any
	_ = json.Unmarshal(p.Body, &m)
	r, _ := m["reasoning"].(map[string]any)
	if r["effort"] != "high" || r["summary"] != nil || m["max_output_tokens"] != float64(900) || m["temperature"] != float64(1) ||
		m["instructions"] != nil || m["stream_options"] != nil || m["stream"] != true || m["store"] != false {
		t.Fatalf("responses body = %s", p.Body)
	}
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "f" {
		t.Fatalf("tools = %v", m["tools"])
	}
}
