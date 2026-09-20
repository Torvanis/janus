package adapter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestEventStreamDecoderReassemblesFrames(t *testing.T) {
	frameA := encodeEventStreamMessage(map[string]string{":event-type": "contentBlockDelta", ":message-type": "event"}, []byte(`{"contentBlockIndex":0,"delta":{"text":"Hi\n"}}`))
	frameB := encodeEventStreamMessage(map[string]string{":event-type": "messageStop"}, []byte(`{"stopReason":"end_turn"}`))
	stream := append(append([]byte{}, frameA...), frameB...)

	// Feed in awkward pieces: mid-prelude, mid-payload, and both frames' tails together.
	var d eventStreamDecoder
	var got []eventStreamMessage
	for _, cut := range [][2]int{{0, 5}, {5, 20}, {20, len(frameA) + 3}, {len(frameA) + 3, len(stream)}} {
		msgs, err := d.Feed(stream[cut[0]:cut[1]])
		if err != nil {
			t.Fatalf("Feed: %v", err)
		}
		got = append(got, msgs...)
	}
	if len(got) != 2 {
		t.Fatalf("decoded %d frames, want 2", len(got))
	}
	if got[0].Headers[":event-type"] != "contentBlockDelta" || got[0].Headers[":message-type"] != "event" || !bytes.Contains(got[0].Payload, []byte(`"Hi\n"`)) {
		t.Fatalf("frame A = %+v", got[0])
	}
	if got[1].Headers[":event-type"] != "messageStop" {
		t.Fatalf("frame B = %+v", got[1])
	}

	corrupt := append([]byte{}, frameA...)
	corrupt[len(corrupt)-1] ^= 0xFF
	if _, err := (&eventStreamDecoder{}).Feed(corrupt); err == nil {
		t.Fatal("corrupt message CRC must be detected")
	}
}

func bedrockBinaryStream() []byte {
	frames := []struct {
		event   string
		payload string
	}{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"The weather"}}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":" is"}}`},
		{"contentBlockStop", `{"contentBlockIndex":0}`},
		{"contentBlockStart", `{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"tooluse_1","name":"get_weather"}}}`},
		{"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"city\":\"Paris\"}"}}}`},
		{"contentBlockStop", `{"contentBlockIndex":1}`},
		{"messageStop", `{"stopReason":"tool_use"}`},
		{"metadata", `{"usage":{"inputTokens":12,"outputTokens":7,"totalTokens":19},"metrics":{"latencyMs":300}}`},
	}
	var out []byte
	for _, f := range frames {
		out = append(out, encodeEventStreamMessage(map[string]string{":event-type": f.event, ":message-type": "event", ":content-type": "application/json"}, []byte(f.payload))...)
	}
	return out
}

// splitLines mimics the proxy's ReadBytes('\n') chunking of a binary body.
func splitLines(b []byte) [][]byte {
	var lines [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			lines = append(lines, b)
			break
		}
		lines = append(lines, b[:i+1])
		b = b[i+1:]
	}
	return lines
}

func TestBedrockStreamTransformerRewritesEventStream(t *testing.T) {
	a := &Bedrock{}
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/vnd.amazon.eventstream")
	hdr.Set("X-Amzn-Requestid", "req-123")
	req := &Request{Path: "/v1/chat/completions", Model: "anthropic.claude-3-haiku"}
	tr := a.NewStreamTransformer(req, hdr)
	if got := hdr.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type not corrected for the translated stream: %q", got)
	}

	var out bytes.Buffer
	for _, line := range splitLines(bedrockBinaryStream()) {
		b, err := tr.TransformStreamLine(line)
		if err != nil {
			t.Fatalf("TransformStreamLine: %v", err)
		}
		out.Write(b)
	}
	raw := out.String()
	if !strings.HasSuffix(raw, "data: [DONE]\n\n") {
		t.Fatalf("stream must end with [DONE]:\n%s", raw)
	}
	var chunks []map[string]any
	for _, frame := range strings.Split(raw, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" || frame == "data: [DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &m); err != nil {
			t.Fatalf("frame %q: %v", frame, err)
		}
		chunks = append(chunks, m)
	}
	// role, text, text, tool open, tool args, finish, usage
	if len(chunks) != 7 {
		t.Fatalf("expected 7 chunks, got %d:\n%s", len(chunks), raw)
	}
	for _, c := range chunks {
		if c["object"] != "chat.completion.chunk" || c["id"] != "chatcmpl-req-123" || c["model"] != "anthropic.claude-3-haiku" {
			t.Fatalf("envelope = %v", c)
		}
	}
	delta := func(i int) map[string]any {
		return chunks[i]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	}
	if delta(0)["role"] != "assistant" || delta(1)["content"] != "The weather" || delta(2)["content"] != " is" {
		t.Fatalf("text chunks = %v %v %v", delta(0), delta(1), delta(2))
	}
	open := delta(3)["tool_calls"].([]any)[0].(map[string]any)
	if open["index"] != float64(0) || open["id"] != "tooluse_1" || open["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("tool open = %v", open)
	}
	args := delta(4)["tool_calls"].([]any)[0].(map[string]any)
	if args["index"] != float64(0) || args["function"].(map[string]any)["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("tool args = %v", args)
	}
	if fr := chunks[5]["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "tool_calls" {
		t.Fatalf("finish_reason = %v", fr)
	}
	u := chunks[6]["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(12) || u["completion_tokens"] != float64(7) || u["total_tokens"] != float64(19) {
		t.Fatalf("usage = %v", u)
	}
}

func TestBedrockStreamCollectorReadsBinaryFrames(t *testing.T) {
	a := &Bedrock{}
	c := a.NewStreamCollector()
	for _, line := range splitLines(bedrockBinaryStream()) {
		c.Feed(line)
	}
	u := c.Usage()
	if !u.Reported || u.TokensIn != 12 || u.TokensOut != 7 || u.FinishReason != "tool_use" {
		t.Fatalf("usage = %+v", u)
	}
}

func TestBedrockStreamTransformerAcceptsJSONFrames(t *testing.T) {
	a := &Bedrock{}
	tr := a.NewStreamTransformer(&Request{Path: "/v1/chat/completions", Model: "m"}, http.Header{})
	var out bytes.Buffer
	for _, line := range []string{
		"data: {\"role\":\"assistant\"}\n", "\n",
		"data: {\"contentBlockIndex\":0,\"delta\":{\"text\":\"hi\"}}\n", "\n",
		"data: {\"stopReason\":\"end_turn\"}\n", "\n",
		"data: {\"usage\":{\"inputTokens\":1,\"outputTokens\":2}}\n", "\n",
	} {
		b, err := tr.TransformStreamLine([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		out.Write(b)
	}
	raw := out.String()
	if !strings.Contains(raw, `"content":"hi"`) || !strings.Contains(raw, `"finish_reason":"stop"`) || !strings.HasSuffix(raw, "data: [DONE]\n\n") {
		t.Fatalf("unexpected output:\n%s", raw)
	}
	// An error body (not a ConverseStream event) is relayed untouched.
	errBody := []byte(`{"message":"The security token included in the request is invalid."}` + "\n")
	tr = a.NewStreamTransformer(&Request{Path: "/v1/chat/completions", Model: "m"}, http.Header{})
	got, err := tr.TransformStreamLine(errBody)
	if err != nil || !bytes.Equal(got, errBody) {
		t.Fatalf("error body must pass through: %q (%v)", got, err)
	}
}

func TestBedrockTransformResponseWrapsConverse(t *testing.T) {
	a := &Bedrock{}
	req := &Request{Path: "/v1/chat/completions", Model: "anthropic.claude-3-haiku"}
	body := []byte(`{"output":{"message":{"role":"assistant","content":[{"text":"Hello"},{"toolUse":{"toolUseId":"t1","name":"f","input":{"a":1}}}]}},
		"stopReason":"tool_use","usage":{"inputTokens":3,"outputTokens":4,"totalTokens":7},"metrics":{"latencyMs":1}}`)
	out, err := a.TransformResponse(req, body)
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
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
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if resp.Object != "chat.completion" || resp.Model != "anthropic.claude-3-haiku" || len(resp.Choices) != 1 {
		t.Fatalf("envelope = %+v", resp)
	}
	c := resp.Choices[0]
	if c.Message.Content != "Hello" || c.FinishReason != "tool_calls" || len(c.Message.ToolCalls) != 1 ||
		c.Message.ToolCalls[0].ID != "t1" || c.Message.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("choice = %+v", c)
	}
	if resp.Usage.PromptTokens != 3 || resp.Usage.CompletionTokens != 4 {
		t.Fatalf("usage = %+v", resp.Usage)
	}

	// Non-Converse bodies (errors, OpenAI-shaped fronts) pass through.
	for _, passthrough := range []string{
		`{"message":"Too many requests"}`,
		`{"id":"x","object":"chat.completion","choices":[]}`,
	} {
		out, err := a.TransformResponse(req, []byte(passthrough))
		if err != nil || string(out) != passthrough {
			t.Fatalf("%s rewritten to %s", passthrough, out)
		}
	}
}
