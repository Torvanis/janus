package subscription

import (
	"encoding/json"
	"slices"
	"testing"
)

// Refusal bodies exactly as the vendors returned them (probed 2026-09-30).
const (
	xaiParamRefused  = `{"code":"invalid-argument","error":"Model grok-4.20-0309-reasoning does not support parameter reasoningEffort."}`
	xaiValueRefused  = "{\"code\":\"invalid-argument\",\"error\":\"This model does not support `reasoning_effort` value `none`.\"}"
	mistralListed    = `{"object":"error","message":"reasoning_effort='medium' is not supported for this model. Must be one of (<ReasoningEffort.none: 'none'>, <ReasoningEffort.high: 'high'>)","type":"invalid_request_invalid_args","param":null,"code":"3051"}`
	mistralListedAlt = `{"object":"error","message":"reasoning_effort low is not supported for this model, supported values: [<ReasoningEffort.high: 'high'>, <ReasoningEffort.none: 'none'>]","type":"invalid_request_invalid_args"}`
	mistralDisabled  = `{"object":"error","message":"reasoning_effort is not enabled for this model","type":"invalid_request_invalid_args","param":null,"code":"3051","raw_status_code":400}`
	copilotNoEffort  = `{"error":{"message":"reasoning_effort \"medium\" was provided, but model gpt-4o-2024-11-20 does not support reasoning effort","code":"invalid_reasoning_effort"}}`
	unrelated400     = `{"error":{"message":"messages: at least one message is required","type":"invalid_request_error"}}`
)

func TestLearnFromRefusal(t *testing.T) {
	cases := []struct {
		name, sent, body string
		ok               bool
		supported        *bool
		levels, rejected []string
	}{
		{"xai param", "medium", xaiParamRefused, true, boolPtr(false), nil, nil},
		{"xai value", "none", xaiValueRefused, true, boolPtr(true), nil, []string{"none"}},
		{"mistral listed", "medium", mistralListed, true, boolPtr(true), []string{"none", "high"}, nil},
		{"mistral listed alt", "low", mistralListedAlt, true, boolPtr(true), []string{"high", "none"}, nil},
		{"mistral disabled", "medium", mistralDisabled, true, boolPtr(false), nil, nil},
		{"copilot", "medium", copilotNoEffort, true, boolPtr(false), nil, nil},
		{"unrelated", "medium", unrelated400, false, nil, nil, nil},
		{"not json", "medium", "Bad Request", false, nil, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := LearnFromRefusal(nil, c.sent, []byte(c.body))
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if !got.Learned || (got.Supported == nil) != (c.supported == nil) || (got.Supported != nil && *got.Supported != *c.supported) {
				t.Fatalf("supported = %v learned=%v, want %v", got.Supported, got.Learned, *c.supported)
			}
			if !slices.Equal(got.Levels, c.levels) || !slices.Equal(got.Rejected, c.rejected) {
				t.Fatalf("levels=%v rejected=%v, want %v %v", got.Levels, got.Rejected, c.levels, c.rejected)
			}
		})
	}
}

func TestFit(t *testing.T) {
	none := NoReasoning()
	mistral := ReasoningLevels([]string{"none", "high"}, "")
	openai := ReasoningLevels([]string{"low", "medium", "high", "xhigh", "max"}, "medium")
	xaiNoNone := &Reasoning{Supported: boolPtr(true), Rejected: []string{"none"}, Learned: true}
	cases := []struct {
		name string
		r    *Reasoning
		want string
		sent string
		chg  bool
	}{
		{"unknown model: unchanged", nil, "medium", "medium", false},
		{"nothing requested", mistral, "", "", false},
		{"no reasoning: omitted", none, "medium", "", true},
		{"no reasoning: none omitted too", none, "none", "", true},
		{"listed: accepted", mistral, "high", "high", false},
		{"listed: medium -> high (tie goes up)", mistral, "medium", "high", true},
		{"listed: low -> none (nearer)", mistral, "minimal", "none", true},
		{"ladder: ultra -> max", openai, "ultra", "max", true},
		{"ladder: minimal -> low", openai, "minimal", "low", true},
		{"unknown value -> default", openai, "turbo", "medium", true},
		{"refused value -> nearest common", xaiNoNone, "none", "low", true},
		{"other values pass", xaiNoNone, "xhigh", "xhigh", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sent, chg := c.r.Fit(c.want)
			if sent != c.sent || chg != c.chg {
				t.Fatalf("Fit(%q) = %q,%v want %q,%v", c.want, sent, chg, c.sent, c.chg)
			}
		})
	}
}

func TestMergeReasoning(t *testing.T) {
	learnedLevels := Reasoning{Supported: boolPtr(true), Levels: []string{"none", "high"}, Learned: true}
	prev := map[string]Reasoning{
		"magistral": learnedLevels,
		"grok":      {Supported: boolPtr(true), Rejected: []string{"none"}, Learned: true},
		"gone":      {Supported: boolPtr(false), Learned: true},
		"flipped":   {Supported: boolPtr(false), Learned: true},
	}
	catalog := map[string]Reasoning{
		"magistral": {Supported: boolPtr(true)}, // catalog: reasons, no list -> keep learned list
		"flipped":   {Supported: boolPtr(true)}, // catalog contradicts learned -> catalog wins
		"gpt":       *ReasoningLevels([]string{"low"}, ""),
	}
	got := MergeReasoning([]string{"magistral", "grok", "flipped", "gpt"}, catalog, prev)
	if !slices.Equal(got["magistral"].Levels, []string{"none", "high"}) {
		t.Fatalf("learned levels lost: %+v", got["magistral"])
	}
	if !slices.Equal(got["grok"].Rejected, []string{"none"}) {
		t.Fatalf("learned refusal lost for catalog-silent model: %+v", got["grok"])
	}
	if _, ok := got["gone"]; ok {
		t.Fatal("facts kept for a model no longer offered")
	}
	if f := got["flipped"]; f.Learned || !*f.Supported {
		t.Fatalf("catalog should override contradicting learned fact: %+v", f)
	}
	if !slices.Equal(got["gpt"].Levels, []string{"low"}) {
		t.Fatalf("catalog facts missing: %+v", got["gpt"])
	}
}

func TestSetEffortShapes(t *testing.T) {
	for _, c := range []struct{ in, value, want string }{
		{`{"reasoning_effort":"medium","x":1}`, "high", `{"reasoning_effort":"high","x":1}`},
		{`{"reasoning_effort":"medium","x":1}`, "", `{"x":1}`},
		{`{"reasoning":{"effort":"medium","summary":"auto"}}`, "high", `{"reasoning":{"effort":"high","summary":"auto"}}`},
		{`{"reasoning":{"effort":"medium","summary":"auto"}}`, "", `{}`},
	} {
		var m map[string]json.RawMessage
		_ = json.Unmarshal([]byte(c.in), &m)
		if RequestedEffort(m) != "medium" {
			t.Fatalf("RequestedEffort(%s) = %q", c.in, RequestedEffort(m))
		}
		SetEffort(m, c.value)
		out, _ := json.Marshal(m)
		var a, b any
		_ = json.Unmarshal(out, &a)
		_ = json.Unmarshal([]byte(c.want), &b)
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if string(ja) != string(jb) {
			t.Fatalf("SetEffort(%s,%q) = %s, want %s", c.in, c.value, ja, jb)
		}
	}
}

func TestEffortsAdvertised(t *testing.T) {
	if e := NoReasoning().Efforts(); e == nil || len(e) != 0 {
		t.Fatalf("no-reasoning model must advertise an empty list, got %v", e)
	}
	var unknown *Reasoning
	if unknown.Efforts() != nil {
		t.Fatal("unknown model must advertise nothing")
	}
	r := &Reasoning{Supported: boolPtr(true), Levels: []string{"none", "low", "high"}, Rejected: []string{"none"}}
	if !slices.Equal(r.Efforts(), []string{"low", "high"}) {
		t.Fatalf("refused values must not be advertised: %v", r.Efforts())
	}
}
