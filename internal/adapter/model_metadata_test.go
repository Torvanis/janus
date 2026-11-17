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
		{"router unknown TTL", "https://openrouter.ai", `{"pricing":{"prompt":"0","input_cache_write":"0.000003","image":"0.01"}}`, 0, map[string]int64{"rate_in_nanousd": 0, "rate_cache_write_5m_nanousd": 3000000000}, true},
		{"router tiered", "https://openrouter.ai", `{"pricing":{"prompt":"0.000002","completion":"0.00001","overrides":[{"context_length":200000,"prompt":"0.000004"}]}}`, 0, map[string]int64{"rate_in_nanousd": 2000000000, "rate_out_nanousd": 10000000000}, true},
		{"xai tiered", "https://api.x.ai", `{"prompt_text_token_price":20000,"prompt_text_token_price_long_context":40000}`, 0, map[string]int64{"rate_in_nanousd": 2000000000}, true},
		// xAI long prompts: at or above the threshold, every token at the long-context price (0 = standard).
		{"xai long context", "https://api.x.ai", `{"prompt_text_token_price":20000,"completion_text_token_price":60000,"cached_prompt_text_token_price":5000,"prompt_image_token_price":20000,"prompt_text_token_price_long_context":40000,"completion_text_token_price_long_context":120000,"cached_prompt_text_token_price_long_context":0,"long_context_threshold":200000}`, 0,
			map[string]int64{"rate_in_nanousd": 2000000000, "rate_out_nanousd": 6000000000, "rate_cached_nanousd": 500000000, "rate_in_nanousd@prompt_over=199999": 4000000000, "rate_out_nanousd@prompt_over=199999": 12000000000}, false},
		// xAI images: ticks (1e-10 USD); the default tier and one line per quality and resolution.
		{"xai images", "https://api.x.ai", `{"image_price":600000000,"pricing":[{"quality":"low","resolution":"1k","price_per_image":400000000},{"quality":"High","resolution":"1.5k","price_per_image":900000000}]}`, 0,
			map[string]int64{"price_image_nanousd": 60000000, "price_image_nanousd@quality=low,resolution=1k": 40000000, "price_image_nanousd@quality=high,resolution=1.5k": 90000000}, false},
		{"xai image token price differs", "https://api.x.ai", `{"prompt_text_token_price":20000,"prompt_image_token_price":30000}`, 0, map[string]int64{"rate_in_nanousd": 2000000000}, true},
		// OpenRouter long prompts: an override on min_prompt_tokens alone becomes long-prompt lines.
		{"router long prompt", "https://openrouter.ai/api", `{"pricing":{"prompt":"0.000001","completion":"0.000005","internal_reasoning":"0.000005","overrides":[{"min_prompt_tokens":272000,"prompt":"0.000002","completion":"0.0000075","input_cache_read":"0.0000001"}]}}`, 0,
			map[string]int64{"rate_in_nanousd": 1000000000, "rate_out_nanousd": 5000000000, "rate_in_nanousd@prompt_over=272000": 2000000000, "rate_out_nanousd@prompt_over=272000": 7500000000, "rate_cached_nanousd@prompt_over=272000": 100000000}, false},
		{"router time of day", "https://openrouter.ai/api", `{"pricing":{"prompt":"0.00000015","completion":"0.0000006","overrides":[{"utc_start":1500,"utc_end":100,"prompt":"0.000000075"}]}}`, 0,
			map[string]int64{"rate_in_nanousd": 150000000, "rate_out_nanousd": 600000000}, true},
		{"router per request", "https://openrouter.ai/api", `{"pricing":{"prompt":"0","completion":"0","request":"0.005"}}`, 0,
			map[string]int64{"rate_in_nanousd": 0, "rate_out_nanousd": 0, "price_request_nanousd": 5000000}, false},
		{"router auto", "https://openrouter.ai/api", `{"pricing":{"prompt":"-1","completion":"-1"}}`, 0, map[string]int64{}, true},
		{"together", "https://api.together.xyz", `{"pricing":{"input":0.88,"output":0.88,"hourly":0,"base":0,"finetune":0}}`, 0, map[string]int64{"rate_in_nanousd": 880000000, "rate_out_nanousd": 880000000}, false},
		{"cerebras public list", "https://api.cerebras.ai", `{"pricing":{"prompt":"0.00000035","completion":"0.00000075"}}`, 0, map[string]int64{"rate_in_nanousd": 350000000, "rate_out_nanousd": 750000000}, false},
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

// Together lists models as a bare array.
func TestTogetherArrayModelList(t *testing.T) {
	m := discoverMetadata(t, "https://api.together.xyz", `[{"id":"meta-llama/Llama-3.3-70B","context_length":131072,"pricing":{"input":0.88,"output":0.88}}]`)
	if len(m) != 1 || m[0].Name != "meta-llama/Llama-3.3-70B" || m[0].ContextWindow != 131072 || m[0].Rates["rate_in_nanousd"] != 880000000 {
		t.Fatalf("%+v", m)
	}
}
