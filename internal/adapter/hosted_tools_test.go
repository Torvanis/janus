package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

// Hosted tools pass through to the Responses request as written, and the
// provider's search activity and citations come back as chat deltas; xAI's
// cost ticks become the exact metered cost.
func TestHostedToolsThroughResponses(t *testing.T) {
	body := []byte(`{"model":"grok-4.6","stream":true,"messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search","filters":{"allowed_domains":["apnews.com"]}},{"type":"x_search"}]}`)
	out, err := chatToPublicResponses(body, "grok-4.6")
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Tools []map[string]any `json:"tools"`
	}
	_ = json.Unmarshal(out, &req)
	if len(req.Tools) != 2 || req.Tools[0]["type"] != "web_search" || req.Tools[0]["filters"] == nil || req.Tools[1]["type"] != "x_search" {
		t.Fatalf("tools = %v", req.Tools)
	}
	tr := newCodexTranslator("grok-4.6")
	var deltas []map[string]any
	for _, ev := range []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","action":{"type":"search","queries":["a","b"],"sources":[{"url":"https://x.test/1"}]}}}`,
		`{"type":"response.output_text.annotation.added","annotation":{"type":"url_citation","url":"https://x.test/1","title":"One"}}`,
	} {
		var e codexEvent
		if err := json.Unmarshal([]byte(ev), &e); err != nil {
			t.Fatal(err)
		}
		deltas = append(deltas, tr.event(e)...)
	}
	b, _ := json.Marshal(deltas)
	for _, want := range []string{`"status":"searching"`, `"query":"a · b"`, `"sources":["https://x.test/1"]`, `"url_citation":{"end_index":0,"start_index":0,"title":"One","url":"https://x.test/1"}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("deltas missing %s: %s", want, b)
		}
	}
	ticks := int64(576780000)
	u := (&codexUsage{InputTokens: 10, OutputTokens: 5, CostInUSDTicks: &ticks}).usage()
	if !u.CostReported || u.CostNano != 57678000 {
		t.Fatalf("usage = %+v", u)
	}
	if got := HostedToolSupport("https://api.openai.com/v1"); !got["web_search"] || got["x_search"] {
		t.Fatalf("openai support = %v", got)
	}
	if got := HostedToolSupport("http://vllm.svc:8000/v1"); got != nil {
		t.Fatalf("self-hosted support = %v", got)
	}
}
