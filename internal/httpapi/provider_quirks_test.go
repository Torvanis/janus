package httpapi

// Provider limits on catalog upstreams, as measured against the live providers on
// 2026-10-15 with Open WebUI-shaped requests: each was a 400 the caller saw.
// The fake upstream answers with the providers' real refusal bodies.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/store"
)

type pickyUpstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
	paths    []string
}

const (
	refuseNone  = `{"error":{"message":"Unsupported value: 'reasoning_effort' does not support 'none' with this model. Supported values are: 'low', 'medium', 'high', and 'xhigh'.","type":"invalid_request_error","param":"reasoning_effort","code":"unsupported_value"}}`
	refuseTemp  = `{"error":{"message":"Unsupported value: 'temperature' does not support 0.7 with this model. Only the default (1) value is supported.","type":"invalid_request_error","param":"temperature","code":"unsupported_value"}}`
	refuseTools = `{"error":{"message":"Function tools with reasoning_effort are not supported for gpt-6.1-sol in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.","type":"invalid_request_error","param":"reasoning_effort","code":null}}`
)

// newPickyUpstream behaves like gpt-6.1-sol: no "none"/"minimal", only the
// default temperature, no tools with an effort on chat completions; the
// Responses API takes everything.
func newPickyUpstream(t *testing.T) *pickyUpstream {
	p := &pickyUpstream{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		p.mu.Lock()
		p.requests = append(p.requests, b)
		p.paths = append(p.paths, r.URL.Path)
		p.mu.Unlock()
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6.1-sol"}]}`))
			return
		}
		if r.URL.Path == "/v1/responses" {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range []string{
				`{"type":"response.created","response":{"id":"resp_1","model":"gpt-6.1-sol"}}`,
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":""}}`,
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"city\":\"Paris\"}"}`,
				`{"type":"response.completed","response":{"usage":{"input_tokens":30,"output_tokens":12}}}`,
			} {
				_, _ = w.Write([]byte("data: " + ev + "\n\n"))
			}
			return
		}
		effort, _ := b["reasoning_effort"].(string)
		_, tools := b["tools"]
		temp, hasTemp := b["temperature"].(float64)
		refuse := func(body string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(body))
		}
		switch {
		case effort == "none" || effort == "minimal":
			refuse(strings.Replace(refuseNone, "'none'", "'"+effort+"'", 1))
		case hasTemp && temp != 1:
			refuse(refuseTemp)
		case tools:
			refuse(refuseTools)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"391"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
		}
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *pickyUpstream) chatCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, path := range p.paths {
		if path == "/v1/chat/completions" || path == "/v1/responses" {
			n++
		}
	}
	return n
}

func (h *harness) pickyModel(t *testing.T, p *pickyUpstream) {
	t.Helper()
	ctx := context.Background()
	up, err := h.store.CreateUpstream(ctx, "openai-picky", "openai_compatible", p.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "gpt-6.1-sol", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	models, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: up.ID})
	m := models[0]
	_ = h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled)
	if _, err := h.store.CreateGrant(ctx, m.ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
}

func adjustment(t *testing.T, h *harness) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		evs := h.waitForUsageEventsNoWait()
		if len(evs) > 0 {
			return evs[len(evs)-1].ReasoningAdjustment
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ""
}

// A reasoning value the model refuses is fitted to the nearest one it
// takes, and the next request goes out fitted without a refusal.
func TestCatalogReasoningEffortFittedAfterRefusal(t *testing.T) {
	h := newHarness(t)
	p := newPickyUpstream(t)
	h.pickyModel(t, p)
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "gpt-6.1-sol", "reasoning_effort": "none",
		"messages": []map[string]string{{"role": "user", "content": "17*23?"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerReasoningAdjusted); got != "none->low" {
		t.Fatalf("adjusted header = %q, want none->low", got)
	}
	if got := adjustment(t, h); got != "none->low" {
		t.Fatalf("usage reasoning_adjustment = %q", got)
	}
	before := p.chatCalls()
	rec = h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "gpt-6.1-sol", "reasoning_effort": "minimal",
		"messages": []map[string]string{{"role": "user", "content": "17*23?"}}})
	if rec.Code != http.StatusOK || p.chatCalls()-before != 1 {
		t.Fatalf("second request: %d with %d upstream calls, want 200 in one call", rec.Code, p.chatCalls()-before)
	}
}

// A sampling hint the model refuses is dropped (and stays dropped).
func TestCatalogRefusedTemperatureDropped(t *testing.T) {
	h := newHarness(t)
	p := newPickyUpstream(t)
	h.pickyModel(t, p)
	body := map[string]any{"model": "gpt-6.1-sol", "reasoning_effort": "high", "temperature": 0.7,
		"messages": []map[string]string{{"role": "user", "content": "17*23?"}}}
	rec := h.do(http.MethodPost, "/v1/chat/completions", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerReasoningAdjusted); got != "temperature->omitted" {
		t.Fatalf("adjusted = %q", got)
	}
	before := p.chatCalls()
	if rec := h.do(http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK || p.chatCalls()-before != 1 {
		t.Fatalf("second request: %d in %d calls", rec.Code, p.chatCalls()-before)
	}
}

// Tools refused on chat completions go through the Responses API, and the
// caller still gets a chat completion with the tool call.
func TestCatalogToolsFallBackToResponses(t *testing.T) {
	h := newHarness(t)
	p := newPickyUpstream(t)
	h.pickyModel(t, p)
	body := map[string]any{"model": "gpt-6.1-sol", "reasoning_effort": "medium",
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}}}},
		"messages": []map[string]string{{"role": "user", "content": "Weather in Paris?"}}}
	rec := h.do(http.MethodPost, "/v1/chat/completions", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name, Arguments string
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not a chat completion: %v %s", err, rec.Body.String())
	}
	if len(out.Choices) != 1 || len(out.Choices[0].Message.ToolCalls) != 1 || out.Choices[0].Message.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` ||
		out.Choices[0].FinishReason != "tool_calls" || out.Usage.PromptTokens != 30 {
		t.Fatalf("translated answer = %s", rec.Body.String())
	}
	if got := rec.Header().Get(headerReasoningAdjusted); got != "tools->responses" {
		t.Fatalf("adjusted = %q", got)
	}
	p.mu.Lock()
	last := p.requests[len(p.requests)-1]
	p.mu.Unlock()
	if r, _ := last["reasoning"].(map[string]any); r["effort"] != "medium" || r["summary"] != nil || last["instructions"] != nil {
		t.Fatalf("responses request = %v", last)
	}
	// Known from now on: straight to Responses, one call.
	before := p.chatCalls()
	if rec := h.do(http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK || p.chatCalls()-before != 1 {
		t.Fatalf("second tool request: %d in %d calls", rec.Code, p.chatCalls()-before)
	}
	p.mu.Lock()
	lastPath := p.paths[len(p.paths)-1]
	p.mu.Unlock()
	if lastPath != "/v1/responses" {
		t.Fatalf("second tool request went to %s", lastPath)
	}
	// A request without tools still uses chat completions.
	before = p.chatCalls()
	if rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "gpt-6.1-sol",
		"messages": []map[string]string{{"role": "user", "content": "hi"}}}); rec.Code != http.StatusOK {
		t.Fatalf("plain request: %d", rec.Code)
	}
	p.mu.Lock()
	lastPath = p.paths[len(p.paths)-1]
	p.mu.Unlock()
	if lastPath != "/v1/chat/completions" || p.chatCalls()-before != 1 {
		t.Fatalf("plain request went to %s", lastPath)
	}
}

// Everything at once (Open WebUI with a preset temperature, an effort the
// model refuses, and tools) is fitted within the retry budget.
func TestCatalogFitsSeveralRefusals(t *testing.T) {
	h := newHarness(t)
	p := newPickyUpstream(t)
	h.pickyModel(t, p)
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "gpt-6.1-sol", "reasoning_effort": "minimal", "temperature": 0.5,
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}}}},
		"messages": []map[string]string{{"role": "user", "content": "Weather in Paris?"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerReasoningAdjusted); got != "minimal->low temperature->omitted tools->responses" {
		t.Fatalf("adjusted = %q", got)
	}
}

// An unrelated 400 is relayed as it is, after one call.
func TestCatalogUnrelatedRefusalRelayed(t *testing.T) {
	h := newHarness(t)
	h.upstreamReply = ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6.1-sol"}]}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"messages: at least one message is required"}}`))
	}))
	defer srv.Close()
	h.pickyModel(t, &pickyUpstream{Server: srv})
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "gpt-6.1-sol", "reasoning_effort": "high", "temperature": 0.3,
		"messages": []map[string]string{}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "at least one message") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// Auto-routing forcing a level on a Claude route: the route's choice
// replaces the caller's, and the Anthropic adapter turns it into the form
// the model takes (Claude 5 refused the budget form it used to get).
func TestApplyThinkingOnClaudeRoutes(t *testing.T) {
	in := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1024},"temperature":0.2}`)
	out, _, ok := applyThinking(in, "high", "anthropic")
	if !ok {
		t.Fatal("not applied")
	}
	a, _ := adapter.Get("anthropic")
	prep, err := a.Prepare(adapter.Upstream{BaseURL: "https://api.anthropic.com/v1", APIKey: "k"}, &adapter.Request{Path: "/v1/chat/completions", Body: out, Model: "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(prep.Body, &m)
	th, _ := m["thinking"].(map[string]any)
	oc, _ := m["output_config"].(map[string]any)
	if th["type"] != "adaptive" || oc["effort"] != "high" || m["temperature"] != nil {
		t.Fatalf("claude 5.5 route body: %s", prep.Body)
	}
	in = []byte(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"hi"}]}`)
	out, _, _ = applyThinking(in, "high", "anthropic")
	prep, _ = a.Prepare(adapter.Upstream{BaseURL: "https://api.anthropic.com/v1", APIKey: "k"}, &adapter.Request{Path: "/v1/chat/completions", Body: out, Model: "claude-sonnet-4-5-20250929"})
	m = nil
	_ = json.Unmarshal(prep.Body, &m)
	th, _ = m["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] != float64(adapter.AnthropicThinkingBudgets["high"]) {
		t.Fatalf("claude 4.5 route body: %s", prep.Body)
	}
}
