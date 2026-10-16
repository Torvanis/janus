package subscription

import (
	"slices"
	"testing"
)

// Refusal bodies exactly as the providers returned them through Janus
// (probed against the live providers, 2026-10-15).
const (
	openaiNoneRefused   = `{"error":{"message":"Unsupported value: 'reasoning_effort' does not support 'none' with this model. Supported values are: 'low', 'medium', 'high', and 'xhigh'.","type":"invalid_request_error","param":"reasoning_effort","code":"unsupported_value"}}`
	geminiMinimal       = `[{"error":{"code":400,"message":"Thinking level MINIMAL is not supported for this model. Please retry with other thinking level.","status":"INVALID_ARGUMENT"}}]`
	geminiXhigh         = `[{"error":{"code":400,"message":"Invalid reasoning_effort: xhigh. Valid values are: high, low, medium, minimal, none","status":"INVALID_ARGUMENT"}}]`
	anthropicEffortLvl  = `{"type":"error","error":{"type":"invalid_request_error","message":"This model does not support effort level 'xhigh'. Supported levels: high, low, max, medium."}}`
	openaiToolsNoEffort = `{"error":{"message":"Function tools with reasoning_effort are not supported for gpt-6.1-sol in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.","type":"invalid_request_error","param":"reasoning_effort","code":null}}`
)

func TestLearnFromProviderRefusals(t *testing.T) {
	cases := []struct {
		name, sent, body string
		ok               bool
		levels, rejected []string
	}{
		{"openai listed", "none", openaiNoneRefused, true, []string{"low", "medium", "high", "xhigh"}, nil},
		{"gemini level refused", "minimal", geminiMinimal, true, nil, []string{"minimal"}},
		{"gemini listed", "xhigh", geminiXhigh, true, []string{"high", "low", "medium", "minimal", "none"}, nil},
		{"anthropic effort level", "xhigh", anthropicEffortLvl, true, []string{"high", "low", "max", "medium"}, nil},
		// Not a refusal of the value: the tools fallback handles it.
		{"openai tools need responses", "medium", openaiToolsNoEffort, false, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := LearnFromRefusal(nil, c.sent, []byte(c.body))
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (%+v)", ok, c.ok, got)
			}
			if !ok {
				return
			}
			if got.Supported == nil || !*got.Supported {
				t.Fatalf("supported = %v, want true", got.Supported)
			}
			if !slices.Equal(got.Levels, c.levels) || !slices.Equal(got.Rejected, c.rejected) {
				t.Fatalf("levels=%v rejected=%v, want %v %v", got.Levels, got.Rejected, c.levels, c.rejected)
			}
		})
	}
}

// What a learned fact makes of the next request: the refused value never
// goes out again, the nearest accepted one does.
func TestFitAfterProviderRefusals(t *testing.T) {
	gem, _ := LearnFromRefusal(nil, "minimal", []byte(geminiMinimal))
	if got, _ := gem.Fit("minimal"); got != "low" {
		t.Fatalf("gemini minimal -> %q, want low", got)
	}
	gem2, _ := LearnFromRefusal(&gem, "xhigh", []byte(geminiXhigh))
	if got, _ := gem2.Fit("xhigh"); got != "high" {
		t.Fatalf("gemini xhigh -> %q, want high", got)
	}
	if got, _ := gem2.Fit("minimal"); got != "low" {
		t.Fatalf("gemini minimal after the list -> %q, want low (minimal is listed but refused)", got)
	}
	oa, _ := LearnFromRefusal(nil, "none", []byte(openaiNoneRefused))
	if got, _ := oa.Fit("none"); got != "low" {
		t.Fatalf("openai none -> %q, want low", got)
	}
	if got, _ := oa.Fit("minimal"); got != "low" {
		t.Fatalf("openai minimal -> %q, want low", got)
	}
}

func TestToolsNeedResponses(t *testing.T) {
	if !ToolsNeedResponses([]byte(openaiToolsNoEffort)) {
		t.Fatal("the OpenAI tools refusal was not recognised")
	}
	if ToolsNeedResponses([]byte(openaiNoneRefused)) {
		t.Fatal("an ordinary effort refusal was taken for the tools refusal")
	}
}

func TestRefusedSamplingParam(t *testing.T) {
	cases := []struct {
		body, want string
		has        []string
	}{
		{`{"type":"error","error":{"type":"invalid_request_error","message":"` + "`temperature`" + ` is deprecated for this model."}}`, "temperature", []string{"temperature"}},
		{`{"type":"error","error":{"type":"invalid_request_error","message":"` + "`top_p`" + ` is deprecated for this model."}}`, "top_p", []string{"temperature", "top_p"}},
		{`{"error":{"message":"Unsupported value: 'temperature' does not support 0.7 with this model. Only the default (1) value is supported.","type":"invalid_request_error","param":"temperature","code":"unsupported_value"}}`, "temperature", []string{"temperature"}},
		{`{"type":"error","error":{"type":"invalid_request_error","message":"` + "`temperature`" + ` may only be set to 1 when thinking is enabled."}}`, "temperature", []string{"temperature"}},
		// The body does not carry it: nothing to drop.
		{`{"type":"error","error":{"message":"` + "`top_p`" + ` is deprecated for this model."}}`, "", []string{"temperature"}},
		{`{"error":{"message":"messages: at least one message is required"}}`, "", []string{"temperature"}},
	}
	for _, c := range cases {
		if got := RefusedSamplingParam([]byte(c.body), c.has); got != c.want {
			t.Errorf("%s: got %q, want %q", c.body, got, c.want)
		}
	}
}
