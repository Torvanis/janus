package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

// Generic hosted tools map onto Claude's server tools; the reply's server
// tool blocks become activity and citations, and each web search is metered
// at Anthropic's per-search price on top of tokens.
func TestAnthropicServerTools(t *testing.T) {
	out, err := (&Anthropic{}).Prepare(Upstream{BaseURL: "https://api.anthropic.com/v1"}, &Request{Path: "/v1/chat/completions", Model: "claude-haiku-5-5",
		Body: []byte(`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search"},{"type":"web_fetch"},{"type":"code_execution"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Tools []map[string]any `json:"tools"`
	}
	_ = json.Unmarshal(out.Body, &body)
	if len(body.Tools) != 3 || body.Tools[0]["type"] != "web_search_20250305" || body.Tools[1]["type"] != "web_fetch_20250910" || body.Tools[2]["type"] != "code_execution_20250825" {
		t.Fatalf("tools = %v", body.Tools)
	}
	tr := (&Anthropic{}).NewStreamTransformer(&Request{Path: "/v1/chat/completions"}, nil)
	var got strings.Builder
	for _, l := range []string{
		`data: {"type":"message_start","message":{"id":"m","model":"claude","usage":{"input_tokens":10}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"s1","name":"web_search","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"news today\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"s1","content":[{"type":"web_search_result","url":"https://apnews.com/a","title":"AP"}]}}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://apnews.com/a","title":"AP"}}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"Talks resume."}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5,"server_tool_use":{"web_search_requests":2}}}`,
		`data: {"type":"message_stop"}`,
	} {
		b, err := tr.TransformStreamLine([]byte(l))
		if err != nil {
			t.Fatal(err)
		}
		got.Write(b)
	}
	s := got.String()
	for _, want := range []string{`"janus_activity":{"query":"news today","status":"searching","type":"web_search"}`, `"sources":["https://apnews.com/a"]`, `"url_citation":{"end_index":0,"start_index":0,"title":"AP","url":"https://apnews.com/a"}`, `"content":"Talks resume."`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("stream missing %s\n%s", want, s)
		}
	}
	col := (&Anthropic{}).NewStreamCollector()
	col.Feed([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":5,"server_tool_use":{"web_search_requests":2}}}`))
	if u := col.Usage(); u.ToolFeeNano != 2*anthropicWebSearchFeeNano || u.ToolCalls["web_search"] != 2 {
		t.Fatalf("usage = %+v", u)
	}
}
