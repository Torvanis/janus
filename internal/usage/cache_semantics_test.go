package usage

import (
	"testing"

	"github.com/torvanis/janus/internal/adapter"
)

// Rates modelled on a Sonnet-class Anthropic card: input $3.00/Mtok,
// cache-read $0.30/Mtok (0.1x input, per Anthropic's published multiplier),
// output $15.00/Mtok.
var anthropicRates = Rates{
	InNano:     3_000_000,
	CachedNano: 300_000,
	OutNano:    15_000_000,
}

// TestDisjointCacheIsBilledOnTop is the regression test for the cache-semantics
// inversion: Anthropic reports cache_read_input_tokens DISJOINT from
// input_tokens, so the cached prefix must be billed IN ADDITION to the fresh
// input, never clamped away.
func TestDisjointCacheIsBilledOnTop(t *testing.T) {
	counts := TokenCounts{In: 100, Cached: 5000, Out: 500, CachedDisjoint: true}

	got := ComputeCostAll(counts, anthropicRates)

	// 100*3.00 + 5000*0.30 + 500*15.00, all per Mtok, in nano-USD.
	want := int64(100*3_000_000+5000*300_000+500*15_000_000) / TokensPerRateUnit
	if got != want {
		t.Fatalf("disjoint cache cost = %d nano, want %d nano", got, want)
	}

	// The bug: clamping Cached to In discarded 4,900 of 5,000 cache-read
	// tokens. Assert we are strictly above that broken figure.
	buggy := ComputeCostAll(TokenCounts{In: 100, Cached: 5000, Out: 500}, anthropicRates)
	if got <= buggy {
		t.Fatalf("disjoint cost %d must exceed the clamped subset cost %d", got, buggy)
	}
}

// TestSubsetCacheIsUnchanged pins the OpenAI convention: cached tokens are
// already inside the input count and must be subtracted, not added. This is the
// pre-existing behaviour and must not regress.
func TestSubsetCacheIsUnchanged(t *testing.T) {
	counts := TokenCounts{In: 1000, Cached: 800, Out: 200}

	got := ComputeCostAll(counts, anthropicRates)

	// fresh = 1000-800 = 200 at input rate, 800 at cached rate.
	want := int64(200*3_000_000+800*300_000+200*15_000_000) / TokensPerRateUnit
	if got != want {
		t.Fatalf("subset cache cost = %d nano, want %d nano", got, want)
	}
}

// TestSubsetCacheStillClamps keeps the guard that made the old code safe for
// OpenAI: a cached count exceeding the input count is nonsense under the subset
// convention and must not produce a negative fresh-input term.
func TestSubsetCacheStillClamps(t *testing.T) {
	got := ComputeCostAll(TokenCounts{In: 10, Cached: 900, Out: 0}, anthropicRates)

	want := int64(10*300_000) / TokensPerRateUnit // all 10 billed as cached
	if got != want {
		t.Fatalf("clamped cost = %d nano, want %d nano", got, want)
	}
	if got < 0 {
		t.Fatal("cost must never go negative")
	}
}

// TestDisjointCacheDoesNotAffectZeroCacheRequests proves the change is
// bit-for-bit inert for requests with no cache activity, under either flag.
func TestDisjointCacheDoesNotAffectZeroCacheRequests(t *testing.T) {
	plain := TokenCounts{In: 1200, Out: 350}
	disjoint := TokenCounts{In: 1200, Out: 350, CachedDisjoint: true}

	if a, b := ComputeCostAll(plain, anthropicRates), ComputeCostAll(disjoint, anthropicRates); a != b {
		t.Fatalf("zero-cache cost differs by convention: subset=%d disjoint=%d", a, b)
	}
}

// TestCacheWritesAreAdditiveUnderBothConventions guards the interaction between
// the new flag and the existing cache-write dimensions, which are additive in
// both conventions.
func TestCacheWritesAreAdditiveUnderBothConventions(t *testing.T) {
	r := Rates{InNano: 3_000_000, CachedNano: 300_000, OutNano: 15_000_000,
		CacheWrite5mNano: 3_750_000, CacheWrite1hNano: 6_000_000}

	for _, disjoint := range []bool{false, true} {
		counts := TokenCounts{In: 1000, Out: 100, CacheWrite5m: 400, CacheWrite1h: 200, CachedDisjoint: disjoint}
		got := ComputeCostAll(counts, r)
		want := int64(1000*3_000_000+100*15_000_000+400*3_750_000+200*6_000_000) / TokensPerRateUnit
		if got != want {
			t.Fatalf("disjoint=%v: cache-write cost = %d nano, want %d nano", disjoint, got, want)
		}
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
// posture: anything unrecognised is unmetered rather than billed on a guess.
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
