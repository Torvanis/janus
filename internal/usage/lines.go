package usage

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Price lines.
//
// A model's price is the five per-million-token rates (Rates) plus any
// number of price lines. A line is a key and a price in nano-USD:
//
//	<base>                      price_video_second_nanousd
//	<base>@<cond>=<v>,<cond>=<v> price_image_nanousd@quality=high,size=1024x1024
//	                            rate_in_nanousd@prompt_over=272000
//
// A base is either one of the five token rates (a conditional token rate,
// still per million tokens) or a unit price (per image, per second of
// video, …). Conditions say when the line applies. The five unconditional
// token rates are not lines: they stay where they always were, and
// ComputeCostAll prices them as it always did.

// Unit price bases, and what one unit is.
const (
	LineImage       = "price_image_nanousd"        // per image
	LineVideoSecond = "price_video_second_nanousd" // per second of video
	LineAudioMinute = "price_audio_minute_nanousd" // per minute of audio
	LineCharacters  = "price_mchar_nanousd"        // per million characters
	LinePages       = "price_kpage_nanousd"        // per thousand pages
	LineRequest     = "price_request_nanousd"      // per request
)

// Conditions a line may carry.
const (
	CondPromptOver = "prompt_over" // the prompt has more than this many tokens (token rates only)
	CondQuality    = "quality"
	CondSize       = "size"
	CondResolution = "resolution"
	CondAudio      = "audio" // "yes" or "no": video with or without sound
	CondTier       = "tier"  // the service tier the provider says it used
)

var tokenRateBases = []string{"rate_in_nanousd", "rate_out_nanousd", "rate_cached_nanousd", "rate_cache_write_5m_nanousd", "rate_cache_write_1h_nanousd"}

var unitBases = []string{LineImage, LineVideoSecond, LineAudioMinute, LineCharacters, LinePages, LineRequest}

var unitConditions = map[string]bool{CondQuality: true, CondSize: true, CondResolution: true, CondAudio: true, CondTier: true}

var conditionValue = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// Line is a parsed line key.
type Line struct {
	Base       string
	Conditions map[string]string
}

// IsTokenRate reports whether base is one of the five token rates.
func IsTokenRate(base string) bool {
	for _, b := range tokenRateBases {
		if b == base {
			return true
		}
	}
	return false
}

// ParseLine reads and validates a line key. A bare token rate is not a
// line (it is one of the five rates) and is refused here.
func ParseLine(key string) (Line, error) {
	base, rest, conditional := strings.Cut(key, "@")
	l := Line{Base: base, Conditions: map[string]string{}}
	token := IsTokenRate(base)
	unit := false
	for _, b := range unitBases {
		unit = unit || b == base
	}
	if !token && !unit {
		return l, fmt.Errorf("unknown price unit %q", base)
	}
	if token && !conditional {
		return l, fmt.Errorf("%q is a token rate, not a price line", base)
	}
	if !conditional {
		return l, nil
	}
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || !conditionValue.MatchString(v) {
			return l, fmt.Errorf("condition %q must be name=value, the value lower-case letters, digits, dot, dash or underscore", part)
		}
		if _, dup := l.Conditions[k]; dup {
			return l, fmt.Errorf("condition %q is given twice", k)
		}
		switch {
		case token && k == CondPromptOver:
			if n, err := strconv.ParseInt(v, 10, 64); err != nil || n <= 0 {
				return l, fmt.Errorf("prompt_over must be a positive number of tokens")
			}
		case !token && unitConditions[k]:
		default:
			return l, fmt.Errorf("condition %q does not apply to %s", k, base)
		}
		l.Conditions[k] = v
	}
	if token && len(l.Conditions) != 1 {
		return l, fmt.Errorf("a conditional token rate takes exactly one condition, prompt_over")
	}
	if l.Key() != key {
		return l, fmt.Errorf("conditions must be in alphabetical order: %s", l.Key())
	}
	return l, nil
}

// Key is the canonical key: conditions in alphabetical order.
func (l Line) Key() string {
	if len(l.Conditions) == 0 {
		return l.Base
	}
	names := make([]string, 0, len(l.Conditions))
	for k := range l.Conditions {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = k + "=" + l.Conditions[k]
	}
	return l.Base + "@" + strings.Join(parts, ",")
}

// ForPrompt returns the token rates that apply to a prompt of this size.
// The size is the whole prompt (TokenCounts.In: every input token, cached or
// not, cache writes included), which is what providers compare with their
// thresholds — Anthropic: "A request's prompt length counts all of its input
// tokens, including cache reads and cache writes"
// (platform.claude.com/docs/en/about-claude/pricing#long-context-pricing).
// Providers that charge more for long prompts charge the whole request at
// the higher rates once the prompt passes the threshold; a rate the
// threshold does not name keeps its ordinary price. With several
// thresholds the highest one passed applies.
func (r Rates) ForPrompt(promptTokens int64) Rates {
	best := int64(0)
	for key := range r.Lines {
		l, err := ParseLine(key)
		if err != nil || !IsTokenRate(l.Base) {
			continue
		}
		if n, _ := strconv.ParseInt(l.Conditions[CondPromptOver], 10, 64); promptTokens > n && n > best {
			best = n
		}
	}
	if best == 0 {
		return r
	}
	suffix := "@" + CondPromptOver + "=" + strconv.FormatInt(best, 10)
	for _, f := range []struct {
		base string
		rate *int64
	}{{"rate_in_nanousd", &r.InNano}, {"rate_out_nanousd", &r.OutNano}, {"rate_cached_nanousd", &r.CachedNano},
		{"rate_cache_write_5m_nanousd", &r.CacheWrite5mNano}, {"rate_cache_write_1h_nanousd", &r.CacheWrite1hNano}} {
		if v, ok := r.Lines[f.base+suffix]; ok {
			*f.rate = v
		}
	}
	return r
}

// UnitPrice finds the line that prices one unit of base for a request with
// these attributes: the line whose conditions all hold, the one with the
// most conditions when several do. ok is false when no line applies, which
// is "not priced", never "free".
func (r Rates) UnitPrice(base string, attrs map[string]string) (nano int64, key string, ok bool) {
	bestN := -1
	for k, v := range r.Lines {
		l, err := ParseLine(k)
		if err != nil || l.Base != base {
			continue
		}
		holds := true
		for name, want := range l.Conditions {
			holds = holds && attrs[name] == want
		}
		if !holds {
			continue
		}
		if n := len(l.Conditions); n > bestN || (n == bestN && k < key) {
			bestN, nano, key, ok = n, v, k, true
		}
	}
	return nano, key, ok
}

// Scaled multiplies every rate and line by factor (batch discounts).
func (r Rates) Scaled(factor float64) Rates {
	out := r
	out.InNano, out.OutNano, out.CachedNano = int64(float64(r.InNano)*factor), int64(float64(r.OutNano)*factor), int64(float64(r.CachedNano)*factor)
	out.CacheWrite5mNano, out.CacheWrite1hNano = int64(float64(r.CacheWrite5mNano)*factor), int64(float64(r.CacheWrite1hNano)*factor)
	if r.Lines != nil {
		out.Lines = make(map[string]int64, len(r.Lines))
		for k, v := range r.Lines {
			out.Lines[k] = int64(float64(v) * factor)
		}
	}
	return out
}
