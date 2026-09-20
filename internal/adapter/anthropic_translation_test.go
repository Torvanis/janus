package adapter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// translate runs chatToMessages and decodes the result for assertions.
func translate(t *testing.T, body string) map[string]any {
	t.Helper()
	out, err := chatToMessages([]byte(body))
	if err != nil {
		t.Fatalf("chatToMessages: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("decode translated body: %v\n%s", err, out)
	}
	return m
}

func chatReq(model string) *Request {
	return &Request{Method: http.MethodPost, Path: "/v1/chat/completions", Model: model, Header: http.Header{}}
}

func TestAnthropicToolsRoundTrip(t *testing.T) {
	m := translate(t, `{"model":"claude","messages":[{"role":"user","content":"weather?"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"Look up weather",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}},
			{"type":"function","function":{"name":"noop"}}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}},
		"parallel_tool_calls":false}`)

	tools := m["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v", tools)
	}
	first := tools[0].(map[string]any)
	if first["name"] != "get_weather" || first["description"] != "Look up weather" {
		t.Fatalf("tool[0] = %v", first)
	}
	schema := first["input_schema"].(map[string]any)
	if schema["type"] != "object" || schema["properties"].(map[string]any)["city"] == nil {
		t.Fatalf("input_schema = %v", schema)
	}
	if _, leaked := first["function"]; leaked {
		t.Fatalf("OpenAI function wrapper leaked into Anthropic tool: %v", first)
	}
	// A tool without parameters still gets the required input_schema.
	second := tools[1].(map[string]any)
	if second["input_schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("tool[1] missing default input_schema: %v", second)
	}
	choice := m["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "get_weather" || choice["disable_parallel_tool_use"] != true {
		t.Fatalf("tool_choice = %v", choice)
	}
}

func TestAnthropicToolChoiceModes(t *testing.T) {
	cases := map[string]string{
		`"auto"`:     "auto",
		`"none"`:     "none",
		`"required"`: "any",
	}
	for raw, want := range cases {
		m := translate(t, `{"model":"c","messages":[{"role":"user","content":"x"}],
			"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":`+raw+`}`)
		if got := m["tool_choice"].(map[string]any)["type"]; got != want {
			t.Errorf("tool_choice %s → %v, want %s", raw, got, want)
		}
	}
	if _, err := chatToMessages([]byte(`{"model":"c","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"bogus"}`)); err == nil {
		t.Fatal("unsupported tool_choice must fail loudly")
	}
}

func TestAnthropicMaxCompletionTokensPreserved(t *testing.T) {
	m := translate(t, `{"model":"c","messages":[{"role":"user","content":"x"}],"max_completion_tokens":777}`)
	if got := m["max_tokens"]; got != float64(777) {
		t.Fatalf("max_tokens = %v, want 777 (from max_completion_tokens)", got)
	}
	// max_completion_tokens supersedes the deprecated max_tokens.
	m = translate(t, `{"model":"c","messages":[{"role":"user","content":"x"}],"max_tokens":10,"max_completion_tokens":20}`)
	if got := m["max_tokens"]; got != float64(20) {
		t.Fatalf("max_tokens = %v, want 20", got)
	}
	m = translate(t, `{"model":"c","messages":[{"role":"user","content":"x"}]}`)
	if got := m["max_tokens"]; got != float64(anthropicDefaultMaxTokens) {
		t.Fatalf("default max_tokens = %v", got)
	}
}

func TestAnthropicScalarFieldTranslation(t *testing.T) {
	m := translate(t, `{"model":"c","messages":[{"role":"user","content":"x"}],
		"stop":"END","user":"user-42","top_p":0.9,"top_k":5,"seed":7,"presence_penalty":0.5,
		"frequency_penalty":0.5,"stream":true,"stream_options":{"include_usage":true},"logprobs":true}`)
	if got := m["stop_sequences"].([]any); len(got) != 1 || got[0] != "END" {
		t.Fatalf("stop_sequences = %v", got)
	}
	if got := m["metadata"].(map[string]any)["user_id"]; got != "user-42" {
		t.Fatalf("metadata.user_id = %v", got)
	}
	if m["top_p"] != 0.9 || m["top_k"] != float64(5) || m["stream"] != true {
		t.Fatalf("scalars = top_p %v top_k %v stream %v", m["top_p"], m["top_k"], m["stream"])
	}
	for _, unsupported := range []string{"seed", "presence_penalty", "frequency_penalty", "stream_options", "logprobs", "user", "max_completion_tokens"} {
		if _, leaked := m[unsupported]; leaked {
			t.Errorf("OpenAI-only field %q leaked into the Messages request", unsupported)
		}
	}
	if _, err := chatToMessages([]byte(`{"model":"c","messages":[],"n":2}`)); err == nil {
		t.Fatal("n>1 must fail loudly")
	}
}

func TestAnthropicResponseFormatBecomesSystemInstruction(t *testing.T) {
	m := translate(t, `{"model":"c","messages":[{"role":"system","content":"be terse"},{"role":"user","content":"x"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}}}`)
	sys := m["system"].(string)
	if !strings.HasPrefix(sys, "be terse") || !strings.Contains(sys, "JSON Schema") || !strings.Contains(sys, `"ok"`) {
		t.Fatalf("system = %q", sys)
	}
	if _, leaked := m["response_format"]; leaked {
		t.Fatal("response_format leaked")
	}
	m = translate(t, `{"model":"c","messages":[{"role":"user","content":"x"}],"response_format":{"type":"text"}}`)
	if _, has := m["system"]; has {
		t.Fatalf("text response_format must add nothing, got system=%v", m["system"])
	}
}

func TestAnthropicImageURLBecomesImageBlock(t *testing.T) {
	m := translate(t, `{"model":"c","messages":[{"role":"user","content":[
		{"type":"text","text":"what is this?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo=","detail":"high"}},
		{"type":"image_url","image_url":{"url":"https://example.com/cat.jpg"}}]}]}`)
	content := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %v", content)
	}
	if content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("content[0] = %v", content[0])
	}
	img := content[1].(map[string]any)
	src := img["source"].(map[string]any)
	if img["type"] != "image" || src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "iVBORw0KGgo=" {
		t.Fatalf("data-URL image block = %v", img)
	}
	remote := content[2].(map[string]any)["source"].(map[string]any)
	if remote["type"] != "url" || remote["url"] != "https://example.com/cat.jpg" {
		t.Fatalf("url image block = %v", remote)
	}
	if _, err := chatToMessages([]byte(`{"model":"c","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`)); err == nil {
		t.Fatal("audio input must fail loudly")
	}
}

func TestAnthropicToolMessagesBecomeToolResults(t *testing.T) {
	m := translate(t, `{"model":"c","messages":[
		{"role":"user","content":"weather in Paris and Rome"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},
			{"id":"call_2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Rome\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"18C"},
		{"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"24C"}]},
		{"role":"user","content":"thanks"}]}`)
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected user/assistant/user turns, got %d: %v", len(msgs), msgs)
	}
	assistant := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("turn 1 = %v", assistant)
	}
	blocks := assistant["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("assistant blocks = %v", blocks)
	}
	use := blocks[0].(map[string]any)
	if use["type"] != "tool_use" || use["id"] != "call_1" || use["name"] != "get_weather" {
		t.Fatalf("tool_use = %v", use)
	}
	if use["input"].(map[string]any)["city"] != "Paris" {
		t.Fatalf("tool_use input must be the decoded object, got %v", use["input"])
	}
	// Both tool results AND the following user text merge into ONE user turn:
	// the Messages API requires tool_result blocks in the single user turn
	// that follows the tool_use.
	user := msgs[2].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("turn 2 = %v", user)
	}
	results := user["content"].([]any)
	if len(results) != 3 {
		t.Fatalf("merged user turn = %v", results)
	}
	r1 := results[0].(map[string]any)
	if r1["type"] != "tool_result" || r1["tool_use_id"] != "call_1" || r1["content"] != "18C" {
		t.Fatalf("tool_result[0] = %v", r1)
	}
	r2 := results[1].(map[string]any)
	if r2["tool_use_id"] != "call_2" || r2["content"].([]any)[0].(map[string]any)["text"] != "24C" {
		t.Fatalf("tool_result[1] = %v", r2)
	}
	if results[2].(map[string]any)["text"] != "thanks" {
		t.Fatalf("trailing user text = %v", results[2])
	}
	for _, msg := range msgs {
		if msg.(map[string]any)["role"] == "tool" {
			t.Fatal("role:tool leaked into the Messages request")
		}
	}
	if _, err := chatToMessages([]byte(`{"model":"c","messages":[{"role":"assistant","tool_calls":[{"id":"x","function":{"name":"f","arguments":"not json"}}]}]}`)); err == nil {
		t.Fatal("malformed tool_call arguments must fail loudly")
	}
}

func TestAnthropicSystemAsArrayLifts(t *testing.T) {
	m := translate(t, `{"model":"c","messages":[
		{"role":"system","content":[{"type":"text","text":"rule one","cache_control":{"type":"ephemeral"}},{"type":"text","text":"rule two"}]},
		{"role":"developer","content":"rule three"},
		{"role":"user","content":"hi"}]}`)
	sys, ok := m["system"].([]any)
	if !ok || len(sys) != 3 {
		t.Fatalf("system = %v", m["system"])
	}
	first := sys[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "rule one" || first["cache_control"] == nil {
		t.Fatalf("system[0] = %v", first)
	}
	if sys[2].(map[string]any)["text"] != "rule three" {
		t.Fatalf("developer prompt not lifted: %v", sys[2])
	}
	if got := len(m["messages"].([]any)); got != 1 {
		t.Fatalf("system turns must not remain in messages: %v", m["messages"])
	}
}

func TestAnthropicTransformResponseWrapsMessage(t *testing.T) {
	a := &Anthropic{now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	body := []byte(`{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-5",
		"content":[{"type":"thinking","thinking":"let me see","signature":"sig"},{"type":"text","text":"It is "},{"type":"text","text":"sunny."},
			{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{"city":"Paris"}}],
		"stop_reason":"tool_use","stop_sequence":null,
		"usage":{"input_tokens":25,"output_tokens":9,"cache_read_input_tokens":5}}`)
	out, err := a.TransformResponse(chatReq("claude-sonnet-4-5"), body)
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}
	var resp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
			Details          struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if resp.Object != "chat.completion" || resp.ID != "msg_01" || resp.Model != "claude-sonnet-4-5" || resp.Created != 1_700_000_000 {
		t.Fatalf("envelope = %+v", resp)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %+v", resp.Choices)
	}
	c := resp.Choices[0]
	if c.Message.Role != "assistant" || c.Message.Content != "It is sunny." || c.Message.ReasoningContent != "let me see" {
		t.Fatalf("message = %+v", c.Message)
	}
	if c.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", c.FinishReason)
	}
	if len(c.Message.ToolCalls) != 1 || c.Message.ToolCalls[0].ID != "toolu_01" || c.Message.ToolCalls[0].Type != "function" ||
		c.Message.ToolCalls[0].Function.Name != "get_weather" || c.Message.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("tool_calls = %+v", c.Message.ToolCalls)
	}
	if resp.Usage.PromptTokens != 25 || resp.Usage.CompletionTokens != 9 || resp.Usage.TotalTokens != 34 || resp.Usage.Details.Cached != 5 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	var raw map[string]any
	_ = json.Unmarshal(out, &raw)
	for _, native := range []string{"stop_reason", "content", "type"} {
		if _, leaked := raw[native]; leaked {
			t.Errorf("Anthropic field %q leaked into chat.completion", native)
		}
	}
}

func TestAnthropicTransformResponseFinishReasons(t *testing.T) {
	a := &Anthropic{}
	for stop, want := range map[string]string{"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length", "refusal": "content_filter"} {
		out, err := a.TransformResponse(chatReq("c"), []byte(`{"id":"m","type":"message","content":[{"type":"text","text":"x"}],"stop_reason":"`+stop+`","usage":{"input_tokens":1,"output_tokens":1}}`))
		if err != nil {
			t.Fatal(err)
		}
		var resp struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Message      struct {
					Content *string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(out, &resp)
		if resp.Choices[0].FinishReason != want {
			t.Errorf("%s → %q, want %q", stop, resp.Choices[0].FinishReason, want)
		}
	}
	// Tool-only replies carry content:null, as OpenAI does.
	out, _ := a.TransformResponse(chatReq("c"), []byte(`{"id":"m","type":"message","content":[{"type":"tool_use","id":"t","name":"f","input":{}}],"stop_reason":"tool_use","usage":{}}`))
	if !bytes.Contains(out, []byte(`"content":null`)) {
		t.Fatalf("tool-only reply should have null content: %s", out)
	}
}

func TestAnthropicTransformResponseLeavesErrorsAndNativePathsAlone(t *testing.T) {
	a := &Anthropic{}
	errBody := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	out, err := a.TransformResponse(chatReq("c"), errBody)
	if err != nil || !bytes.Equal(out, errBody) {
		t.Fatalf("error body must pass through: %s (%v)", out, err)
	}
	native := []byte(`{"id":"msg_1","type":"message","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	out, err = a.TransformResponse(&Request{Path: "/v1/messages"}, native)
	if err != nil || !bytes.Equal(out, native) {
		t.Fatalf("native /v1/messages callers must get the Messages shape back: %s", out)
	}
	if _, ok := a.NewStreamTransformer(&Request{Path: "/v1/messages"}, nil).(PassthroughStream); !ok {
		t.Fatal("native path stream must be a pass-through")
	}
}

// collectChunks feeds SSE lines through the transformer and returns the
// decoded chat.completion.chunk frames plus whether [DONE] was emitted.
func collectChunks(t *testing.T, tr StreamTransformer, stream string) ([]map[string]any, bool, string) {
	t.Helper()
	var out bytes.Buffer
	for _, line := range strings.SplitAfter(stream, "\n") {
		if line == "" {
			continue
		}
		b, err := tr.TransformStreamLine([]byte(line))
		if err != nil {
			t.Fatalf("TransformStreamLine(%q): %v", line, err)
		}
		out.Write(b)
	}
	raw := out.String()
	var chunks []map[string]any
	done := false
	for _, frame := range strings.Split(raw, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("non-data frame emitted: %q", frame)
		}
		payload := strings.TrimPrefix(frame, "data: ")
		if payload == "[DONE]" {
			done = true
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("chunk is not JSON: %q: %v", payload, err)
		}
		chunks = append(chunks, m)
	}
	return chunks, done, raw
}

const anthropicSSEFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","stop_reason":null,"usage":{"input_tokens":25,"output_tokens":1,"cache_read_input_tokens":4}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type": "ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" \"Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicStreamTransformerRewritesToChunks(t *testing.T) {
	a := &Anthropic{now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	tr := a.NewStreamTransformer(chatReq("claude-sonnet-4-5"), nil)
	chunks, done, raw := collectChunks(t, tr, anthropicSSEFixture)

	if strings.Contains(raw, "event:") || strings.Contains(raw, "message_start") || strings.Contains(raw, "content_block") {
		t.Fatalf("Anthropic-typed SSE leaked to the client:\n%s", raw)
	}
	if !done {
		t.Fatalf("stream must end with data: [DONE]:\n%s", raw)
	}
	// role, "Hello", " there", tool open, 2 arg deltas, finish, usage
	if len(chunks) != 8 {
		t.Fatalf("expected 8 chunks, got %d:\n%s", len(chunks), raw)
	}
	for i, c := range chunks {
		if c["object"] != "chat.completion.chunk" || c["id"] != "msg_01" || c["model"] != "claude-sonnet-4-5" || c["created"] != float64(1_700_000_000) {
			t.Fatalf("chunk %d envelope = %v", i, c)
		}
	}
	delta := func(i int) map[string]any {
		return chunks[i]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	}
	if delta(0)["role"] != "assistant" {
		t.Fatalf("first chunk must carry the role: %v", chunks[0])
	}
	if delta(1)["content"] != "Hello" || delta(2)["content"] != " there" {
		t.Fatalf("text deltas = %v / %v", delta(1), delta(2))
	}
	open := delta(3)["tool_calls"].([]any)[0].(map[string]any)
	// Content block 1 is the FIRST tool call → tool_calls index 0.
	if open["index"] != float64(0) || open["id"] != "toolu_01" || open["type"] != "function" ||
		open["function"].(map[string]any)["name"] != "get_weather" || open["function"].(map[string]any)["arguments"] != "" {
		t.Fatalf("tool open chunk = %v", open)
	}
	arg1 := delta(4)["tool_calls"].([]any)[0].(map[string]any)
	arg2 := delta(5)["tool_calls"].([]any)[0].(map[string]any)
	if arg1["index"] != float64(0) || arg1["function"].(map[string]any)["arguments"] != `{"city":` ||
		arg2["function"].(map[string]any)["arguments"] != ` "Paris"}` {
		t.Fatalf("argument deltas = %v / %v", arg1, arg2)
	}
	if _, hasID := arg1["id"]; hasID {
		t.Fatalf("argument deltas must not repeat the id: %v", arg1)
	}
	finishChoice := chunks[6]["choices"].([]any)[0].(map[string]any)
	if finishChoice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish chunk = %v", chunks[6])
	}
	for i := 0; i < 6; i++ {
		if fr := chunks[i]["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != nil {
			t.Fatalf("chunk %d finish_reason = %v, want null", i, fr)
		}
	}
	usageChunk := chunks[7]
	if len(usageChunk["choices"].([]any)) != 0 {
		t.Fatalf("usage chunk must carry empty choices: %v", usageChunk)
	}
	u := usageChunk["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(25) || u["completion_tokens"] != float64(15) || u["total_tokens"] != float64(40) ||
		u["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(4) {
		t.Fatalf("usage = %v", u)
	}
	if !strings.HasSuffix(raw, "data: [DONE]\n\n") {
		t.Fatalf("[DONE] must be last:\n%s", raw)
	}
}

func TestAnthropicStreamTransformerThinkingErrorsAndPassThrough(t *testing.T) {
	a := &Anthropic{}
	tr := a.NewStreamTransformer(chatReq("c"), nil)
	chunks, _, _ := collectChunks(t, tr, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"c\",\"usage\":{\"input_tokens\":1}}}\n\n"+
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n"+
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"abc\"}}\n\n"+
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":3}}\n\n")
	if len(chunks) != 3 {
		t.Fatalf("expected role, reasoning, finish chunks; got %d", len(chunks))
	}
	if d := chunks[1]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any); d["reasoning_content"] != "hmm" {
		t.Fatalf("thinking delta = %v", d)
	}
	if fr := chunks[2]["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "length" {
		t.Fatalf("finish_reason = %v", fr)
	}

	// Error events become OpenAI-style error frames.
	tr = a.NewStreamTransformer(chatReq("c"), nil)
	out, err := tr.TransformStreamLine([]byte(`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), `data: {"error":{"type":"overloaded_error","message":"Overloaded"}}`) {
		t.Fatalf("error frame = %q", out)
	}

	// A JSON error body answering a stream:true request is not SSE and must
	// reach the client untouched.
	tr = a.NewStreamTransformer(chatReq("c"), nil)
	body := []byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}` + "\n")
	out, err = tr.TransformStreamLine(body)
	if err != nil || !bytes.Equal(out, body) {
		t.Fatalf("non-SSE line must pass through: %q", out)
	}
}

func TestAnthropicStreamTransformerFinishesOnceWithoutMessageStop(t *testing.T) {
	a := &Anthropic{}
	tr := a.NewStreamTransformer(chatReq("c"), nil)
	// Some proxies terminate with the OpenAI sentinel instead of message_stop.
	_, done, raw := collectChunks(t, tr, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	if !done || strings.Count(raw, "[DONE]") != 1 {
		t.Fatalf("expected exactly one [DONE]:\n%s", raw)
	}
	more, _ := tr.TransformStreamLine([]byte("data: {\"type\":\"message_stop\"}\n"))
	if len(more) != 0 {
		t.Fatalf("finish must be idempotent, got %q", more)
	}
}
