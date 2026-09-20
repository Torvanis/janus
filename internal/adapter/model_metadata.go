package adapter

import (
	"encoding/json"
	"math/big"
	"net/url"
	"regexp"
	"sort"
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
	}
	return ""
}

func discoveryPrices(adapterType string, up Upstream, row map[string]json.RawMessage) (map[string]int64, []string) {
	rates := map[string]int64{}
	warnings := []string{}
	provider := MetadataProvider(adapterType, up.BaseURL)
	var fields map[string]string
	scale := int64(1)
	pricing := row
	switch provider {
	case "xai":
		// USD cents / 100M tokens -> nano-USD / 1M tokens: multiply by 100,000.
		scale = 100000
		fields = map[string]string{"prompt_text_token_price": "rate_in_nanousd", "completion_text_token_price": "rate_out_nanousd", "cached_prompt_text_token_price": "rate_cached_nanousd"}
		for k, v := range row {
			if strings.Contains(k, "long_context") && nonzeroJSON(v) {
				return rates, []string{"unsupported: conditional pricing"}
			}
			if strings.Contains(k, "price") && fields[k] == "" && !strings.Contains(k, "long_context") && nonzeroJSON(v) {
				warnings = append(warnings, "unsupported: "+k)
			}
		}
	case "openrouter":
		scale = 1000000000000000 // USD/token -> nano-USD/Mtok.
		pricing = nil
		if err := json.Unmarshal(row["pricing"], &pricing); err != nil {
			return rates, warnings
		}
		if v, ok := pricing["overrides"]; ok && nonemptyJSON(v) {
			return rates, []string{"unsupported: conditional pricing.overrides"}
		}
		fields = map[string]string{"prompt": "rate_in_nanousd", "completion": "rate_out_nanousd", "input_cache_read": "rate_cached_nanousd", "input_cache_write_1h": "rate_cache_write_1h_nanousd"}
		for k, v := range pricing {
			if fields[k] == "" && k != "overrides" && (nonzeroJSON(v) || k == "input_cache_write") {
				warnings = append(warnings, "unsupported: pricing."+k)
			}
		}
	default:
		return rates, warnings
	}
	for k, field := range fields {
		v, ok := pricing[k]
		if !ok || string(v) == "null" {
			continue
		}
		n, ok := scaledPrice(v, scale)
		if !ok {
			warnings = append(warnings, "invalid price: "+k)
			continue
		}
		rates[field] = n
	}
	sort.Strings(warnings)
	return rates, warnings
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
