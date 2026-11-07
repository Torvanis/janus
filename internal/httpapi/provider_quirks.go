package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// The change is recorded on the usage event (reasoning_adjustment) and in
// the X-Janus-Reasoning-Adjusted header.

const maxQuirkRetries = 3

// modelQuirks is what Janus learned about one catalog model's limits.
type modelQuirks struct {
	dropParams        []string // sampling hints the model refuses
	toolsViaResponses bool     // chat completions refuse tools: use Responses
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
}

func (f catalogFit) label() string {
	var parts []string
	if l := f.reasoning.label(); l != "" {
		parts = append(parts, l)
	}
	for _, p := range f.dropped {
		parts = append(parts, p+"->omitted")
	}
	if f.responses {
		parts = append(parts, "tools->responses")
	}
	return strings.Join(parts, " ")
}

// fitCatalogBody applies what is known about a catalog model to a chat
// body before it is sent. It returns the body, whether the request must
// go through the Responses API, and what changed.
func (s *Server) fitCatalogBody(modelID string, body []byte, fit catalogFit) ([]byte, bool, catalogFit) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, false, fit
	}
	q := s.modelQuirks(modelID)
	changed := false
	if facts := s.routedReasoning(modelID); facts != nil {
		requested := strings.ToLower(strings.TrimSpace(subscription.RequestedEffort(m)))
		if fit.reasoning.requested == "" {
			fit.reasoning.requested = requested
		}
		if requested != "" {
			if sent, ch := facts.Fit(requested); ch {
				subscription.SetEffort(m, sent)
				fit.reasoning.sent = sent
				changed = true
			} else {
				fit.reasoning.sent = requested
			}
		}
	} else if fit.reasoning.requested == "" {
		r := strings.ToLower(strings.TrimSpace(subscription.RequestedEffort(m)))
		fit.reasoning = reasoningFit{requested: r, sent: r}
	}
	for _, p := range q.dropParams {
		if _, ok := m[p]; ok {
			delete(m, p)
			changed = true
			if !slices.Contains(fit.dropped, p) {
				fit.dropped = append(fit.dropped, p)
			}
		}
	}
	viaResponses := q.toolsViaResponses && hasTools(m)
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
// a fitted reasoning value, a dropped sampling hint, or the Responses API.
// ok is false when the error is about something Janus cannot fix.
func (s *Server) learnCatalogRefusal(modelID, adapterType string, body, raw []byte, viaResponses bool, fit *catalogFit) (quirkRetry, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return quirkRetry{}, false
	}
	q := s.modelQuirks(modelID)
	if !viaResponses && adapterType == "openai_compatible" && hasTools(m) && subscription.ToolsNeedResponses(raw) {
		q.toolsViaResponses = true
		s.rememberQuirks(modelID, q)
		fit.responses = true
		return quirkRetry{body: body, viaResponses: true}, true
	}
	sent := strings.ToLower(strings.TrimSpace(subscription.RequestedEffort(m)))
	if sent != "" {
		if learned, ok := subscription.LearnFromRefusal(s.routedReasoning(modelID), sent, raw); ok {
			s.rememberRoutedReasoning(modelID, learned)
			if fitted, changed := learned.Fit(sent); changed && fitted != sent {
				subscription.SetEffort(m, fitted)
				if fit.reasoning.requested == "" {
					fit.reasoning.requested = sent
				}
				fit.reasoning.sent = fitted
				out, err := json.Marshal(m)
				if err == nil {
					return quirkRetry{body: out, viaResponses: viaResponses}, true
				}
			}
			return quirkRetry{}, false
		}
	}
	if p := subscription.RefusedSamplingParam(raw, presentSampling(m)); p != "" {
		if !slices.Contains(q.dropParams, p) {
			q.dropParams = append(q.dropParams, p)
		}
		s.rememberQuirks(modelID, q)
		delete(m, p)
		fit.dropped = append(fit.dropped, p)
		out, err := json.Marshal(m)
		if err == nil {
			return quirkRetry{body: out, viaResponses: viaResponses}, true
		}
	}
	return quirkRetry{}, false
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
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			// Only follow the provider's own host with the credential.
			if req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
				return nil
			}
			fix(req.Header)
			return nil
		}
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
