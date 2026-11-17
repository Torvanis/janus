package httpapi

import (
	"context"
	"encoding/base64"
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

// probedProvider is a provider with a hosted reasoning model: it refuses
// max_tokens and temperature, lists its effort levels when given a bad one,
// stops reasoning at "none", sees images, calls tools and fits a schema.
// Like OpenAI's chat models it takes no sound or video parts, and says so.
type probedProvider struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	// gate, when set, holds every request until it is closed.
	gate       chan struct{}
	upstreamID string
}

func newProbedProvider(t *testing.T) *probedProvider {
	p := &probedProvider{}
	refuse := func(w http.ResponseWriter, param, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "param": param, "type": "invalid_request_error"}})
	}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		p.mu.Lock()
		p.bodies = append(p.bodies, body)
		gate := p.gate
		p.mu.Unlock()
		if gate != nil {
			<-gate
		}
		if _, has := body["max_tokens"]; has {
			refuse(w, "max_tokens", "Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.")
			return
		}
		if _, has := body["temperature"]; has {
			refuse(w, "temperature", "Unsupported value: 'temperature' does not support 0.7 with this model.")
			return
		}
		reasoning := 24
		if effort, has := body["reasoning_effort"].(string); has {
			switch effort {
			case "none":
				reasoning = 0
			case "low", "medium", "high":
			default:
				refuse(w, "reasoning_effort", "Invalid value: '"+effort+"'. Supported values are: 'none', 'low', 'medium', and 'high'.")
				return
			}
		}
		if strings.Contains(string(raw), `"input_audio"`) {
			refuse(w, "messages[0]", "Invalid 'messages[0]'. Content blocks are expected to be either text or image_url type.")
			return
		}
		if strings.Contains(string(raw), `"video_url"`) {
			refuse(w, "messages[0].content[1].type", "Invalid value: 'video_url'. Supported values are: 'text', 'image_url', 'input_audio', 'refusal', 'audio', and 'file'.")
			return
		}
		msg := map[string]any{"role": "assistant", "content": "43"}
		if strings.Contains(string(raw), "image_url") {
			msg["content"] = "Green"
		}
		if _, has := body["tools"]; has {
			msg["content"] = nil
			msg["tool_calls"] = []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "janus_probe_time", "arguments": "{}"}}}
		}
		if _, has := body["response_format"]; has {
			msg["content"] = `{"ok":true}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "cmpl-probe", "choices": []any{map[string]any{"message": msg, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 4 + reasoning, "completion_tokens_details": map[string]any{"reasoning_tokens": reasoning}}})
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *probedProvider) sent() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any{}, p.bodies...)
}

// probeModel adds a priced chat model on the provider, in the given status.
func (h *harness) probeModel(t *testing.T, p *probedProvider, name, status string, modalities ...string) *store.Model {
	t.Helper()
	ctx := context.Background()
	if p.upstreamID == "" {
		up, err := h.store.CreateUpstream(ctx, "probed", "openai_compatible", p.URL, "", "")
		if err != nil {
			t.Fatal(err)
		}
		p.upstreamID = up.ID
	}
	if len(modalities) == 0 {
		modalities = []string{"chat"}
	}
	if _, err := h.store.UpsertDiscoveredModel(ctx, p.upstreamID, name, modalities); err != nil {
		t.Fatal(err)
	}
	m, err := h.store.ModelByUpstreamAndName(ctx, p.upstreamID, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelRates(ctx, m.ID, store.RateCard{RateInNano: 10_000_000_000, RateOutNano: 30_000_000_000, EffectiveFrom: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelStatus(ctx, m.ID, status); err != nil {
		t.Fatal(err)
	}
	return m
}

// probeDone waits for the model's probe to leave the running state.
func (h *harness) probeDone(t *testing.T, id string) *store.Model {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, err := h.store.ModelByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if p := m.CapabilityProbe; p != nil && p.Status != capability.ProbeRunning && p.Status != capability.ProbeQueued {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the probe did not finish")
	return nil
}

func probeUsage(t *testing.T, h *harness, want int) []*store.UsageEvent {
	t.Helper()
	var out []*store.UsageEvent
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out = out[:0]
		evs, _, _ := h.store.ListRequests(context.Background(), store.RequestFilter{Limit: 200, IncludeInternal: true})
		for _, ev := range evs {
			if ev.ClientApp == store.InternalProbeClientApp {
				out = append(out, ev)
			}
		}
		if len(out) >= want {
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("probe usage rows: %d, want %d", len(out), want)
	return nil
}

func TestCapabilityProbeMeasuresAndRecords(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := newProbedProvider(t)
	m := h.probeModel(t, p, "reasoner-1", store.ModelEnabled)
	admin := adminSession(t, h)
	// The administrator already said this model takes no files; a probe leaves that alone.
	if rec := h.doAsSession(admin, http.MethodPut, "/api/v1/admin/models/"+m.ID+"/capabilities", map[string]any{"overrides": map[string]any{adapter.CapStructured: false}}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}

	rec := h.doAsSession(admin, http.MethodPost, "/api/v1/admin/models/"+m.ID+"/capabilities/probe", nil)
	var view struct {
		Probed capability.ProbeRun `json:"probed"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if rec.Code != http.StatusAccepted || view.Probed.Status != capability.ProbeRunning || view.Probed.By != "manual" {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	done := h.probeDone(t, m.ID)
	run, caps := done.CapabilityProbe, done.Capabilities
	if run.Status != capability.ProbeOK || run.Requests != 13 || run.InputTokens != 160 || run.OutputTokens == 0 {
		t.Fatalf("run: %+v", run)
	}
	want := map[string]any{
		adapter.CapOutputCap: adapter.OutputCapMaxCompletionTokens, adapter.CapThinking: true, adapter.CapThinkingDefault: true, adapter.CapThinkingOff: adapter.ThinkingOffEffortNone,
		adapter.CapInImage: true, adapter.CapTools: true,
	}
	for k, v := range want {
		if caps[k].Value != v || caps[k].Source != capability.SourceProbe || caps[k].Note == "" {
			t.Errorf("%s = %+v, want %v measured with a note", k, caps[k], v)
		}
	}
	if l, _ := caps[adapter.CapThinkingLevels].Value.([]string); strings.Join(l, ",") != "none,low,medium,high" {
		t.Errorf("levels: %+v", caps[adapter.CapThinkingLevels])
	}
	if l, _ := caps[adapter.CapRefusedParams].Value.([]string); strings.Join(l, ",") != "temperature" {
		t.Errorf("refused: %+v", caps[adapter.CapRefusedParams])
	}
	// Sound and video went as Chat sends attachments, and the provider's refusals are the answer.
	for key, note := range map[string]string{adapter.CapInAudio: "Content blocks are expected", adapter.CapInVideo: "Invalid value: 'video_url'"} {
		if c := caps[key]; c.Value != false || c.Source != capability.SourceProbe || !strings.Contains(c.Note, note) {
			t.Errorf("%s = %+v, want no from the provider's refusal", key, c)
		}
	}
	var audioPart, videoPart map[string]any
	for _, b := range p.sent() {
		parts, _ := b["messages"].([]any)[0].(map[string]any)["content"].([]any)
		for _, part := range parts {
			switch part := part.(map[string]any); part["type"] {
			case "input_audio":
				audioPart = part["input_audio"].(map[string]any)
			case "video_url":
				videoPart = part["video_url"].(map[string]any)
			}
		}
	}
	if audioPart == nil || audioPart["format"] != "wav" || audioPart["data"] != base64.StdEncoding.EncodeToString(capability.ProbeSound()) {
		t.Errorf("input_audio part: %v", audioPart != nil)
	}
	if videoPart == nil || videoPart["url"] != "data:video/mp4;base64,"+base64.StdEncoding.EncodeToString(capability.ProbeVideo()) {
		t.Errorf("video_url part: %v", videoPart != nil)
	}
	if caps[adapter.CapStructured].Source != capability.SourceAdmin || caps[adapter.CapStructured].Value != false {
		t.Errorf("the probe overrode the administrator: %+v", caps[adapter.CapStructured])
	}
	sent := p.sent()
	for i, b := range sent {
		if b["model"] != "reasoner-1" {
			t.Errorf("request %d went to model %v", i, b["model"])
		}
		if _, has := b["response_format"]; has {
			t.Errorf("request %d measured what the administrator had already set", i)
		}
	}

	// Every request is metered as platform overhead and kept out of the request log.
	rows := probeUsage(t, h, 13)
	paid := 0
	for _, ev := range rows {
		if ev.EndpointPath != "/internal/capability/probe" || ev.ModelID != m.ID || ev.UserID != "" || !strings.HasPrefix(ev.RequestID, "capprobe-") {
			t.Errorf("usage row: %+v", ev)
		}
		if ev.HTTPStatus == 200 {
			paid++
			if ev.TokensIn != 20 || ev.CostNano == 0 {
				t.Errorf("an answered request must be metered: %+v", ev)
			}
		} else if ev.CostNano != 0 {
			t.Errorf("a refused request must cost nothing: %+v", ev)
		}
	}
	if paid != 8 {
		t.Errorf("answered requests: %d", paid)
	}
	if listed, _, _ := h.store.ListRequests(ctx, store.RequestFilter{Limit: 200}); len(listed) != 0 {
		t.Errorf("probe requests show in the request log: %d", len(listed))
	}
	if entries, _, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "model_capabilities_probed", Limit: 5}); err != nil || len(entries) != 1 {
		t.Errorf("audit: %v %d", err, len(entries))
	}

	// The provider goes away: the next probe fails and what was measured stays.
	p.Close()
	if rec := h.doAsSession(admin, http.MethodPost, "/api/v1/admin/models/"+m.ID+"/capabilities/probe", nil); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body.String())
	}
	failed := h.probeDone(t, m.ID)
	if failed.CapabilityProbe.Status != capability.ProbeFailed || failed.CapabilityProbe.Detail == "" || failed.Capabilities[adapter.CapInImage].Source != capability.SourceProbe {
		t.Errorf("failed run: %+v %+v", failed.CapabilityProbe, failed.Capabilities[adapter.CapInImage])
	}
}

func TestCapabilityProbeOnEnable(t *testing.T) {
	h := newHarness(t)
	p := newProbedProvider(t)
	admin := adminSession(t, h)
	// The harness turns measuring off; a new installation has it on
	// (TestCapabilityProbeSettingDefault).
	if err := h.store.SetCapabilityProbeSetting(context.Background(), store.CapabilityProbeSetting{OnEnable: true}); err != nil {
		t.Fatal(err)
	}
	enable := func(m *store.Model) {
		t.Helper()
		if rec := h.doAsSession(admin, http.MethodPatch, "/api/v1/admin/models/"+m.ID, map[string]any{"status": "enabled"}); rec.Code != http.StatusOK {
			t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
		}
	}

	first := h.probeModel(t, p, "reasoner-1", store.ModelPending)
	enable(first)
	if done := h.probeDone(t, first.ID); done.CapabilityProbe.Status != capability.ProbeOK || done.CapabilityProbe.By != "enabled" || done.Capabilities[adapter.CapTools].Value != true {
		t.Fatalf("enabling did not measure the model: %+v", done.CapabilityProbe)
	}
	// Editing a model that is already enabled measures nothing.
	before := len(p.sent())
	if rec := h.doAsSession(admin, http.MethodPatch, "/api/v1/admin/models/"+first.ID, map[string]any{"display_name": "Reasoner", "status": "enabled"}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	// A model that makes images is never sent a request.
	image := h.probeModel(t, p, "painter-1", store.ModelPending, "image")
	enable(image)
	if rec := h.doAsSession(admin, http.MethodPost, "/api/v1/admin/models/"+image.ID+"/capabilities/probe", nil); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "only chat models") {
		t.Errorf("an image model was accepted for measuring: %d %s", rec.Code, rec.Body.String())
	}

	// Turned off: enabling measures nothing; the button still works.
	if rec := h.doAsSession(admin, http.MethodPut, "/api/v1/admin/system/capability-probe", map[string]any{"on_enable": false}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if rec := h.doAsSession(admin, http.MethodGet, "/api/v1/admin/system/capability-probe", nil); !strings.Contains(rec.Body.String(), `"on_enable":false`) {
		t.Errorf("setting not stored: %s", rec.Body.String())
	}
	if rec := h.doAsSession(admin, http.MethodPut, "/api/v1/admin/system/capability-probe", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty setting: %d", rec.Code)
	}
	second := h.probeModel(t, p, "reasoner-2", store.ModelPending)
	enable(second)
	time.Sleep(150 * time.Millisecond)
	if got, _ := h.store.ModelByID(context.Background(), second.ID); got.CapabilityProbe != nil || len(p.sent()) != before {
		t.Errorf("measured with the setting off: %+v, %d new requests", got.CapabilityProbe, len(p.sent())-before)
	}

	// Measure all: only what has no finished measurement.
	rec := h.doAsSession(admin, http.MethodPost, "/api/v1/admin/models/capabilities/probe", map[string]any{"only_unmeasured": true})
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"started":2`) {
		// reasoner-2 and the harness's own test-model; reasoner-1 is done, painter-1 is not a chat model.
		t.Fatalf("measure all: %d %s", rec.Code, rec.Body.String())
	}
	if done := h.probeDone(t, second.ID); done.CapabilityProbe.Status != capability.ProbeOK || done.CapabilityProbe.By != "all" {
		t.Errorf("measure all: %+v", done.CapabilityProbe)
	}
	h.probeDone(t, h.model.ID)

	_, token := h.otherUser(t, "probe-member", "probe-member@example.com")
	for _, path := range []string{"/api/v1/admin/models/" + first.ID + "/capabilities/probe", "/api/v1/admin/models/capabilities/probe"} {
		if rec := h.doWithToken(token, http.MethodPost, path, map[string]any{}); rec.Code != http.StatusForbidden {
			t.Errorf("non-admin %s: %d", path, rec.Code)
		}
	}
	if rec := h.doWithToken(token, http.MethodPut, "/api/v1/admin/system/capability-probe", map[string]any{"on_enable": true}); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin setting: %d", rec.Code)
	}
}

// Two people press the button: one probe runs.
func TestCapabilityProbeRunsOnceAtATime(t *testing.T) {
	h := newHarness(t)
	p := newProbedProvider(t)
	p.gate = make(chan struct{})
	m := h.probeModel(t, p, "reasoner-1", store.ModelEnabled)
	admin := adminSession(t, h)
	for i := 0; i < 3; i++ {
		if rec := h.doAsSession(admin, http.MethodPost, "/api/v1/admin/models/"+m.ID+"/capabilities/probe", nil); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"status":"running"`) {
			t.Fatalf("press %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(p.sent()); n != 1 {
		t.Errorf("%d first requests in flight, want 1", n)
	}
	close(p.gate)
	if done := h.probeDone(t, m.ID); done.CapabilityProbe.Requests != 14 {
		t.Errorf("run: %+v", done.CapabilityProbe)
	}
	if n := len(p.sent()); n != 14 {
		t.Errorf("%d requests sent in all, want one probe's 14", n)
	}
	if entries, _, _ := h.store.ListAudit(context.Background(), store.AuditFilter{Action: "model_capabilities_probed", Limit: 5}); len(entries) != 1 {
		t.Errorf("audit rows: %d", len(entries))
	}
}
