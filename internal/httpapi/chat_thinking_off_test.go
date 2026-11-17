package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/capability"
	"github.com/torvanis/janus/internal/store"
)

// anthropicFake is a Messages API that records what it was sent.
type anthropicFake struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func (f *anthropicFake) sent() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.bodies...)
}

func (h *harness) anthropicModels(t *testing.T, names ...string) (*anthropicFake, map[string]*store.Model) {
	t.Helper()
	f := &anthropicFake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		f.mu.Lock()
		f.bodies = append(f.bodies, b)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"391"}],` +
			`"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":2}}`))
	}))
	t.Cleanup(f.Close)
	ctx := context.Background()
	up, err := h.store.CreateUpstream(ctx, "anthropic-thinking", "anthropic", f.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*store.Model{}
	for _, name := range names {
		if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, name, []string{"chat"}); err != nil {
			t.Fatal(err)
		}
		m, err := h.store.ModelByUpstreamAndName(ctx, up.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		_ = h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled)
		if _, err := h.store.CreateGrant(ctx, m.ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
			t.Fatal(err)
		}
		out[name] = m
	}
	h.server.InvalidateConfigCache()
	return f, out
}

// "Off" turns thinking off entirely wherever the model can do that, by the
// model's own switch; a model that always thinks gets its least thinking;
// and the user's model list says which is which, from the same rule.
func TestThinkingOffEntirely(t *testing.T) {
	h := newHarness(t)
	none := func(model string) *httptest.ResponseRecorder {
		return h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": model, "reasoning_effort": "none",
			"messages": []map[string]string{{"role": "user", "content": "17*23?"}}})
	}
	claude, models := h.anthropicModels(t, "claude-haiku-5-5", "claude-opus-5-5")
	levels := []string{"low", "medium", "high", "xhigh", "max"}
	// What Anthropic's model list says (read by the provider reader).
	h.record(t, models["claude-haiku-5-5"].ID, capability.SourceProvider, map[string]any{
		adapter.CapThinking: true, adapter.CapThinkingLevels: levels, adapter.CapThinkingOff: adapter.ThinkingOffDisabled})
	h.record(t, models["claude-opus-5-5"].ID, capability.SourceProvider, map[string]any{
		adapter.CapThinking: true, adapter.CapThinkingLevels: levels, adapter.CapThinkingOff: adapter.ThinkingOffNever})

	t.Run("Claude that can turn thinking off: thinking disabled", func(t *testing.T) {
		rec := none("claude-haiku-5-5")
		sent := claude.sent()
		if rec.Code != http.StatusOK || len(sent) != 1 {
			t.Fatalf("%d in %d calls: %s", rec.Code, len(sent), rec.Body.String())
		}
		thinking, _ := sent[0]["thinking"].(map[string]any)
		if thinking["type"] != "disabled" || sent[0]["output_config"] != nil || sent[0]["reasoning_effort"] != nil {
			t.Fatalf("sent %v", sent[0])
		}
		if got := rec.Header().Get(headerReasoningAdjusted); got != "none->thinking_disabled" {
			t.Errorf("adjusted = %q", got)
		}
		// Any other effort is still the adaptive form.
		if rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "claude-haiku-5-5", "reasoning_effort": "high",
			"messages": []map[string]string{{"role": "user", "content": "x"}}}); rec.Code != http.StatusOK {
			t.Fatalf("high: %d", rec.Code)
		}
		high := claude.sent()[1]
		if th, _ := high["thinking"].(map[string]any); th["type"] != "adaptive" {
			t.Errorf("high sent %v", high)
		}
	})
	t.Run("Claude that always thinks: its least effort", func(t *testing.T) {
		rec := none("claude-opus-5-5")
		sent := claude.sent()
		last := sent[len(sent)-1]
		cfg, _ := last["output_config"].(map[string]any)
		if rec.Code != http.StatusOK || cfg["effort"] != "low" {
			t.Fatalf("%d sent %v", rec.Code, last)
		}
		if got := rec.Header().Get(headerReasoningAdjusted); got != "none->low" {
			t.Errorf("adjusted = %q", got)
		}
	})
	t.Run("a measured switch beats a level list that holds none", func(t *testing.T) {
		// vLLM lists "none" among the values it accepts, yet the chat
		// template thinks anyway; the probe found enable_thinking works.
		srv, m := h.reasoningModel(t, "qwen-template", false)
		h.record(t, m.ID, capability.SourceProbe, map[string]any{adapter.CapThinkingLevels: []string{"none", "low", "high"}, adapter.CapThinkingOff: adapter.ThinkingOffEnableThinking})
		rec := none("qwen-template")
		sent := srv.sent()
		kwargs, _ := sent[0]["chat_template_kwargs"].(map[string]any)
		if rec.Code != http.StatusOK || sent[0]["reasoning_effort"] != nil || kwargs["enable_thinking"] != false {
			t.Fatalf("%d sent %v", rec.Code, sent)
		}
	})
	t.Run("measured to always think although none is accepted: least level", func(t *testing.T) {
		srv, m := h.reasoningModel(t, "always-thinks", false)
		h.record(t, m.ID, capability.SourceProbe, map[string]any{adapter.CapThinkingLevels: []string{"high", "none", "medium"}, adapter.CapThinkingOff: adapter.ThinkingOffNever})
		if rec := none("always-thinks"); rec.Code != http.StatusOK || srv.sent()[0]["reasoning_effort"] != "medium" {
			t.Fatalf("%d sent %v", rec.Code, srv.sent())
		}
	})

	// The user's model list carries the same answer, for Chat's picker.
	h.reasoningModel(t, "nothing-known", false)
	rec := h.do(http.MethodGet, "/api/v1/models", nil)
	var list struct {
		Models []struct {
			Name        string `json:"name"`
			ThinkingOff string `json:"thinking_off"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %v", rec.Code, err)
	}
	got := map[string]string{}
	for _, m := range list.Models {
		got[m.Name] = m.ThinkingOff
	}
	want := map[string]string{
		"claude-haiku-5-5": adapter.ThinkingOffDisabled, "claude-opus-5-5": adapter.ThinkingOffNever,
		"qwen-template": adapter.ThinkingOffEnableThinking, "always-thinks": adapter.ThinkingOffNever, "nothing-known": "",
	}
	for name, w := range want {
		if v, ok := got[name]; !ok || v != w {
			t.Errorf("%s: thinking_off = %q (listed %v), want %q", name, v, ok, w)
		}
	}
}

func TestThinkingOffSwitch(t *testing.T) {
	model := func(adapterType string, layers map[string]map[string]any) *store.Model {
		m := &store.Model{AdapterType: adapterType, Capabilities: map[string]capability.Resolved{}}
		for source, caps := range layers {
			for k, v := range caps {
				m.Capabilities[k] = capability.Resolved{Value: v, Source: source}
			}
		}
		return m
	}
	prov := capability.SourceProvider
	for _, c := range []struct {
		name string
		m    *store.Model
		want string
	}{
		{"nothing known", model("openai_compatible", nil), ""},
		{"provider switch the adapter carries", model("anthropic", map[string]map[string]any{prov: {adapter.CapThinkingOff: adapter.ThinkingOffDisabled}}), adapter.ThinkingOffDisabled},
		{"same switch, adapter not shown to carry it", model("openai_compatible", map[string]map[string]any{prov: {adapter.CapThinkingOff: adapter.ThinkingOffDisabled}}), ""},
		{"provider: always thinks", model("openai_compatible", map[string]map[string]any{prov: {adapter.CapThinkingOff: adapter.ThinkingOffNever}}), adapter.ThinkingOffNever},
		{"Gemini budget zero", model("gemini", map[string]map[string]any{prov: {adapter.CapThinkingOff: adapter.ThinkingOffBudgetZero}}), adapter.ThinkingOffBudgetZero},
		{"admin switch", model("ollama", map[string]map[string]any{capability.SourceAdmin: {adapter.CapThinkingOff: adapter.ThinkingOffThink}}), adapter.ThinkingOffThink},
		{"levels take none", model("openai_compatible", map[string]map[string]any{prov: {adapter.CapThinkingLevels: []string{"none", "high"}}}), adapter.ThinkingOffEffortNone},
		{"does not think", model("openai_compatible", map[string]map[string]any{prov: {adapter.CapThinking: false, adapter.CapThinkingOff: adapter.ThinkingOffNever}}), ""},
	} {
		if got := thinkingOffSwitch(c.m); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if thinkingOffSwitch(nil) != "" {
		t.Error("nil model")
	}
}

// A chat set to "Off" reaches the model as the model's own switch, not
// as an effort value it would ignore.
func TestChatOffSendsTheModelsSwitch(t *testing.T) {
	h, sess := chatHarness(t)
	m := h.modelNamed(t, "test-model")
	h.record(t, m.ID, capability.SourceProbe, map[string]any{adapter.CapThinkingLevels: []string{"none", "low", "high"}, adapter.CapThinkingOff: adapter.ThinkingOffEnableThinking})
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "test-model", "reasoning_effort": "none"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var conv struct {
		ID string `json:"id"`
	}
	decodeInto(t, rec, &conv)
	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": "What is 7 x 8?"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	body := lastUpstreamJSON(t, h)
	kwargs, _ := body["chat_template_kwargs"].(map[string]any)
	if body["reasoning_effort"] != nil || kwargs["enable_thinking"] != false {
		t.Fatalf("sent reasoning_effort=%v chat_template_kwargs=%v", body["reasoning_effort"], body["chat_template_kwargs"])
	}
}
