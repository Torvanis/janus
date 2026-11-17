package adapter

import (
	"net/http"
	"testing"
)

// A decimal quote is parsed exactly and rounded half up to the nano-USD:
// never through float64, where 0.1+0.2-style drift or 15-digit limits would
// move the recorded cost off the provider's figure.
func TestQuotedNanoFromDecimalIsExact(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0.0000935", 93_500, true},                 // Requesty doc example
		{"0.000123456789", 123_457, true},           // sub-nano: 123456.789 rounds up
		{"0.0000000004", 0, true},                   // 0.4 nano rounds down
		{"0.0000000005", 1, true},                   // exactly half rounds up
		{"2.4e-05", 24_000, true},                   // Perplexity prints exponents
		{"1.0000000000000001", 1_000_000_000, true}, // beyond float64 precision
		{"12345.678901234", 12_345_678_901_234, true},
		{"0", 0, true}, // a quoted $0 is a price
		{"-0.01", 0, false},
		{"abc", 0, false},
		{"", 0, false},
		{"null", 0, false},
	} {
		got, ok := quotedNanoFromDecimal(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("quotedNanoFromDecimal(%q) = %d,%v want %d,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// Each provider's documented usage shape yields its quote; the same parser
// serves chat, embeddings, images and Responses bodies.
func TestParseOpenAIUsageReadsEveryProviderQuote(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int64
		ok   bool
	}{
		{"openrouter chat", `{"usage":{"prompt_tokens":194,"completion_tokens":2,"total_tokens":196,"cost":0.000123456789,"is_byok":false,"cost_details":{"upstream_inference_cost":null}}}`, 123_457, true},
		{"openrouter byok adds the provider's inference to OpenRouter's fee", `{"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.00001,"is_byok":true,"cost_details":{"upstream_inference_cost":0.0002}}}`, 210_000, true},
		{"openrouter non-byok ignores upstream_inference_cost", `{"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.00001,"is_byok":false,"cost_details":{"upstream_inference_cost":0.0002}}}`, 10_000, true},
		{"openrouter free model quotes zero", `{"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0}}`, 0, true},
		{"requesty", `{"usage":{"prompt_tokens":13,"completion_tokens":17,"total_tokens":30,"cost":0.0000935}}`, 93_500, true},
		{"perplexity object", `{"usage":{"prompt_tokens":8,"completion_tokens":439,"cost":{"input_tokens_cost":2.4e-05,"output_tokens_cost":0.006585,"request_cost":0.006,"total_cost":0.012609}}}`, 12_609_000, true},
		{"deepinfra", `{"usage":{"prompt_tokens":15,"completion_tokens":16,"total_tokens":31,"estimated_cost":0.0000268}}`, 26_800, true},
		{"xai ticks win over any other field", `{"usage":{"prompt_tokens":1,"completion_tokens":1,"cost_in_usd_ticks":158500,"cost":9}}`, 15_850, true},
		{"openrouter embeddings", `{"object":"list","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":5,"total_tokens":5,"cost":0.0000001}}`, 100, true},
		{"openrouter responses (nested)", `{"type":"response.completed","response":{"usage":{"input_tokens":194,"output_tokens":2,"total_tokens":196,"cost":0.95}}}`, 950_000_000, true},
		{"no quote", `{"usage":{"prompt_tokens":10,"completion_tokens":2}}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, _ := parseOpenAIUsage([]byte(tc.body))
			if u.CostReported != tc.ok || u.CostNano != tc.want {
				t.Fatalf("CostNano=%d CostReported=%v, want %d,%v", u.CostNano, u.CostReported, tc.want, tc.ok)
			}
		})
	}
}

// The Responses translator (codex adapter: OpenAI, xAI, OpenRouter and
// Perplexity Responses) reads the same quotes, and a quoted 0 ticks is a
// price.
func TestCodexUsageReadsQuotes(t *testing.T) {
	c := &Codex{}
	for _, tc := range []struct {
		body string
		want int64
		ok   bool
	}{
		{`{"usage":{"input_tokens":5,"output_tokens":1,"cost_in_usd_ticks":0}}`, 0, true},
		{`{"usage":{"input_tokens":5,"output_tokens":1,"cost_in_usd_ticks":37756000}}`, 3_775_600, true},
		{`{"usage":{"input_tokens":5,"output_tokens":1,"cost":0.0000935}}`, 93_500, true},
		{`{"usage":{"input_tokens":5,"output_tokens":1,"cost":{"total_cost":0.0022}}}`, 2_200_000, true},
		{`{"usage":{"input_tokens":5,"output_tokens":1}}`, 0, false},
	} {
		u, _ := c.ExtractUsage([]byte(tc.body))
		if u.CostNano != tc.want || u.CostReported != tc.ok {
			t.Errorf("%s: CostNano=%d CostReported=%v want %d,%v", tc.body, u.CostNano, u.CostReported, tc.want, tc.ok)
		}
	}
	// Streamed: the terminal event carries the quote.
	col := c.NewStreamCollector()
	col.Feed([]byte(`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n"))
	col.Feed([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":1,"cost":0.000001}}}` + "\n"))
	if u := col.Usage(); u.CostNano != 1_000 || !u.CostReported {
		t.Fatalf("streamed Responses quote: %+v", u)
	}
}

// A streamed chat completion quotes its cost on the final usage chunk only.
func TestOpenAIStreamCollectorKeepsFinalChunkQuote(t *testing.T) {
	col := (&OpenAICompatible{name: "openai_compatible"}).NewStreamCollector()
	col.Feed([]byte(`data: {"choices":[{"delta":{"content":"Hi"}}]}` + "\n"))
	col.Feed([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n"))
	col.Feed([]byte(`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":1,"cost":0.0000123456,"is_byok":false}}` + "\n"))
	col.Feed([]byte("data: [DONE]\n"))
	if u := col.Usage(); u.CostNano != 12_346 || !u.CostReported || u.TokensIn != 9 {
		t.Fatalf("stream usage = %+v", u)
	}
}

// LiteLLM quotes in a header; a quote in the body (from the provider that
// ran the call) wins over it.
func TestHeaderQuote(t *testing.T) {
	h := http.Header{}
	h.Set("x-litellm-response-cost", "0.00042")
	u := Usage{}
	u.ApplyHeaderQuote(h)
	if !u.CostReported || u.CostNano != 420_000 {
		t.Fatalf("header quote: %+v", u)
	}
	body := Usage{CostNano: 7, CostReported: true}
	body.ApplyHeaderQuote(h)
	if body.CostNano != 7 {
		t.Fatalf("header replaced a body quote: %+v", body)
	}
	none := Usage{}
	none.ApplyHeaderQuote(http.Header{})
	if none.CostReported {
		t.Fatal("no header, no quote")
	}
}
