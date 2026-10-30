package adapter

import (
	"encoding/json"
	"testing"
)

// Streaming requests get stream_options.include_usage only where the API
// takes it. The Responses API refuses the field with a 400
// ("Unknown parameter: 'stream_options.include_usage'"), which broke every
// streamed /v1/responses call through Janus (found in a live test).
func TestStreamUsageOnlyForCompletionsEndpoints(t *testing.T) {
	a := &OpenAICompatible{name: "openai_compatible"}
	body := []byte(`{"model":"m","stream":true,"input":"hi"}`)
	for path, want := range map[string]bool{
		"/v1/chat/completions":     true,
		"/v1/completions":          true,
		"/v1/responses":            false,
		"/v1/audio/speech":         false,
		"/v1/audio/transcriptions": false,
	} {
		p, err := a.Prepare(Upstream{BaseURL: "https://api.example/v1"}, &Request{Path: path, Body: body, Streaming: true,
			Header: map[string][]string{"Content-Type": {"application/json"}}})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		_ = json.Unmarshal(p.Body, &got)
		if _, has := got["stream_options"]; has != want {
			t.Errorf("%s: stream_options injected = %v, want %v", path, has, want)
		}
	}
}

// Whisper reports {"usage":{"type":"duration","seconds":4}}: that is the
// request's billing size, not ~bytes/4 of the uploaded audio (a 4-second
// clip was recorded as 46,586 input tokens).
func TestParseWhisperDurationUsage(t *testing.T) {
	u, ok := parseOpenAIUsage([]byte(`{"text":"hello","usage":{"type":"duration","seconds":3.2}}`))
	if !ok || !u.Reported || u.TokensIn != 4 || u.TokensOut != 0 || !u.DurationBilled {
		t.Fatalf("usage = %+v ok=%v", u, ok)
	}
	// Token-reporting transcription models keep their token counts.
	u, _ = parseOpenAIUsage([]byte(`{"text":"hi","usage":{"type":"tokens","input_tokens":38,"output_tokens":16}}`))
	if u.TokensIn != 38 || u.TokensOut != 16 || u.DurationBilled {
		t.Fatalf("token usage = %+v", u)
	}
}
