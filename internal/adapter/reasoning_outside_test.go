package adapter

import "testing"

// X.ai reports reasoning tokens outside completion_tokens and bills them at the
// output rate (live 2026-11-16, grok-4.6: prompt 639 [512 cached], completion 1,
// reasoning 99, total 739, cost_in_usd_ticks 11,100,000 = 127x$2 + 512x$0.5 +
// 100x$6 per 1M). OpenAI counts reasoning inside completion_tokens.
func TestReasoningTokensRecordedAsBilled(t *testing.T) {
	cases := []struct {
		name string
		body string
		out  int64
	}{
		{"xai reasoning outside completion", `{"usage":{"prompt_tokens":639,"completion_tokens":1,"total_tokens":739,` +
			`"prompt_tokens_details":{"cached_tokens":512},"completion_tokens_details":{"reasoning_tokens":99},"cost_in_usd_ticks":11100000}}`, 100},
		{"openai reasoning inside completion", `{"usage":{"prompt_tokens":20,"completion_tokens":150,"total_tokens":170,` +
			`"completion_tokens_details":{"reasoning_tokens":128}}}`, 150},
		{"no total: leave as reported", `{"usage":{"prompt_tokens":20,"completion_tokens":5,` +
			`"completion_tokens_details":{"reasoning_tokens":40}}}`, 5},
		{"responses api outside", `{"usage":{"input_tokens":10,"output_tokens":3,"total_tokens":50,` +
			`"output_tokens_details":{"reasoning_tokens":37}}}`, 40},
	}
	for _, c := range cases {
		u, ok := parseOpenAIUsage([]byte(c.body))
		if !ok || u.TokensOut != c.out {
			t.Errorf("%s: TokensOut = %d (ok %v), want %d", c.name, u.TokensOut, ok, c.out)
		}
	}
}
