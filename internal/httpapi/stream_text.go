package httpapi

import (
	"bytes"
	"encoding/json"
)

// streamTextCounter measures the generated text a stream carried, for the
// byte-count metering fallback.
//
// The fallback estimates output tokens as bytes/4, a ratio calibrated on
// natural-language TEXT. Applied to the raw SSE bytes of a stream it is badly
// wrong: every token arrives wrapped in its own ~150-byte
// `data: {"id":…,"choices":[{"delta":{"content":"…"}}]}` frame, so a stream
// that never delivered its usage block (cut mid-flight, or an upstream that
// ignores include_usage) was billed 10-20x the tokens it produced. Counting
// only the text inside the frames puts the estimate back on the scale the
// ratio was calibrated for.
//
// It reads the gateway's OUTPUT frames (after the adapter's transformer), so
// every provider is measured in one shape: chat.completion.chunk (content,
// reasoning, tool-call arguments), legacy completions (`text`) and the
// Responses API (`delta` strings). measured stays false until a recognizable
// frame is seen, so an unknown stream shape keeps the old raw-byte estimate
// rather than being metered as zero.
type streamTextCounter struct {
	bytes    int64
	measured bool
}

type streamTextFrame struct {
	Choices []struct {
		Text  *string `json:"text"`
		Delta *struct {
			Content          *string `json:"content"`
			ReasoningContent *string `json:"reasoning_content"`
			Reasoning        *string `json:"reasoning"`
			ToolCalls        []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	// Responses API text/argument deltas carry the text as a string.
	Delta json.RawMessage `json:"delta"`
}

// Feed counts the text in one chunk of transformed output, which may hold
// several SSE frames.
func (c *streamTextCounter) Feed(out []byte) {
	for len(out) > 0 {
		var line []byte
		if i := bytes.IndexByte(out, '\n'); i >= 0 {
			line, out = out[:i], out[i+1:]
		} else {
			line, out = out, nil
		}
		payload, ok := sseDataPayload(line)
		if !ok {
			continue
		}
		var frame streamTextFrame
		if err := json.Unmarshal(payload, &frame); err != nil {
			continue
		}
		if len(frame.Choices) > 0 {
			c.measured = true
		}
		for _, ch := range frame.Choices {
			if ch.Text != nil {
				c.bytes += int64(len(*ch.Text))
			}
			if d := ch.Delta; d != nil {
				for _, s := range []*string{d.Content, d.ReasoningContent, d.Reasoning} {
					if s != nil {
						c.bytes += int64(len(*s))
					}
				}
				for _, tc := range d.ToolCalls {
					c.bytes += int64(len(tc.Function.Name) + len(tc.Function.Arguments))
				}
			}
		}
		if len(frame.Delta) > 0 && frame.Delta[0] == '"' {
			var s string
			if json.Unmarshal(frame.Delta, &s) == nil {
				c.bytes += int64(len(s))
				c.measured = true
			}
		}
	}
}

// estimateBase is the byte count the token estimate should use: the text
// measured inside the frames when the stream shape was recognized, otherwise
// the raw relayed bytes.
func (c *streamTextCounter) estimateBase(rawBytes int64) int64 {
	if c.measured {
		return c.bytes
	}
	return rawBytes
}

func sseDataPayload(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || payload[0] != '{' {
		return nil, false
	}
	return payload, true
}
