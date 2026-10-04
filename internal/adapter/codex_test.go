package adapter

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func codexReq(body string, streaming bool) *Request {
	return &Request{Path: "/v1/chat/completions", Model: "gpt-5.5", Body: []byte(body), Streaming: streaming, Header: http.Header{}}
}

func TestCodexChatToResponsesRequest(t *testing.T) {
	c := &Codex{}
	body := `{"model":"my/openai/gpt-5.5","temperature":0.2,"max_tokens":50,"user":"sess-1",
	 "messages":[
	  {"role":"system","content":"Be brief."},
	  {"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get","arguments":"{\"a\":1}"}}]},
	  {"role":"tool","tool_call_id":"call_1","content":"42"}],
	 "tools":[{"type":"function","function":{"name":"get","description":"d","parameters":{"type":"object"}}}],
	 "tool_choice":"auto","reasoning_effort":"low"}`
	p, err := c.Prepare(Upstream{BaseURL: "https://chatgpt.com/backend-api/codex", APIKey: "tok"}, codexReq(body, false))
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != "https://chatgpt.com/backend-api/codex/responses" {
		t.Fatalf("url %s", p.URL)
	}
	if p.Header.Get("Authorization") != "Bearer tok" || p.Header.Get("session_id") != "sess-1" {
		t.Fatalf("headers %v", p.Header)
	}
	var out map[string]any
	if err := json.Unmarshal(p.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out["model"] != "gpt-5.5" || out["instructions"] != "Be brief." || out["store"] != false || out["stream"] != true {
		t.Fatalf("top-level: %v", out)
	}
	for _, dropped := range []string{"temperature", "max_tokens", "max_output_tokens", "messages"} {
		if _, ok := out[dropped]; ok {
			t.Fatalf("%s should not be sent: %v", dropped, out)
		}
	}
	input := out["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input: %v", input)
	}
	user := input[0].(map[string]any)["content"].([]any)
	if user[1].(map[string]any)["type"] != "input_image" {
		t.Fatalf("image part: %v", user)
	}
	if fc := input[1].(map[string]any); fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["arguments"] != `{"a":1}` {
		t.Fatalf("function_call: %v", fc)
	}
	if fo := input[2].(map[string]any); fo["type"] != "function_call_output" || fo["output"] != "42" {
		t.Fatalf("function_call_output: %v", fo)
	}
	tool := out["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "get" {
		t.Fatalf("tool flattened: %v", tool)
	}
	if out["reasoning"].(map[string]any)["effort"] != "low" {
		t.Fatalf("reasoning: %v", out["reasoning"])
	}
}

func TestCodexRefusesWhatItCannotExpress(t *testing.T) {
	c := &Codex{}
	up := Upstream{BaseURL: "https://x", APIKey: "t"}
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"hi"}],"n":2}`,
		`{"messages":[]}`,
		`{"messages":[{"role":"tool","content":"x"}]}`,
	} {
		if _, err := c.Prepare(up, codexReq(body, false)); err == nil {
			t.Errorf("expected refusal for %s", body)
		}
	}
	if _, err := c.Prepare(up, &Request{Path: "/v1/embeddings", Body: []byte(`{}`), Header: http.Header{}}); err == nil {
		t.Error("embeddings should be refused")
	}
}

const codexStreamFixture = `event: response.created
data: {"type":"response.created","response":{"id":"resp_abc","model":"gpt-5.5"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hel"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"lo"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_9","name":"get"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"a\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"2}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_abc","status":"completed","usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":3}}}}

`

func TestCodexBufferedResponseFolding(t *testing.T) {
	c := &Codex{}
	req := codexReq(`{"messages":[{"role":"user","content":"hi"}]}`, false)
	if !c.BuffersEventStream(req) {
		t.Fatal("non-streaming chat must be buffered")
	}
	out, err := c.TransformResponse(req, []byte(codexStreamFixture))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	ch := r.Choices[0]
	if r.Object != "chat.completion" || r.ID != "chatcmpl-abc" || ch.Message.Content == nil || *ch.Message.Content != "Hello" ||
		ch.Finish != "tool_calls" || len(ch.Message.ToolCalls) != 1 || ch.Message.ToolCalls[0].Function.Arguments != `{"a":2}` ||
		r.Usage.Prompt != 11 || r.Usage.Completion != 7 {
		t.Fatalf("folded: %s", out)
	}
	u, ok := c.ExtractUsage([]byte(codexStreamFixture))
	if !ok || u.TokensIn != 11 || u.TokensOut != 7 || u.TokensCached != 3 {
		t.Fatalf("usage %+v", u)
	}
}

func TestCodexStreamTranslation(t *testing.T) {
	c := &Codex{}
	req := codexReq(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`, true)
	if c.BuffersEventStream(req) {
		t.Fatal("streaming chat must not be buffered")
	}
	st := c.NewStreamTransformer(req, nil)
	var all strings.Builder
	for _, line := range strings.Split(codexStreamFixture, "\n") {
		out, err := st.TransformStreamLine([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		all.Write(out)
	}
	s := all.String()
	for _, want := range []string{`"role":"assistant"`, `"content":"Hel"`, `"content":"lo"`, `"id":"call_9"`,
		`"arguments":"2}"`, `"finish_reason":"tool_calls"`, `"prompt_tokens":11`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Fatalf("stream missing %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "response.") {
		t.Fatalf("Responses events leaked to a chat client:\n%s", s)
	}
	// An error body (not SSE) is relayed untouched.
	st = c.NewStreamTransformer(req, nil)
	out, _ := st.TransformStreamLine([]byte(`{"detail":"Unauthorized"}`))
	if string(out) != `{"detail":"Unauthorized"}` {
		t.Fatalf("error body altered: %s", out)
	}
}

func TestCodexHeadersFromJWT(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-1"}})
	tok := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
	h := CodexHeaders(tok)
	if h["ChatGPT-Account-ID"] != "acct-1" || h["Authorization"] != "Bearer "+tok || h["originator"] == "" {
		t.Fatalf("headers %v", h)
	}
	if _, ok := CodexHeaders("opaque")["ChatGPT-Account-ID"]; ok {
		t.Fatal("non-JWT token should not yield an account header")
	}
}

func TestCodexIsPersonalOnly(t *testing.T) {
	for _, typ := range Types() {
		if typ == CodexAdapterType {
			t.Fatal("codex must not be offered as an upstream type")
		}
	}
}
