// Package usage owns metering semantics: cost arithmetic, modality
// classification, and the asynchronous writer that persists usage events.
//
// In local-only mode (JANUS_LOCAL_ONLY) the cost functions below are simply
// never invoked by the recording path — usage events carry cost_nanousd = 0
// while token and request accounting continues unchanged. The arithmetic here
// is deliberately unaware of the mode so that disabling it later resumes cost
// tracking byte-for-byte.
package usage

import (
	"math"
	"strings"
	"time"

	"github.com/torvanis/janus/internal/adapter"
)

// NanoPerUSD is the fixed-point scale for money. Every monetary value in Janus
// is an integer count of nano-USD (1e-9 USD), so cost arithmetic is exact and
// never subject to floating-point drift.
const NanoPerUSD = 1_000_000_000

// TokensPerRateUnit is the denominator of a rate card: rates are quoted per
// million tokens.
const TokensPerRateUnit = 1_000_000

// Accounting methods recorded on every event. Each value answers "where did
// the figures on this row come from?" so the admin view and analytics can
// tell an exact bill from an estimate from a gap:
//
//   - AccountingUpstream: the upstream reported token counts and the rate
//     card priced them. Exact.
//   - AccountingUpstreamCost: the upstream reported the request's COST
//     directly (X.ai cost_in_usd_ticks). Exact, and independent of the rate
//     card, which cannot express per-image pricing.
//   - AccountingBytes: a text-shaped response carried no usage block, so the
//     tokens are ~bytes/4. An estimate; admins should check the upstream.
//   - AccountingUnmetered: a media response (audio/image/video) carried no
//     usage block and byte estimation would be wrong by orders of magnitude,
//     so it was recorded with zero tokens and zero cost. A CONFIGURATION GAP
//     — the upstream's usage extraction needs a real signal — surfaced
//     distinctly so it is never mistaken for a text estimate or a free
//     request.
//   - AccountingNone: recorded for observability only (upstream error,
//     pre-proxy rejection); deliberately not costed.
const (
	AccountingUpstream     = "upstream_reported"
	AccountingUpstreamCost = "upstream_reported_cost"
	AccountingBytes        = "byte_count_fallback"
	AccountingUnmetered    = "unmetered_modality"
	// AccountingNone marks an event that was recorded for observability but
	// deliberately not costed (upstream error responses, pre-proxy
	// rejections). Distinguishable in the data from an event billed at zero.
	AccountingNone = "not_billable"
)

// Throughput sources recorded beside tokens-per-second figures.
const (
	// ThroughputUpstream: the provider measured its own prompt/generation
	// time (Groq, llama.cpp, Ollama) and the figures are forwarded as-is.
	ThroughputUpstream = "upstream"
	// ThroughputCalculated: derived by the gateway from token counts and its
	// own clock — includes network and proxy time, so it is a lower bound on
	// what the provider achieved.
	ThroughputCalculated = "calculated"
)

// TokensPerSecond is tokens / duration, or 0 when either is non-positive so a
// zero-length window never produces an infinite rate.
func TokensPerSecond(tokens int64, d time.Duration) float64 {
	if tokens <= 0 || d <= 0 {
		return 0
	}
	return float64(tokens) / d.Seconds()
}

// Rates are the per-million-token prices in force for a request, one field per
// billing dimension. CacheWrite5mNano and CacheWrite1hNano price prompt-cache
// writes with 5-minute and 1-hour TTLs the way Anthropic bills them; providers
// that do not charge for cache writes leave them 0 and the terms vanish.
type Rates struct {
	InNano           int64
	OutNano          int64
	CachedNano       int64
	CacheWrite5mNano int64
	CacheWrite1hNano int64
}

// TokenCounts carries every token dimension of one request that has a price.
// CacheWrite5m and CacheWrite1h are ADDITIVE to In: upstreams that bill cache
// writes (Anthropic) report them separately from input_tokens, so they are
// never subtracted from the fresh-input count the way Cached is.
//
// CachedDisjoint selects between the two providers' opposite cache conventions
// (see adapter.Usage): false = Cached is a SUBSET of In and must be subtracted
// before applying the fresh-input rate (OpenAI, Gemini); true = Cached is
// DISJOINT from In and is billed on top (Anthropic).
type TokenCounts struct {
	In             int64
	Out            int64
	Cached         int64
	CacheWrite5m   int64
	CacheWrite1h   int64
	CachedDisjoint bool
}

// ComputeCostAll prices one request in nano-USD across all five billing
// dimensions.
//
// Cache-read tokens are billed at the cached rate in both conventions; what
// differs is whether they were already counted inside In:
//
//	subset   (OpenAI):    (in − cached)·rate_in + cached·rate_cached + …
//	disjoint (Anthropic):  in·rate_in           + cached·rate_cached + …
//
// The subset branch clamps Cached to In because a cached count larger than the
// input count is only meaningful under the disjoint convention; clamping a
// genuinely disjoint count would discard the entire cached prefix (a measured
// 23x undercharge on a real Anthropic cache hit).
//
// Cache-write tokens are reported separately from input tokens under both
// conventions and are always billed on top, adding the terms
// cw5m*rate_cw5m + cw1h*rate_cw1h.
//
// The five products are summed BEFORE the single truncating division so a
// request with zero cache-write tokens (or zero cache-write rates) produces
// bit-for-bit the same cost as before those dimensions existed. Division is
// integer and truncating, which biases cost very slightly downward — never
// upward — so the gateway can never over-report spend.
func ComputeCostAll(t TokenCounts, r Rates) int64 {
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

// ComputeCost is the pre-cache-write, three-dimension form of ComputeCostAll.
// This convenience wrapper is used by tests; production uses ComputeCostAll.
func ComputeCost(tokensIn, tokensOut, tokensCached int64, r Rates) int64 {
	return ComputeCostAll(TokenCounts{In: tokensIn, Out: tokensOut, Cached: tokensCached}, r)
}

// USD converts nano-USD to a float for presentation only. Never feed the result
// back into arithmetic that is later persisted.
func USD(nano int64) float64 { return float64(nano) / float64(NanoPerUSD) }

// NanoFromUSD converts a human-entered dollar amount to storage units. This is
// the single point where floating-point dollars enter the integer-money domain,
// so it must round to the nearest nano-USD: a plain int64 conversion truncates
// toward zero and turns 0.07 into 69,999,999 nano-USD.
func NanoFromUSD(usd float64) int64 { return int64(math.Round(usd * float64(NanoPerUSD))) }

// EstimateTokensFromBytes is the fallback used when an upstream reports no token
// counts (custom endpoints, unknown response shapes on the catch-all route).
// The ~4 bytes/token ratio is the standard English-text approximation; events
// priced this way are tagged AccountingBytes so admins can spot them.
//
// It is calibrated for TEXT and must only ever be applied to text-shaped
// responses: callers gate it on ByteEstimatable(modality) and record media
// responses without usage as AccountingUnmetered instead. Applied to an MP3 or
// a base64 PNG it is off by two orders of magnitude in either direction.
func EstimateTokensFromBytes(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return n / 4
}

// ByteEstimatable reports whether EstimateTokensFromBytes is a defensible
// fallback for a modality.
//
// The ~4 bytes/token ratio only holds for natural-language text. Audio, image
// and video responses are binary (or base64/URL-wrapped) payloads whose byte
// count has no relationship to what the provider charges — estimating them
// produces errors of two to three orders of magnitude in either direction, and
// those fabricated tokens then gate quota enforcement. Such requests are
// recorded unmetered (AccountingUnmetered) instead of billed on a guess.
//
// The list is deliberately an allowlist of text-shaped modalities: a modality
// added later is unmetered until someone decides it is text, which fails safe.
func ByteEstimatable(modality string) bool {
	switch modality {
	case adapter.ModalityChat, adapter.ModalityEmbedding, adapter.ModalityResponse,
		adapter.ModalityFunction, adapter.ModalityAssistant, adapter.ModalityThread,
		adapter.ModalityModeration, adapter.ModalityFineTune, adapter.ModalitySTT:
		// STT is text OUT of audio: the response body is a transcript, so the
		// byte ratio is meaningful even though the request carried audio.
		return true
	default:
		// image, audio, tts, video, file — binary or media payloads.
		return false
	}
}

// ModalityForPath classifies a request path into the canonical 14-value
// modality enum. Classification happens at request time and is stored
// explicitly, so analytics never has to re-parse paths.
func ModalityForPath(path string) string {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "/chat/completions"), strings.HasSuffix(p, "/completions"):
		return adapter.ModalityChat
	case strings.Contains(p, "/embeddings"):
		return adapter.ModalityEmbedding
	case strings.Contains(p, "/images/"):
		return adapter.ModalityImage
	case strings.Contains(p, "/audio/speech"):
		return adapter.ModalityTTS
	case strings.Contains(p, "/audio/transcriptions"), strings.Contains(p, "/audio/translations"):
		return adapter.ModalitySTT
	case strings.Contains(p, "/audio/"):
		return adapter.ModalityAudio
	case strings.Contains(p, "/video"):
		return adapter.ModalityVideo
	case strings.Contains(p, "/responses"):
		return adapter.ModalityResponse
	case strings.Contains(p, "/assistants"):
		return adapter.ModalityAssistant
	case strings.Contains(p, "/threads"):
		return adapter.ModalityThread
	case strings.Contains(p, "/files"):
		return adapter.ModalityFile
	case strings.Contains(p, "/moderations"):
		return adapter.ModalityModeration
	case strings.Contains(p, "/fine_tuning"):
		return adapter.ModalityFineTune
	default:
		return adapter.ModalityChat
	}
}

// ModalityLabel renders a modality for the UI.
func ModalityLabel(m string) string {
	switch m {
	case adapter.ModalityTTS:
		return "Text to speech"
	case adapter.ModalitySTT:
		return "Speech to text"
	case adapter.ModalityFineTune:
		return "Fine-tuning"
	default:
		if m == "" {
			return "Unknown"
		}
		return strings.ToUpper(m[:1]) + strings.ReplaceAll(m[1:], "_", " ")
	}
}
