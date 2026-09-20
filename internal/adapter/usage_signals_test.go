package adapter

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// X.ai image generation carries no token counts; cost_in_usd_ticks is the only
// billing signal and must surface as a reported cost, not as "no usage".
func TestOpenAIUsageParsesXAICostTicks(t *testing.T) {
	a := &OpenAICompatible{}
	body := []byte(`{"data":[{"url":"https://x/img.png"}],"usage":{"cost_in_usd_ticks":700000000}}`)
	u, ok := a.ExtractUsage(body)
	if !ok {
		t.Fatal("cost-only usage must count as found")
	}
	if u.Reported {
		t.Fatal("no token counts were present; Reported must stay false")
	}
	if !u.CostReported || u.CostNano != 70_000_000 { // 7e8 ticks × 1e-10 USD = $0.07 = 7e7 nano-USD
		t.Fatalf("cost = %d reported=%v, want 70000000 reported", u.CostNano, u.CostReported)
	}
}

func TestOpenAIUsageParsesTopLevelCostTicks(t *testing.T) {
	u, ok := parseOpenAIUsage([]byte(`{"cost_in_usd_ticks":15,"data":[]}`))
	if !ok || !u.CostReported {
		t.Fatal("top-level cost_in_usd_ticks must be recognised")
	}
	if u.CostNano != 2 { // 15 ticks = 1.5 nano-USD, rounded to nearest
		t.Fatalf("cost nano = %d, want 2", u.CostNano)
	}
}

// Streamed TTS reports exact usage on the terminal speech.audio.done frame.
func TestOpenAIStreamCollectorReadsSpeechAudioDone(t *testing.T) {
	a := &OpenAICompatible{}
	c := a.NewStreamCollector()
	c.Feed([]byte(`data: {"type":"speech.audio.delta","audio":"AAAA"}`))
	c.Feed([]byte(`data: {"type":"speech.audio.done","usage":{"input_tokens":42,"output_tokens":1500,"total_tokens":1542}}`))
	u := c.Usage()
	if !u.Reported || u.TokensIn != 42 || u.TokensOut != 1500 {
		t.Fatalf("usage = %+v, want in=42 out=1500 reported", u)
	}
}

// The Responses API nests usage under "response" on response.completed.
func TestOpenAIStreamCollectorReadsResponsesCompleted(t *testing.T) {
	a := &OpenAICompatible{}
	c := a.NewStreamCollector()
	c.Feed([]byte(`data: {"type":"response.output_text.delta","delta":"hi"}`))
	c.Feed([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4},"output_tokens":3,"total_tokens":13}}}`))
	u := c.Usage()
	if !u.Reported || u.TokensIn != 10 || u.TokensOut != 3 || u.TokensCached != 4 {
		t.Fatalf("usage = %+v, want in=10 out=3 cached=4", u)
	}
}

func TestOpenAIUsageDerivesGroqThroughput(t *testing.T) {
	body := []byte(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_time":0.01,"completion_time":0.1}}`)
	u, ok := parseOpenAIUsage(body)
	if !ok || !u.ThroughputReported {
		t.Fatalf("usage = %+v, want throughput reported", u)
	}
	if !approx(u.TokensInPerSecond, 10000) || !approx(u.TokensOutPerSecond, 500) {
		t.Fatalf("throughput in=%v out=%v, want 10000/500", u.TokensInPerSecond, u.TokensOutPerSecond)
	}
}

func TestOpenAIUsageReadsLlamaCppTimings(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":20,"completion_tokens":30},"timings":{"prompt_n":20,"prompt_per_second":800.5,"predicted_n":30,"predicted_per_second":41.2}}`)
	u, _ := parseOpenAIUsage(body)
	if !u.ThroughputReported || !approx(u.TokensInPerSecond, 800.5) || !approx(u.TokensOutPerSecond, 41.2) {
		t.Fatalf("usage = %+v", u)
	}
}

func TestOllamaUsageDerivesThroughputFromDurations(t *testing.T) {
	a := &Ollama{}
	body := []byte(`{"done":true,"done_reason":"stop","prompt_eval_count":10,"prompt_eval_duration":500000000,"eval_count":40,"eval_duration":2000000000}`)
	u, ok := a.ExtractUsage(body)
	if !ok || !u.ThroughputReported {
		t.Fatalf("usage = %+v", u)
	}
	if !approx(u.TokensInPerSecond, 20) || !approx(u.TokensOutPerSecond, 20) {
		t.Fatalf("throughput in=%v out=%v, want 20/20", u.TokensInPerSecond, u.TokensOutPerSecond)
	}
}

// Plain OpenAI usage carries neither cost nor throughput; the flags must stay
// false so the proxy labels its own derivation as calculated.
func TestOpenAIUsageWithoutSignalsLeavesFlagsFalse(t *testing.T) {
	u, ok := parseOpenAIUsage([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":7}}`))
	if !ok || u.CostReported || u.ThroughputReported {
		t.Fatalf("usage = %+v", u)
	}
}

// Bedrock reports cache writes; they must reach the 5m tier and be disjoint.
func TestBedrockUsageKeepsCacheWrites(t *testing.T) {
	a := &Bedrock{}
	u, ok := a.ExtractUsage([]byte(`{"usage":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":100,"cacheWriteInputTokens":200},"stopReason":"end_turn"}`))
	if !ok {
		t.Fatal("expected usage")
	}
	if u.TokensCacheWrite5m != 200 || u.TokensCached != 100 || !u.CachedDisjoint {
		t.Fatalf("usage = %+v, want cache write 200, cached 100, disjoint", u)
	}
}

// Anthropic streams report cache reads/writes on message_start and the output
// count on message_delta; the shared merge must keep every dimension.
func TestAnthropicStreamCollectorKeepsCacheWrites(t *testing.T) {
	a := &Anthropic{}
	c := a.NewStreamCollector()
	c.Feed([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":200,"ephemeral_1h_input_tokens":300},"output_tokens":1}}}`))
	c.Feed([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`))
	u := c.Usage()
	if !u.Reported || u.TokensIn != 10 || u.TokensOut != 42 || u.TokensCached != 100 {
		t.Fatalf("usage = %+v", u)
	}
	if u.TokensCacheWrite5m != 200 || u.TokensCacheWrite1h != 300 || !u.CachedDisjoint {
		t.Fatalf("cache writes must survive the stream merge: %+v", u)
	}
	if u.FinishReason != "end_turn" {
		t.Fatalf("finish reason = %q", u.FinishReason)
	}
}
