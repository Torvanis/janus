package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// Reasoning-effort fitting for personal subscriptions.
//
// Clients send one reasoning_effort to every model ("medium" is a common
// default), but plan models differ: some take no reasoning setting at all
// (GitHub Copilot gpt-4o, Mistral ministral, xAI grok-4.20 400 on the field
// itself), some take a short list (Mistral reasoning models: none | high),
// some refuse one value (xAI grok-4.5+ refuse "none"). Janus fits the
// request to the model instead of letting it fail:
//
//  1. What the vendor catalog says (OpenAI levels, Copilot
//     capabilities.supports.reasoning_effort, Mistral capabilities.reasoning).
//  2. What the vendor told us when it refused a value (learned, remembered
//     per connection and model), for vendors whose catalog is silent (xAI).
//  3. Otherwise the request goes out unchanged.

// Reasoning is what Janus knows about one model's reasoning setting.
type Reasoning struct {
	// Supported is nil when unknown; false means the field must be omitted.
	Supported *bool `json:"supported,omitempty"`
	// Levels is the complete set of accepted values, when known.
	Levels []string `json:"levels,omitempty"`
	// Rejected are values the vendor refused (learned from errors).
	Rejected []string `json:"rejected,omitempty"`
	// Default is the vendor's own default level, when published.
	Default string `json:"default,omitempty"`
	// Learned marks facts that came from a refusal rather than the catalog.
	Learned bool `json:"learned,omitempty"`
}

// ModelInfo is one catalog entry with its reasoning facts.
type ModelInfo struct {
	ID        string
	Reasoning *Reasoning
}

// ModelLister is implemented by providers whose catalog carries per-model
// capabilities. Models then returns the same IDs.
type ModelLister interface {
	ListModels(ctx context.Context, client *http.Client, accessToken string) ([]ModelInfo, error)
}

// FetchModels lists a provider's models and, where the catalog says, each
// model's reasoning facts.
func FetchModels(ctx context.Context, p Provider, client *http.Client, accessToken string) ([]string, map[string]Reasoning, error) {
	lister, ok := p.(ModelLister)
	if !ok {
		ids, err := p.Models(ctx, client, accessToken)
		return ids, nil, err
	}
	infos, err := lister.ListModels(ctx, client, accessToken)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, 0, len(infos))
	caps := map[string]Reasoning{}
	for _, m := range infos {
		ids = append(ids, m.ID)
		if m.Reasoning != nil {
			caps[m.ID] = *m.Reasoning
		}
	}
	return ids, caps, nil
}

func idsOf(infos []ModelInfo) []string {
	out := make([]string, 0, len(infos))
	for _, m := range infos {
		out = append(out, m.ID)
	}
	return out
}

// MergeReasoning combines a fresh catalog with what was learned before. The
// catalog is authoritative where it is specific; learned details (the exact
// accepted values, refused values) survive a catalog that only says "this
// model reasons" without listing values (Mistral), and learned facts are
// kept for models the catalog is silent about (xAI). Models no longer
// offered are dropped.
func MergeReasoning(models []string, catalog, previous map[string]Reasoning) map[string]Reasoning {
	out := map[string]Reasoning{}
	for _, m := range models {
		cat, inCatalog := catalog[m]
		prev, learned := previous[m]
		learned = learned && prev.Learned
		switch {
		case inCatalog && learned && len(cat.Levels) == 0 && sameSupport(cat.Supported, prev.Supported):
			out[m] = prev
		case inCatalog:
			out[m] = cat
		case learned:
			out[m] = prev
		}
	}
	return out
}

// sameSupport: the catalog does not contradict what was learned.
func sameSupport(catalog, learned *bool) bool {
	return catalog == nil || learned == nil || *catalog == *learned
}

func boolPtr(b bool) *bool { return &b }

// NoReasoning is a model that takes no reasoning setting.
func NoReasoning() *Reasoning { return &Reasoning{Supported: boolPtr(false)} }

// ReasoningLevels is a model that accepts exactly these values.
func ReasoningLevels(levels []string, def string) *Reasoning {
	norm := normalizeLevels(levels)
	if len(norm) == 0 {
		return NoReasoning()
	}
	return &Reasoning{Supported: boolPtr(true), Levels: norm, Default: strings.ToLower(strings.TrimSpace(def))}
}

func normalizeLevels(levels []string) []string {
	out := []string{}
	for _, l := range levels {
		l = strings.ToLower(strings.TrimSpace(l))
		if l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// effortLadder orders the reasoning values vendors use, least to most.
var effortLadder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// commonEfforts are the values nearly every reasoning model accepts; used
// when a value was refused but the full set is unknown.
var commonEfforts = []string{"low", "medium", "high"}

func rank(level string) int { return slices.Index(effortLadder, level) }

// nearest picks the allowed value closest to want on the ladder; a tie goes
// to the higher value (the caller asked for at least that much thought).
func nearest(want string, allowed []string) string {
	w := rank(want)
	best, bestDist := "", 1<<30
	for _, a := range allowed {
		r := rank(a)
		if r < 0 {
			continue
		}
		d := r - w
		if d < 0 {
			d = -d
		}
		if d < bestDist || (d == bestDist && r > rank(best)) {
			best, bestDist = a, d
		}
	}
	return best
}

// Fit decides what to send for a requested effort. It returns the value to
// send ("" = omit the field) and whether that differs from the request.
func (r *Reasoning) Fit(want string) (string, bool) {
	want = strings.ToLower(strings.TrimSpace(want))
	if r == nil || want == "" {
		return want, false
	}
	if r.Supported != nil && !*r.Supported {
		return "", true
	}
	allowed := r.Levels
	if len(allowed) == 0 {
		if !slices.Contains(r.Rejected, want) {
			return want, false
		}
		allowed = commonEfforts
	}
	allowed = slices.DeleteFunc(slices.Clone(allowed), func(l string) bool { return slices.Contains(r.Rejected, l) })
	if slices.Contains(allowed, want) {
		return want, false
	}
	if rank(want) < 0 {
		// A value Janus does not know: the model default is the safe choice.
		return r.Default, true
	}
	if got := nearest(want, allowed); got != "" {
		return got, true
	}
	return "", true
}

// Efforts is what /v1/models advertises: the accepted values, an empty list
// when the model takes none, nil when unknown.
func (r *Reasoning) Efforts() []string {
	if r == nil {
		return nil
	}
	if r.Supported != nil && !*r.Supported {
		return []string{}
	}
	if len(r.Levels) == 0 {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(r.Levels), func(l string) bool { return slices.Contains(r.Rejected, l) })
}

var (
	// "Must be one of (… 'none' …, 'high')" / "supported values: [...]" (Mistral).
	reListedValues = regexp.MustCompile(`(?i)(must be one of|supported values|allowed values|valid values|expected one of)`)
	reQuoted       = regexp.MustCompile("['\"`]([a-z]+)['\"`]")
	// "does not support `reasoning_effort` value `none`" (xAI).
	reValue = regexp.MustCompile("(?i)value\\s*['\"`]?([a-z]+)['\"`]?")
	// The field itself: "does not support parameter reasoningEffort" (xAI),
	// "is not enabled for this model" (Mistral), "does not support reasoning
	// effort" (Copilot), "Unsupported parameter: 'reasoning_effort'" (OpenAI).
	reMentionsReasoning = regexp.MustCompile(`(?i)reasoning[ _.]?effort|reasoning`)
	reRefusal           = regexp.MustCompile(`(?i)not supported|does not support|not enabled|unsupported|unknown parameter|unrecognized`)
)

// LearnFromRefusal reads a vendor's 400 body after it refused a reasoning
// value. It returns the updated facts, or ok=false when the error is about
// something else.
func LearnFromRefusal(prev *Reasoning, sent string, body []byte) (Reasoning, bool) {
	msg := errorMessage(body)
	if msg == "" || !reMentionsReasoning.MatchString(msg) || !reRefusal.MatchString(msg) {
		return Reasoning{}, false
	}
	next := Reasoning{Learned: true}
	if prev != nil {
		next = *prev
		next.Learned = true
	}
	if loc := reListedValues.FindStringIndex(msg); loc != nil {
		levels := []string{}
		for _, m := range reQuoted.FindAllStringSubmatch(msg[loc[1]:], -1) {
			if rank(m[1]) >= 0 && !slices.Contains(levels, m[1]) {
				levels = append(levels, m[1])
			}
		}
		if len(levels) > 0 {
			next.Supported, next.Levels = boolPtr(true), levels
			return next, true
		}
	}
	if m := reValue.FindStringSubmatch(msg); m != nil && strings.EqualFold(m[1], sent) && rank(strings.ToLower(m[1])) >= 0 {
		v := strings.ToLower(m[1])
		if !slices.Contains(next.Rejected, v) {
			next.Rejected = append(next.Rejected, v)
		}
		next.Supported = boolPtr(true)
		return next, true
	}
	next.Supported, next.Levels, next.Rejected = boolPtr(false), nil, nil
	return next, true
}

// errorMessage pulls the human text out of the error shapes vendors use.
func errorMessage(body []byte) string {
	var shapes struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
		Detail  json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &shapes) != nil {
		return strings.TrimSpace(string(body))
	}
	parts := []string{shapes.Message}
	for _, raw := range []json.RawMessage{shapes.Error, shapes.Detail} {
		if len(raw) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			parts = append(parts, s)
			continue
		}
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.Message != "" {
			parts = append(parts, obj.Message)
		} else {
			parts = append(parts, string(raw))
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// RequestedEffort reads the reasoning value from a chat or responses body:
// top-level reasoning_effort, or reasoning.effort.
func RequestedEffort(body map[string]json.RawMessage) string {
	var s string
	if raw, ok := body["reasoning_effort"]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	if raw, ok := body["reasoning"]; ok {
		var obj struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(raw, &obj) == nil {
			return obj.Effort
		}
	}
	return ""
}

// SetEffort writes value in whichever form the client used; "" removes it
// (the whole reasoning object, when effort was its only purpose).
func SetEffort(body map[string]json.RawMessage, value string) {
	if _, ok := body["reasoning_effort"]; ok {
		if value == "" {
			delete(body, "reasoning_effort")
		} else {
			body["reasoning_effort"], _ = json.Marshal(value)
		}
		return
	}
	raw, ok := body["reasoning"]
	if !ok {
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return
	}
	if value == "" {
		delete(obj, "effort")
		// Other reasoning keys (summary, enabled…) are meaningless to a
		// model with no reasoning setting and some vendors 400 on them.
		delete(body, "reasoning")
		return
	}
	obj["effort"], _ = json.Marshal(value)
	body["reasoning"], _ = json.Marshal(obj)
}
