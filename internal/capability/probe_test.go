package capability

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
)

// fakeModel answers probe requests the way one kind of server does.
type fakeModel struct {
	// refuseMaxTokens: only max_completion_tokens is taken.
	refuseMaxTokens bool
	// efforts the server accepts; nil means it does not check the value.
	efforts []string
	// effortError is what it says when refusing an effort.
	effortError string
	// reasons by default; offBy names the request key that stops it.
	reasons bool
	offBy   string
	// onBy names the request key that starts reasoning when it is off;
	// "effort" means it reasons only when an effort other than none is sent.
	onBy string
	// image: "sees", "refuses" or "ignores".
	image string
	// sound and video: "takes", "refuses", "wrong" (answers, wrongly) or
	// "ignores".
	sound, video string
	// tools: "calls", "refuses" or "ignores".
	tools string
	// refusedSampling lists sampling parameters it refuses.
	refusedSampling []string
	// schema: "fits", "refuses" or "ignores".
	schema string
	// status, when set, answers everything with that status.
	status int
	// failAfter makes every request after the first N answer 429.
	failAfter int

	bodies []map[string]any
}

func refusal(param, msg string) Reply { return Reply{Status: 400, Param: param, Error: msg} }

func (f *fakeModel) call(_ context.Context, body map[string]any) (Reply, error) {
	f.bodies = append(f.bodies, body)
	if f.status != 0 {
		return Reply{Status: f.status, Error: "nope"}, nil
	}
	if f.failAfter > 0 && len(f.bodies) > f.failAfter {
		return Reply{Status: 429, Error: "slow down"}, nil
	}
	if _, has := body["max_tokens"]; has && f.refuseMaxTokens {
		return refusal("max_tokens", "Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."), nil
	}
	for _, p := range f.refusedSampling {
		if _, has := body[p]; has {
			return refusal(p, "Unsupported value: '"+p+"' does not support 0.7 with this model."), nil
		}
	}
	reasons := f.reasons
	if effort, has := body["reasoning_effort"].(string); has && f.efforts != nil {
		if !contains(f.efforts, effort) {
			return refusal("reasoning_effort", f.effortError), nil
		}
		if effort == "none" {
			reasons = false
		}
	}
	switch f.offBy {
	case "enable_thinking":
		if kw, ok := body["chat_template_kwargs"].(map[string]any); ok && kw["enable_thinking"] == false {
			reasons = false
		}
	case "think":
		if body["think"] == false {
			reasons = false
		}
	}
	switch f.onBy {
	case "effort":
		if effort, has := body["reasoning_effort"].(string); has && effort != "none" {
			reasons = true
		}
	case "enable_thinking":
		if kw, ok := body["chat_template_kwargs"].(map[string]any); ok && kw["enable_thinking"] == true {
			reasons = true
		}
	}
	r := Reply{Status: 200, Text: "43", InputTokens: 12, OutputTokens: 3}
	if reasons {
		r.Reasoning, r.ReasoningTokens, r.OutputTokens = "17 plus 26 is 43.", 9, 12
	}
	msg := body["messages"].([]any)[0].(map[string]any)
	if parts, ok := msg["content"].([]any); ok && len(parts) == 2 {
		switch parts[1].(map[string]any)["type"] {
		case "image_url":
			switch f.image {
			case "sees":
				r.Text = "Green."
			case "refuses":
				return refusal("", "This model does not support image input: it is not a multimodal model."), nil
			default:
				r.Text = "I cannot see any image."
			}
		case "input_audio":
			switch f.sound {
			case "takes":
				r.Text = "47"
			case "refuses":
				return refusal("audio", "At most 0 audio(s) may be provided in one prompt."), nil
			case "wrong":
				r.Text = "42"
			default:
				r.Text = "I cannot hear audio."
			}
		case "video_url":
			switch f.video {
			case "takes":
				r.Text = "Blue."
			case "refuses":
				return refusal("messages[0].content[1].type", "Invalid value: 'video_url'. Supported values are: 'text', 'image_url', 'input_audio', 'refusal', 'audio', and 'file'."), nil
			case "wrong":
				r.Text = "Green."
			default:
				r.Text = "I cannot see a video."
			}
		}
	}
	if _, has := body["tools"]; has {
		switch f.tools {
		case "calls":
			r.Text, r.ToolCalls = "", []string{probeToolName}
		case "refuses":
			return refusal("tools", "tools is not supported by this model"), nil
		case "reasoning":
			return refusal("reasoning_effort", "tools cannot be combined with reasoning_effort on this model"), nil
		case "responses":
			return refusal("", "Function tools with reasoning_effort are not supported for this model in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'."), nil
		}
	}
	if _, has := body["response_format"]; has {
		switch f.schema {
		case "fits":
			r.Text = `{"ok": true}`
		case "refuses":
			return refusal("response_format", "response_format of type json_schema is not supported"), nil
		default:
			r.Text = "ok is true"
		}
	}
	return r, nil
}

var probeNow = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

func probe(t *testing.T, f *fakeModel, adapterType string, known adapter.Capabilities) (Facts, ProbeRun) {
	t.Helper()
	var s State
	s.SetProvider(FromProvider(known, probeNow))
	return RunProbe(context.Background(), f.call, ProbeInput{Known: s.Profile(), AdapterType: adapterType, By: "manual", Now: func() time.Time { return probeNow }})
}

func values(f Facts) map[string]any {
	out := map[string]any{}
	for k, v := range f {
		out[k] = v.Value
	}
	return out
}

func TestProbeHostedReasoningModel(t *testing.T) {
	f := &fakeModel{refuseMaxTokens: true, efforts: []string{"none", "low", "medium", "high"}, reasons: true,
		effortError: "Invalid value: 'janus-probe'. Supported values are: 'none', 'low', 'medium', and 'high'.",
		image:       "sees", tools: "calls", refusedSampling: []string{"temperature"}, schema: "fits"}
	facts, run := probe(t, f, "openai_compatible", nil)
	want := map[string]any{
		adapter.CapOutputCap: adapter.OutputCapMaxCompletionTokens, adapter.CapThinking: true, adapter.CapThinkingDefault: true,
		adapter.CapThinkingLevels: []string{"none", "low", "medium", "high"}, adapter.CapThinkingOff: adapter.ThinkingOffEffortNone,
		adapter.CapInImage: true, adapter.CapTools: true, adapter.CapRefusedParams: []string{"temperature"}, adapter.CapStructured: true,
	}
	if got := values(facts); !reflect.DeepEqual(got, want) {
		t.Errorf("facts:\n got %#v\nwant %#v", got, want)
	}
	// 2 baseline, 1 invalid effort (the provider listed the rest), 1 off, image, sound, video, tool, 5 sampling, schema.
	if run.Status != ProbeOK || run.Requests != 14 || run.InputTokens == 0 || run.By != "manual" {
		t.Errorf("run: %+v", run)
	}
	if !strings.Contains(facts[adapter.CapOutputCap].Note, "max_tokens") || !strings.Contains(facts[adapter.CapThinkingLevels].Note, "Supported values") {
		t.Errorf("notes must say why: %q / %q", facts[adapter.CapOutputCap].Note, facts[adapter.CapThinkingLevels].Note)
	}
	// Once reasoning can be turned off, later requests turn it off; every later request uses the accepted cap.
	for i, b := range f.bodies[1:] {
		if _, has := b["max_tokens"]; has {
			t.Errorf("request %d still used the refused parameter", i+1)
		}
	}
	if last := f.bodies[len(f.bodies)-1]; last["reasoning_effort"] != "none" {
		t.Errorf("the schema probe should ask for no reasoning: %v", last)
	}
}

// A server that takes any parameter and ignores what it does not know:
// acceptance proves nothing, only the reply does.
func TestProbeServerThatIgnoresParameters(t *testing.T) {
	f := &fakeModel{reasons: true, offBy: "enable_thinking", image: "refuses", tools: "ignores", schema: "ignores"}
	facts, run := probe(t, f, "vlm", nil)
	want := map[string]any{
		adapter.CapOutputCap: adapter.OutputCapMaxTokens, adapter.CapThinking: true, adapter.CapThinkingDefault: true,
		adapter.CapThinkingOff: adapter.ThinkingOffEnableThinking, adapter.CapInImage: false, adapter.CapRefusedParams: []string{},
	}
	if got := values(facts); !reflect.DeepEqual(got, want) {
		t.Errorf("facts:\n got %#v\nwant %#v", got, want)
	}
	for _, k := range []string{adapter.CapThinkingLevels, adapter.CapTools, adapter.CapStructured} {
		if _, claimed := facts[k]; claimed {
			t.Errorf("%s was claimed from an answer that proved nothing", k)
		}
	}
	if run.Status != ProbeOK {
		t.Errorf("run: %+v", run)
	}
}

func TestProbeReasoningOffByDefaultIsFound(t *testing.T) {
	f := &fakeModel{onBy: "enable_thinking", image: "ignores", tools: "calls", schema: "fits"}
	facts, _ := probe(t, f, "llama_cpp", nil)
	got := values(facts)
	if got[adapter.CapThinking] != true || got[adapter.CapThinkingOff] != adapter.ThinkingOffEnableThinking {
		t.Errorf("facts: %#v", got)
	}
	if _, claimed := got[adapter.CapInImage]; claimed {
		t.Errorf("a model that said it saw nothing is not proof either way: %#v", got[adapter.CapInImage])
	}
}

func TestProbeModelWithoutReasoning(t *testing.T) {
	f := &fakeModel{efforts: []string{}, effortError: "Unrecognized request argument supplied: reasoning_effort", image: "sees", tools: "refuses", schema: "refuses"}
	facts, run := probe(t, f, "openai_compatible", nil)
	want := map[string]any{
		adapter.CapOutputCap: adapter.OutputCapMaxTokens, adapter.CapThinking: false, adapter.CapThinkingLevels: []string{},
		adapter.CapInImage: true, adapter.CapTools: false, adapter.CapRefusedParams: []string{}, adapter.CapStructured: false,
	}
	if got := values(facts); !reflect.DeepEqual(got, want) {
		t.Errorf("facts:\n got %#v\nwant %#v", got, want)
	}
	// 1 baseline, 1 invalid effort, 7 real efforts, image, sound, video, tool, 5 sampling, schema. No thinking switches.
	if run.Requests != 19 {
		t.Errorf("requests: %d", run.Requests)
	}
}

// A model that decides per question whether to reason: a plain reply
// without reasoning proves nothing about its default. Its tools work, but
// only on the Responses API, and the provider says so.
func TestProbeModelThatReasonsOnlySometimes(t *testing.T) {
	f := &fakeModel{efforts: []string{"none", "low", "high"}, effortError: "Supported values are: 'none', 'low', and 'high'.", onBy: "effort",
		image: "sees", tools: "responses", schema: "fits"}
	facts, _ := probe(t, f, "openai_compatible", nil)
	got := values(facts)
	if _, claimed := got[adapter.CapThinkingDefault]; claimed {
		t.Errorf("no reasoning on one easy question is not evidence of the default: %#v", got[adapter.CapThinkingDefault])
	}
	if got[adapter.CapThinking] != true || got[adapter.CapThinkingOff] != adapter.ThinkingOffEffortNone {
		t.Errorf("it reasoned at high and not at none: %#v", got)
	}
	if got[adapter.CapTools] != true || got[adapter.CapToolsEndpoint] != adapter.ToolsEndpointResponses || !strings.Contains(facts[adapter.CapToolsEndpoint].Note, "/v1/responses") {
		t.Errorf("tools: %#v / %q", got, facts[adapter.CapToolsEndpoint].Note)
	}
	if last := f.bodies[len(f.bodies)-1]; last["reasoning_effort"] != "none" {
		t.Errorf("later requests should turn reasoning off: %v", last)
	}

	// The same refusal without a way out says nothing about tools.
	f = &fakeModel{efforts: []string{"low"}, effortError: "bad effort", image: "sees", schema: "fits"}
	f.tools = "reasoning"
	facts, _ = probe(t, f, "openai_compatible", nil)
	if _, claimed := facts[adapter.CapTools]; claimed {
		t.Errorf("a refusal about reasoning is not a verdict on tools: %#v", facts[adapter.CapTools])
	}
}

func TestProbeReasoningThatCannotBeTurnedOff(t *testing.T) {
	f := &fakeModel{reasons: true, efforts: []string{"low", "high"}, effortError: "reasoning_effort must be one of: low, high", image: "sees", tools: "calls", schema: "fits"}
	facts, _ := probe(t, f, "openai_compatible", nil)
	got := values(facts)
	if got[adapter.CapThinkingOff] != adapter.ThinkingOffNever || !reflect.DeepEqual(got[adapter.CapThinkingLevels], []string{"low", "high"}) {
		t.Errorf("facts: %#v", got)
	}
	if note := facts[adapter.CapThinkingOff].Note; !strings.Contains(note, adapter.ThinkingOffEnableThinking) {
		t.Errorf("the note must list what was tried: %q", note)
	}
	// Reasoning cannot be stopped, so later requests ask for the least.
	if last := f.bodies[len(f.bodies)-1]; last["reasoning_effort"] != "low" {
		t.Errorf("later requests should use the lowest level: %v", last)
	}
}

// What the provider or the administrator already said is not measured again.
func TestProbeSkipsWhatIsKnown(t *testing.T) {
	f := &fakeModel{reasons: true, efforts: []string{"low", "medium", "high"}, effortError: "x", image: "ignores", tools: "ignores", schema: "ignores"}
	known := adapter.Capabilities{adapter.CapInImage: true, adapter.CapThinking: true, adapter.CapThinkingLevels: []string{"low", "medium", "high"}, adapter.CapThinkingOff: adapter.ThinkingOffNever,
		adapter.CapTools: true, adapter.CapStructured: true, adapter.CapOutputCap: adapter.OutputCapMaxCompletionTokens, adapter.CapRefusedParams: []string{}}
	facts, run := probe(t, f, "anthropic", known)
	if run.Requests != 1 || run.Status != ProbeOK {
		t.Errorf("only the baseline should be sent: %+v", run)
	}
	if got := values(facts); !reflect.DeepEqual(got, map[string]any{adapter.CapThinkingDefault: true}) {
		t.Errorf("facts: %#v", got)
	}
	if _, has := f.bodies[0]["max_completion_tokens"]; !has {
		t.Errorf("the known output parameter must be used: %v", f.bodies[0])
	}
}

func TestProbeFailuresClaimNothing(t *testing.T) {
	t.Run("the model refuses everything", func(t *testing.T) {
		facts, run := probe(t, &fakeModel{status: 401}, "openai_compatible", nil)
		if run.Status != ProbeFailed || len(facts) != 0 || !strings.Contains(run.Detail, "401") || run.Requests != 1 {
			t.Errorf("%+v %#v", run, facts)
		}
	})
	t.Run("no answer at all", func(t *testing.T) {
		facts, run := RunProbe(context.Background(), func(context.Context, map[string]any) (Reply, error) { return Reply{}, errors.New("dial tcp: timeout") },
			ProbeInput{AdapterType: "vlm"})
		if run.Status != ProbeFailed || len(facts) != 0 || run.Detail == "" {
			t.Errorf("%+v %#v", run, facts)
		}
	})
	t.Run("rate limited part way", func(t *testing.T) {
		f := &fakeModel{reasons: true, efforts: []string{"low"}, effortError: "bad effort", failAfter: 3, image: "sees", tools: "calls", schema: "fits"}
		facts, run := probe(t, f, "openai_compatible", nil)
		got := values(facts)
		// Baseline, the invalid effort and "none" were answered; everything after was 429.
		if run.Status != ProbeOK || got[adapter.CapOutputCap] != adapter.OutputCapMaxTokens {
			t.Errorf("%+v %#v", run, got)
		}
		for _, k := range []string{adapter.CapThinkingLevels, adapter.CapThinkingOff, adapter.CapInImage, adapter.CapInAudio, adapter.CapInVideo, adapter.CapTools, adapter.CapRefusedParams, adapter.CapStructured} {
			if _, claimed := got[k]; claimed {
				t.Errorf("%s was claimed from a rate-limited request: %#v", k, got[k])
			}
		}
	})
	t.Run("the request limit holds", func(t *testing.T) {
		n := 0
		_, run := RunProbe(context.Background(), func(context.Context, map[string]any) (Reply, error) {
			n++
			if n > MaxProbeRequests {
				t.Fatalf("request %d sent past the limit", n)
			}
			// Refuses every effort, reasons always: the longest path.
			return Reply{Status: 200, Text: "43", ReasoningTokens: 5}, nil
		}, ProbeInput{AdapterType: "openai_compatible"})
		if run.Requests > MaxProbeRequests {
			t.Errorf("requests: %d", run.Requests)
		}
	})
}

func TestClaimAndFinish(t *testing.T) {
	var s State
	s.Probe = Facts{adapter.CapTools: {Value: true, At: probeNow}}
	if !s.Claim("manual", probeNow) || s.Probed.Status != ProbeRunning {
		t.Fatal("first claim")
	}
	if s.Claim("enabled", probeNow.Add(time.Minute)) {
		t.Error("a second probe started while the first was running")
	}
	if !s.Claim("enabled", probeNow.Add(ProbeClaimTTL+time.Second)) {
		t.Error("an abandoned claim must expire")
	}
	// A model waiting its turn holds its place for longer, then counts as abandoned too.
	var q State
	if !q.Queue("all", probeNow) || q.Probed.Status != ProbeQueued || q.Claim("manual", probeNow.Add(ProbeClaimTTL+time.Minute)) {
		t.Errorf("queued: %+v", q.Probed)
	}
	q.Settle(probeNow.Add(time.Hour))
	if q.Probed.Status != ProbeQueued {
		t.Errorf("settled too early: %+v", q.Probed)
	}
	q.Begin("all", probeNow.Add(time.Hour))
	q.Settle(probeNow.Add(time.Hour + ProbeClaimTTL))
	if q.Probed.Status != ProbeFailed || !strings.Contains(q.Probed.Detail, "interrupted") {
		t.Errorf("an abandoned run must read as failed: %+v", q.Probed)
	}
	// A failed run keeps the earlier measurement.
	s.Finish(Facts{}, ProbeRun{At: probeNow, Status: ProbeFailed, Detail: "down"})
	if s.Probe[adapter.CapTools].Value != true || s.Probed.Status != ProbeFailed {
		t.Errorf("failed run: %+v", s)
	}
	s.Finish(Facts{adapter.CapInImage: {Value: true, At: probeNow}}, ProbeRun{At: probeNow, Status: ProbeOK, Requests: 4})
	if _, old := s.Probe[adapter.CapTools]; old || s.Probe[adapter.CapInImage].Value != true {
		t.Errorf("a good run replaces the measurement: %+v", s.Probe)
	}
	if back, err := Parse(s.String()); err != nil || back.Probed.Requests != 4 || len(back.WithoutProbe().Probe) != 0 || len(back.Probe) != 1 {
		t.Errorf("round trip: %v %+v", err, back)
	}
}

// A switch the record names is tried, not trusted. Ollama's think: false
// works on its native API only; through the OpenAI-compatible route the
// model keeps reasoning, and reasoning_effort "none" is what stops it.
func TestProbeDoesNotTrustANamedOffSwitch(t *testing.T) {
	f := &fakeModel{reasons: true, efforts: []string{"none", "low", "medium", "high"}, image: "ignores", tools: "ignores", schema: "ignores"}
	facts, _ := probe(t, f, "ollama", adapter.Capabilities{adapter.CapThinking: true, adapter.CapThinkingOff: adapter.ThinkingOffThink})
	if got := facts[adapter.CapThinkingOff]; got.Value != adapter.ThinkingOffEffortNone {
		t.Errorf("thinking.off = %+v, want the switch that worked", got)
	}

	// Where the named switch does work, it is confirmed as measured.
	f = &fakeModel{reasons: true, offBy: "think", image: "ignores", tools: "ignores", schema: "ignores"}
	facts, _ = probe(t, f, "ollama", adapter.Capabilities{adapter.CapThinking: true, adapter.CapThinkingOff: adapter.ThinkingOffThink})
	if got := facts[adapter.CapThinkingOff]; got.Value != adapter.ThinkingOffThink {
		t.Errorf("thinking.off = %+v, want the named switch confirmed", got)
	}
}
