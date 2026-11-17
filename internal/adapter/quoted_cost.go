package adapter

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net/http"
	"net/url"
	"strings"
)

// Provider-quoted cost.
//
// Owner ruling: whenever the upstream provider quotes the cost of a call,
// that exact quote IS the call's cost, over the rate card and over any
// administrator price line. Every quote a provider can send is read here,
// so the OpenAI-wire parser (parseOpenAIUsage) and the Responses parser
// (codexUsage) cannot drift in what they accept.
//
// Shapes, per provider (sources cited in the commit that added this file):
//
//   - xAI: usage.cost_in_usd_ticks (integer, 1e-10 USD) on chat
//     completions, Responses, image and video generation (and the finished
//     video poll), or the same field beside usage. Exact.
//   - OpenRouter, Requesty: usage.cost, a JSON number in USD (OpenRouter
//     calls the unit "credits"; 1 credit = 1 USD). OpenRouter sends it on
//     every response, the final stream chunk included, with no request flag
//     (usage.include is deprecated and ignored). With bring-your-own-key
//     (usage.is_byok = true) usage.cost is only OpenRouter's fee: the
//     provider bills the key holder usage.cost_details.upstream_inference_cost
//     separately, so the call cost is their sum.
//   - Perplexity: usage.cost is an object; usage.cost.total_cost (USD) is
//     the whole charge, request fee and search included.
//   - DeepInfra: usage.estimated_cost (USD). Despite the name it is the
//     amount DeepInfra bills for the call.
//   - LiteLLM proxy: the x-litellm-response-cost response header (USD), on
//     buffered responses only (QuotedCostFromHeader).
//
// Precision: a decimal quote is parsed exactly (math/big, no float64) and
// rounded half-up to the nano-USD the usage_event column stores. Anything
// finer than 1e-9 USD (xAI ticks are 1e-10; OpenRouter sometimes prints 12
// decimals) is rounded per call, so a sum of many rows can differ from the
// provider's invoice by at most half a nano-USD per call.

// quotingHosts are the provider APIs that quote each call's cost (above).
// A LiteLLM proxy quotes too, but it runs at an address of the customer's
// choosing and cannot be recognized from its URL.
var quotingHosts = map[string]bool{
	"api.x.ai":           true,
	"openrouter.ai":      true,
	"router.requesty.ai": true,
	"api.perplexity.ai":  true,
	"api.deepinfra.com":  true,
}

// QuotesCallCost reports whether the upstream at baseURL quotes the cost of
// each call, so that quote (not Janus's prices) is what a call costs.
func QuotesCallCost(baseURL string) bool {
	u, err := url.Parse(baseURL)
	return err == nil && quotingHosts[strings.ToLower(u.Hostname())]
}

// nanoPerUSD is 1e9 as a big integer.
var nanoPerUSD = big.NewRat(1_000_000_000, 1)

// quotedNanoFromDecimal converts a decimal USD amount as written in JSON
// ("0.0000935", "2.4e-05", "12") into nano-USD, exactly, rounding half up.
// Negative, non-finite, or unparseable input reports ok=false.
func quotedNanoFromDecimal(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return 0, false
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return 0, false
	}
	r.Mul(r, nanoPerUSD)
	// Round half up: floor(r + 1/2).
	r.Add(r, big.NewRat(1, 2))
	q := new(big.Int).Quo(r.Num(), r.Denom())
	if !q.IsInt64() {
		return 0, false
	}
	return q.Int64(), true
}

// quotedNanoFromTicks converts xAI's cost_in_usd_ticks (1e-10 USD) into
// nano-USD, rounding half up.
func quotedNanoFromTicks(ticks int64) (int64, bool) {
	if ticks < 0 {
		return 0, false
	}
	return (ticks + xaiTicksPerNanoUSD/2) / xaiTicksPerNanoUSD, true
}

// quotedCostFields is the part of a usage object that can carry a quote.
// Every field is raw so a number is never routed through float64.
type quotedCostFields struct {
	// Cost is a number (OpenRouter, Requesty) or an object with
	// total_cost (Perplexity).
	Cost          json.RawMessage `json:"cost"`
	IsBYOK        bool            `json:"is_byok"`
	EstimatedCost json.RawMessage `json:"estimated_cost"`
	CostDetails   *struct {
		UpstreamInferenceCost json.RawMessage `json:"upstream_inference_cost"`
	} `json:"cost_details"`
}

// quote returns the quoted cost of the call in nano-USD, or ok=false when
// the usage object quotes nothing.
func (f quotedCostFields) quote() (int64, bool) {
	if nano, ok := rawCostNano(f.Cost); ok {
		if f.IsBYOK && f.CostDetails != nil {
			// BYOK: cost is OpenRouter's fee; the provider bills the
			// inference to the key holder on top of it.
			if up, ok := rawCostNano(f.CostDetails.UpstreamInferenceCost); ok {
				nano += up
			}
		}
		return nano, true
	}
	return rawCostNano(f.EstimatedCost)
}

// rawCostNano reads a USD amount that is either a JSON number, a numeric
// string, or a Perplexity-style object with total_cost.
func rawCostNano(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0, false
	}
	switch raw[0] {
	case '{':
		var obj struct {
			TotalCost json.RawMessage `json:"total_cost"`
		}
		if json.Unmarshal(raw, &obj) != nil {
			return 0, false
		}
		return rawCostNano(obj.TotalCost)
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		return quotedNanoFromDecimal(s)
	default:
		return quotedNanoFromDecimal(string(raw))
	}
}

// QuotedCostNano reads a provider's cost quote as it appears in JSON (a
// number, a numeric string, or a Perplexity-style {"total_cost": …} object)
// into nano-USD, exactly. Used by the decision and classifier parsers so
// every quote takes the same path as the proxy's.
func QuotedCostNano(raw json.RawMessage) (int64, bool) { return rawCostNano(raw) }

// LiteLLMCostHeader is the response header a LiteLLM proxy sets to what the
// call cost (USD), on buffered responses.
const LiteLLMCostHeader = "X-Litellm-Response-Cost"

// QuotedCostFromHeader reads a cost quoted in a response header (LiteLLM's
// x-litellm-response-cost), in nano-USD. Only buffered responses are read:
// a streamed response's headers leave before the call has finished, so a
// header there cannot be the call's cost.
func QuotedCostFromHeader(h http.Header) (int64, bool) {
	v := h.Get(LiteLLMCostHeader)
	if v == "" {
		return 0, false
	}
	return quotedNanoFromDecimal(v)
}

// ApplyHeaderQuote sets a header-quoted cost on u when the body quoted none.
// A quote in the body wins: it comes from the provider that ran the call,
// while a header can be added by a proxy in front of it.
func (u *Usage) ApplyHeaderQuote(h http.Header) {
	if u.CostReported {
		return
	}
	if nano, ok := QuotedCostFromHeader(h); ok {
		u.CostNano, u.CostReported = nano, true
	}
}
