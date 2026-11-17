package adapter

import (
	"encoding/json"
	"math/big"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Provider identity comes from the configured endpoint, never a model name or
// an untrusted owned_by string. Compatible APIs do not share pricing units.
func MetadataProvider(adapterType, baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Hostname()) {
	case "api.openai.com":
		if adapterType == "openai_compatible" {
			return "openai"
		}
	case "api.x.ai":
		if adapterType == "openai_compatible" {
			return "xai"
		}
	case "openrouter.ai":
		if adapterType == "openai_compatible" {
			return "openrouter"
		}
	case "api.anthropic.com":
		if adapterType == "anthropic" {
			return "anthropic"
		}
	case "generativelanguage.googleapis.com":
		if adapterType == "gemini" {
			return "google"
		}
	}
	return ""
}

// Price fields a provider's model list publishes, read into the model's
// automatic ("upstream") prices: the five token rates and price lines
// (usage lines: conditional token rates, per-image prices). Anything the
// reader cannot express is named in a warning and left out, never guessed.
//
// richer is the provider's richer model row where one was fetched (xAI's
// language-models list, Cerebras's public list), nil otherwise.
func discoveryPrices(adapterType string, up Upstream, row, richer map[string]json.RawMessage) (map[string]int64, []string) {
	switch CapabilityProvider(adapterType, up.BaseURL) {
	case "xai":
		return xaiPrices(row)
	case "openrouter":
		return openRouterPrices(row)
	case "cerebras":
		// Only the public list carries prices; they are OpenRouter-shaped
		// (USD per token, as strings).
		if richer == nil {
			return map[string]int64{}, []string{}
		}
		return openRouterPrices(richer)
	case "together":
		return togetherPrices(row)
	}
	return map[string]int64{}, []string{}
}

// xAI: token prices in USD cents per 100M tokens; image prices in ticks
// (1e-10 USD). Verified live 2026-11-14: grok-imagine-image image_price
// 200000000 = $0.02, the published per-image price.
func xaiPrices(row map[string]json.RawMessage) (map[string]int64, []string) {
	rates, warnings := map[string]int64{}, []string{}
	const centsPer100M = 100000 // -> nano-USD per 1M tokens
	base := map[string]string{"prompt_text_token_price": "rate_in_nanousd", "completion_text_token_price": "rate_out_nanousd", "cached_prompt_text_token_price": "rate_cached_nanousd"}
	for k, field := range base {
		if v, ok := row[k]; ok && nonemptyJSON(v) {
			if n, ok := scaledPrice(v, centsPer100M); ok {
				rates[field] = n
			} else {
				warnings = append(warnings, "invalid price: "+k)
			}
		}
	}
	// Long prompts: at or above long_context_threshold every token of the
	// request is billed at the *_long_context prices (0 = the standard
	// price). A line's prompt_over is "more than", so threshold-1.
	var threshold int64
	_ = json.Unmarshal(row["long_context_threshold"], &threshold)
	for k, field := range base {
		v, ok := row[k+"_long_context"]
		if !ok || !nonzeroJSON(v) {
			continue
		}
		if threshold <= 1 {
			warnings = append(warnings, "unsupported: "+k+"_long_context without a threshold")
			continue
		}
		if n, ok := scaledPrice(v, centsPer100M); ok {
			rates[field+"@prompt_over="+strconv.FormatInt(threshold-1, 10)] = n
		}
	}
	// Image input tokens are priced as input; a different price cannot be
	// expressed by the five rates.
	if v, ok := row["prompt_image_token_price"]; ok && nonzeroJSON(v) && string(v) != string(row["prompt_text_token_price"]) {
		warnings = append(warnings, "unsupported: prompt_image_token_price differs from the text price")
	}
	// Images: the default tier, then one line per quality and resolution.
	const ticksPerNano = 10
	if v, ok := row["image_price"]; ok && nonzeroJSON(v) {
		if n, ok := scaledPrice(v, 1); ok {
			rates["price_image_nanousd"] = (n + ticksPerNano/2) / ticksPerNano
		}
	}
	var tiers []struct {
		Quality    string          `json:"quality"`
		Resolution string          `json:"resolution"`
		Price      json.RawMessage `json:"price_per_image"`
	}
	if v, ok := row["pricing"]; ok && json.Unmarshal(v, &tiers) == nil {
		for _, t := range tiers {
			n, ok := scaledPrice(t.Price, 1)
			key := lineKey("price_image_nanousd", "quality", t.Quality, "resolution", t.Resolution)
			if !ok || key == "" {
				warnings = append(warnings, "invalid price: pricing[]")
				continue
			}
			rates[key] = (n + ticksPerNano/2) / ticksPerNano
		}
	}
	for k, v := range row {
		if strings.Contains(k, "price") && !knownXAIPrice[k] && nonzeroJSON(v) {
			warnings = append(warnings, "unsupported: "+k)
		}
	}
	sort.Strings(warnings)
	return rates, warnings
}

var knownXAIPrice = map[string]bool{
	"prompt_text_token_price": true, "completion_text_token_price": true, "cached_prompt_text_token_price": true,
	"prompt_text_token_price_long_context": true, "completion_text_token_price_long_context": true,
	"cached_prompt_text_token_price_long_context": true, "prompt_image_token_price": true, "image_price": true, "pricing": true,
}

// OpenRouter (and Cerebras's public list): USD per token as strings, in
// row.pricing. overrides[] entries conditioned only on min_prompt_tokens
// become long-prompt lines; time-of-day entries cannot be expressed.
func openRouterPrices(row map[string]json.RawMessage) (map[string]int64, []string) {
	rates, warnings := map[string]int64{}, []string{}
	const perToken = 1_000_000_000_000_000 // USD/token -> nano-USD/Mtok
	var pricing map[string]json.RawMessage
	if err := json.Unmarshal(row["pricing"], &pricing); err != nil {
		return rates, warnings
	}
	tokenFields := map[string]string{"prompt": "rate_in_nanousd", "completion": "rate_out_nanousd", "input_cache_read": "rate_cached_nanousd",
		"input_cache_write": "rate_cache_write_5m_nanousd", "input_cache_write_1h": "rate_cache_write_1h_nanousd"}
	for k, field := range tokenFields {
		v, ok := pricing[k]
		if !ok || !nonemptyJSON(v) {
			continue
		}
		if strings.Trim(string(v), `"`) == "-1" {
			// A router: the price depends on the model it picks. The
			// request is billed by the cost OpenRouter reports.
			if k == "prompt" {
				warnings = append(warnings, "variable price: billed by the cost the provider reports")
			}
			continue
		}
		if n, ok := scaledPrice(v, perToken); ok {
			rates[field] = n
		} else {
			warnings = append(warnings, "invalid price: "+k)
		}
	}
	if v, ok := pricing["request"]; ok && nonzeroJSON(v) {
		if n, ok := scaledPrice(v, 1_000_000_000); ok {
			rates["price_request_nanousd"] = n
		}
	}
	var overrides []map[string]json.RawMessage
	_ = json.Unmarshal(pricing["overrides"], &overrides)
	for _, o := range overrides {
		var minPrompt int64
		timed, unknown := false, false
		for k, v := range o {
			switch k {
			case "min_prompt_tokens":
				_ = json.Unmarshal(v, &minPrompt)
			case "utc_start", "utc_end", "utc_days":
				timed = true
			default:
				if _, ok := priceDecimal(v); !ok {
					unknown = true // a condition this reader does not know: skip the entry
				}
			}
		}
		switch {
		case timed:
			warnings = append(warnings, "unsupported: time-of-day pricing (the price in force when the list was read is used)")
			continue
		case unknown || minPrompt <= 0:
			warnings = append(warnings, "unsupported: conditional pricing.overrides")
			continue
		}
		for k, field := range tokenFields {
			if v, ok := o[k]; ok && nonemptyJSON(v) {
				if n, ok := scaledPrice(v, perToken); ok {
					rates[field+"@prompt_over="+strconv.FormatInt(minPrompt, 10)] = n
				}
			}
		}
	}
	for k, v := range pricing {
		switch k {
		case "prompt", "completion", "input_cache_read", "input_cache_write", "input_cache_write_1h", "request", "overrides", "discount":
			continue
		case "internal_reasoning":
			if string(v) == string(pricing["completion"]) {
				continue // billed as output tokens already
			}
		}
		if nonzeroJSON(v) {
			warnings = append(warnings, "unsupported: pricing."+k)
		}
	}
	sort.Strings(warnings)
	return rates, dedupeStrings(warnings)
}

// Together: pricing.input / pricing.output in USD per 1M tokens.
func togetherPrices(row map[string]json.RawMessage) (map[string]int64, []string) {
	rates, warnings := map[string]int64{}, []string{}
	var pricing map[string]json.RawMessage
	if json.Unmarshal(row["pricing"], &pricing) != nil {
		return rates, warnings
	}
	for k, field := range map[string]string{"input": "rate_in_nanousd", "output": "rate_out_nanousd", "cached_input": "rate_cached_nanousd"} {
		if v, ok := pricing[k]; ok && nonemptyJSON(v) {
			if n, ok := scaledPrice(v, 1_000_000_000); ok {
				rates[field] = n
			} else {
				warnings = append(warnings, "invalid price: pricing."+k)
			}
		}
	}
	for _, k := range []string{"hourly", "base", "finetune"} {
		if v, ok := pricing[k]; ok && nonzeroJSON(v) {
			warnings = append(warnings, "unsupported: pricing."+k)
		}
	}
	sort.Strings(warnings)
	return rates, warnings
}

// lineKey builds a canonical price line key from name/value conditions
// (sorted, lower-case); "" when a value cannot be a condition value.
func lineKey(base string, kv ...string) string {
	type cond struct{ k, v string }
	var cs []cond
	for i := 0; i+1 < len(kv); i += 2 {
		v := strings.ToLower(strings.TrimSpace(kv[i+1]))
		if v == "" {
			continue
		}
		if !lineValue.MatchString(v) {
			return ""
		}
		cs = append(cs, cond{kv[i], v})
	}
	if len(cs) == 0 {
		return base
	}
	sort.Slice(cs, func(a, b int) bool { return cs[a].k < cs[b].k })
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.k + "=" + c.v
	}
	return base + "@" + strings.Join(parts, ",")
}

var lineValue = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

func dedupeStrings(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func nonemptyJSON(v json.RawMessage) bool {
	s := strings.TrimSpace(string(v))
	return s != "null" && s != "[]" && s != "{}" && s != ""
}
func nonzeroJSON(v json.RawMessage) bool {
	r, ok := priceDecimal(v)
	return nonemptyJSON(v) && (!ok || r.Sign() != 0)
}

// Limit exponent size before big.Rat parsing to bound work on malformed feeds.
var decimalPrice = regexp.MustCompile(`^-?(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)(?:[eE][+-]?[0-9]{1,3})?$`)

func priceDecimal(v json.RawMessage) (*big.Rat, bool) {
	str := strings.Trim(string(v), "\"")
	if len(str) > 100 || !decimalPrice.MatchString(str) {
		return nil, false
	}
	return new(big.Rat).SetString(str)
}

// Decimal rational arithmetic avoids float overflow and loss when converting
// tiny provider prices. Round once to the nearest representable nano-USD.
func scaledPrice(v json.RawMessage, scale int64) (int64, bool) {
	r, ok := priceDecimal(v)
	if !ok || r.Sign() < 0 {
		return 0, false
	}
	r.Mul(r, new(big.Rat).SetInt64(scale))
	r.Add(r, big.NewRat(1, 2))
	n := new(big.Int).Quo(r.Num(), r.Denom())
	if !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}
