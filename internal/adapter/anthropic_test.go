package adapter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestAnthropicPrepareChatTranslation(t *testing.T) {
	a := &Anthropic{}
	up := Upstream{ID: "up-a", BaseURL: "https://api.anthropic.com", APIKey: "sk-ant-test"}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer janus-downstream-token")
	hdr.Set("Content-Type", "application/json")
	req := &Request{
		Method: http.MethodPost,
		Path:   "/v1/chat/completions",
		Header: hdr,
		Model:  "claude-3-haiku",
		Body: []byte(`{"model":"claude-3-haiku","messages":[` +
			`{"role":"system","content":"rule one"},` +
			`{"role":"system","content":"rule two"},` +
			`{"role":"user","content":"hi"}],` +
			`"temperature":0.2,"stop":["END"]}`),
	}

	prep, err := a.Prepare(up, req)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if want := "https://api.anthropic.com/v1/messages"; prep.URL != want {
		t.Fatalf("URL = %q, want %q", prep.URL, want)
	}
	if got := prep.Header.Get("x-api-key"); got != "sk-ant-test" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := prep.Header.Get("anthropic-version"); got != AnthropicVersion {
		t.Fatalf("anthropic-version = %q, want %q", got, AnthropicVersion)
	}
	if got := prep.Header.Get("Authorization"); got != "" {
		t.Fatalf("downstream Authorization must not be forwarded, got %q", got)
	}

	var msg struct {
		Model     string           `json:"model"`
		System    string           `json:"system"`
		Stream    bool             `json:"stream"`
		MaxTokens int              `json:"max_tokens"`
		Temp      float64          `json:"temperature"`
		Stop      []string         `json:"stop_sequences"`
		Messages  []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(prep.Body, &msg); err != nil {
		t.Fatalf("prepared body is not valid JSON: %v", err)
	}
	if msg.Model != "claude-3-haiku" {
		t.Fatalf("model = %q", msg.Model)
	}
	if msg.System != "rule one\n\nrule two" {
		t.Fatalf("system prompts must merge into the top-level field, got %q", msg.System)
	}
	if len(msg.Messages) != 1 || msg.Messages[0]["role"] != "user" {
		t.Fatalf("system messages must leave the messages array: %s", prep.Body)
	}
	if msg.MaxTokens != 4096 {
		t.Fatalf("max_tokens must default to 4096 (Messages API requires it), got %d", msg.MaxTokens)
	}
	if msg.Temp != 0.2 {
		t.Fatalf("temperature = %v", msg.Temp)
	}
	if len(msg.Stop) != 1 || msg.Stop[0] != "END" {
		t.Fatalf("stop must map to stop_sequences: %s", prep.Body)
	}
}

func TestAnthropicPreparePassThrough(t *testing.T) {
	a := &Anthropic{}
	up := Upstream{BaseURL: "https://api.anthropic.com/", APIKey: "sk-ant-test"}
	body := []byte(`{"prompt":"native"}`)
	req := &Request{Method: http.MethodPost, Path: "/v1/complete", Header: http.Header{}, Body: body}

	prep, err := a.Prepare(up, req)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if want := "https://api.anthropic.com/v1/complete"; prep.URL != want {
		t.Fatalf("URL = %q, want %q", prep.URL, want)
	}
	if !bytes.Equal(prep.Body, body) {
		t.Fatal("non-chat bodies must pass through untouched")
	}
}

func TestAnthropicExtractUsage(t *testing.T) {
	a := &Anthropic{}

	u, ok := a.ExtractUsage([]byte(`{"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":3}}`))
	if !ok || u.TokensIn != 10 || u.TokensOut != 20 || u.TokensCached != 3 || u.FinishReason != "end_turn" {
		t.Fatalf("usage = %+v ok=%v", u, ok)
	}

	// message_start shape: usage nested under message.
	u, ok = a.ExtractUsage([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":9,"output_tokens":1}}}`))
	if !ok || u.TokensIn != 9 || u.TokensOut != 1 {
		t.Fatalf("nested message usage = %+v ok=%v", u, ok)
	}

	if _, ok := a.ExtractUsage([]byte(`{}`)); ok {
		t.Fatal("zero usage must not be reported")
	}
}

// TestAnthropicCacheWriteExtraction locks the cache-write token accounting on
// non-streaming Messages responses: the modern usage.cache_creation breakdown
// fills both TTL buckets, the legacy top-level cache_creation_input_tokens
// total maps exclusively to the 5m bucket, a present breakdown wins over the
// legacy total (summing both would double-bill), and responses without any
// cache fields report zero for both — byte-identical to pre-cache-write
// behaviour.
func TestAnthropicCacheWriteExtraction(t *testing.T) {
	a := &Anthropic{}
	tests := []struct {
		name   string
		body   string
		want5m int64
		want1h int64
	}{
		{
			name: "5m only",
			body: `{"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":500,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":600},"cache_read_input_tokens":50}}`,
			want5m: 600, want1h: 0,
		},
		{
			name: "1h only",
			body: `{"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":500,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":400},"cache_read_input_tokens":50}}`,
			want5m: 0, want1h: 400,
		},
		{
			name: "both TTLs",
			body: `{"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":500,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":200,"ephemeral_1h_input_tokens":300},"cache_read_input_tokens":50}}`,
			want5m: 200, want1h: 300,
		},
		{
			name: "legacy total maps to 5m bucket",
			body: `{"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":500,` +
				`"cache_creation_input_tokens":250,"cache_read_input_tokens":50}}`,
			want5m: 250, want1h: 0,
		},
		{
			name: "breakdown present wins over redundant legacy total",
			body: `{"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":500,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":60,"ephemeral_1h_input_tokens":40},` +
				`"cache_creation_input_tokens":100,"cache_read_input_tokens":50}}`,
			want5m: 60, want1h: 40,
		},
		{
			name:   "absent cache fields report zero",
			body:   `{"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":50}}`,
			want5m: 0, want1h: 0,
		},
		{
			name: "message_start shape nests the breakdown under message.usage",
			body: `{"type":"message_start","message":{"usage":{"input_tokens":1000,"output_tokens":1,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":150,"ephemeral_1h_input_tokens":250}}}}`,
			want5m: 150, want1h: 250,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, ok := a.ExtractUsage([]byte(tc.body))
			if !ok {
				t.Fatalf("usage was not reported: %+v", u)
			}
			if u.TokensCacheWrite5m != tc.want5m || u.TokensCacheWrite1h != tc.want1h {
				t.Fatalf("cache writes = (%d, %d), want (%d, %d)",
					u.TokensCacheWrite5m, u.TokensCacheWrite1h, tc.want5m, tc.want1h)
			}
			if u.TokensIn != 1000 || u.TokensOut == 0 {
				t.Fatalf("base token counts disturbed: %+v", u)
			}
		})
	}
}

// TestAnthropicStreamCollectorCacheWrite proves cache-write counts survive a
// streamed response: message_start carries the breakdown, later frames do
// not, and the final usage keeps the counts alongside the delta-reported
// output tokens.
func TestAnthropicStreamCollectorCacheWrite(t *testing.T) {
	c := (&Anthropic{}).NewStreamCollector()
	c.Feed([]byte("event: message_start"))
	c.Feed([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":1000,"output_tokens":0,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":150,"ephemeral_1h_input_tokens":250},"cache_read_input_tokens":50}}}`))
	c.Feed([]byte(`data: {"type":"content_block_delta","delta":{"text":"hello"}}`))
	c.Feed([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":100}}`))
	c.Feed([]byte("data: [DONE]"))

	u := c.Usage()
	if !u.Reported {
		t.Fatal("streamed usage must be reported")
	}
	if u.TokensCacheWrite5m != 150 || u.TokensCacheWrite1h != 250 {
		t.Fatalf("cache writes = (%d, %d), want (150, 250) from message_start", u.TokensCacheWrite5m, u.TokensCacheWrite1h)
	}
	if u.TokensIn != 1000 || u.TokensOut != 100 || u.TokensCached != 50 {
		t.Fatalf("token counts = (%d, %d, %d), want (1000, 100, 50)", u.TokensIn, u.TokensOut, u.TokensCached)
	}
}

// TestAnthropicStreamCollectorLegacyCacheWrite covers older streams where
// message_start reports only the legacy un-broken-down total: it lands in the
// 5m bucket, the TTL legacy cache writes always had.
func TestAnthropicStreamCollectorLegacyCacheWrite(t *testing.T) {
	c := (&Anthropic{}).NewStreamCollector()
	c.Feed([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":80,"output_tokens":1,"cache_creation_input_tokens":40}}}`))
	c.Feed([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`))

	u := c.Usage()
	if u.TokensCacheWrite5m != 40 || u.TokensCacheWrite1h != 0 {
		t.Fatalf("cache writes = (%d, %d), want (40, 0) — legacy total maps to the 5m bucket",
			u.TokensCacheWrite5m, u.TokensCacheWrite1h)
	}
}

func TestAnthropicStreamCollector(t *testing.T) {
	c := (&Anthropic{}).NewStreamCollector()
	c.Feed([]byte("event: message_start"))
	c.Feed([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":25,"output_tokens":1,"cache_read_input_tokens":5}}}`))
	c.Feed([]byte(`data: {"type":"content_block_delta","delta":{"text":"hello"}}`))
	c.Feed([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":72}}`))
	c.Feed([]byte("data: [DONE]"))

	u := c.Usage()
	if !u.Reported {
		t.Fatal("streamed usage must be reported")
	}
	if u.TokensIn != 25 {
		t.Fatalf("TokensIn = %d, want 25 (from message_start)", u.TokensIn)
	}
	if u.TokensOut != 72 {
		t.Fatalf("TokensOut = %d, want 72 (from message_delta)", u.TokensOut)
	}
	if u.TokensCached != 5 {
		t.Fatalf("TokensCached = %d, want 5", u.TokensCached)
	}
	// Anthropic streams carry the finish reason nested under delta; it must
	// land on the usage event (this was silently dropped before).
	if u.FinishReason != "end_turn" {
		t.Fatalf("FinishReason = %q, want end_turn (from message_delta.delta)", u.FinishReason)
	}
}
