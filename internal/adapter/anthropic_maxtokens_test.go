package adapter

import (
	"encoding/json"
	"testing"
)

// With no cap named, a streamed reply from a current Claude may use the
// model's output limit; a non-streamed one and older models keep 4,096;
// a named cap always wins.
func TestAnthropicDefaultOutputCapIsTheModelLimitWhenStreaming(t *testing.T) {
	for _, c := range []struct {
		body string
		want float64
	}{
		{`{"model":"claude-haiku-5-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`, 64000},
		{`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`, 64000},
		{`{"model":"claude-opus-4-1","stream":true,"messages":[{"role":"user","content":"hi"}]}`, 32000},
		{`{"model":"claude-haiku-5-5","stream":false,"messages":[{"role":"user","content":"hi"}]}`, anthropicDefaultMaxTokens},
		{`{"model":"claude-3-5-haiku-latest","stream":true,"messages":[{"role":"user","content":"hi"}]}`, anthropicDefaultMaxTokens},
		{`{"model":"claude-haiku-5-5","stream":true,"max_tokens":300,"messages":[{"role":"user","content":"hi"}]}`, 300},
	} {
		out, err := chatToMessages([]byte(c.body))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(out, &m)
		if m["max_tokens"] != c.want {
			t.Errorf("%s: max_tokens = %v, want %v", c.body, m["max_tokens"], c.want)
		}
	}
}
