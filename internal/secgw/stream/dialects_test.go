package stream

import (
	"encoding/json"
	"strings"
	"testing"
)

// Regression tests for the 2026-11 security review, batch 3: the hold-back
// buffer read only chat completion chunks, so a streamed Responses,
// Anthropic, Gemini or legacy completion reply was released unscanned.

const streamSecret = "AKIAIOSFODNN7EXAMPLE"

func sse(v any) []byte {
	payload, _ := json.Marshal(v)
	return append(append([]byte("data: "), payload...), '\n', '\n')
}

// dialects builds one text delta frame per streaming API.
var dialects = map[string]func(text string) []byte{
	"responses output text": func(text string) []byte {
		return sse(map[string]any{"type": "response.output_text.delta", "item_id": "msg_1", "output_index": 0, "content_index": 0, "delta": text})
	},
	"responses tool arguments": func(text string) []byte {
		return sse(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_1", "output_index": 1, "delta": text})
	},
	"anthropic text": func(text string) []byte {
		return sse(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
	},
	"anthropic tool input": func(text string) []byte {
		return sse(map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": text}})
	},
	"anthropic thinking": func(text string) []byte {
		return sse(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": text}})
	},
	"legacy completion": func(text string) []byte {
		return sse(map[string]any{"choices": []map[string]any{{"index": 0, "text": text}}})
	},
	"gemini": func(text string) []byte {
		return sse(map[string]any{"candidates": []map[string]any{{"index": 0, "content": map[string]any{"parts": []map[string]any{{"text": text}}}}}})
	},
}

func joined(frames [][]byte) string {
	var sb strings.Builder
	for _, f := range frames {
		sb.Write(f)
	}
	return sb.String()
}

func TestHoldbackReadsEveryStreamingDialect(t *testing.T) {
	// The secret arrives split across frames, with text around it.
	pieces := []string{"the key is AKIA", "IOSFOD", "NN7EXAMPLE and", " that is all ", strings.Repeat("padding ", 20)}
	for name, frame := range dialects {
		// Block: nothing of the secret is ever released.
		h := New(64, len(streamSecret), substringScanner(streamSecret, true))
		var out [][]byte
		cut := false
		for _, p := range pieces {
			res := h.Feed(frame(p))
			out = append(out, res.Release...)
			cut = cut || res.Cut
		}
		out = append(out, h.Flush()...)
		if !cut || strings.Contains(joined(out), "IOSFOD") || strings.Contains(joined(out), "NN7EXAMPLE") {
			t.Errorf("%s (block): cut=%v released %q", name, cut, joined(out))
		}
		// Redact: the stream goes on without the secret (the marker takes
		// its place, spread over the frames the secret arrived in).
		h = New(64, len(streamSecret), substringScanner(streamSecret, false))
		out, redactions := nil, 0
		for _, p := range pieces {
			res := h.Feed(frame(p))
			out = append(out, res.Release...)
			redactions += res.Redactions
		}
		out = append(out, h.Flush()...)
		got := joined(out)
		if redactions != 1 || strings.Contains(got, "IOSFOD") || strings.Contains(got, "NN7EXAMPLE") || !strings.Contains(got, "[RED") || !strings.Contains(got, "that is all") {
			t.Errorf("%s (redact): redactions=%d released %q", name, redactions, got)
		}
		if len(out) != len(pieces) {
			t.Errorf("%s (redact): %d frames released, want %d", name, len(out), len(pieces))
		}
	}
}

// The Responses API sends the whole text again when a part, an item and the
// response end. Those copies must not undo a redaction or slip past a block.
func TestHoldbackChecksFramesThatRepeatTheText(t *testing.T) {
	text := "the key is " + streamSecret + " ok"
	done := []any{
		map[string]any{"type": "response.output_text.done", "item_id": "msg_1", "output_index": 0, "content_index": 0, "text": text},
		map[string]any{"type": "response.content_part.done", "item_id": "msg_1", "part": map[string]any{"type": "output_text", "text": text}},
		map[string]any{"type": "response.output_item.done", "item": map[string]any{"id": "msg_1", "type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}}}},
		map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "output": []any{
			map[string]any{"id": "msg_1", "type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}}}}}},
	}
	delta := dialects["responses output text"]

	h := New(8, len(streamSecret), substringScanner(streamSecret, false))
	var out [][]byte
	redactions, matches := 0, 0
	feed := func(raw []byte) {
		res := h.Feed(raw)
		out = append(out, res.Release...)
		redactions += res.Redactions
		matches += len(res.Matches)
	}
	feed([]byte("event: response.output_text.delta\n"))
	feed(delta(text))
	for _, d := range done {
		feed(sse(d))
	}
	out = append(out, h.Flush()...)
	got := joined(out)
	if strings.Contains(got, streamSecret) {
		t.Fatalf("a repeated frame carried the secret out: %q", got)
	}
	if redactions != 1 || matches != 1 {
		t.Errorf("redactions=%d matches=%d, want the one secret counted once", redactions, matches)
	}
	if !strings.Contains(got, `"id":"resp_1"`) || !strings.Contains(got, "event: response.output_text.delta\n") || strings.Count(got, "REDACTED") != 5 {
		t.Errorf("the frames were not kept whole: %q", got)
	}

	// Block: a secret that appears only in a closing frame still cuts.
	h = New(8, len(streamSecret), substringScanner(streamSecret, true))
	if res := h.Feed(delta("nothing here")); res.Cut {
		t.Fatal("cut on clean text")
	}
	res := h.Feed(sse(done[3]))
	if !res.Cut || res.Blocked == nil || len(res.Release) != 0 || len(h.Flush()) != 0 {
		t.Fatalf("a closing frame with the secret was not cut: %+v", res)
	}

	// Observe: reported once, the text untouched.
	observe := func(_ string, full string, from int) []Match {
		ms := substringScanner(streamSecret, false)("", full, from)
		for i := range ms {
			ms[i].Observe = true
		}
		return ms
	}
	h = New(8, len(streamSecret), observe)
	out, matches = nil, 0
	for _, raw := range [][]byte{delta(text), sse(done[0]), sse(done[3])} {
		res := h.Feed(raw)
		out = append(out, res.Release...)
		matches += len(res.Matches)
	}
	out = append(out, h.Flush()...)
	if matches != 1 || strings.Count(joined(out), streamSecret) != 3 {
		t.Errorf("observe: matches=%d, text %q", matches, joined(out))
	}
}

// Frames of other dialects that carry no text are passed on untouched.
func TestHoldbackLeavesOtherFramesAlone(t *testing.T) {
	h := New(8, 20, substringScanner(streamSecret, true))
	frames := [][]byte{
		[]byte("event: message_start\n"),
		sse(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "role": "assistant", "content": []any{}}}),
		sse(map[string]any{"type": "response.audio.delta", "delta": strings.Repeat("QUJD", 400)}),
		sse(map[string]any{"id": "chatcmpl-1", "choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}}}}),
		[]byte("data: [DONE]\n\n"),
	}
	var out [][]byte
	for _, f := range frames {
		out = append(out, h.Feed(f).Release...)
	}
	out = append(out, h.Flush()...)
	if len(out) != len(frames) {
		t.Fatalf("%d frames released, want %d", len(out), len(frames))
	}
	for i := range frames {
		if string(out[i]) != string(frames[i]) {
			t.Errorf("frame %d changed:\n got %q\nwant %q", i, out[i], frames[i])
		}
	}
}
