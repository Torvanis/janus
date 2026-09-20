// Package ratecards ships dated reference metadata. Discovery uses exact,
// provider-scoped canonical IDs as an automatic fallback; the explicit legacy
// seed endpoint also supports its configured aliases. Reference prices are a
// starting point and should be checked against the provider's current pricing.
package ratecards

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
)

//go:embed ratecards.json
var raw []byte

// Entry is one reference rate card, in USD per million tokens. The two
// cache-write dimensions price prompt-cache writes (Anthropic bills 5-minute
// and 1-hour TTL writes above the base input rate); providers that do not
// bill cache writes simply omit the fields and they default to zero.
// ContextWindowTokens is the model's context window size in tokens as
// published by the provider when the seed was authored; 0 means the seed does
// not know it and the model keeps whatever an administrator set.
type Entry struct {
	Provider               string   `json:"provider"`
	Model                  string   `json:"model"`
	Aliases                []string `json:"aliases,omitempty"`
	InUSDPerMTok           float64  `json:"in_usd_per_mtok"`
	OutUSDPerMTok          float64  `json:"out_usd_per_mtok"`
	CachedUSDPerMTok       float64  `json:"cached_usd_per_mtok"`
	CacheWrite5mUSDPerMTok float64  `json:"cache_write_5m_usd_per_mtok"`
	CacheWrite1hUSDPerMTok float64  `json:"cache_write_1h_usd_per_mtok"`
	ContextWindowTokens    int64    `json:"context_window_tokens,omitempty"`
}

type seedFile struct {
	Comment   string  `json:"comment"`
	Updated   string  `json:"updated"`
	Ratecards []Entry `json:"ratecards"`
}

var (
	once   sync.Once
	parsed seedFile
	parse  error
)

func load() error {
	once.Do(func() {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			parse = fmt.Errorf("parse bundled ratecards.json: %w", err)
			return
		}
		if len(parsed.Ratecards) == 0 {
			parse = fmt.Errorf("bundled ratecards.json contains no entries")
		}
	})
	return parse
}

// Reference returns the bundled entries and the date they were authored.
func Reference() ([]Entry, string, error) {
	if err := load(); err != nil {
		return nil, "", err
	}
	return parsed.Ratecards, parsed.Updated, nil
}

// Match finds the reference entry for a discovered model name, matching the
// canonical name or any alias case-insensitively. Returns nil when the model
// is not in the seed.
func Match(name string) *Entry {
	if err := load(); err != nil {
		return nil
	}
	needle := strings.ToLower(strings.TrimSpace(name))
	for i := range parsed.Ratecards {
		e := &parsed.Ratecards[i]
		if strings.ToLower(e.Model) == needle {
			return e
		}
		for _, alias := range e.Aliases {
			if strings.ToLower(alias) == needle {
				return e
			}
		}
	}
	return nil
}

// Automatic returns only exact, provider-scoped entries. Omitted dimensions
// remain unknown; the legacy Match/apply API keeps its explicit seed behavior.
func Automatic(provider, name string) (map[string]int64, []string) {
	out := map[string]int64{}
	if provider == "openai" && (name == "gpt-6-astra" || name == "gpt-5.6-sol") {
		out["context_window"] = 1050000
		return out, []string{"reference: conditional pricing is unsupported"}
	}
	if provider == "" || load() != nil {
		return out, nil
	}
	var file struct {
		Ratecards []map[string]json.RawMessage `json:"ratecards"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return out, nil
	}
	for i, e := range parsed.Ratecards {
		if e.Provider != provider || e.Model != name {
			continue
		}
		if e.ContextWindowTokens > 0 {
			out["context_window"] = e.ContextWindowTokens
		}
		for input, output := range map[string]string{"in_usd_per_mtok": "rate_in_nanousd", "out_usd_per_mtok": "rate_out_nanousd", "cached_usd_per_mtok": "rate_cached_nanousd", "cache_write_5m_usd_per_mtok": "rate_cache_write_5m_nanousd", "cache_write_1h_usd_per_mtok": "rate_cache_write_1h_nanousd"} {
			if v, ok := file.Ratecards[i][input]; ok {
				var price float64
				if json.Unmarshal(v, &price) == nil {
					out[output] = int64(math.Round(price * 1e9))
				}
			}
		}
		break
	}
	return out, nil
}
