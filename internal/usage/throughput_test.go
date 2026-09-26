package usage

import (
	"testing"
	"time"
)

// Input speed must count only what the provider processed. A request that
// read 1,000 prompt tokens with 900 served from cache processed 100: its
// input rate over one second is 100 tok/s, not 1,000.
func TestProcessedInputTokensExcludesCacheReads(t *testing.T) {
	cases := []struct {
		name string
		in   TokenCounts
		want int64
	}{
		{"subset convention subtracts cached", TokenCounts{In: 1000, Cached: 900}, 100},
		{"no cache is unchanged", TokenCounts{In: 1000}, 1000},
		{"fully cached processes nothing", TokenCounts{In: 1000, Cached: 1000}, 0},
		{"inconsistent report never goes negative", TokenCounts{In: 100, Cached: 900}, 0},
		// Anthropic/Bedrock: input_tokens already excludes cache reads, and
		// cache writes are prompt the provider really processed.
		{"disjoint keeps fresh input", TokenCounts{In: 50, Cached: 9000, CachedDisjoint: true}, 50},
		{"disjoint adds cache writes", TokenCounts{In: 50, Cached: 9000, CacheWrite5m: 200, CacheWrite1h: 30, CachedDisjoint: true}, 280},
	}
	for _, tc := range cases {
		if got := ProcessedInputTokens(tc.in); got != tc.want {
			t.Errorf("%s: ProcessedInputTokens(%+v) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
	if got := TokensPerSecond(ProcessedInputTokens(TokenCounts{In: 1000, Cached: 900}), time.Second); got != 100 {
		t.Errorf("rate for 1000 in / 900 cached over 1s = %v, want 100", got)
	}
}
