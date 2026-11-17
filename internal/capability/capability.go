// Package capability holds what Janus knows about what one model can take,
// give and be asked, and where each piece of that knowledge came from.
//
// Three sources, strongest last:
//
//  1. provider: what the provider's own model list says (free, read at
//     every discovery).
//  2. probe: what Janus measured by sending the model a few tiny requests.
//     A measurement beats a description.
//  3. admin: what an administrator set by hand. It beats both.
//
// The keys and value types are defined beside the provider readers, in
// internal/adapter/capabilities.go. A key no source speaks to is unknown,
// which is not the same as no.
package capability

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/adapter"
)

// Sources of a fact.
const (
	SourceProvider = "provider"
	SourceProbe    = "probe"
	SourceAdmin    = "admin"
)

// Fact is one value from one source.
type Fact struct {
	Value any       `json:"value"`
	At    time.Time `json:"at"`
	// Note says how a probe came to the value ("400: Unsupported
	// parameter: 'max_tokens'"), so a person can check the reasoning.
	Note string `json:"note,omitempty"`
}

// Facts maps capability keys to facts from one source.
type Facts map[string]Fact

// State is everything known about one model, by source. It is what is
// stored on the model row.
type State struct {
	Provider Facts `json:"provider,omitempty"`
	Probe    Facts `json:"probe,omitempty"`
	Admin    Facts `json:"admin,omitempty"`
	// Probed describes the last probe run.
	Probed *ProbeRun `json:"probed,omitempty"`
}

// ProbeRun is the outcome of one probe of a model.
type ProbeRun struct {
	At time.Time `json:"at"`
	// Status is ProbeOK, ProbeFailed (nothing could be learned: the model
	// did not answer) or ProbeSkipped (not a model that is probed).
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Requests and tokens the probe spent.
	Requests     int   `json:"requests"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// By is who asked for it: "enabled", "manual" or "refusal".
	By string `json:"by,omitempty"`
}

// Probe statuses.
const (
	ProbeOK      = "ok"
	ProbeFailed  = "failed"
	ProbeSkipped = "skipped"
)

// Resolved is the value in force for one key, and the source it came from.
type Resolved struct {
	Value  any       `json:"value"`
	Source string    `json:"source"`
	At     time.Time `json:"at"`
	Note   string    `json:"note,omitempty"`
}

var keys = func() map[string]adapter.CapabilityKey {
	out := map[string]adapter.CapabilityKey{}
	for _, k := range adapter.CapabilityKeys() {
		out[k.Key] = k
	}
	return out
}()

// Normalize checks a value against its key and returns it in the one Go
// type that kind is held in: bool, string, []string or int64. Values read
// back from JSON arrive as float64 and []any, so every value passes through
// here on the way in and on the way out.
func Normalize(key string, v any) (any, error) {
	def, ok := keys[key]
	if !ok {
		return nil, fmt.Errorf("unknown capability %q", key)
	}
	switch def.Kind {
	case adapter.CapKindBool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
	case adapter.CapKindString:
		if s, ok := v.(string); ok {
			s = strings.ToLower(strings.TrimSpace(s))
			if len(def.Enum) == 0 && s != "" && len(s) <= 64 {
				return s, nil
			}
			for _, e := range def.Enum {
				if s == e {
					return s, nil
				}
			}
			if len(def.Enum) > 0 {
				return nil, fmt.Errorf("%s must be one of %s", key, strings.Join(def.Enum, ", "))
			}
		}
	case adapter.CapKindInt:
		switch n := v.(type) {
		case int64:
			if n >= 0 {
				return n, nil
			}
		case int:
			if n >= 0 {
				return int64(n), nil
			}
		case float64:
			if n >= 0 && n == float64(int64(n)) {
				return int64(n), nil
			}
		case json.Number:
			if i, err := n.Int64(); err == nil && i >= 0 {
				return i, nil
			}
		}
	case adapter.CapKindList:
		var in []string
		switch l := v.(type) {
		case []string:
			in = l
		case []any:
			for _, e := range l {
				s, ok := e.(string)
				if !ok {
					return nil, fmt.Errorf("%s must be a list of strings", key)
				}
				in = append(in, s)
			}
		default:
			return nil, fmt.Errorf("%s must be a list of strings", key)
		}
		out := []string{}
		seen := map[string]bool{}
		for _, s := range in {
			s = strings.ToLower(strings.TrimSpace(s))
			if s == "" || len(s) > 64 || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
		if len(out) > 32 {
			return nil, fmt.Errorf("%s has too many values", key)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s must be a %s", key, def.Kind)
}

// FromProvider turns what a provider's model list said into facts, dropping
// anything that is not a known key or the right type: a provider's list is
// outside input and is never trusted to be well formed.
func FromProvider(c adapter.Capabilities, at time.Time) Facts {
	out := Facts{}
	for k, v := range c {
		if n, err := Normalize(k, v); err == nil {
			out[k] = Fact{Value: n, At: at}
		}
	}
	return out
}

// Parse reads a stored state. Empty input is an empty state.
func Parse(raw string) (State, error) {
	var s State
	if strings.TrimSpace(raw) == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return State{}, fmt.Errorf("read model capabilities: %w", err)
	}
	for _, facts := range []Facts{s.Provider, s.Probe, s.Admin} {
		for k, f := range facts {
			n, err := Normalize(k, f.Value)
			if err != nil {
				// A key retired by a later version: forget it.
				delete(facts, k)
				continue
			}
			f.Value = n
			facts[k] = f
		}
	}
	return s, nil
}

// String is the stored form.
func (s State) String() string {
	b, err := json.Marshal(s)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Record is the capability state of several models, keyed by model name:
// what one personal-plan connection knows about each of its models (the
// provider layer is what the vendor's model list said, the probe layer
// what the owner's Measure found, the admin layer what the owner set).
type Record map[string]State

// ParseRecord reads a stored record. Empty input is an empty record; a
// model whose state cannot be read is dropped rather than failing the rest.
func ParseRecord(raw string) Record {
	out := Record{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	var states map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &states) != nil {
		return out
	}
	for model, one := range states {
		if s, err := Parse(string(one)); err == nil {
			out[model] = s
		}
	}
	return out
}

// String is the stored form.
func (r Record) String() string {
	if len(r) == 0 {
		return "{}"
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Resolve returns the value in force for every known key: admin over probe
// over provider.
func (s State) Resolve() map[string]Resolved {
	out := map[string]Resolved{}
	for _, layer := range []struct {
		source string
		facts  Facts
	}{{SourceProvider, s.Provider}, {SourceProbe, s.Probe}, {SourceAdmin, s.Admin}} {
		for k, f := range layer.facts {
			out[k] = Resolved{Value: f.Value, Source: layer.source, At: f.At, Note: f.Note}
		}
	}
	return out
}

// SourceDefault marks a value no source said, taken from a built-in table
// of what a vendor's plans are documented to accept (personal-plan models
// only; see internal/subscription/plan_capabilities.go). It is the weakest
// source: anything the vendor's list, a measurement or the person says
// replaces it.
const SourceDefault = "default"

// ResolveWith is Resolve over a layer of built-in defaults: a key no source
// in the state speaks to takes its default, marked SourceDefault.
func (s State) ResolveWith(defaults Facts) map[string]Resolved {
	out := s.Resolve()
	for k, f := range defaults {
		if _, said := out[k]; said {
			continue
		}
		if n, err := Normalize(k, f.Value); err == nil {
			out[k] = Resolved{Value: n, Source: SourceDefault, At: f.At, Note: f.Note}
		}
	}
	return out
}

// ProfileWith is Profile over a layer of built-in defaults.
func (s State) ProfileWith(defaults Facts) Profile {
	return Profile{resolved: s.ResolveWith(defaults)}
}

// SetProvider replaces what the provider said, keeping the date of a fact
// whose value did not change (so "since" means since it was first said).
// It reports whether anything changed.
func (s *State) SetProvider(next Facts) bool {
	changed := len(next) != len(s.Provider)
	for k, f := range next {
		if prev, ok := s.Provider[k]; ok && equal(prev.Value, f.Value) {
			f.At = prev.At
			next[k] = f
		} else {
			changed = true
		}
	}
	if len(next) == 0 {
		next = nil
	}
	s.Provider = next
	return changed
}

// SetAdmin applies an administrator's edit: a value sets the override, nil
// removes it.
func (s *State) SetAdmin(edits map[string]any, at time.Time) error {
	names := make([]string, 0, len(edits))
	for k := range edits {
		names = append(names, k)
	}
	sort.Strings(names)
	next := Facts{}
	for k, f := range s.Admin {
		next[k] = f
	}
	for _, k := range names {
		v := edits[k]
		if v == nil {
			if _, ok := keys[k]; !ok {
				return fmt.Errorf("unknown capability %q", k)
			}
			delete(next, k)
			continue
		}
		n, err := Normalize(k, v)
		if err != nil {
			return err
		}
		next[k] = Fact{Value: n, At: at}
	}
	if len(next) == 0 {
		next = nil
	}
	s.Admin = next
	return nil
}

func equal(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Profile is the resolved state as typed fields, for code that shapes
// requests. A nil pointer or nil slice means unknown.
type Profile struct {
	resolved map[string]Resolved
}

// Profile resolves the state into a Profile.
func (s State) Profile() Profile { return Profile{resolved: s.Resolve()} }

// Bool returns a yes/no capability; known is false when no source said.
func (p Profile) Bool(key string) (value, known bool) {
	r, ok := p.resolved[key]
	if !ok {
		return false, false
	}
	b, ok := r.Value.(bool)
	return b, ok
}

// Text returns a string capability, "" when unknown.
func (p Profile) Text(key string) string {
	s, _ := p.resolved[key].Value.(string)
	return s
}

// List returns a list capability; known is false when no source said (an
// empty list that is known means "none").
func (p Profile) List(key string) (values []string, known bool) {
	r, ok := p.resolved[key]
	if !ok {
		return nil, false
	}
	l, ok := r.Value.([]string)
	return l, ok
}

// Int returns a number capability, 0 when unknown.
func (p Profile) Int(key string) int64 {
	n, _ := p.resolved[key].Value.(int64)
	return n
}

// Known reports whether anything at all is known.
func (p Profile) Known() bool { return len(p.resolved) > 0 }
