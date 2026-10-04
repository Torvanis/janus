package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeGemini serves the two lists Gemini discovery reads, shaped like the
// real API (captured 2026-10-04): the OpenAI-compatible list names models
// "models/<id>" with no context window; the native list carries
// inputTokenLimit and supportedGenerationMethods and wants x-goog-api-key.
func fakeGemini(t *testing.T, nativeStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1beta/openai/models":
			if r.Header.Get("Authorization") != "Bearer gk" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"object":"list","data":[
				{"id":"models/gemini-2.5-flash","object":"model","owned_by":"google","display_name":"Gemini 2.5 Flash"},
				{"id":"models/gemini-embedding-2","object":"model","owned_by":"google"},
				{"id":"models/veo-3.1-generate-preview","object":"model","owned_by":"google"},
				{"id":"models/gemini-3.8-live","object":"model","owned_by":"google"},
				{"id":"models/aqa","object":"model","owned_by":"google"},
				{"id":"models/lyria-3.5","object":"model","owned_by":"google"},
				{"id":"models/brand-new-model","object":"model","owned_by":"google"}]}`))
		case "/v1beta/models":
			if r.Header.Get("x-goog-api-key") != "gk" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if nativeStatus != http.StatusOK {
				w.WriteHeader(nativeStatus)
				return
			}
			_, _ = w.Write([]byte(`{"models":[
				{"name":"models/gemini-2.5-flash","inputTokenLimit":1048576,"supportedGenerationMethods":["generateContent","countTokens","createCachedContent"]},
				{"name":"models/gemini-embedding-2","inputTokenLimit":8192,"supportedGenerationMethods":["embedContent","countTokens"]},
				{"name":"models/veo-3.1-generate-preview","inputTokenLimit":480,"supportedGenerationMethods":["predictLongRunning"]},
				{"name":"models/gemini-3.8-live","inputTokenLimit":131072,"supportedGenerationMethods":["bidiGenerateContent"]},
				{"name":"models/aqa","inputTokenLimit":7168,"supportedGenerationMethods":["generateAnswer"]},
				{"name":"models/lyria-3.5","inputTokenLimit":1048576,"supportedGenerationMethods":["generateContent","countTokens"]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestGeminiDiscoverEnrichesAndFilters(t *testing.T) {
	srv := fakeGemini(t, http.StatusOK)
	defer srv.Close()
	a, err := Get("gemini")
	if err != nil {
		t.Fatal(err)
	}
	models, err := a.Discover(context.Background(), Upstream{Name: "g", BaseURL: srv.URL + "/v1beta/openai", APIKey: "gk"}, srv.Client())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	got := map[string]DiscoveredModel{}
	for _, m := range models {
		got[m.Name] = m
	}
	// Live-only and AQA models cannot be called through the OpenAI endpoint.
	for _, gone := range []string{"gemini-3.8-live", "aqa", "models/gemini-3.8-live"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s should not be offered: it has no OpenAI-compatible method", gone)
		}
	}
	cases := []struct {
		name     string
		window   int64
		modality string
	}{
		{"gemini-2.5-flash", 1048576, ModalityChat},
		{"gemini-embedding-2", 8192, ModalityEmbedding},
		{"veo-3.1-generate-preview", 480, ModalityVideo},
		{"lyria-3.5", 1048576, ModalityAudio},
		// A model only the OpenAI list knows is kept, with the name-based guess.
		{"brand-new-model", 0, ModalityChat},
	}
	for _, c := range cases {
		m, ok := got[c.name]
		if !ok {
			t.Errorf("%s missing (names must have the models/ prefix removed): %v", c.name, models)
			continue
		}
		if m.ContextWindow != c.window {
			t.Errorf("%s context window = %d, want %d", c.name, m.ContextWindow, c.window)
		}
		if len(m.Modalities) != 1 || m.Modalities[0] != c.modality {
			t.Errorf("%s modalities = %v, want [%s]", c.name, m.Modalities, c.modality)
		}
		if len(m.MetadataWarnings) != 0 {
			t.Errorf("%s warnings = %v", c.name, m.MetadataWarnings)
		}
	}
	if len(models) != len(cases) {
		t.Errorf("got %d models, want %d", len(models), len(cases))
	}
}

func TestGeminiDiscoverSurvivesNativeListFailure(t *testing.T) {
	srv := fakeGemini(t, http.StatusInternalServerError)
	defer srv.Close()
	a, _ := Get("gemini")
	models, err := a.Discover(context.Background(), Upstream{Name: "g", BaseURL: srv.URL + "/v1beta/openai", APIKey: "gk"}, srv.Client())
	if err != nil {
		t.Fatalf("a failing detail list must not fail discovery: %v", err)
	}
	if len(models) != 7 {
		t.Fatalf("got %d models, want all 7 from the OpenAI list", len(models))
	}
	for _, m := range models {
		if strings.HasPrefix(m.Name, "models/") {
			t.Errorf("name %q keeps the models/ prefix", m.Name)
		}
		if len(m.MetadataWarnings) == 0 || !strings.Contains(m.MetadataWarnings[0], "Gemini model details unavailable") {
			t.Errorf("%s: want a warning naming the missing details, got %v", m.Name, m.MetadataWarnings)
		}
	}
}

func TestGeminiPrepareTargetsOpenAIEndpoint(t *testing.T) {
	a, _ := Get("gemini")
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Authorization", "Bearer client-token")
	for _, base := range []string{
		"https://generativelanguage.googleapis.com",
		"https://generativelanguage.googleapis.com/",
		"https://generativelanguage.googleapis.com/v1beta/openai",
		"https://generativelanguage.googleapis.com/v1beta/openai/",
	} {
		prep, err := a.Prepare(Upstream{BaseURL: base, APIKey: "gk"}, &Request{
			Method: http.MethodPost, Path: "/v1/chat/completions", Header: hdr, Streaming: true, Model: "gemini-2.5-flash",
			Body: []byte(`{"model":"gemini-2.5-flash","stream":true,"messages":[]}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if want := "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"; prep.URL != want {
			t.Errorf("base %q -> %s, want %s", base, prep.URL, want)
		}
		if prep.Header.Get("Authorization") != "Bearer gk" {
			t.Errorf("base %q: the client's token must be replaced by the Gemini key", base)
		}
		if !strings.Contains(string(prep.Body), `"include_usage":true`) {
			t.Errorf("streaming request must ask for usage: %s", prep.Body)
		}
	}
}

// Real response bodies from gemini-2.5-flash (2026-10-04). completion_tokens
// leaves out thinking, total_tokens includes it; Google bills it as output.
func TestGeminiUsageCountsThinkingAsOutput(t *testing.T) {
	a, _ := Get("gemini")
	cases := []struct {
		body    string
		in, out int64
	}{
		{`{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"391","role":"assistant"}}],"usage":{"completion_tokens":25,"prompt_tokens":11,"total_tokens":727}}`, 11, 716},
		// reasoning_effort "none": no thinking, total = prompt + completion.
		{`{"choices":[{"finish_reason":"stop"}],"usage":{"completion_tokens":35,"prompt_tokens":11,"total_tokens":46}}`, 11, 35},
	}
	for _, c := range cases {
		u, ok := a.ExtractUsage([]byte(c.body))
		if !ok || u.TokensIn != c.in || u.TokensOut != c.out || u.FinishReason != "stop" {
			t.Errorf("usage = %+v ok=%v, want in=%d out=%d", u, ok, c.in, c.out)
		}
		if u.HiddenReasoning != (c.out != 35) {
			t.Errorf("HiddenReasoning = %v for out=%d: set only when thinking was added", u.HiddenReasoning, c.out)
		}
	}
	// Implicit prompt-cache hit (real body): cached tokens are a subset of
	// the prompt, as with OpenAI, and priced at the cached rate.
	u, _ := a.ExtractUsage([]byte(`{"choices":[{"finish_reason":"stop"}],"usage":{"completion_tokens":19,"prompt_tokens":4634,"prompt_tokens_details":{"cached_tokens":4088},"total_tokens":4653}}`))
	if u.TokensIn != 4634 || u.TokensCached != 4088 || u.CachedDisjoint || u.TokensOut != 19 {
		t.Errorf("cache hit usage = %+v, want in=4634 cached=4088 (subset) out=19", u)
	}
	col := a.NewStreamCollector()
	for _, line := range []string{
		`data: {"choices":[{"delta":{"content":"No","role":"assistant"},"index":0}],"model":"gemini-2.5-flash","object":"chat.completion.chunk"}`,
		`data: {"choices":[{"delta":{"content":", 391 = 17 × 23."},"finish_reason":"stop","index":0}],"object":"chat.completion.chunk","usage":{"completion_tokens":24,"prompt_tokens":11,"total_tokens":561}}`,
		`data: [DONE]`,
	} {
		col.Feed([]byte(line))
	}
	if u := col.Usage(); u.TokensIn != 11 || u.TokensOut != 550 || !u.Reported || u.FinishReason != "stop" || !u.HiddenReasoning {
		t.Errorf("stream usage = %+v, want in=11 out=550 (24 answer + 526 thinking)", u)
	}
}

func TestGeminiIsPricedFromGoogleReference(t *testing.T) {
	if got := MetadataProvider("gemini", "https://generativelanguage.googleapis.com/v1beta/openai"); got != "google" {
		t.Errorf("provider = %q, want google", got)
	}
	// A generic OpenAI-compatible upstream pointed at a Gemini-shaped host is
	// not trusted to be Google: provider identity comes from the adapter.
	if got := MetadataProvider("openai_compatible", "https://generativelanguage.googleapis.com/v1beta/openai"); got != "" {
		t.Errorf("openai_compatible on the Gemini host = %q, want no provider", got)
	}
}

// TestLiveGemini runs the adapter against the real Gemini API:
//
//	JANUS_LIVE_GEMINI_KEY_FILE=/path/to/gemini-api-key go test -run TestLiveGemini ./internal/adapter
func TestLiveGemini(t *testing.T) {
	path := os.Getenv("JANUS_LIVE_GEMINI_KEY_FILE")
	if path == "" {
		t.Skip("set JANUS_LIVE_GEMINI_KEY_FILE to run against the real Gemini API")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	up := Upstream{Name: "live", BaseURL: "https://generativelanguage.googleapis.com", APIKey: strings.TrimSpace(string(raw))}
	a, _ := Get("gemini")
	models, err := a.Discover(context.Background(), up, http.DefaultClient)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var flash *DiscoveredModel
	for i := range models {
		if strings.HasPrefix(models[i].Name, "models/") {
			t.Errorf("%s keeps the models/ prefix", models[i].Name)
		}
		if models[i].Name == "gemini-2.5-flash" {
			flash = &models[i]
		}
	}
	if flash == nil || flash.ContextWindow < 1000000 {
		t.Fatalf("gemini-2.5-flash missing or without its 1M context window: %+v", flash)
	}
	t.Logf("discovered %d models; gemini-2.5-flash context=%d warnings=%v", len(models), flash.ContextWindow, flash.MetadataWarnings)

	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	body := []byte(`{"model":"gemini-2.5-flash","stream":true,"reasoning_effort":"low","messages":[{"role":"user","content":"Is 391 prime? One sentence."}]}`)
	prep, err := a.Prepare(up, &Request{Method: http.MethodPost, Path: "/v1/chat/completions", Header: hdr, Body: body, Streaming: true, Model: "gemini-2.5-flash"})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, prep.URL, strings.NewReader(string(prep.Body)))
	req.Header = prep.Header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("Gemini answered HTTP %d (free-tier rate limit?); discovery passed", resp.StatusCode)
	}
	col := a.NewStreamCollector()
	var text strings.Builder
	dec := make([]byte, 0, 64<<10)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		dec = append(dec, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	for _, line := range strings.Split(string(dec), "\n") {
		col.Feed([]byte(line))
		if p, ok := sseData([]byte(line)); ok {
			var f struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal(p, &f) == nil && len(f.Choices) > 0 {
				text.WriteString(f.Choices[0].Delta.Content)
			}
		}
	}
	u := col.Usage()
	t.Logf("answer=%q usage in=%d out=%d finish=%s", text.String(), u.TokensIn, u.TokensOut, u.FinishReason)
	if !u.Reported || u.TokensIn == 0 || u.TokensOut == 0 || text.Len() == 0 {
		t.Fatalf("streamed call metered nothing: %+v", u)
	}
}
