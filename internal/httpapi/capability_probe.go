package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/capability"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/usage"
)

// Capability probes: Janus measures what a model's provider does not say
// (internal/capability/probe.go has the measuring; this file sends the
// requests, records what they cost and stores the result).
//
// A probe runs when an administrator enables a chat model (unless that is
// turned off), and when one asks for it. Models that were already enabled
// before this existed are not measured on upgrade: the "measure all" action
// does that when an administrator chooses.

const capabilityProbeTimeout = 5 * time.Minute

func (s *Server) mountCapabilityProbeRoutes(r chi.Router) {
	r.Post("/admin/models/{id}/capabilities/probe", s.handleProbeModelCapabilities)
	r.Post("/admin/models/capabilities/probe", s.handleProbeAllModelCapabilities)
	r.Get("/admin/system/capability-probe", s.handleGetCapabilityProbeSetting)
	r.Put("/admin/system/capability-probe", s.handlePutCapabilityProbeSetting)
}

// probeSkipReason says why a model is not one a probe measures, or "".
// Only chat models are probed: a probe of a model that makes images, audio
// or video would cost real money, and embeddings have nothing to measure.
func probeSkipReason(m *store.Model) string {
	if m.ClassifierRole != "" {
		return "classifier and decision models are not measured"
	}
	for _, modality := range m.Modalities {
		if modality == "chat" {
			return ""
		}
	}
	return "only chat models are measured; this one is " + strings.Join(m.Modalities, ", ")
}

// claimCapabilityProbe marks the model as being measured. It returns the
// model as stored and whether this caller holds the claim.
func (s *Server) claimCapabilityProbe(ctx context.Context, modelID, by string) (*store.Model, bool, error) {
	claimed := false
	m, err := s.Store.UpdateModelCapabilities(ctx, modelID, func(state *capability.State) (bool, error) {
		claimed = state.Claim(by, time.Now().UTC())
		return claimed, nil
	})
	return m, claimed, err
}

// startCapabilityProbe claims the model and measures it in the background.
// It returns the model with the claim on it. started is false when another
// probe of the model is already running.
func (s *Server) startCapabilityProbe(ctx context.Context, modelID, by string) (m *store.Model, started bool, err error) {
	m, claimed, err := s.claimCapabilityProbe(ctx, modelID, by)
	if err != nil || !claimed {
		return m, false, err
	}
	s.InvalidateConfigCache()
	go func() {
		defer func() {
			if p := recover(); p != nil {
				s.Logger.Error("capability probe panicked", "panic", p, "model_id", modelID)
			}
		}()
		bg, cancel := context.WithTimeout(context.Background(), capabilityProbeTimeout)
		defer cancel()
		s.runCapabilityProbe(bg, modelID, by)
	}()
	return m, true, nil
}

// runCapabilityProbe measures a model whose claim the caller holds, and
// stores the outcome. It always ends the claim.
func (s *Server) runCapabilityProbe(ctx context.Context, modelID, by string) {
	run := capability.ProbeRun{At: time.Now().UTC(), By: by, Status: capability.ProbeFailed}
	var facts capability.Facts
	defer func() {
		// Store on a fresh context: the probe's own may have run out.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if _, err := s.Store.UpdateModelCapabilities(ctx, modelID, func(state *capability.State) (bool, error) {
			state.Finish(facts, run)
			return true, nil
		}); err != nil {
			s.Logger.ErrorContext(ctx, "capability probe: store result", "error", err.Error(), "model_id", modelID)
		}
		s.InvalidateConfigCache()
		s.Logger.InfoContext(ctx, "capability probe finished", "model_id", modelID, "by", by, "status", run.Status, "detail", run.Detail,
			"requests", run.Requests, "facts", len(facts))
	}()
	m, err := s.Store.ModelByID(ctx, modelID)
	if err != nil {
		run.Detail = "the model could not be read"
		return
	}
	if reason := probeSkipReason(m); reason != "" {
		run.Status, run.Detail = capability.ProbeSkipped, reason
		return
	}
	upstream, err := s.Store.UpstreamByID(ctx, m.UpstreamID)
	if err != nil || !upstream.Enabled {
		run.Detail = "its upstream is turned off"
		return
	}
	provider, err := adapter.Get(upstream.AdapterType)
	if err != nil {
		run.Detail = "its upstream type is not known"
		return
	}
	apiKey, err := s.Cipher.Decrypt(upstream.EncryptedKey())
	if err != nil {
		run.Detail = "the upstream credential cannot be decrypted"
		return
	}
	up := adapter.Upstream{ID: upstream.ID, Name: upstream.Name, BaseURL: upstream.BaseURL, APIKey: apiKey, AdapterType: upstream.AdapterType}
	requestID := "capprobe-" + store.NewID()
	call := func(ctx context.Context, body map[string]any) (capability.Reply, error) {
		return s.capabilityProbeCall(ctx, m, up, provider, body, requestID)
	}
	facts, run = capability.RunProbe(ctx, call, capability.ProbeInput{Known: m.CapabilityState().WithoutProbe().Profile(), AdapterType: upstream.AdapterType, By: by})
}

// capabilityProbeCall sends one probe request through the model's adapter,
// exactly as written (nothing is fitted or retried: the refusal is the
// measurement), and meters it as platform overhead.
func (s *Server) capabilityProbeCall(ctx context.Context, m *store.Model, up adapter.Upstream, provider adapter.Adapter, body map[string]any, requestID string) (capability.Reply, error) {
	body["model"] = m.Name
	raw, err := json.Marshal(body)
	if err != nil {
		return capability.Reply{}, err
	}
	req := &adapter.Request{Method: http.MethodPost, Path: "/v1/chat/completions", Header: http.Header{"Content-Type": {"application/json"}},
		Body: raw, Model: m.Name, Context: ctx, Client: s.ownUpstreamClient(ctx, up.ID, up.BaseURL)}
	prepared, err := provider.Prepare(up, req)
	if err != nil {
		// The adapter itself refuses the request shape: that is an answer.
		return capability.Reply{Status: http.StatusBadRequest, Error: err.Error(), Local: true}, nil
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, prepared.URL, bytes.NewReader(prepared.Body))
	if err != nil {
		return capability.Reply{}, err
	}
	hreq.Header = prepared.Header
	event := &store.UsageEvent{ModelID: m.ID, ModelName: m.PublicName(), UpstreamID: up.ID, EndpointPath: "/internal/capability/probe",
		HTTPMethod: http.MethodPost, Modality: "chat", RequestBytes: int64(len(raw)), ClientApp: store.InternalProbeClientApp, RequestID: requestID}
	started := time.Now()
	defer func() {
		event.LatencyMs = int(time.Since(started).Milliseconds())
		event.UpstreamMs = event.LatencyMs
		s.writeUsage(ctx, event, "capability probe: write usage event", nil)
	}()
	resp, err := s.ownUpstreamClient(ctx, up.ID, up.BaseURL).Do(hreq)
	if err != nil {
		event.HTTPStatus, event.ErrorCode, event.AccountingMode = http.StatusBadGateway, CodeUpstreamDown, usage.AccountingNone
		return capability.Reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	event.HTTPStatus, event.ResponseBytes = resp.StatusCode, int64(len(answer))
	if err != nil {
		event.AccountingMode = usage.AccountingNone
		return capability.Reply{}, err
	}
	reply := capability.Reply{Status: resp.StatusCode}
	if resp.StatusCode/100 != 2 {
		event.AccountingMode = usage.AccountingNone
		reply.Error, reply.Param = upstreamErrorText(answer), upstreamErrorParam(answer)
		if reply.Error == "" {
			reply.Error = truncate(strings.Join(strings.Fields(string(answer)), " "), 200)
		}
		return reply, nil
	}
	u, _ := provider.ExtractUsage(answer)
	rates := usage.Rates{InNano: m.RateInNano, OutNano: m.RateOutNano, CachedNano: m.RateCachedNano}
	if card := s.cachedRateCard(ctx, m.ID); card.OK {
		rates = usage.Rates{InNano: card.In, OutNano: card.Out, CachedNano: card.Cached}
	}
	s.applyUsage(event, u, rates, -1)
	if s.Config != nil && s.Config.LocalOnly {
		event.CostNano = 0
	}
	reply.InputTokens, reply.OutputTokens = event.TokensIn, event.TokensOut
	out, err := provider.TransformResponse(req, answer)
	if err != nil {
		return capability.Reply{}, fmt.Errorf("read the model's answer: %w", err)
	}
	readProbeAnswer(out, &reply)
	if reply.ReasoningTokens == 0 {
		// Providers whose native answer the adapter rewrote may report
		// reasoning tokens only in the original.
		var native capability.Reply
		readProbeAnswer(answer, &native)
		reply.ReasoningTokens = native.ReasoningTokens
	}
	if up.AdapterType == "gemini" {
		if thoughts, counted := geminiProbeThoughts(answer); counted {
			reply.ReasoningCounted = true
			reply.ReasoningTokens = max(reply.ReasoningTokens, thoughts)
		}
	}
	return reply, nil
}

// geminiProbeThoughts reads how many tokens a Gemini answer spent thinking.
// Gemini returns no thinking text unless asked and, on its OpenAI-compatible
// route, no thinking count either: the thoughts are what total_tokens holds
// beyond prompt and completion. counted is false when the usage is missing,
// so a reply without it is not read as one without thinking.
func geminiProbeThoughts(raw []byte) (thoughts int64, counted bool) {
	var u struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
		UsageMetadata *struct {
			TotalTokenCount    int64 `json:"totalTokenCount"`
			ThoughtsTokenCount int64 `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return 0, false
	}
	if m := u.UsageMetadata; m != nil && m.TotalTokenCount > 0 {
		return m.ThoughtsTokenCount, true
	}
	if c := u.Usage; c != nil && c.TotalTokens > 0 && c.PromptTokens > 0 {
		return max(c.TotalTokens-c.PromptTokens-c.CompletionTokens, 0), true
	}
	return 0, false
}

// readProbeAnswer reads the parts of a chat completion a probe judges by.
func readProbeAnswer(raw []byte, reply *capability.Reply) {
	var full struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        any    `json:"reasoning"`
				ToolCalls        []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
			OutputDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
			ReasoningTokens    int64 `json:"reasoning_tokens"`
			ThoughtsTokenCount int64 `json:"thoughtsTokenCount"`
		} `json:"usage"`
		UsageMetadata struct {
			ThoughtsTokenCount int64 `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal(raw, &full) != nil {
		return
	}
	reply.Text = answerText(raw)
	for _, n := range []int64{full.Usage.CompletionDetails.ReasoningTokens, full.Usage.OutputDetails.ReasoningTokens, full.Usage.ReasoningTokens,
		full.Usage.ThoughtsTokenCount, full.UsageMetadata.ThoughtsTokenCount} {
		if n > reply.ReasoningTokens {
			reply.ReasoningTokens = n
		}
	}
	if len(full.Choices) == 0 {
		return
	}
	msg := full.Choices[0].Message
	reply.Reasoning = msg.ReasoningContent
	if text, ok := msg.Reasoning.(string); ok && reply.Reasoning == "" {
		reply.Reasoning = text
	}
	for _, call := range msg.ToolCalls {
		reply.ToolCalls = append(reply.ToolCalls, call.Function.Name)
	}
}

// upstreamErrorParam is the parameter a provider's error names, if any.
func upstreamErrorParam(raw []byte) string {
	var e struct {
		Error struct {
			Param string `json:"param"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	return e.Error.Param
}

// handleProbeModelCapabilities serves POST
// /api/v1/admin/models/{id}/capabilities/probe: measure this model now. It
// answers at once; the view's "probed" shows running until it is done.
func (s *Server) handleProbeModelCapabilities(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	m, err := s.Store.ModelByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That model"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if reason := probeSkipReason(m); reason != "" {
		WriteError(w, r, ErrInvalidRequest("This model cannot be measured: "+reason+"."))
		return
	}
	m, started, err := s.startCapabilityProbe(r.Context(), id, "manual")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if started {
		s.audit(r, "model_capabilities_probed", "model", id, nil, map[string]any{"model": m.Name})
	}
	WriteJSON(w, http.StatusAccepted, modelCapabilityView(m))
}

// handleProbeAllModelCapabilities serves POST
// /api/v1/admin/models/capabilities/probe: measure every enabled chat
// model, one after another. With {"only_unmeasured": true} models that
// already have a finished measurement are left alone.
func (s *Server) handleProbeAllModelCapabilities(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OnlyUnmeasured bool `json:"only_unmeasured"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	models, err := s.Store.ListModels(r.Context(), store.ModelFilter{Status: store.ModelEnabled})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var ids []string
	for _, m := range models {
		if probeSkipReason(m) != "" {
			continue
		}
		if body.OnlyUnmeasured && m.CapabilityProbe != nil && m.CapabilityProbe.Status == capability.ProbeOK {
			continue
		}
		// Claim now, so the answer and the list show what is queued.
		queued := false
		if _, err := s.Store.UpdateModelCapabilities(r.Context(), m.ID, func(state *capability.State) (bool, error) {
			queued = state.Queue("all", time.Now().UTC())
			return queued, nil
		}); err == nil && queued {
			ids = append(ids, m.ID)
		}
	}
	s.InvalidateConfigCache()
	if len(ids) > 0 {
		s.audit(r, "model_capabilities_probed", "model", "", nil, map[string]any{"models": len(ids), "only_unmeasured": body.OnlyUnmeasured})
		go func() {
			defer func() {
				if p := recover(); p != nil {
					s.Logger.Error("capability probe (all) panicked", "panic", p)
				}
			}()
			for _, id := range ids {
				bg, cancel := context.WithTimeout(context.Background(), capabilityProbeTimeout)
				if _, err := s.Store.UpdateModelCapabilities(bg, id, func(state *capability.State) (bool, error) {
					state.Begin("all", time.Now().UTC())
					return true, nil
				}); err == nil {
					s.runCapabilityProbe(bg, id, "all")
				}
				cancel()
			}
		}()
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{"started": len(ids)})
}

func (s *Server) handleGetCapabilityProbeSetting(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.CapabilityProbeSetting(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, v)
}

// handlePutCapabilityProbeSetting serves PUT
// /api/v1/admin/system/capability-probe: {"on_enable": bool}.
func (s *Server) handlePutCapabilityProbeSetting(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OnEnable *bool `json:"on_enable"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.OnEnable == nil {
		WriteError(w, r, ErrInvalidRequest("Say whether models are measured when enabled (on_enable).").WithParam("on_enable"))
		return
	}
	before, err := s.Store.CapabilityProbeSetting(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	after := store.CapabilityProbeSetting{OnEnable: *body.OnEnable}
	if err := s.Store.SetCapabilityProbeSetting(r.Context(), after); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "capability_probe_setting_changed", "system", store.SettingCapabilityProbe, before, after)
	WriteJSON(w, http.StatusOK, after)
}

// probeOnEnable measures a chat model that an administrator just enabled,
// when that is on. Failures only log: enabling the model already succeeded.
func (s *Server) probeOnEnable(ctx context.Context, m *store.Model) {
	if probeSkipReason(m) != "" {
		return
	}
	setting, err := s.Store.CapabilityProbeSetting(ctx)
	if err != nil || !setting.OnEnable {
		return
	}
	if _, _, err := s.startCapabilityProbe(ctx, m.ID, "enabled"); err != nil {
		s.Logger.WarnContext(ctx, "capability probe on enable: could not start", "error", err.Error(), "model_id", m.ID)
	}
}
