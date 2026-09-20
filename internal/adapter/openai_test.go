package adapter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func openAIChatReq(model, body string) *Request {
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	return &Request{Method: http.MethodPost, Path: "/v1/chat/completions", Header: hdr, Model: model, Body: []byte(body)}
}

func TestOpenAIDefaultsGPT5ReasoningEffortWhenToolsPresent(t *testing.T) {
	a := &OpenAICompatible{name: "openai_compatible"}
	up := Upstream{BaseURL: "https://api.openai.com", APIKey: "sk"}

	withTools := `{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`
	for _, model := range []string{"gpt-5", "gpt-5.6-sol", "gpt-5-mini", "gpt-5.1-codex", "openai/gpt-5.2", "GPT-5-Pro"} {
		prep, err := a.Prepare(up, openAIChatReq(model, withTools))
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		var body map[string]any
		_ = json.Unmarshal(prep.Body, &body)
		if body["reasoning_effort"] != "none" {
			t.Errorf("%s: reasoning_effort = %v, want \"none\"", model, body["reasoning_effort"])
		}
		if body["tools"] == nil || body["messages"] == nil {
			t.Errorf("%s: other fields lost: %s", model, prep.Body)
		}
	}
}

func TestOpenAIReasoningEffortDefaultIsNarrow(t *testing.T) {
	a := &OpenAICompatible{name: "openai_compatible"}
	up := Upstream{BaseURL: "https://api.openai.com", APIKey: "sk"}
	tools := `"tools":[{"type":"function","function":{"name":"f"}}]`

	cases := map[string]string{
		"no tools":               `{"model":"gpt-5","messages":[]}`,
		"empty tools":            `{"model":"gpt-5","messages":[],"tools":[]}`,
		"non-gpt-5 model":        `{"model":"gpt-4.1","messages":[],` + tools + `}`,
		"gpt-4o with tools":      `{"model":"gpt-4o","messages":[],` + tools + `}`,
		"lookalike gpt-50":       `{"model":"gpt-50","messages":[],` + tools + `}`,
		"caller chose an effort": `{"model":"gpt-5","messages":[],"reasoning_effort":"high",` + tools + `}`,
		"caller chose none":      `{"model":"gpt-5","messages":[],"reasoning_effort":"none",` + tools + `}`,
		"caller chose null":      `{"model":"gpt-5","messages":[],"reasoning_effort":null,` + tools + `}`,
		"tools key in text only": `{"model":"gpt-5","messages":[{"role":"user","content":"\"tools\""}]}`,
	}
	for name, body := range cases {
		var before map[string]json.RawMessage
		_ = json.Unmarshal([]byte(body), &before)
		var model string
		_ = json.Unmarshal(before["model"], &model)
		prep, err := a.Prepare(up, openAIChatReq(model, body))
		if err != nil {
			t.Fatalf("%s: Prepare: %v", name, err)
		}
		var after map[string]json.RawMessage
		_ = json.Unmarshal(prep.Body, &after)
		if !bytes.Equal(before["reasoning_effort"], after["reasoning_effort"]) {
			t.Errorf("%s: reasoning_effort changed from %s to %s", name, before["reasoning_effort"], after["reasoning_effort"])
		}
		if _, added := after["reasoning_effort"]; added && before["reasoning_effort"] == nil {
			t.Errorf("%s: reasoning_effort injected: %s", name, prep.Body)
		}
	}

	// Only the real OpenAI adapter absorbs the quirk; local servers behind the
	// vlm/llama_cpp aliases never see the field.
	vlm := &OpenAICompatible{name: "vlm"}
	prep, _ := vlm.Prepare(up, openAIChatReq("gpt-5", `{"model":"gpt-5","messages":[],`+tools+`}`))
	if bytes.Contains(prep.Body, []byte("reasoning_effort")) {
		t.Fatalf("vlm adapter must not inject reasoning_effort: %s", prep.Body)
	}
	// Other paths are untouched too.
	prep, _ = a.Prepare(up, &Request{Path: "/v1/responses", Header: openAIChatReq("", "").Header, Model: "gpt-5", Body: []byte(`{"model":"gpt-5",` + tools + `}`)})
	if bytes.Contains(prep.Body, []byte("reasoning_effort")) {
		t.Fatalf("/v1/responses must not be rewritten: %s", prep.Body)
	}
}

func TestOpenAITransformResponseNormalizesReasoningFields(t *testing.T) {
	a := &OpenAICompatible{name: "openai_compatible"}
	req := openAIChatReq("grok-4", "")

	// OpenRouter-style `reasoning` becomes the canonical reasoning_content.
	out, err := a.TransformResponse(req, []byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning":"thinking…"},"finish_reason":"stop"}],"usage":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Choices []struct {
			Message map[string]any `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(out, &resp)
	msg := resp.Choices[0].Message
	if msg["reasoning_content"] != "thinking…" || msg["reasoning"] != nil || msg["content"] != "hi" {
		t.Fatalf("message = %v", msg)
	}

	// xAI's reasoning_content is already canonical → byte-for-byte unchanged.
	xai := []byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning_content":"…"},"finish_reason":"stop"}]}`)
	out, _ = a.TransformResponse(req, xai)
	if !bytes.Equal(out, xai) {
		t.Fatalf("canonical body must be untouched:\n%s", out)
	}
	// Both present: keep reasoning_content, drop the alias.
	out, _ = a.TransformResponse(req, []byte(`{"choices":[{"message":{"reasoning_content":"a","reasoning":"b"}}]}`))
	if !bytes.Contains(out, []byte(`"reasoning_content":"a"`)) || bytes.Contains(out, []byte(`"reasoning":`)) {
		t.Fatalf("alias must be dropped in favour of reasoning_content: %s", out)
	}
	// Plain OpenAI bodies are byte-for-byte pass-through.
	plain := []byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	out, _ = a.TransformResponse(req, plain)
	if !bytes.Equal(out, plain) {
		t.Fatalf("plain body rewritten: %s", out)
	}
	// Native non-chat paths are never parsed.
	if out, _ := a.TransformResponse(&Request{Path: "/v1/embeddings"}, []byte(`{"reasoning":1}`)); string(out) != `{"reasoning":1}` {
		t.Fatalf("non-chat body rewritten: %s", out)
	}
}

func TestOpenAIStreamTransformerNormalizesReasoningDeltas(t *testing.T) {
	a := &OpenAICompatible{name: "openai_compatible"}
	tr := a.NewStreamTransformer(openAIChatReq("grok-4", ""), nil)

	line := []byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning":"hm"},"finish_reason":null}]}` + "\n")
	out, err := tr.TransformStreamLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte("data: ")) || !bytes.HasSuffix(out, []byte("\n\n")) ||
		!bytes.Contains(out, []byte(`"reasoning_content":"hm"`)) || bytes.Contains(out, []byte(`"reasoning":`)) {
		t.Fatalf("delta not normalised: %q", out)
	}
	for _, passthrough := range []string{
		"data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n",
		"data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"reasoning_content\":\"hi\"}}]}\n",
		"data: [DONE]\n",
		"\n",
		": keep-alive\n",
	} {
		out, err := tr.TransformStreamLine([]byte(passthrough))
		if err != nil || string(out) != passthrough {
			t.Fatalf("line %q rewritten to %q (%v)", passthrough, out, err)
		}
	}
}
