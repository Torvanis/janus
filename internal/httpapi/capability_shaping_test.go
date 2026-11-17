package httpapi

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
	"github.com/torvanis/janus/internal/capability"
	"github.com/torvanis/janus/internal/store"
)

// record sets one layer of a model's capability record and makes the
// proxy see it.
func (h *harness) record(t *testing.T, modelID, source string, caps map[string]any) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := h.store.UpdateModelCapabilities(context.Background(), modelID, func(s *capability.State) (bool, error) {
		switch source {
		case capability.SourceAdmin:
			return true, s.SetAdmin(caps, now)
		case capability.SourceProbe:
			s.Probe = capability.Facts{}
			for k, v := range caps {
				n, err := capability.Normalize(k, v)
				if err != nil {
					return false, err
				}
				s.Probe[k] = capability.Fact{Value: n, At: now, Note: "measured in a test"}
			}
		default:
			s.SetProvider(capability.FromProvider(adapter.Capabilities(caps), now))
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
}

func (h *harness) modelNamed(t *testing.T, name string) *store.Model {
	t.Helper()
	models, err := h.store.ListModels(context.Background(), store.ModelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no model %q", name)
	return nil
}

func (p *pickyUpstream) last() (map[string]any, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[len(p.requests)-1], p.paths[len(p.paths)-1]
}

// What is known about a model is applied before the request is sent: no
// refusal, one call.
func TestRequestShapedFromCapabilityRecord(t *testing.T) {
	h := newHarness(t)
	p := newPickyUpstream(t)
	h.pickyModel(t, p)
	m := h.modelNamed(t, "gpt-6.1-sol")
	h.record(t, m.ID, capability.SourceProbe, map[string]any{
		adapter.CapThinkingLevels: []string{"low", "medium", "high", "xhigh"}, adapter.CapRefusedParams: []string{"temperature"},
		adapter.CapTools: true, adapter.CapToolsEndpoint: adapter.ToolsEndpointResponses,
	})
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}}}}

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "gpt-6.1-sol", "reasoning_effort": "none", "temperature": 0.7,
		"messages": []map[string]string{{"role": "user", "content": "17*23?"}}})
	body, path := p.last()
	if rec.Code != http.StatusOK || p.chatCalls() != 1 || path != "/v1/chat/completions" {
		t.Fatalf("%d in %d calls to %s: %s", rec.Code, p.chatCalls(), path, rec.Body.String())
	}
	if body["reasoning_effort"] != "low" || body["temperature"] != nil {
		t.Errorf("sent %v", body)
	}
	if got := rec.Header().Get(headerReasoningAdjusted); got != "none->low temperature->omitted" {
		t.Errorf("adjusted = %q", got)
	}

	// Tools go straight to the endpoint that takes them.
	rec = h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "gpt-6.1-sol", "reasoning_effort": "high", "tools": tools,
		"messages": []map[string]string{{"role": "user", "content": "weather in Paris?"}}})
	if _, path := p.last(); rec.Code != http.StatusOK || p.chatCalls() != 2 || path != "/v1/responses" {
		t.Fatalf("tools: %d, %d calls, last to %s", rec.Code, p.chatCalls(), path)
	}

	// The catalog tells clients which values the model takes.
	rec = h.do(http.MethodGet, "/v1/models", nil)
	var list struct {
		Data []struct {
			ID    string `json:"id"`
			Janus struct {
				Efforts []string `json:"reasoning_efforts"`
			} `json:"janus"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	found := false
	for _, e := range list.Data {
		switch e.ID {
		case "gpt-6.1-sol":
			found = strings.Join(e.Janus.Efforts, ",") == "low,medium,high,xhigh"
		case "test-model":
			if e.Janus.Efforts != nil {
				t.Errorf("a model nothing is known about must not list efforts: %v", e.Janus.Efforts)
			}
		}
	}
	if !found {
		t.Errorf("/v1/models does not list the accepted efforts: %s", rec.Body.String())
	}
}

// A caller's refused request fixes that request. Where the record already
// speaks it changes nothing shared: the next request is sent as the record
// says, and the model is measured again by Janus itself.
func TestRefusalDoesNotOverrideTheRecord(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.SetCapabilityProbeSetting(ctx, store.CapabilityProbeSetting{OnEnable: true}); err != nil {
		t.Fatal(err)
	}
	p := newPickyUpstream(t)
	h.pickyModel(t, p)
	m := h.modelNamed(t, "gpt-6.1-sol")
	_ = h.store.SetModelRates(ctx, m.ID, store.RateCard{RateInNano: 1, RateOutNano: 1, EffectiveFrom: time.Now().UTC().Add(-time.Hour)})
	// The administrator's record is wrong on purpose: it says "none" is taken and no sampling setting is refused.
	h.record(t, m.ID, capability.SourceAdmin, map[string]any{
		adapter.CapThinkingLevels: []string{"none", "low", "medium", "high"}, adapter.CapRefusedParams: []string{},
	})
	ask := func(body map[string]any) (*httptest.ResponseRecorder, int) {
		t.Helper()
		before := p.chatCalls()
		body["model"], body["messages"] = "gpt-6.1-sol", []map[string]string{{"role": "user", "content": "17*23?"}}
		rec := h.do(http.MethodPost, "/v1/chat/completions", body)
		return rec, p.chatCalls() - before
	}

	rec, _ := ask(map[string]any{"reasoning_effort": "none"})
	if rec.Code != http.StatusOK || rec.Header().Get(headerReasoningAdjusted) != "none->low" {
		t.Fatalf("the refused request must still be fixed and answered: %d %q", rec.Code, rec.Header().Get(headerReasoningAdjusted))
	}
	// The refusal made Janus measure the model again, once.
	done := h.probeDone(t, m.ID)
	if done.CapabilityProbe.By != "refusal" || done.CapabilityProbe.Status != capability.ProbeOK {
		t.Fatalf("no measurement after the refusal: %+v", done.CapabilityProbe)
	}
	if got := done.Capabilities[adapter.CapThinkingLevels]; got.Source != capability.SourceAdmin {
		t.Errorf("the administrator's value must still win: %+v", got)
	}
	h.server.InvalidateConfigCache()

	// Nothing was learned for everyone: the same request is refused first again.
	if rec, calls := ask(map[string]any{"reasoning_effort": "none"}); rec.Code != http.StatusOK || calls != 2 {
		t.Errorf("second request: %d in %d calls, want the refusal and the fixed resend", rec.Code, calls)
	}
	if h.server.routedReasoning(m.ID) != nil {
		t.Errorf("a refusal changed what is shared about a model whose record already speaks")
	}
	for i := 0; i < 2; i++ {
		if rec, calls := ask(map[string]any{"reasoning_effort": "high", "temperature": 0.7}); rec.Code != http.StatusOK || calls != 2 || rec.Header().Get(headerReasoningAdjusted) != "temperature->omitted" {
			t.Errorf("temperature, request %d: %d in %d calls, %q", i+1, rec.Code, calls, rec.Header().Get(headerReasoningAdjusted))
		}
	}
	if q := h.server.modelQuirks(m.ID); len(q.dropParams) != 0 {
		t.Errorf("a refusal was remembered for everyone: %+v", q)
	}
	// One measurement per model per window, however many refusals arrive.
	time.Sleep(100 * time.Millisecond)
	if again, _ := h.store.ModelByID(ctx, m.ID); !again.CapabilityProbe.At.Equal(done.CapabilityProbe.At) {
		t.Errorf("a second measurement was started by later refusals")
	}
}

// reasoningServer answers chat requests and records them; it refuses
// max_tokens when told to.
type reasoningServer struct {
	*httptest.Server
	mu              sync.Mutex
	bodies          []map[string]any
	refuseMaxTokens bool
}

func (h *harness) reasoningModel(t *testing.T, name string, refuseMaxTokens bool) (*reasoningServer, *store.Model) {
	t.Helper()
	srv := &reasoningServer{refuseMaxTokens: refuseMaxTokens}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		srv.mu.Lock()
		srv.bodies = append(srv.bodies, b)
		srv.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if _, has := b["max_tokens"]; has && srv.refuseMaxTokens {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"max_tokens"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"391"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	up, err := h.store.CreateUpstream(ctx, "up-"+name, "openai_compatible", srv.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
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
	h.server.InvalidateConfigCache()
	return srv, m
}

func (s *reasoningServer) sent() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any{}, s.bodies...)
}

func TestOutputLimitParameterIsRenamed(t *testing.T) {
	h := newHarness(t)
	ask := func(model string) *httptest.ResponseRecorder {
		return h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": model, "max_tokens": 321,
			"messages": []map[string]string{{"role": "user", "content": "17*23?"}}})
	}
	t.Run("known from the record: renamed before it is sent", func(t *testing.T) {
		srv, m := h.reasoningModel(t, "o-known", true)
		h.record(t, m.ID, capability.SourceProbe, map[string]any{adapter.CapOutputCap: adapter.OutputCapMaxCompletionTokens})
		rec := ask("o-known")
		sent := srv.sent()
		if rec.Code != http.StatusOK || len(sent) != 1 || sent[0]["max_completion_tokens"] != float64(321) || sent[0]["max_tokens"] != nil {
			t.Fatalf("%d, sent %v", rec.Code, sent)
		}
		if got := rec.Header().Get(headerReasoningAdjusted); got != "max_tokens->max_completion_tokens" {
			t.Errorf("adjusted = %q", got)
		}
	})
	t.Run("not known: the refusal is fixed, and remembered while the record is silent", func(t *testing.T) {
		srv, _ := h.reasoningModel(t, "o-unknown", true)
		if rec := ask("o-unknown"); rec.Code != http.StatusOK || len(srv.sent()) != 2 {
			t.Fatalf("%d in %d calls: %s", rec.Code, len(srv.sent()), rec.Body.String())
		}
		if rec := ask("o-unknown"); rec.Code != http.StatusOK || len(srv.sent()) != 3 || srv.sent()[2]["max_completion_tokens"] != float64(321) {
			t.Fatalf("second request: %d, %d calls, %v", rec.Code, len(srv.sent()), srv.sent()[len(srv.sent())-1])
		}
	})
	t.Run("a model that takes max_tokens is left alone", func(t *testing.T) {
		srv, m := h.reasoningModel(t, "o-plain", false)
		h.record(t, m.ID, capability.SourceProbe, map[string]any{adapter.CapOutputCap: adapter.OutputCapMaxTokens})
		if rec := ask("o-plain"); rec.Code != http.StatusOK || srv.sent()[0]["max_tokens"] != float64(321) || rec.Header().Get(headerReasoningAdjusted) != "" {
			t.Fatalf("%d %v", rec.Code, srv.sent())
		}
	})
}

// "No reasoning" is sent as the switch the model uses, when that switch
// was shown to work.
func TestNoReasoningUsesTheModelsOwnSwitch(t *testing.T) {
	h := newHarness(t)
	ask := func(model string, extra map[string]any) *httptest.ResponseRecorder {
		body := map[string]any{"model": model, "reasoning_effort": "none", "messages": []map[string]string{{"role": "user", "content": "17*23?"}}}
		for k, v := range extra {
			body[k] = v
		}
		return h.do(http.MethodPost, "/v1/chat/completions", body)
	}
	t.Run("measured: enable_thinking false", func(t *testing.T) {
		srv, m := h.reasoningModel(t, "qwen-local", false)
		h.record(t, m.ID, capability.SourceProbe, map[string]any{adapter.CapThinking: true, adapter.CapThinkingOff: adapter.ThinkingOffEnableThinking})
		rec := ask("qwen-local", map[string]any{"chat_template_kwargs": map[string]any{"add_generation_prompt": true}})
		sent := srv.sent()
		kwargs, _ := sent[0]["chat_template_kwargs"].(map[string]any)
		if rec.Code != http.StatusOK || len(sent) != 1 || sent[0]["reasoning_effort"] != nil || kwargs["enable_thinking"] != false || kwargs["add_generation_prompt"] != true {
			t.Fatalf("%d, sent %v", rec.Code, sent)
		}
		if got := rec.Header().Get(headerReasoningAdjusted); got != "none->enable_thinking_false" {
			t.Errorf("adjusted = %q", got)
		}
		// Any other effort is not touched.
		if rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "qwen-local", "reasoning_effort": "high",
			"messages": []map[string]string{{"role": "user", "content": "x"}}}); rec.Code != http.StatusOK || srv.sent()[1]["reasoning_effort"] != "high" || srv.sent()[1]["chat_template_kwargs"] != nil {
			t.Errorf("high: %v", srv.sent()[1])
		}
	})
	t.Run("the model takes effort none: sent as it is", func(t *testing.T) {
		srv, m := h.reasoningModel(t, "takes-none", false)
		h.record(t, m.ID, capability.SourceProbe, map[string]any{adapter.CapThinkingLevels: []string{"none", "low", "high"}, adapter.CapThinkingOff: adapter.ThinkingOffEffortNone})
		if rec := ask("takes-none", nil); rec.Code != http.StatusOK || srv.sent()[0]["reasoning_effort"] != "none" || rec.Header().Get(headerReasoningAdjusted) != "" {
			t.Fatalf("%d %v", rec.Code, srv.sent())
		}
	})
	t.Run("a switch only the provider's list names is not used", func(t *testing.T) {
		srv, m := h.reasoningModel(t, "listed-only", false)
		h.record(t, m.ID, capability.SourceProvider, map[string]any{adapter.CapThinking: true, adapter.CapThinkingOff: adapter.ThinkingOffThink})
		if rec := ask("listed-only", nil); rec.Code != http.StatusOK || srv.sent()[0]["reasoning_effort"] != "none" || srv.sent()[0]["think"] != nil {
			t.Fatalf("%d %v", rec.Code, srv.sent())
		}
	})
	t.Run("a model that does not reason gets no effort at all", func(t *testing.T) {
		srv, m := h.reasoningModel(t, "plain", false)
		h.record(t, m.ID, capability.SourceProvider, map[string]any{adapter.CapThinking: false})
		if rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "plain", "reasoning_effort": "medium",
			"messages": []map[string]string{{"role": "user", "content": "x"}}}); rec.Code != http.StatusOK || srv.sent()[0]["reasoning_effort"] != nil {
			t.Fatalf("%d %v", rec.Code, srv.sent())
		}
	})
}

// Chat decides what a model can be handed from the record alone; with
// nothing known the answer is no, whatever the model is called (owner rule:
// nothing is inferred from a model's name).
func TestChatReadsWhatAModelTakesFromTheRecord(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, sees := h.reasoningModel(t, "plain-local-model", false)
	_, blind := h.reasoningModel(t, "gpt-4o-lookalike", false)
	_, named := h.reasoningModel(t, "gemini-omni-lookalike", false)
	for _, name := range []string{"plain-local-model", "gpt-4o-lookalike", "gemini-omni-lookalike"} {
		if h.server.chatVision(ctx, nil, name) || h.server.chatTakes(ctx, nil, name, store.ChatAttachmentAudio) || h.server.chatTakes(ctx, nil, name, store.ChatAttachmentVideo) {
			t.Errorf("%s: nothing is known, yet Chat treats it as taking media (a guess from the name)", name)
		}
	}
	h.record(t, sees.ID, capability.SourceProbe, map[string]any{adapter.CapInImage: true, adapter.CapInAudio: true})
	h.record(t, blind.ID, capability.SourceProvider, map[string]any{adapter.CapInImage: false, adapter.CapInVideo: false})
	if !h.server.chatVision(ctx, nil, "plain-local-model") {
		t.Error("a model measured to see images was treated as blind")
	}
	if h.server.chatVision(ctx, nil, "gpt-4o-lookalike") {
		t.Error("a model its provider says takes no images was sent them")
	}
	if !h.server.chatTakes(ctx, nil, "plain-local-model", store.ChatAttachmentAudio) || h.server.chatTakes(ctx, nil, "plain-local-model", store.ChatAttachmentVideo) {
		t.Error("audio is known, video is not")
	}
	if h.server.chatTakes(ctx, nil, "gpt-4o-lookalike", store.ChatAttachmentVideo) {
		t.Error("video is known to be refused")
	}
	// Administrator > probe > provider.
	h.record(t, named.ID, capability.SourceProvider, map[string]any{adapter.CapInVideo: true, adapter.CapInAudio: true})
	h.record(t, named.ID, capability.SourceProbe, map[string]any{adapter.CapInAudio: false})
	h.record(t, named.ID, capability.SourceAdmin, map[string]any{adapter.CapInVideo: false})
	if h.server.chatTakes(ctx, nil, "gemini-omni-lookalike", store.ChatAttachmentVideo) {
		t.Error("the administrator's No lost to the provider's Yes")
	}
	if h.server.chatTakes(ctx, nil, "gemini-omni-lookalike", store.ChatAttachmentAudio) {
		t.Error("the measured No lost to the provider's Yes")
	}
}

func TestProfileReasoning(t *testing.T) {
	h := newHarness(t)
	_, m := h.reasoningModel(t, "m", false)
	if profileReasoning(m) != nil || profileReasoning(nil) != nil {
		t.Error("nothing known must be nil, so what a replica learned can fill in")
	}
	h.record(t, m.ID, capability.SourceProvider, map[string]any{adapter.CapThinking: true, adapter.CapThinkingLevels: []string{}})
	got, _ := h.store.ModelByID(context.Background(), m.ID)
	if r := profileReasoning(got); r == nil || len(r.Efforts()) != 0 || r.Efforts() == nil {
		t.Errorf("a reasoning model that takes no effort setting: %+v", r)
	}
	if sent, changed := profileReasoning(got).Fit("high"); sent != "" || !changed {
		t.Errorf("the effort must be left out: %q %v", sent, changed)
	}
}
