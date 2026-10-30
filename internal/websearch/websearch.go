// Package websearch runs a web search through the service an administrator
// configured for Janus Chat (SearXNG, Brave, Exa, Tavily). It is used only
// for models whose provider runs no search of its own. It performs HTTP
// searches and nothing else — Janus never executes model-written code.
package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Result is one search hit.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Config selects and authenticates the service.
type Config struct {
	Provider   string // searxng | brave | exa | tavily
	URL        string // SearXNG base URL
	Key        string // API key (Brave, Exa, Tavily)
	MaxResults int
}

// Endpoints, overridable in tests.
var (
	BraveURL  = "https://api.search.brave.com/res/v1/web/search"
	ExaURL    = "https://api.exa.ai/search"
	TavilyURL = "https://api.tavily.com/search"
)

// ErrNotConfigured means the settings cannot run a search.
var ErrNotConfigured = errors.New("web search is not set up")

const maxBody = 2 << 20

// Search runs query and returns at most cfg.MaxResults results.
func Search(ctx context.Context, client *http.Client, cfg Config, query string) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty search query")
	}
	if len(query) > 400 {
		query = query[:400]
	}
	n := cfg.MaxResults
	if n <= 0 || n > 10 {
		n = 5
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var (
		req *http.Request
		err error
	)
	switch cfg.Provider {
	case "searxng":
		if cfg.URL == "" {
			return nil, ErrNotConfigured
		}
		u := strings.TrimRight(cfg.URL, "/") + "/search?" + url.Values{"q": {query}, "format": {"json"}, "safesearch": {"1"}}.Encode()
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err == nil {
			// SearXNG's bot detection logs an error for every request
			// without a client address; Janus is the only client.
			req.Header.Set("X-Real-IP", "127.0.0.1")
		}
	case "brave":
		if cfg.Key == "" {
			return nil, ErrNotConfigured
		}
		u := BraveURL + "?" + url.Values{"q": {query}, "count": {fmt.Sprint(n)}}.Encode()
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err == nil {
			req.Header.Set("X-Subscription-Token", cfg.Key)
		}
	case "exa":
		if cfg.Key == "" {
			return nil, ErrNotConfigured
		}
		body, _ := json.Marshal(map[string]any{"query": query, "numResults": n, "contents": map[string]any{"highlights": map[string]any{"numSentences": 3}}})
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, ExaURL, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("x-api-key", cfg.Key)
			req.Header.Set("Content-Type", "application/json")
		}
	case "tavily":
		if cfg.Key == "" {
			return nil, ErrNotConfigured
		}
		body, _ := json.Marshal(map[string]any{"query": query, "max_results": n, "search_depth": "basic"})
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, TavilyURL, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+cfg.Key)
			req.Header.Set("Content-Type", "application/json")
		}
	default:
		return nil, ErrNotConfigured
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s did not answer: %w", Label(cfg.Provider), err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(cfg.Provider, resp.StatusCode, raw)
	}
	out, err := parse(cfg.Provider, raw)
	if err != nil {
		return nil, fmt.Errorf("%s sent a reply Janus could not read: %w", Label(cfg.Provider), err)
	}
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}

func parse(provider string, raw []byte) ([]Result, error) {
	var out []Result
	switch provider {
	case "searxng":
		var r struct {
			Results []struct {
				Title   string `json:"title"`
				URL     string `json:"url"`
				Content string `json:"content"`
			} `json:"results"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		for _, x := range r.Results {
			out = append(out, Result{Title: x.Title, URL: x.URL, Snippet: x.Content})
		}
	case "brave":
		var r struct {
			Web struct {
				Results []struct {
					Title         string   `json:"title"`
					URL           string   `json:"url"`
					Description   string   `json:"description"`
					ExtraSnippets []string `json:"extra_snippets"`
				} `json:"results"`
			} `json:"web"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		for _, x := range r.Web.Results {
			out = append(out, Result{Title: stripTags(x.Title), URL: x.URL, Snippet: stripTags(x.Description)})
		}
	case "exa":
		var r struct {
			Results []struct {
				Title      string   `json:"title"`
				URL        string   `json:"url"`
				Highlights []string `json:"highlights"`
				Text       string   `json:"text"`
			} `json:"results"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		for _, x := range r.Results {
			snip := strings.Join(x.Highlights, " … ")
			if snip == "" {
				snip = x.Text
			}
			out = append(out, Result{Title: x.Title, URL: x.URL, Snippet: snip})
		}
	case "tavily":
		var r struct {
			Results []struct {
				Title   string `json:"title"`
				URL     string `json:"url"`
				Content string `json:"content"`
			} `json:"results"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		for _, x := range r.Results {
			out = append(out, Result{Title: x.Title, URL: x.URL, Snippet: x.Content})
		}
	}
	clean := out[:0]
	for _, r := range out {
		u, err := url.Parse(r.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		if len(r.Snippet) > 800 {
			r.Snippet = r.Snippet[:800] + "…"
		}
		clean = append(clean, r)
	}
	return clean, nil
}

func statusError(provider string, code int, raw []byte) error {
	name := Label(provider)
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		if provider == "searxng" {
			return fmt.Errorf("%s refused the request (%d). Check that the instance allows API access and has the JSON format enabled", name, code)
		}
		return fmt.Errorf("%s refused the API key (%d). Check the key", name, code)
	case code == http.StatusTooManyRequests:
		return fmt.Errorf("%s says the search limit was reached (429). Try again later or raise the plan's limit", name)
	case code == http.StatusForbidden && provider == "searxng":
		return fmt.Errorf("%s refused JSON output. Add json to search.formats in its settings.yml", name)
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 160 {
		msg = msg[:160] + "…"
	}
	return fmt.Errorf("%s answered %d: %s", name, code, msg)
}

// Label is a provider's display name.
func Label(provider string) string {
	switch provider {
	case "searxng":
		return "SearXNG"
	case "brave":
		return "Brave Search"
	case "exa":
		return "Exa"
	case "tavily":
		return "Tavily"
	}
	return provider
}

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// FormatForModel renders results as the tool result a model reads.
func FormatForModel(query string, results []Result) string {
	if len(results) == 0 {
		return "No results for “" + query + "”."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Web results for “%s” (searched %s). Cite the sources you use as markdown links.\n", query, time.Now().UTC().Format("2006-01-02"))
	for i, r := range results {
		fmt.Fprintf(&b, "\n[%d] %s\n%s\n%s\n", i+1, r.Title, r.URL, r.Snippet)
	}
	return b.String()
}
