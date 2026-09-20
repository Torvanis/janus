package adapter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type metadataTransport func(*http.Request) (*http.Response, error)

func (f metadataTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func discoverMetadata(t *testing.T, host, body string) []DiscoveredModel {
	t.Helper()
	a, _ := Get("openai_compatible")
	models, err := a.Discover(context.Background(), Upstream{BaseURL: host}, &http.Client{Transport: metadataTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return models
}
func TestProviderMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, host, body string
		context          int64
		rates            map[string]int64
		warnings         bool
	}{
		{"xai units", "https://api.x.ai", `{"context_length":131072,"prompt_text_token_price":20000,"cached_prompt_text_token_price":0,"completion_text_token_price":50000}`, 131072, map[string]int64{"rate_in_nanousd": 2000000000, "rate_cached_nanousd": 0, "rate_out_nanousd": 5000000000}, false},
		{"router units", "https://openrouter.ai/api", `{"context_length":32000,"pricing":{"prompt":"0.000002","completion":"0","input_cache_read":"0.0000002","input_cache_write_1h":"0.000003"}}`, 32000, map[string]int64{"rate_in_nanousd": 2000000000, "rate_out_nanousd": 0, "rate_cached_nanousd": 200000000, "rate_cache_write_1h_nanousd": 3000000000}, false},
		{"router unknown TTL", "https://openrouter.ai", `{"pricing":{"prompt":"0","input_cache_write":"0.000003","image":"0.01"}}`, 0, map[string]int64{"rate_in_nanousd": 0}, true},
		{"router tiered", "https://openrouter.ai", `{"pricing":{"prompt":"0.000002","completion":"0.00001","overrides":[{"context_length":200000,"prompt":"0.000004"}]}}`, 0, map[string]int64{}, true},
		{"xai tiered", "https://api.x.ai", `{"prompt_text_token_price":20000,"prompt_text_token_price_long_context":40000}`, 0, map[string]int64{}, true},
		{"non-decimal", "https://openrouter.ai", `{"pricing":{"prompt":"1/2","completion":"0x1p2"}}`, 0, map[string]int64{}, true},
		{"nano rounding", "https://openrouter.ai", `{"pricing":{"prompt":"0.0000000000000006","completion":"0.0000000000000004"}}`, 0, map[string]int64{"rate_in_nanousd": 1, "rate_out_nanousd": 0}, false},
		{"invalid", "https://openrouter.ai", `{"pricing":{"prompt":"-1","completion":"garbage","input_cache_read":"1e40"}}`, 0, map[string]int64{}, true},
		{"untrusted provider", "https://openrouter.ai.example.org", `{"context_length":100,"pricing":{"prompt":"1"},"prompt_text_token_price":20000}`, 100, map[string]int64{}, false},
		{"missing", "https://api.x.ai", `{}`, 0, map[string]int64{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var row map[string]any
			_ = json.Unmarshal([]byte(tc.body), &row)
			row["id"] = "test"
			body, _ := json.Marshal(map[string]any{"data": []any{row}})
			m := discoverMetadata(t, tc.host, string(body))[0]
			if m.ContextWindow != tc.context {
				t.Fatalf("context=%d want %d", m.ContextWindow, tc.context)
			}
			if len(m.Rates) != len(tc.rates) {
				t.Fatalf("rates=%v want %v", m.Rates, tc.rates)
			}
			for k, v := range tc.rates {
				if got, ok := m.Rates[k]; !ok || got != v {
					t.Errorf("%s=%d (%v), want %d", k, got, ok, v)
				}
			}
			if (len(m.MetadataWarnings) > 0) != tc.warnings {
				t.Errorf("warnings=%v", m.MetadataWarnings)
			}
		})
	}
}
func TestOpenRouterLiveParserSmoke(t *testing.T) {
	path := os.Getenv("JANUS_TEST_OPENROUTER_FIXTURE")
	if path == "" {
		t.Skip("optional captured public fixture")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ms := discoverMetadata(t, "https://openrouter.ai/api", string(b))
	if len(ms) != 435 {
		t.Fatalf("rows=%d", len(ms))
	}
	priced, warned := 0, 0
	for _, m := range ms {
		if len(m.Rates) > 0 {
			priced++
		}
		if len(m.MetadataWarnings) > 0 {
			warned++
		}
	}
	if priced == 0 || warned == 0 {
		t.Fatalf("priced=%d warned=%d", priced, warned)
	}
	t.Logf("parsed %d rows: %d with supported prices, %d flagged", len(ms), priced, warned)
}
