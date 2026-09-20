package usage

import (
	"testing"

	"github.com/torvanis/janus/internal/adapter"
)

func TestComputeCost(t *testing.T) {
	// $10.00 per million input tokens, $30.00 out, $1.00 cached.
	rates := Rates{InNano: 10 * NanoPerUSD, OutNano: 30 * NanoPerUSD, CachedNano: 1 * NanoPerUSD}

	tests := []struct {
		name                      string
		in, out, cached, wantNano int64
	}{
		{name: "no usage"},
		{
			name: "one million fresh input tokens costs ten dollars",
			in:   1_000_000, wantNano: 10 * NanoPerUSD,
		},
		{
			name: "output priced at its own rate",
			out:  1_000_000, wantNano: 30 * NanoPerUSD,
		},
		{
			name: "cached tokens are a subset of input and bill at the cached rate",
			in:   1_000_000, cached: 500_000,
			// 500k fresh @ $10/M = $5.00, 500k cached @ $1/M = $0.50
			wantNano: 5*NanoPerUSD + NanoPerUSD/2,
		},
		{
			name: "cached larger than input is clamped rather than credited",
			in:   1000, cached: 5000,
			wantNano: 1000 * (1 * NanoPerUSD) / TokensPerRateUnit,
		},
		{
			name: "mixed request",
			in:   1500, out: 700, cached: 500,
			// 1000*10 + 500*1 + 700*30 = 10000 + 500 + 21000 nano-units per token-million
			wantNano: (1000*10*NanoPerUSD + 500*1*NanoPerUSD + 700*30*NanoPerUSD) / TokensPerRateUnit,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComputeCost(tc.in, tc.out, tc.cached, rates); got != tc.wantNano {
				t.Fatalf("ComputeCost(%d,%d,%d) = %d nano-USD, want %d", tc.in, tc.out, tc.cached, got, tc.wantNano)
			}
		})
	}
}

// TestComputeCostAll covers the five-dimension cost formula:
//
//	(in − cached)·rate_in + cached·rate_cached + out·rate_out
//	  + cw5m·rate_cw5m + cw1h·rate_cw1h
//
// with a single truncating division, including zero rates, zero cache-write
// tokens (regression: identical to the pre-cache-write output), large rates
// ($50/MTok, no int64 overflow), and mixed 5m+1h writes.
func TestComputeCostAll(t *testing.T) {
	base := Rates{InNano: 10 * NanoPerUSD, OutNano: 30 * NanoPerUSD, CachedNano: 1 * NanoPerUSD}

	tests := []struct {
		name     string
		tokens   TokenCounts
		rates    Rates
		wantNano int64
	}{
		{
			name: "all zeros",
		},
		{
			name:   "zero rates price nothing regardless of tokens",
			tokens: TokenCounts{In: 1_000_000, Out: 1_000_000, Cached: 500_000, CacheWrite5m: 100_000, CacheWrite1h: 50_000},
			// Rates zero-valued: a self-hosted model priced at $0 costs $0.
			wantNano: 0,
		},
		{
			name:   "zero cache-write tokens with cache-write rates set is a no-op",
			tokens: TokenCounts{In: 1500, Out: 700, Cached: 500},
			rates: Rates{InNano: base.InNano, OutNano: base.OutNano, CachedNano: base.CachedNano,
				CacheWrite5mNano: 10 * NanoPerUSD, CacheWrite1hNano: 5 * NanoPerUSD},
			// Identical to the historical three-dimension "mixed request" case.
			wantNano: (1000*10*NanoPerUSD + 500*1*NanoPerUSD + 700*30*NanoPerUSD) / TokensPerRateUnit,
		},
		{
			name:     "zero cache-write rates with cache-write tokens reported is a no-op",
			tokens:   TokenCounts{In: 1500, Out: 700, Cached: 500, CacheWrite5m: 10_000, CacheWrite1h: 5_000},
			rates:    base,
			wantNano: (1000*10*NanoPerUSD + 500*1*NanoPerUSD + 700*30*NanoPerUSD) / TokensPerRateUnit,
		},
		{
			name:   "mixed 5m and 1h cache writes are additive to input",
			tokens: TokenCounts{In: 1_000_000, CacheWrite5m: 10_000, CacheWrite1h: 5_000},
			rates: Rates{InNano: 10 * NanoPerUSD,
				CacheWrite5mNano: 10 * NanoPerUSD, CacheWrite1hNano: 5 * NanoPerUSD},
			// 1M fresh in @ $10/M = $10; 10k cw5m @ $10/M = $0.10; 5k cw1h @ $5/M = $0.025.
			// Cache writes never reduce the fresh-input count.
			wantNano: 10*NanoPerUSD + 100_000_000 + 25_000_000,
		},
		{
			name:   "large rates at fifty dollars per million tokens do not overflow",
			tokens: TokenCounts{In: 1_000_000, Out: 1_000_000, Cached: 500_000, CacheWrite5m: 100_000, CacheWrite1h: 50_000},
			rates: Rates{InNano: 50_000_000_000, OutNano: 50_000_000_000, CachedNano: 25_000_000_000,
				CacheWrite5mNano: 1_000_000_000, CacheWrite1hNano: 500_000_000},
			// 500k fresh @ $50/M = $25,000; 500k cached @ $25/M = $12,500;
			// 1M out @ $50/M = $50,000; 100k cw5m @ $1/M = $0.10; 50k cw1h @ $0.50/M = $0.025.
			wantNano: 25_000_000_000 + 12_500_000_000 + 50_000_000_000 + 100_000_000 + 25_000_000,
		},
		{
			name:   "anthropic-shaped fable 5 request",
			tokens: TokenCounts{In: 200_000, Out: 8_000, Cached: 150_000, CacheWrite5m: 40_000, CacheWrite1h: 12_000},
			rates: Rates{InNano: 10 * NanoPerUSD, OutNano: 50 * NanoPerUSD, CachedNano: 1 * NanoPerUSD,
				CacheWrite5mNano: NanoFromUSD(12.50), CacheWrite1hNano: 20 * NanoPerUSD},
			// 50k fresh @ $10/M + 150k cached @ $1/M + 8k out @ $50/M
			//   + 40k cw5m @ $12.50/M + 12k cw1h @ $20/M
			wantNano: (50_000*10*NanoPerUSD + 150_000*1*NanoPerUSD + 8_000*50*NanoPerUSD +
				40_000*12_500_000_000 + 12_000*20*NanoPerUSD) / TokensPerRateUnit,
		},
		{
			name:   "sub-nano cache-write products truncate downward",
			tokens: TokenCounts{CacheWrite5m: 1, CacheWrite1h: 1},
			rates:  Rates{CacheWrite5mNano: 1, CacheWrite1hNano: 1},
			// 1·1 + 1·1 = 2 nano-units per token-million → truncates to 0.
			wantNano: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComputeCostAll(tc.tokens, tc.rates); got != tc.wantNano {
				t.Fatalf("ComputeCostAll(%+v, %+v) = %d nano-USD, want %d", tc.tokens, tc.rates, got, tc.wantNano)
			}
		})
	}
}

// TestComputeCostAllMatchesLegacyWithoutCacheWrites pins historical prices
// independently of the implementation being tested.
func TestComputeCostAllMatchesLegacyWithoutCacheWrites(t *testing.T) {
	rates := Rates{InNano: 10 * NanoPerUSD, OutNano: 30 * NanoPerUSD, CachedNano: NanoPerUSD,
		CacheWrite5mNano: 12_500_000_000, CacheWrite1hNano: 20 * NanoPerUSD}
	cases := []struct {
		tokens TokenCounts
		want   int64
	}{
		{TokenCounts{}, 0},
		{TokenCounts{In: 1}, 10_000},
		{TokenCounts{In: 1_000_000}, 10_000_000_000},
		{TokenCounts{In: 1500, Out: 700, Cached: 500}, 31_500_000},
		{TokenCounts{In: 1000, Cached: 5000}, 1_000_000},
		{TokenCounts{In: 123_457, Out: 76_543, Cached: 11_111}, 3_430_861_000},
	}
	for _, tc := range cases {
		if got := ComputeCostAll(tc.tokens, rates); got != tc.want {
			t.Errorf("ComputeCostAll(%+v) = %d, want historical price %d", tc.tokens, got, tc.want)
		}
	}
}

// TestComputeCostAllClampsCachedLikeLegacy locks the clamp semantics: cached
// tokens larger than input are clamped, never credited, exactly as before —
// and cache-write tokens are unaffected by the clamp.
func TestComputeCostAllClampsCachedLikeLegacy(t *testing.T) {
	rates := Rates{InNano: 10 * NanoPerUSD, CachedNano: 1 * NanoPerUSD, CacheWrite5mNano: 2 * NanoPerUSD}
	got := ComputeCostAll(TokenCounts{In: 1000, Cached: 5000, CacheWrite5m: 1000}, rates)
	// 0 fresh, 1000 cached @ $1/M, 1000 cw5m @ $2/M.
	want := (1000*1*NanoPerUSD + 1000*2*NanoPerUSD) / int64(TokensPerRateUnit)
	if got != want {
		t.Fatalf("clamped cost = %d nano-USD, want %d", got, want)
	}
}

func TestComputeCostNeverExceedsExactValue(t *testing.T) {
	// Integer division truncates downward, so reported spend is never inflated.
	// One token against a rate finer than the nano-USD grid rounds to zero.
	rates := Rates{InNano: 1}
	if got := ComputeCost(1, 0, 0, rates); got != 0 {
		t.Fatalf("sub-nano cost = %d, want truncation to 0", got)
	}
	// A rate that divides evenly stays exact.
	if got := ComputeCost(1, 0, 0, Rates{InNano: 3 * NanoPerUSD}); got != 3000 {
		t.Fatalf("one token at $3/Mtok = %d nano-USD, want 3000 (exactly $0.000003)", got)
	}
}

func TestModalityForPath(t *testing.T) {
	tests := map[string]string{
		"/v1/chat/completions":     adapter.ModalityChat,
		"/v1/completions":          adapter.ModalityChat,
		"/v1/embeddings":           adapter.ModalityEmbedding,
		"/v1/images/generations":   adapter.ModalityImage,
		"/v1/images/edits":         adapter.ModalityImage,
		"/v1/audio/speech":         adapter.ModalityTTS,
		"/v1/audio/transcriptions": adapter.ModalitySTT,
		"/v1/audio/translations":   adapter.ModalitySTT,
		"/v1/responses":            adapter.ModalityResponse,
		"/v1/assistants":           adapter.ModalityAssistant,
		"/v1/threads/abc/messages": adapter.ModalityThread,
		"/v1/files":                adapter.ModalityFile,
		"/v1/moderations":          adapter.ModalityModeration,
		"/v1/fine_tuning/jobs":     adapter.ModalityFineTune,
		"/v1/something/unknown":    adapter.ModalityChat,
	}
	for path, want := range tests {
		t.Run(path, func(t *testing.T) {
			if got := ModalityForPath(path); got != want {
				t.Fatalf("ModalityForPath(%q) = %q, want %q", path, got, want)
			}
		})
	}
}

// TestModalityEnumCoversSupportedEndpoints locks the documented modality enum: every value the
// supported endpoints use must exist in the canonical enum the UI and validation use.
func TestModalityEnumCoversSupportedEndpoints(t *testing.T) {
	required := []string{
		"chat", "embedding", "image", "audio", "video", "response",
		"assistant", "function", "file", "moderation", "fine_tune",
	}
	have := map[string]bool{}
	for _, m := range adapter.AllModalities {
		have[m] = true
	}
	for _, want := range required {
		if !have[want] {
			t.Errorf("documented modality %q is missing from adapter.AllModalities", want)
		}
	}
}

// TestNanoFromUSDRoundsToNearestNano locks the money boundary: NanoFromUSD is
// the single point where human-entered float dollars become integer nano-USD,
// and it must round rather than truncate (regression: int64(0.07 * 1e9)
// truncated to 69,999,999).
func TestNanoFromUSDRoundsToNearestNano(t *testing.T) {
	tests := []struct {
		usd  float64
		want int64
	}{
		{0, 0},
		{0.07, 70_000_000},
		{0.1, 100_000_000},
		{29.99, 29_990_000_000},
		{3, 3_000_000_000},
		{0.000000001, 1}, // one nano-USD exactly
	}
	for _, tc := range tests {
		if got := NanoFromUSD(tc.usd); got != tc.want {
			t.Errorf("NanoFromUSD(%v) = %d, want %d", tc.usd, got, tc.want)
		}
	}
}

func TestEstimateTokensFromBytes(t *testing.T) {
	if got := EstimateTokensFromBytes(0); got != 0 {
		t.Fatalf("empty body estimated at %d tokens, want 0", got)
	}
	if got := EstimateTokensFromBytes(4000); got != 1000 {
		t.Fatalf("4000 bytes estimated at %d tokens, want 1000", got)
	}
}
