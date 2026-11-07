package store

import (
	"net/url"
	"strings"
)

// Janus-run web search (user ruling 2026-10-22): for models whose provider
// runs no search of its own (vLLM, llama.cpp, Ollama, …), Janus can search
// for them through a search service the administrator picks. Off by
// default. Providers that search themselves (OpenAI, xAI, Claude, Gemini)
// always use their own search. Janus only runs SEARCHES — never code.

// Search providers.
const (
	SearchProviderSearXNG = "searxng"
	SearchProviderBrave   = "brave"
	SearchProviderExa     = "exa"
	SearchProviderTavily  = "tavily"
)

// SearchProviders lists them in display order.
var SearchProviders = []string{SearchProviderSearXNG, SearchProviderBrave, SearchProviderExa, SearchProviderTavily}

// SearchProviderNeedsKey reports a provider that authenticates with an API
// key (SearXNG is self-hosted and keyless).
func SearchProviderNeedsKey(p string) bool { return p != SearchProviderSearXNG }

// ChatSearchSettings configures the Janus-run search.
type ChatSearchSettings struct {
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider"`
	// URL is the SearXNG instance (http(s)://host[:port]); its JSON output
	// format must be enabled.
	URL string `json:"url"`
	// Keys holds each provider's API key, encrypted with the instance key,
	// so switching provider keeps the others. Never sent to the console:
	// the view reports KeysSet only.
	Keys map[string]string `json:"keys,omitempty"`
	// MaxResults per search (1–10).
	MaxResults int `json:"max_results"`
	// Deployed is the SearXNG address the deployment last applied
	// (JANUS_CHAT_SEARCH_SEARXNG_URL). Janus applies a deployment address
	// once; an administrator's later changes are kept across restarts.
	Deployed string `json:"deployed,omitempty"`
}

// KeySet reports a stored key for provider.
func (c ChatSearchSettings) KeySet(provider string) bool { return c.Keys[provider] != "" }

// Ready reports settings that can run a search.
func (c ChatSearchSettings) Ready() bool {
	if !c.Enabled || c.Provider == "" {
		return false
	}
	if c.Provider == SearchProviderSearXNG {
		return c.URL != ""
	}
	return c.KeySet(c.Provider)
}

// Check normalizes and validates the search settings alone.
func (c *ChatSearchSettings) Check() error { return c.normalize() }

func (c *ChatSearchSettings) normalize() error {
	c.Provider = strings.TrimSpace(strings.ToLower(c.Provider))
	c.URL = strings.TrimRight(strings.TrimSpace(c.URL), "/")
	if c.MaxResults <= 0 {
		c.MaxResults = 5
	}
	if c.MaxResults > 10 {
		c.MaxResults = 10
	}
	for k, v := range c.Keys {
		if v == "" || !validSearchProvider(k) || !SearchProviderNeedsKey(k) {
			delete(c.Keys, k)
		}
	}
	if c.Provider != "" && !validSearchProvider(c.Provider) {
		return &ValidationError{Field: "search.provider", Message: "unknown search service " + c.Provider}
	}
	if c.URL != "" {
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return &ValidationError{Field: "search.url", Message: "the SearXNG address must look like https://search.example.com"}
		}
	}
	if !c.Enabled {
		return nil
	}
	switch {
	case c.Provider == "":
		return &ValidationError{Field: "search.provider", Message: "choose a search service before turning web search on"}
	case c.Provider == SearchProviderSearXNG && c.URL == "":
		return &ValidationError{Field: "search.url", Message: "enter your SearXNG address before turning web search on"}
	case SearchProviderNeedsKey(c.Provider) && !c.KeySet(c.Provider):
		return &ValidationError{Field: "search.key", Message: "add the API key before turning web search on"}
	}
	return nil
}

func validSearchProvider(p string) bool {
	for _, x := range SearchProviders {
		if x == p {
			return true
		}
	}
	return false
}
