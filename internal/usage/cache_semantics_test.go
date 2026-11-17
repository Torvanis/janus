package usage

import (
	"testing"

	"github.com/torvanis/janus/internal/adapter"
)

// Rates modeled on a Sonnet-class Anthropic card: input $3.00/Mtok,
// cache-read $0.30/Mtok (0.1x input, per Anthropic's published multiplier),
// output $15.00/Mtok.
var anthropicRates = Rates{
	InNano:     3_000_000,
	CachedNano: 300_000,
	OutNano:    15_000_000,
}

// TestCacheReadsAreBilledAsPartOfThePrompt is the regression test for the
// cache-semantics inversion: an Anthropic cache hit (16 fresh + 5,000 read,
// recorded as In 5,100 since input tokens include cached tokens) bills the
// cached prefix at the cache-read rate, never clamps it away.
func TestCacheReadsAreBilledAsPartOfThePrompt(t *testing.T) {
	counts := TokenCounts{In: 5100, Cached: 5000, Out: 500}

	got := ComputeCostAll(counts, anthropicRates)

	// 100*3.00 + 5000*0.30 + 500*15.00, all per Mtok, in nano-USD.
	want := int64(100*3_000_000+5000*300_000+500*15_000_000) / TokensPerRateUnit
	if got != want {
		t.Fatalf("cache-hit cost = %d nano, want %d nano", got, want)
	}

	// The old bug: the fresh count alone as In clamped 4,900 of 5,000
	// cache-read tokens away. Assert we are strictly above that figure.
	buggy := ComputeCostAll(TokenCounts{In: 100, Cached: 5000, Out: 500}, anthropicRates)
	if got <= buggy {
		t.Fatalf("whole-prompt cost %d must exceed the clamped cost %d", got, buggy)
	}
}

// TestOpenAICachedIsPartOfIn pins the OpenAI convention, which is the one
// convention now: cached tokens are inside the input count and are billed at
// the cached rate instead of the input rate.
func TestOpenAICachedIsPartOfIn(t *testing.T) {
	counts := TokenCounts{In: 1000, Cached: 800, Out: 200}

	got := ComputeCostAll(counts, anthropicRates)

	// fresh = 1000-800 = 200 at input rate, 800 at cached rate.
	want := int64(200*3_000_000+800*300_000+200*15_000_000) / TokensPerRateUnit
	if got != want {
		t.Fatalf("cached cost = %d nano, want %d nano", got, want)
	}
}

// TestCachedLargerThanInClamps keeps the guard against an inconsistent
// report: a cached count exceeding the whole prompt must not produce a
// negative fresh-input term.
func TestCachedLargerThanInClamps(t *testing.T) {
	got := ComputeCostAll(TokenCounts{In: 10, Cached: 900, Out: 0}, anthropicRates)

	want := int64(10*300_000) / TokensPerRateUnit // all 10 billed as cached
	if got != want {
		t.Fatalf("clamped cost = %d nano, want %d nano", got, want)
	}
	if got < 0 {
		t.Fatal("cost must never go negative")
	}
	// Writes beyond what is left of the prompt are clamped too.
	if got := ComputeCostAll(TokenCounts{In: 10, Cached: 4, CacheWrite5m: 100}, Rates{InNano: 1e6, CachedNano: 1e6, CacheWrite5mNano: 1e6}); got != 10 {
		t.Fatalf("over-reported writes cost = %d nano, want 10 (the whole prompt once)", got)
	}
}

// TestCacheWritesArePartOfIn: cache writes are part of the prompt, billed at
// their own rates instead of the input rate.
func TestCacheWritesArePartOfIn(t *testing.T) {
	r := Rates{InNano: 3_000_000, CachedNano: 300_000, OutNano: 15_000_000,
		CacheWrite5mNano: 3_750_000, CacheWrite1hNano: 6_000_000}

	counts := TokenCounts{In: 1600, Out: 100, CacheWrite5m: 400, CacheWrite1h: 200}
	got := ComputeCostAll(counts, r)
	want := int64(1000*3_000_000+100*15_000_000+400*3_750_000+200*6_000_000) / TokensPerRateUnit
	if got != want {
		t.Fatalf("cache-write cost = %d nano, want %d nano", got, want)
	}
}

// TestByteEstimatableRejectsMediaModalities is the regression test for the
// byte-count fallback: media responses must never be billed on a bytes/4 guess.
func TestByteEstimatableRejectsMediaModalities(t *testing.T) {
	for _, m := range []string{
		adapter.ModalityTTS,
		adapter.ModalityImage,
		adapter.ModalityAudio,
		adapter.ModalityVideo,
		adapter.ModalityFile,
	} {
		if ByteEstimatable(m) {
			t.Errorf("modality %q must not be byte-estimatable: a binary body has no bytes/token ratio", m)
		}
	}
}

// TestByteEstimatableAllowsTextModalities keeps the fallback working where it
// is actually defensible — otherwise unknown text endpoints silently stop
// metering.
func TestByteEstimatableAllowsTextModalities(t *testing.T) {
	for _, m := range []string{
		adapter.ModalityChat,
		adapter.ModalityEmbedding,
		adapter.ModalityResponse,
		adapter.ModalityModeration,
		adapter.ModalitySTT, // transcript out
	} {
		if !ByteEstimatable(m) {
			t.Errorf("modality %q should remain byte-estimatable", m)
		}
	}
}

// TestByteEstimatableFailsSafeOnUnknownModality documents the allowlist
// posture: anything unrecognized is unmetered rather than billed on a guess.
func TestByteEstimatableFailsSafeOnUnknownModality(t *testing.T) {
	if ByteEstimatable("hologram") {
		t.Fatal("unknown modality must default to not-estimatable")
	}
}

// TestTTSOverchargeIsGone reproduces the measured production bug end to end at
// the arithmetic level: a 30 KB MP3 previously became 7,680 phantom output
// tokens (~159x the real 48) under the $12/Mtok audio rate.
func TestTTSOverchargeIsGone(t *testing.T) {
	if ByteEstimatable(adapter.ModalityTTS) {
		t.Fatal("TTS must not be byte-estimated")
	}

	const mp3Bytes = 30720
	phantom := EstimateTokensFromBytes(mp3Bytes)
	if phantom != 7680 {
		t.Fatalf("sanity: expected the old estimator to yield 7680, got %d", phantom)
	}

	ttsRates := Rates{InNano: 600_000, OutNano: 12_000_000} // $0.60 / $12.00
	phantomCost := ComputeCostAll(TokenCounts{Out: phantom}, ttsRates)
	realCost := ComputeCostAll(TokenCounts{In: 5, Out: 48}, ttsRates)

	if phantomCost <= realCost*100 {
		t.Fatalf("expected the old fallback to be ~2 orders of magnitude high: phantom=%d real=%d", phantomCost, realCost)
	}
}
