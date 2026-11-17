package capability

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
)

// fakeGemini answers probe requests the way Google's Gemini routes do, as
// seen through the adapter: thinking is hidden and only counted, the
// OpenAI-compatible route lists the same effort values for every model and
// a model refuses a level it lacks.
type fakeGemini struct {
	// defaultThinks: it thinks when the request says nothing.
	defaultThinks bool
	// offWorks: thinkingBudget 0 ("none") stops it thinking; otherwise it
	// still thinks a little (3.x Flash).
	offWorks bool
	// refuseBudget: a budget of 0 is refused (2.5 Pro).
	refuseBudget bool
	// refusedLevels are levels this model refuses.
	refusedLevels []string
	// uncounted: replies carry no usage, so thinking cannot be counted.
	uncounted bool

	bodies []map[string]any
}

func (f *fakeGemini) call(_ context.Context, body map[string]any) (Reply, error) {
	f.bodies = append(f.bodies, body)
	effort, hasEffort := body["reasoning_effort"].(string)
	thoughts := int64(0)
	if f.defaultThinks {
		thoughts = 36
	}
	if hasEffort {
		switch {
		case effort == "janus-probe":
			return refusal("", `[{"error":{"code":400,"message":"Invalid reasoning_effort: janus-probe. Valid values are: high, low, medium, minimal, none, xhigh","status":"INVALID_ARGUMENT"}}]`), nil
		case contains(f.refusedLevels, effort):
			return refusal("", "Thinking level "+strings.ToUpper(effort)+" is not supported for this model. Please retry with other thinking level."), nil
		case effort == "none" && f.refuseBudget:
			return refusal("", "Budget 0 is invalid. This model only works in thinking mode."), nil
		case effort == "none" && f.offWorks:
			thoughts = 0
		case effort == "none":
			thoughts = 68
		default:
			thoughts = 191
		}
	}
	r := Reply{Status: 200, Text: "43", ReasoningTokens: thoughts, ReasoningCounted: !f.uncounted, InputTokens: 17, OutputTokens: 2 + thoughts}
	if f.uncounted {
		r.ReasoningTokens = 0
	}
	if tools, ok := body["tools"].([]any); ok {
		if fn, _ := tools[0].(map[string]any)["type"].(string); fn == "function" {
			r.Text, r.ToolCalls = "", []string{probeToolName}
		}
	}
	if parts, ok := body["messages"].([]any)[0].(map[string]any)["content"].([]any); ok && len(parts) == 2 {
		r.Text = map[any]string{"image_url": "Green", "input_audio": "47", "video_url": "Blue"}[parts[1].(map[string]any)["type"]]
	}
	if _, has := body["response_format"]; has {
		r.Text = `{"ok":true}`
	}
	return r, nil
}

func probeGemini(t *testing.T, f *fakeGemini, known adapter.Capabilities) (Facts, ProbeRun) {
	t.Helper()
	var s State
	s.SetProvider(FromProvider(known, probeNow))
	return RunProbe(context.Background(), f.call, ProbeInput{Known: s.Profile(), AdapterType: "gemini", By: "manual", Now: func() time.Time { return probeNow }})
}

// offRequests are the requests that asked for no thinking on the harder question.
func (f *fakeGemini) offRequests() []map[string]any {
	var out []map[string]any
	for _, b := range f.bodies {
		if b["reasoning_effort"] == "none" && b["messages"].([]any)[0].(map[string]any)["content"] == geminiThinkQuestion {
			out = append(out, b)
		}
	}
	return out
}

// What Google's model list says about every Gemini chat model.
var geminiListed = adapter.Capabilities{adapter.CapThinking: true}

// Gemini 2.5 Flash: thinks by default, a budget of 0 turns it off on both routes.
func TestProbeGeminiThinkingTurnsOff(t *testing.T) {
	f := &fakeGemini{defaultThinks: true, offWorks: true}
	facts, run := probeGemini(t, f, geminiListed)
	got := values(facts)
	if got[adapter.CapThinkingOff] != adapter.ThinkingOffBudgetZero || got[adapter.CapThinkingDefault] != true {
		t.Fatalf("facts: %#v", got)
	}
	if l := got[adapter.CapThinkingLevels]; !reflect.DeepEqual(l, []string{"none", "minimal", "low", "medium", "high", "xhigh"}) {
		t.Errorf("levels: %#v", l)
	}
	if note := facts[adapter.CapThinkingOff].Note; !strings.Contains(note, "OpenAI-compatible route and the native route") || !strings.Contains(note, "by default") {
		t.Errorf("note: %q", note)
	}
	// The off form went once through each route: plain, and with a Google-run tool (the native API, as Chat sends).
	off := f.offRequests()
	if len(off) != 2 {
		t.Fatalf("off requests: %d", len(off))
	}
	if _, has := off[0]["tools"]; has {
		t.Errorf("the first off request must take the plain route: %v", off[0])
	}
	if tools, _ := off[1]["tools"].([]any); len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_fetch" {
		t.Errorf("the second off request must take the native route: %v", off[1])
	}
	// Later content probes ask for no thinking.
	if last := f.bodies[len(f.bodies)-1]; last["reasoning_effort"] != "none" {
		t.Errorf("schema probe: %v", last)
	}
	// baseline, invalid effort, 6 listed levels, 2 off, image, sound, video, tool, 5 sampling, schema.
	if run.Status != ProbeOK || run.Requests != 20 {
		t.Errorf("run: %+v", run)
	}
	// Sound and video go as Chat sends them (the adapter takes them to the native API) and are heard and seen.
	if got[adapter.CapInAudio] != true || got[adapter.CapInVideo] != true {
		t.Errorf("sound/video: %v / %v", got[adapter.CapInAudio], got[adapter.CapInVideo])
	}
}

// Gemini 3.8 Flash: thinkingBudget 0 still thinks (its lowest level), and
// "minimal" is refused although the route lists it.
func TestProbeGeminiThinkingCannotTurnOff(t *testing.T) {
	f := &fakeGemini{defaultThinks: true, refusedLevels: []string{"minimal"}}
	facts, _ := probeGemini(t, f, geminiListed)
	got := values(facts)
	if got[adapter.CapThinkingOff] != adapter.ThinkingOffNever {
		t.Fatalf("off: %#v", got[adapter.CapThinkingOff])
	}
	if !strings.Contains(facts[adapter.CapThinkingOff].Note, "68 thinking tokens") {
		t.Errorf("note: %q", facts[adapter.CapThinkingOff].Note)
	}
	if l := got[adapter.CapThinkingLevels]; !reflect.DeepEqual(l, []string{"none", "low", "medium", "high", "xhigh"}) {
		t.Errorf("levels: %#v", l)
	}
	if !strings.Contains(facts[adapter.CapThinkingLevels].Note, "MINIMAL is not supported") {
		t.Errorf("the refusal is the evidence: %q", facts[adapter.CapThinkingLevels].Note)
	}
	// It thought on the first route: the second is not needed.
	if n := len(f.offRequests()); n != 1 {
		t.Errorf("off requests: %d", n)
	}
	if last := f.bodies[len(f.bodies)-1]; last["reasoning_effort"] != "low" {
		t.Errorf("content probes should ask for the least thinking: %v", last)
	}
}

// Gemini 4: no thinking on the plain question by default, so the switch
// counts only once the model is seen thinking at its top level.
func TestProbeGeminiOffNeedsThinkingSeen(t *testing.T) {
	f := &fakeGemini{offWorks: true}
	facts, _ := probeGemini(t, f, geminiListed)
	if got := values(facts); got[adapter.CapThinkingOff] != adapter.ThinkingOffBudgetZero {
		t.Fatalf("facts: %#v", got)
	}
	if !strings.Contains(facts[adapter.CapThinkingOff].Note, "at xhigh") {
		t.Errorf("note: %q", facts[adapter.CapThinkingOff].Note)
	}
	if _, claimed := facts[adapter.CapThinkingDefault]; claimed {
		t.Errorf("a plain reply without thinking says nothing about the default")
	}
}

func TestProbeGeminiOffLeftUnknownWithoutEvidence(t *testing.T) {
	for name, f := range map[string]*fakeGemini{
		// The replies carry no usage: zero thinking cannot be told from silence.
		"uncounted": {defaultThinks: true, offWorks: true, uncounted: true},
		// It never thinks on these questions, so no thinking at "none" proves nothing.
		"never seen thinking": {offWorks: true, refusedLevels: []string{"minimal", "low", "medium", "high", "xhigh"}},
	} {
		t.Run(name, func(t *testing.T) {
			facts, _ := probeGemini(t, f, geminiListed)
			if v, claimed := facts[adapter.CapThinkingOff]; claimed {
				t.Errorf("off recorded without evidence: %+v", v)
			}
		})
	}
}

// Gemini 2.5 Pro: a budget of 0 is refused.
func TestProbeGeminiBudgetRefused(t *testing.T) {
	f := &fakeGemini{defaultThinks: true, refuseBudget: true}
	facts, _ := probeGemini(t, f, geminiListed)
	if got := values(facts); got[adapter.CapThinkingOff] != adapter.ThinkingOffNever {
		t.Fatalf("facts: %#v", got)
	}
	if note := facts[adapter.CapThinkingOff].Note; !strings.Contains(note, "only works in thinking mode") {
		t.Errorf("note: %q", note)
	}
	if l := values(facts)[adapter.CapThinkingLevels]; !reflect.DeepEqual(l, []string{"minimal", "low", "medium", "high", "xhigh"}) {
		t.Errorf("levels: %#v", l)
	}
}

// What the record already says is not measured again: "never" is taken as said.
func TestProbeGeminiKnownNeverSendsNoOffRequest(t *testing.T) {
	f := &fakeGemini{defaultThinks: true, offWorks: true}
	known := adapter.Capabilities{adapter.CapThinking: true, adapter.CapThinkingOff: adapter.ThinkingOffNever}
	facts, _ := probeGemini(t, f, known)
	if _, claimed := facts[adapter.CapThinkingOff]; claimed || len(f.offRequests()) != 0 {
		t.Errorf("measured what was said: %+v, %d requests", facts[adapter.CapThinkingOff], len(f.offRequests()))
	}
}
