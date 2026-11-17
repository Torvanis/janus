package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/torvanis/janus/internal/safehttp"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

// Provider quirks for catalog upstreams (organization-paid providers).
//
// Open WebUI and other OpenAI-shaped clients send one set of settings to
// every model: a reasoning effort typed by the user ("minimal", "xhigh"), a
// temperature from a preset, tools. Providers each refuse a different part
// of that: GPT-6 sol/astra refuse "none" and "minimal", Gemini refuses
// "minimal" and "xhigh", grok refuses "none", Claude 5 refuses any
// temperature or top_p, GPT-5/6 refuse a non-default temperature and any
// function tools with an effort on chat completions. Each was a 400 the
// caller saw.
//
// Janus now fits the request instead. On a 400 that names what was refused,
// before anything is relayed (a 400 is not billed), it learns the model's
// limit, fixes the body and resends; up to three times, one fix each:
//
//   - a refused reasoning value: the nearest value the model takes
//     (subscription.LearnFromRefusal, as for personal plans and routes);
//   - a refused sampling hint (temperature, top_p, …): dropped;
//   - function tools refused on chat completions: the request goes through
//     the Responses API instead (adapter.OpenAIResponses).
//
// What is learned is kept per model on this replica for a day and applied
// to the next request up front, so the refusal costs one round trip once.
// It only fills in where the model's capability record is silent: the
// record comes first and a refusal never overrides it for other callers
// (capability_shaping.go).
// The change is recorded on the usage event (reasoning_adjustment) and in
// the X-Janus-Reasoning-Adjusted header.

const maxQuirkRetries = 3

// modelQuirks is what Janus learned about one catalog model's limits.
type modelQuirks struct {
	dropParams        []string // sampling hints the model refuses
	toolsViaResponses bool     // chat completions refuse tools: use Responses
	capRename         bool     // max_tokens is refused: send max_completion_tokens
	learned           time.Time
}

var catalogQuirks sync.Map // *Server -> *sync.Map (model ID -> modelQuirks)

func (s *Server) quirkMap() *sync.Map {
	v, _ := catalogQuirks.LoadOrStore(s, &sync.Map{})
	return v.(*sync.Map)
}

func (s *Server) modelQuirks(modelID string) modelQuirks {
	v, ok := s.quirkMap().Load(modelID)
	if !ok {
		return modelQuirks{}
	}
	q := v.(modelQuirks)
	if time.Since(q.learned) > learnedReasoningTTL {
		s.quirkMap().Delete(modelID)
		return modelQuirks{}
	}
	return q
}

func (s *Server) rememberQuirks(modelID string, q modelQuirks) {
	q.learned = time.Now()
	s.quirkMap().Store(modelID, q)
}

// presentSampling lists the sampling hints a chat body carries.
func presentSampling(m map[string]json.RawMessage) []string {
	var out []string
	for _, p := range subscription.SamplingParams() {
		if raw, ok := m[p]; ok && string(raw) != "null" {
			out = append(out, p)
		}
	}
	return out
}

func hasTools(m map[string]json.RawMessage) bool {
	var tools []json.RawMessage
	return json.Unmarshal(m["tools"], &tools) == nil && len(tools) > 0
}

// catalogFit is what fitting did to one request, for the record.
type catalogFit struct {
	reasoning reasoningFit
	dropped   []string
	responses bool
	// thinkingOff names the switch used in place of reasoning effort "none".
	thinkingOff string
	// capRenamed: max_tokens was sent as max_completion_tokens.
	capRenamed bool
}

func (f catalogFit) label() string {
	var parts []string
	if l := f.reasoning.label(); l != "" {
		parts = append(parts, l)
	}
	for _, p := range f.dropped {
		parts = append(parts, p+"->omitted")
	}
	if f.thinkingOff != "" {
		parts = append(parts, "none->"+f.thinkingOff)
	}
	if f.capRenamed {
		parts = append(parts, "max_tokens->max_completion_tokens")
	}
	if f.responses {
		parts = append(parts, "tools->responses")
	}
	return strings.Join(parts, " ")
}

// fitCatalogBody applies what is known about a catalog model to a chat
// body before it is sent: the model's capability record, and what this
// replica learned from refusals where the record is silent. It returns the
// body, whether the request must go through the Responses API, and what
// changed.
func (s *Server) fitCatalogBody(model *store.Model, body []byte, fit catalogFit) ([]byte, bool, catalogFit) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, false, fit
	}
	q := s.modelQuirks(model.ID)
	shape := shapeOf(model)
	changed := false
	requested := strings.ToLower(strings.TrimSpace(subscription.RequestedEffort(m)))
	if fit.reasoning.requested == "" {
		fit.reasoning = reasoningFit{requested: requested, sent: requested}
	}
	offName := thinkingOffSwitch(model)
	offFields := thinkingOffFields(offName)
	facts := s.modelReasoning(model)
	if requested == "none" && offFields != nil {
		// No reasoning was asked for, and this model's switch for that is
		// not the effort value "none". It wins over a level list that
		// holds "none": such a list says what the server accepts, while
		// the switch was shown to turn reasoning off.
		if applyThinkingOff(m, offFields) {
			fit.reasoning.sent, fit.thinkingOff = requested, offName
			changed = true
		}
	} else if least := leastEffort(facts); requested == "none" && offName == adapter.ThinkingOffNever && least != "" {
		// The model always thinks: send the least it does.
		subscription.SetEffort(m, least)
		fit.reasoning.sent = least
		changed = true
	} else if facts != nil && requested != "" {
		if sent, ch := facts.Fit(requested); ch {
			subscription.SetEffort(m, sent)
			fit.reasoning.sent = sent
			changed = true
		}
	}
	for _, p := range mergeDropped(shape, q.dropParams) {
		if raw, ok := m[p]; ok && string(raw) != "null" {
			delete(m, p)
			changed = true
			if !slices.Contains(fit.dropped, p) {
				fit.dropped = append(fit.dropped, p)
			}
		}
	}
	if shape.capRename || (!shape.capKnown && q.capRename) {
		if renameOutputCap(m) {
			fit.capRenamed, changed = true, true
		}
	}
	viaResponses := hasTools(m) && (shape.responses || (!shape.endpointKnown && q.toolsViaResponses))
	if viaResponses {
		fit.responses = true
	}
	if !changed {
		return body, viaResponses, fit
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body, viaResponses, fit
	}
	return out, viaResponses, fit
}

// quirkRetry is one resend after a refusal.
type quirkRetry struct {
	body         []byte
	viaResponses bool
}

// learnCatalogRefusal reads a 400 from a catalog model and decides the fix:
// a fitted reasoning value, a dropped sampling hint, a renamed output limit,
// or the Responses API. ok is false when the error is about something Janus
// cannot fix.
//
// The fix always applies to this request. It is remembered for the next
// one only where the model's capability record says nothing: where the
// record speaks, a caller's refused request must not change how the model
// is called for everyone, so the model is measured again instead.
func (s *Server) learnCatalogRefusal(ctx context.Context, model *store.Model, adapterType string, body, raw []byte, viaResponses bool, fit *catalogFit) (quirkRetry, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return quirkRetry{}, false
	}
	q := s.modelQuirks(model.ID)
	shape := shapeOf(model)
	fixed := func(retry quirkRetry) (quirkRetry, bool) {
		s.probeAfterRefusal(ctx, model)
		return retry, true
	}
	if !viaResponses && adapterType == "openai_compatible" && hasTools(m) && subscription.ToolsNeedResponses(raw) {
		if !shape.endpointKnown {
			q.toolsViaResponses = true
			s.rememberQuirks(model.ID, q)
		}
		fit.responses = true
		return fixed(quirkRetry{body: body, viaResponses: true})
	}
	sent := strings.ToLower(strings.TrimSpace(subscription.RequestedEffort(m)))
	if sent != "" {
		recorded := profileReasoning(model)
		prev := recorded
		if prev == nil {
			prev = s.routedReasoning(model.ID)
		}
		if learned, ok := subscription.LearnFromRefusal(prev, sent, raw); ok {
			if recorded == nil {
				s.rememberRoutedReasoning(model.ID, learned)
			}
			if fitted, changed := learned.Fit(sent); changed && fitted != sent {
				subscription.SetEffort(m, fitted)
				if fit.reasoning.requested == "" {
					fit.reasoning.requested = sent
				}
				fit.reasoning.sent = fitted
				out, err := json.Marshal(m)
				if err == nil {
					return fixed(quirkRetry{body: out, viaResponses: viaResponses})
				}
			}
			return quirkRetry{}, false
		}
	}
	if p := subscription.RefusedSamplingParam(raw, presentSampling(m)); p != "" {
		if !shape.refusedKnown {
			if !slices.Contains(q.dropParams, p) {
				q.dropParams = append(q.dropParams, p)
			}
			s.rememberQuirks(model.ID, q)
		}
		delete(m, p)
		fit.dropped = append(fit.dropped, p)
		out, err := json.Marshal(m)
		if err == nil {
			return fixed(quirkRetry{body: out, viaResponses: viaResponses})
		}
	}
	if refusesMaxTokens(raw) && renameOutputCap(m) {
		if !shape.capKnown {
			q.capRename = true
			s.rememberQuirks(model.ID, q)
		}
		fit.capRenamed = true
		out, err := json.Marshal(m)
		if err == nil {
			return fixed(quirkRetry{body: out, viaResponses: viaResponses})
		}
	}
	return quirkRetry{}, false
}

// refusesMaxTokens reports whether a provider's 400 refuses max_tokens and
// points at max_completion_tokens (OpenAI reasoning models).
func refusesMaxTokens(raw []byte) bool {
	var e struct {
		Error struct {
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return false
	}
	return strings.Contains(e.Error.Message, "max_completion_tokens") && (e.Error.Param == "max_tokens" || strings.Contains(e.Error.Message, "'max_tokens'"))
}

// resendFitted prepares and sends one fitted retry. It returns the new
// response, the adapter that must translate it and the request it
// answers, or ok=false when the retry could not be sent (the original
// refusal is then relayed).
func (s *Server) resendFitted(ctx context.Context, r *http.Request, base adapter.Adapter, up adapter.Upstream, req *adapter.Request, retry quirkRetry, event *store.UsageEvent) (*http.Response, adapter.Adapter, *adapter.Request, bool) {
	a := base
	if retry.viaResponses {
		a = adapter.ResponsesFallback()
	}
	next := *req
	next.Body = retry.body
	prepared, err := a.Prepare(up, &next)
	if err != nil {
		return nil, nil, nil, false
	}
	if r.URL.RawQuery != "" {
		prepared.URL += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(ctx, r.Method, prepared.URL, bytes.NewReader(prepared.Body))
	if err != nil {
		return nil, nil, nil, false
	}
	out.Header = prepared.Header
	out.ContentLength = int64(len(prepared.Body))
	// The same client, timeouts and redirect policy as the first send: a
	// fitted resend of a streamed reply is bounded by silence, not length.
	resp, err := s.sendUpstream(ctx, out, prepared, event, next.Body)
	if err != nil {
		return nil, nil, nil, false
	}
	return resp, a, &next, true
}

// sendUpstream sends one proxied request (the first attempt or a fitted
// resend) with the client its kind of request needs: the long-wait client
// for media and for non-streamed provider-run tools, the adapter's redirect
// policy, and the streaming idle timer instead of the total timeout.
func (s *Server) sendUpstream(ctx context.Context, out *http.Request, prepared *adapter.Prepared, event *store.UsageEvent, body []byte) (*http.Response, error) {
	clientModality := event.Modality
	if !event.Streaming && usesHostedTools(body) {
		// Provider-run tools (web/X search, code interpreter) work
		// before the first byte of a non-streamed reply: 30 s of
		// searching is normal, so wait like a media request does.
		clientModality = adapter.ModalityImage
	}
	client := s.upstreamHTTPClientFor(ctx, clientModality)
	if prepared != nil && prepared.OnRedirect != nil {
		// Same pooled transport; only the redirect policy differs.
		fix := prepared.OnRedirect
		c := *client
		c.CheckRedirect = safehttp.CheckRedirect(fix)
		client = &c
	}
	return doUpstream(client, out, event.Streaming, s.effectiveUpstreamTimeouts(ctx).Total)
}

// readRefusal reads a 400 body (bounded) and puts it back for relaying.
func readRefusal(resp *http.Response) []byte {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusalBody))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return raw
}
