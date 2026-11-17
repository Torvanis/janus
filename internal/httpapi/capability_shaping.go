package httpapi

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/capability"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

// Request shaping from a model's capability record.
//
// The record (what the provider said, what Janus measured, what an
// administrator set) is applied to a request before it is sent, so a
// setting the model would refuse never reaches it: a reasoning effort is
// fitted to the values the model takes, "no reasoning" becomes the switch
// that model uses, refused sampling settings are left out, max_tokens is
// renamed where only max_completion_tokens is taken, and tools go through
// the Responses API where that is the only place they work.
//
// What one caller's refused request teaches (provider_quirks.go) fills in
// only where the record is silent. Where the record speaks, a refusal fixes
// that one request and nothing shared: one caller cannot change how a model
// is called for everyone. Instead the model is measured again, by Janus,
// with its own requests.

// profileReasoning is what the capability record says about a model's
// reasoning values, or nil when it does not say.
func profileReasoning(m *store.Model) *subscription.Reasoning {
	if m == nil {
		return nil
	}
	p := m.CapabilityProfile()
	if thinks, known := p.Bool(adapter.CapThinking); known && !thinks {
		return subscription.NoReasoning()
	}
	if levels, known := p.List(adapter.CapThinkingLevels); known {
		return subscription.ReasoningLevels(levels, p.Text(adapter.CapThinkingDefaultLevel))
	}
	return nil
}

// modelReasoning is what is known about a catalog model's reasoning
// values: the capability record first, else what this replica learned.
func (s *Server) modelReasoning(m *store.Model) *subscription.Reasoning {
	if r := profileReasoning(m); r != nil {
		return r
	}
	return s.routedReasoning(m.ID)
}

// thinkingOffFields is the request fields that turn reasoning off for a
// model whose switch is not reasoning_effort "none".
func thinkingOffFields(off string) map[string]any {
	switch off {
	case adapter.ThinkingOffEnableThinking:
		return map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}
	case adapter.ThinkingOffThink:
		return map[string]any{"think": false}
	case adapter.ThinkingOffDisabled:
		return map[string]any{"thinking": map[string]any{"type": "disabled"}}
	case adapter.ThinkingOffReasoningFalse:
		return map[string]any{"reasoning": map[string]any{"enabled": false}}
	case adapter.ThinkingOffBudgetZero:
		// The Gemini adapter sends "none" as thinkingBudget 0; Gemini's
		// OpenAI-compatible route does the same itself.
		return map[string]any{"reasoning_effort": "none"}
	}
	return nil
}

// thinkingOffSwitch is how Janus turns a catalog model's thinking off: a
// ThinkingOff* switch it sends for "no thinking", ThinkingOffNever when
// the model always thinks, "" when that is not known. Chat offers "Off"
// from this answer, and the proxy applies it, so the two always agree.
//
//   - A switch measured by a probe or set by an administrator wins: it was
//     shown to work through this model's adapter (or someone decided).
//   - Else reasoning_effort "none", when the model's levels take it.
//   - Else what the provider's list says, when it says the model always
//     thinks, or names a switch this adapter is known to carry
//     (adapter.CarriesThinkingOff). Any other switch only the provider
//     names is not used: nothing has shown that the adapter carries it.
func thinkingOffSwitch(m *store.Model) string {
	if m == nil {
		return ""
	}
	r, said := m.Capabilities[adapter.CapThinkingOff]
	off, _ := r.Value.(string)
	if said && off != "" && r.Source != capability.SourceProvider {
		return off
	}
	p := m.Capabilities
	if thinks, known := p[adapter.CapThinking].Value.(bool); known && !thinks {
		return "" // nothing to turn off
	}
	if levels, known := p[adapter.CapThinkingLevels].Value.([]string); known && slices.Contains(levels, "none") {
		return adapter.ThinkingOffEffortNone
	}
	if said && (off == adapter.ThinkingOffNever || adapter.CarriesThinkingOff(m.AdapterType, off)) {
		return off
	}
	return ""
}

// leastEffort is the lowest thinking level a model takes other than
// "none", or "" when its levels are not known.
func leastEffort(facts *subscription.Reasoning) string {
	levels := slices.DeleteFunc(facts.Efforts(), func(l string) bool { return l == "none" })
	if len(levels) == 0 {
		return ""
	}
	least, _ := subscription.ReasoningLevels(levels, "").Fit("none")
	return least
}

// applyThinkingOff replaces a request for no reasoning with the model's own
// switch. It reports whether the body changed.
func applyThinkingOff(body map[string]json.RawMessage, fields map[string]any) bool {
	if len(fields) == 0 {
		return false
	}
	subscription.SetEffort(body, "")
	for k, v := range fields {
		if k == "chat_template_kwargs" {
			// Keep the caller's other template settings.
			merged := map[string]any{}
			_ = json.Unmarshal(body[k], &merged)
			for kk, vv := range v.(map[string]any) {
				merged[kk] = vv
			}
			v = merged
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return false
		}
		body[k] = raw
	}
	return true
}

// profileShape is what the capability record says about request shape.
type profileShape struct {
	refused       []string // sampling settings the model refuses
	refusedKnown  bool
	responses     bool // function tools only work on the Responses API
	endpointKnown bool
	capRename     bool // max_tokens must be sent as max_completion_tokens
	capKnown      bool
}

func shapeOf(m *store.Model) profileShape {
	var out profileShape
	if m == nil {
		return out
	}
	p := m.CapabilityProfile()
	out.refused, out.refusedKnown = p.List(adapter.CapRefusedParams)
	if endpoint := p.Text(adapter.CapToolsEndpoint); endpoint != "" {
		out.endpointKnown, out.responses = true, endpoint == adapter.ToolsEndpointResponses
	}
	if cap := p.Text(adapter.CapOutputCap); cap != "" {
		out.capKnown, out.capRename = true, cap == adapter.OutputCapMaxCompletionTokens
	}
	return out
}

// renameOutputCap sends max_tokens as max_completion_tokens. The value is
// kept: the caller chose it.
func renameOutputCap(body map[string]json.RawMessage) bool {
	raw, has := body["max_tokens"]
	if !has || string(raw) == "null" {
		return false
	}
	delete(body, "max_tokens")
	if _, already := body["max_completion_tokens"]; !already {
		body["max_completion_tokens"] = raw
	}
	return true
}

// refusalProbeEvery is how often a refusal may cause one model to be
// measured again, on this replica.
const refusalProbeEvery = 6 * time.Hour

var refusalProbes sync.Map // *Server -> *sync.Map (model ID -> time.Time)

// probeAfterRefusal measures a model again after a provider refused a
// setting the record did not warn about: the record is out of date, or
// was never filled in. It is rate limited per model and follows the same
// switch as measuring on enable, so callers cannot make Janus spend at will.
func (s *Server) probeAfterRefusal(ctx context.Context, m *store.Model) {
	if m == nil || probeSkipReason(m) != "" {
		return
	}
	v, _ := refusalProbes.LoadOrStore(s, &sync.Map{})
	last := v.(*sync.Map)
	now := time.Now()
	if at, ok := last.Load(m.ID); ok && now.Sub(at.(time.Time)) < refusalProbeEvery {
		return
	}
	last.Store(m.ID, now)
	// Another replica may have measured it since: the stored run decides.
	if run := m.CapabilityProbe; run != nil && run.Status == capability.ProbeOK && now.Sub(run.At) < refusalProbeEvery {
		return
	}
	modelID := m.ID
	go func() {
		defer func() {
			if p := recover(); p != nil {
				s.Logger.Error("capability probe after refusal panicked", "panic", p, "model_id", modelID)
			}
		}()
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		setting, err := s.Store.CapabilityProbeSetting(bg)
		if err != nil || !setting.OnEnable {
			return
		}
		if _, _, err := s.startCapabilityProbe(bg, modelID, "refusal"); err != nil {
			s.Logger.WarnContext(bg, "capability probe after refusal: could not start", "error", err.Error(), "model_id", modelID)
		}
	}()
}

// mergeDropped returns the sampling settings to leave out: what the record
// says, and what this replica learned where the record is silent.
func mergeDropped(shape profileShape, learned []string) []string {
	if shape.refusedKnown {
		return shape.refused
	}
	return slices.Clone(learned)
}
