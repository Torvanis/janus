package adapter

import (
	"bytes"
	"encoding/json"
	"net/http"

	lru "github.com/hashicorp/golang-lru/v2"
)

// Gemini thought signatures on tool calls.
//
// With thinking on (every Gemini 3 model), Gemini attaches a
// thought_signature to each function call (OpenAI surface:
// tool_calls[].extra_content.google.thought_signature) and refuses the next
// turn when the call comes back without it: "Function call is missing a
// thought_signature in functionCall parts". Janus relays the signature, but
// most OpenAI clients (Open WebUI among them) rebuild tool calls from
// id/name/arguments and drop the field, so every tool round trip failed on
// the second request.
//
// Janus therefore remembers each signature by tool-call id as it relays the
// response, and puts it back on a replayed call that lacks one. A call
// Janus did not see (another replica, an expired entry, a conversation
// started elsewhere) gets Google's documented placeholder, which skips the
// check rather than failing the request.

// geminiSignatureSkip is the value Google documents for function calls
// whose signature is not available.
const geminiSignatureSkip = "skip_thought_signature_validator"

var geminiSignatures, _ = lru.New[string, string](20000)

// rememberGeminiSignatures records the signatures in a response body or a
// stream chunk.
func rememberGeminiSignatures(payload []byte) {
	if !bytes.Contains(payload, []byte("thought_signature")) {
		return
	}
	var env struct {
		Choices []struct {
			Message struct {
				ToolCalls []geminiToolCall `json:"tool_calls"`
			} `json:"message"`
			Delta struct {
				ToolCalls []geminiToolCall `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &env) != nil {
		return
	}
	for _, c := range env.Choices {
		for _, tc := range append(c.Message.ToolCalls, c.Delta.ToolCalls...) {
			if sig := tc.ExtraContent.Google.ThoughtSignature; tc.ID != "" && sig != "" {
				geminiSignatures.Add(tc.ID, sig)
			}
		}
	}
}

type geminiToolCall struct {
	ID           string `json:"id"`
	ExtraContent struct {
		Google struct {
			ThoughtSignature string `json:"thought_signature"`
		} `json:"google"`
	} `json:"extra_content"`
}

// restoreGeminiSignatures puts a signature back on every replayed assistant
// tool call that lacks one. The body is returned unchanged when nothing is
// missing.
func restoreGeminiSignatures(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"tool_calls"`)) {
		return body
	}
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		return body
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(req["messages"], &msgs) != nil {
		return body
	}
	changed := false
	for _, m := range msgs {
		var role string
		_ = json.Unmarshal(m["role"], &role)
		if role != "assistant" || len(m["tool_calls"]) == 0 {
			continue
		}
		var calls []map[string]any
		if json.Unmarshal(m["tool_calls"], &calls) != nil {
			continue
		}
		touched := false
		for _, c := range calls {
			extra, _ := c["extra_content"].(map[string]any)
			google, _ := extra["google"].(map[string]any)
			if sig, _ := google["thought_signature"].(string); sig != "" {
				continue
			}
			id, _ := c["id"].(string)
			sig, ok := geminiSignatures.Get(id)
			if !ok {
				sig = geminiSignatureSkip
			}
			if extra == nil {
				extra = map[string]any{}
			}
			if google == nil {
				google = map[string]any{}
			}
			google["thought_signature"] = sig
			extra["google"] = google
			c["extra_content"] = extra
			touched = true
		}
		if touched {
			m["tool_calls"], _ = json.Marshal(calls)
			changed = true
		}
	}
	if !changed {
		return body
	}
	req["messages"], _ = json.Marshal(msgs)
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

// geminiSignatureStream records signatures from each chunk it relays.
type geminiSignatureStream struct{ inner StreamTransformer }

func (s geminiSignatureStream) TransformStreamLine(line []byte) ([]byte, error) {
	if payload, ok := sseData(line); ok {
		rememberGeminiSignatures(payload)
	}
	return s.inner.TransformStreamLine(line)
}

func geminiStreamWithSignatures(inner StreamTransformer, req *Request, _ http.Header) StreamTransformer {
	if !isChatCompletions(req) {
		return inner
	}
	return geminiSignatureStream{inner: inner}
}
