package usage

import (
	"fmt"
	"math/rand"
	"testing"
)

// Billing must not move when Anthropic and Bedrock usage is recorded the way
// it is billed (input tokens include cache reads and writes). These tests
// keep the previous cost function verbatim and compare it with the current
// one over the counts each provider's adapter produced before and produces
// now.

// legacyTokenCounts is TokenCounts as it was: In was only the fresh part when
// CachedDisjoint (Anthropic, Bedrock), the whole prompt otherwise (OpenAI).
type legacyTokenCounts struct {
	In, Out, Cached, CacheWrite5m, CacheWrite1h int64
	CachedDisjoint                              bool
}

// legacyComputeCostAll is the previous ComputeCostAll, verbatim.
func legacyComputeCostAll(t legacyTokenCounts, r Rates) int64 {
	fresh := t.In
	if !t.CachedDisjoint {
		if t.Cached > t.In {
			t.Cached = t.In
		}
		fresh = t.In - t.Cached
	}
	total := fresh*r.InNano + t.Cached*r.CachedNano + t.Out*r.OutNano +
		t.CacheWrite5m*r.CacheWrite5mNano + t.CacheWrite1h*r.CacheWrite1hNano
	return total / TokensPerRateUnit
}

// legacyPromptTokens is the previous TokenCounts.PromptTokens, verbatim.
func legacyPromptTokens(t legacyTokenCounts) int64 {
	if t.CachedDisjoint {
		return t.In + t.Cached + t.CacheWrite5m + t.CacheWrite1h
	}
	return t.In + t.CacheWrite5m + t.CacheWrite1h
}

// legacyProcessedInputTokens is the previous ProcessedInputTokens, verbatim.
func legacyProcessedInputTokens(t legacyTokenCounts) int64 {
	if t.CachedDisjoint {
		return max(0, t.In) + max(0, t.CacheWrite5m) + max(0, t.CacheWrite1h)
	}
	return max(0, t.In-max(0, t.Cached))
}

// asBilled is what the adapters record now for the same provider report: the
// fresh part plus cache reads plus cache writes for Anthropic and Bedrock;
// OpenAI-style counts are unchanged.
func asBilled(t legacyTokenCounts) TokenCounts {
	in := t.In
	if t.CachedDisjoint {
		in += t.Cached + t.CacheWrite5m + t.CacheWrite1h
	}
	return TokenCounts{In: in, Out: t.Out, Cached: t.Cached, CacheWrite5m: t.CacheWrite5m, CacheWrite1h: t.CacheWrite1h}
}

func legacyCost(t legacyTokenCounts, r Rates) int64 {
	return legacyComputeCostAll(t, r.ForPrompt(legacyPromptTokens(t)))
}

func billedCost(t TokenCounts, r Rates) int64 { return ComputeCostAll(t, r.ForPrompt(t.In)) }

// TestBillingUnchangedByRecordingTokensAsBilled is the byte-identical proof:
// every provider × reads × writes × rate card (plain, with 1h writes, with
// long-prompt lines on either side of the threshold) costs exactly what it
// did before.
func TestBillingUnchangedByRecordingTokensAsBilled(t *testing.T) {
	// Sonnet-class card: $3 in, $15 out, $0.30 read, $3.75 5m write, $6 1h write.
	plain := Rates{InNano: 3e9, OutNano: 15e9, CachedNano: 3e8, CacheWrite5mNano: 3_750_000_000, CacheWrite1hNano: 6e9}
	// Haiku 5.5-style card: prompts over 100,000 tokens bill every token higher.
	long := plain
	long.Lines = map[string]int64{
		"rate_in_nanousd@prompt_over=100000":             15e9,
		"rate_out_nanousd@prompt_over=100000":            75e9,
		"rate_cached_nanousd@prompt_over=100000":         15e8,
		"rate_cache_write_5m_nanousd@prompt_over=100000": 18_750_000_000,
		"rate_cache_write_1h_nanousd@prompt_over=100000": 30e9,
	}
	noWriteRates := Rates{InNano: 3e9, OutNano: 15e9, CachedNano: 3e8}
	cards := map[string]Rates{"plain": plain, "long-prompt lines": long, "no write rates": noWriteRates}

	type report struct {
		provider string
		counts   legacyTokenCounts
	}
	var reports []report
	for _, fresh := range []int64{0, 1, 50_000, 99_999, 100_001, 250_000} {
		for _, read := range []int64{0, 40_000, 60_000} {
			for _, w := range [][2]int64{{0, 0}, {2_000, 0}, {0, 1_000}, {2_000, 1_000}} {
				reports = append(reports,
					report{"anthropic", legacyTokenCounts{In: fresh, Out: 700, Cached: read, CacheWrite5m: w[0], CacheWrite1h: w[1], CachedDisjoint: true}},
					// Converse reports one cache-write count, billed at the 5m rate.
					report{"bedrock", legacyTokenCounts{In: fresh, Out: 700, Cached: read, CacheWrite5m: w[0] + w[1], CachedDisjoint: true}},
				)
			}
			// OpenAI-style: the prompt includes the cached part; no cache writes.
			reports = append(reports, report{"openai", legacyTokenCounts{In: fresh + read, Out: 700, Cached: read}})
		}
	}
	// An inconsistent OpenAI-style report (cached > prompt) keeps its clamp.
	reports = append(reports, report{"openai", legacyTokenCounts{In: 10, Cached: 900, Out: 5}})

	crossed := 0
	for name, r := range cards {
		for _, rep := range reports {
			before := legacyCost(rep.counts, r)
			now := asBilled(rep.counts)
			if after := billedCost(now, r); after != before {
				t.Errorf("%s %s %+v: cost %d, was %d", name, rep.provider, rep.counts, after, before)
			}
			if got, want := ProcessedInputTokens(now), legacyProcessedInputTokens(rep.counts); got != want {
				t.Errorf("%s %+v: processed input %d, was %d", rep.provider, rep.counts, got, want)
			}
			if name == "long-prompt lines" && rep.counts.CachedDisjoint && rep.counts.In <= 100_000 && now.In > 100_000 {
				crossed++ // fresh part under the threshold, whole prompt over it
			}
		}
	}
	if crossed == 0 {
		t.Fatal("the matrix must include prompts whose fresh part is under the long-prompt threshold and whole prompt over it")
	}
}

// TestBillingUnchangedRandomized: the same comparison over random reports.
func TestBillingUnchangedRandomized(t *testing.T) {
	rng := rand.New(rand.NewSource(269))
	for i := 0; i < 20000; i++ {
		r := Rates{InNano: rng.Int63n(1e11), OutNano: rng.Int63n(1e11), CachedNano: rng.Int63n(1e10),
			CacheWrite5mNano: rng.Int63n(1e11), CacheWrite1hNano: rng.Int63n(1e11)}
		if i%3 == 0 {
			r.Lines = map[string]int64{fmt.Sprintf("rate_in_nanousd@prompt_over=%d", 1+rng.Int63n(500_000)): rng.Int63n(2e11)}
		}
		c := legacyTokenCounts{In: rng.Int63n(400_000), Out: rng.Int63n(50_000), Cached: rng.Int63n(400_000)}
		if i%2 == 0 { // Anthropic / Bedrock
			c.CachedDisjoint = true
			c.CacheWrite5m, c.CacheWrite1h = rng.Int63n(50_000), rng.Int63n(50_000)
		} else if c.Cached > c.In && i%10 != 1 { // OpenAI: cached is part of the prompt (bad data kept 1 in 10)
			c.Cached = rng.Int63n(c.In + 1)
		}
		if before, after := legacyCost(c, r), billedCost(asBilled(c), r); before != after {
			t.Fatalf("case %d %+v rates %+v: cost %d, was %d", i, c, r, after, before)
		}
	}
}

// TestWorkedExampleTokensAsBilled is the owner's example: 50,000 fresh input
// + 40,000 cache reads is a 90,000-token prompt, 44% of it cached, and costs
// what it always did.
func TestWorkedExampleTokensAsBilled(t *testing.T) {
	r := Rates{InNano: 3e9, OutNano: 15e9, CachedNano: 3e8}
	before := legacyComputeCostAll(legacyTokenCounts{In: 50_000, Cached: 40_000, Out: 1_000, CachedDisjoint: true}, r)
	now := TokenCounts{In: 90_000, Cached: 40_000, Out: 1_000}
	if now.In != 90_000 {
		t.Fatal("tokens_in must be the whole prompt")
	}
	if rate := float64(now.Cached) / float64(now.In); rate < 0.444 || rate > 0.445 {
		t.Fatalf("cache rate = %.4f, want 0.444", rate)
	}
	// 50,000 × $3/M + 40,000 × $0.30/M + 1,000 × $15/M = $0.15 + $0.012 + $0.015.
	if got := ComputeCostAll(now, r); got != before || got != 177_000_000 {
		t.Fatalf("cost = %d nano-USD, was %d, want 177,000,000 ($0.177)", got, before)
	}
}
