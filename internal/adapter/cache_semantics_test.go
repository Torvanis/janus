package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

// Input tokens include cached tokens, as providers bill them: every adapter
// reports TokensIn as the whole prompt, with cache reads and writes as parts
// of it (see Usage). Anthropic and Bedrock report the fresh part only, so
// their adapters add the parts back.

// TestAnthropicUsageIsTheWholePrompt: 16 fresh + 4,183 read + 100 written
// (5m) + 20 written (1h) is a 4,319-token prompt.
func TestAnthropicUsageIsTheWholePrompt(t *testing.T) {
	a := &Anthropic{}
	body := []byte(`{"type":"message","stop_reason":"end_turn","usage":{
		"input_tokens":16,"output_tokens":4,"cache_read_input_tokens":4183,
		"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":20}}}`)

	u, ok := a.ExtractUsage(body)
	if !ok {
		t.Fatal("expected usage to be reported")
	}
	if u.TokensIn != 16+4183+100+20 || u.TokensCached != 4183 || u.TokensCacheWrite5m != 100 || u.TokensCacheWrite1h != 20 || u.TokensOut != 4 {
		t.Fatalf("usage = %+v, want in 4319 (16 fresh + 4183 read + 120 written), cached 4183, writes 100/20, out 4", u)
	}
}

// TestAnthropicStreamUsageIsTheWholePrompt covers the streaming path, where
// usage is split across message_start (inputs) and message_delta (outputs and
// the running totals). Each part is counted once, however many frames repeat
// it.
func TestAnthropicStreamUsageIsTheWholePrompt(t *testing.T) {
	a := &Anthropic{}
	c := a.NewStreamCollector()
	c.Feed([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":16,"cache_read_input_tokens":4183,"cache_creation_input_tokens":50,"output_tokens":1}}}`))
	if u := c.Usage(); u.TokensIn != 16+4183+50 {
		t.Fatalf("mid-stream TokensIn = %d, want 4249 (the mid-stream quota check reads it)", u.TokensIn)
	}
	c.Feed([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":16,"cache_read_input_tokens":4183,"cache_creation_input_tokens":50,"output_tokens":4}}`))

	u := c.Usage()
	if u.TokensIn != 16+4183+50 || u.TokensCached != 4183 || u.TokensCacheWrite5m != 50 || u.TokensOut != 4 {
		t.Fatalf("usage = %+v, want in 4249, cached 4183, write5m 50, out 4", u)
	}
	if again := c.Usage(); again.TokensIn != u.TokensIn {
		t.Fatalf("Usage() must not accumulate: %d then %d", u.TokensIn, again.TokensIn)
	}
}

// TestBedrockUsageIsTheWholePrompt: Converse counts cache reads and writes
// beside inputTokens, buffered and streamed.
func TestBedrockUsageIsTheWholePrompt(t *testing.T) {
	a := &Bedrock{}
	u, ok := a.ExtractUsage([]byte(`{"stopReason":"end_turn","usage":{"inputTokens":12,"outputTokens":34,"cacheReadInputTokens":500,"cacheWriteInputTokens":70}}`))
	if !ok || u.TokensIn != 12+500+70 || u.TokensCached != 500 || u.TokensCacheWrite5m != 70 {
		t.Fatalf("buffered usage = %+v, want in 582, cached 500, write5m 70", u)
	}
	c := a.NewStreamCollector()
	c.Feed([]byte(`{"metadata":{"usage":{"inputTokens":12,"outputTokens":34,"cacheReadInputTokens":500,"cacheWriteInputTokens":70}}}`))
	c.Feed([]byte(`{"usage":{"inputTokens":12,"outputTokens":34,"cacheReadInputTokens":500,"cacheWriteInputTokens":70}}`))
	if u := c.Usage(); u.TokensIn != 582 || u.TokensCached != 500 || u.TokensCacheWrite5m != 70 || u.TokensOut != 34 {
		t.Fatalf("streamed usage = %+v, want in 582, cached 500, write5m 70, out 34", u)
	}
}

// TestOpenAIUsageIsUnchanged: OpenAI already reports the whole prompt.
func TestOpenAIUsageIsUnchanged(t *testing.T) {
	a := &OpenAICompatible{}
	body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":50,
		"prompt_tokens_details":{"cached_tokens":800}}}`)

	u, ok := a.ExtractUsage(body)
	if !ok {
		t.Fatal("expected usage to be reported")
	}
	if u.TokensIn != 1000 || u.TokensCached != 800 {
		t.Fatalf("usage = %+v, want in 1000 (cached 800 inside it)", u)
	}
}

// openAIUsageOf decodes the usage object of a translated OpenAI-format body
// (chat.completion) or of the last usage-carrying chunk of a translated
// stream.
func openAIUsageOf(t *testing.T, out []byte) map[string]any {
	t.Helper()
	var last map[string]any
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if line == "" || line == "[DONE]" {
			continue
		}
		var v map[string]any
		if json.Unmarshal([]byte(line), &v) == nil {
			if u, ok := v["usage"].(map[string]any); ok {
				last = u
			}
		}
	}
	if last == nil {
		t.Fatalf("no usage in %s", out)
	}
	return last
}

func checkOpenAIUsage(t *testing.T, label string, u map[string]any, prompt, completion, cached float64) {
	t.Helper()
	details, _ := u["prompt_tokens_details"].(map[string]any)
	if u["prompt_tokens"] != prompt || u["completion_tokens"] != completion || u["total_tokens"] != prompt+completion || details["cached_tokens"] != cached {
		t.Fatalf("%s: usage = %v, want prompt %v (whole prompt), completion %v, total %v, cached %v", label, u, prompt, completion, prompt+completion, cached)
	}
}

// TestOpenAIFormatUsageFromAnthropicAndBedrock: a response translated to the
// OpenAI shape reports usage in OpenAI's convention — prompt_tokens is the
// whole prompt (50,000 fresh + 40,000 cache reads + 2,000 + 1,000 written =
// 93,000), cached_tokens the cache reads within it, total_tokens = prompt +
// completion — buffered and streamed, for Anthropic and Bedrock.
func TestOpenAIFormatUsageFromAnthropicAndBedrock(t *testing.T) {
	req := &Request{Path: "/v1/chat/completions", Model: "m"}

	a := &Anthropic{}
	out, err := a.TransformResponse(req, []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude",
		"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":50000,"output_tokens":700,"cache_read_input_tokens":40000,
		"cache_creation":{"ephemeral_5m_input_tokens":2000,"ephemeral_1h_input_tokens":1000}}}`))
	if err != nil {
		t.Fatal(err)
	}
	checkOpenAIUsage(t, "anthropic buffered", openAIUsageOf(t, out), 93000, 700, 40000)

	st := a.NewStreamTransformer(req, nil)
	var streamed []byte
	for _, line := range []string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":50000,"output_tokens":1,"cache_read_input_tokens":40000,"cache_creation":{"ephemeral_5m_input_tokens":2000,"ephemeral_1h_input_tokens":1000}}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":700}}`,
		`data: {"type":"message_stop"}`,
	} {
		frame, err := st.TransformStreamLine([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		streamed = append(streamed, frame...)
	}
	checkOpenAIUsage(t, "anthropic streamed", openAIUsageOf(t, streamed), 93000, 700, 40000)

	// Native /v1/messages responses are relayed as Anthropic sent them.
	native := []byte(`{"type":"message","usage":{"input_tokens":50000,"output_tokens":700,"cache_read_input_tokens":40000}}`)
	if out, _ := a.TransformResponse(&Request{Path: "/v1/messages", Model: "m"}, native); string(out) != string(native) {
		t.Fatalf("native Messages body changed: %s", out)
	}

	b := &Bedrock{}
	out, err = b.TransformResponse(req, []byte(`{"output":{"message":{"content":[{"text":"hi"}]}},"stopReason":"end_turn",
		"usage":{"inputTokens":50000,"outputTokens":700,"cacheReadInputTokens":40000,"cacheWriteInputTokens":3000}}`))
	if err != nil {
		t.Fatal(err)
	}
	checkOpenAIUsage(t, "bedrock buffered", openAIUsageOf(t, out), 93000, 700, 40000)

	bt := b.NewStreamTransformer(req, nil)
	streamed = nil
	for _, line := range []string{
		`{"role":"assistant"}`,
		`{"contentBlockIndex":0,"delta":{"text":"hi"}}`,
		`{"stopReason":"end_turn"}`,
		`{"usage":{"inputTokens":50000,"outputTokens":700,"cacheReadInputTokens":40000,"cacheWriteInputTokens":3000},"metrics":{"latencyMs":1}}`,
	} {
		frame, err := bt.TransformStreamLine([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		streamed = append(streamed, frame...)
	}
	checkOpenAIUsage(t, "bedrock streamed", openAIUsageOf(t, streamed), 93000, 700, 40000)
}
