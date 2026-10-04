package httpapi

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/usage"
)

// chunkFrame is one realistic vLLM/OpenAI chat.completion.chunk carrying a
// single token of text: ~150 bytes of framing around a handful of characters.
func chunkFrame(i int, text string) string {
	return fmt.Sprintf(`data: {"id":"chatcmpl-8f3a9c2e41d04b7a","object":"chat.completion.chunk","created":1790831148,"model":"test-model","choices":[{"index":%d,"delta":{"content":%q},"logprobs":null,"finish_reason":null}]}`, 0*i, text)
}

// TestProxyTruncatedStreamIsAFailureAndEstimatedFromText reproduces the
// stress-test finding: an upstream that closes a stream after N
// tokens, without [DONE], finish_reason or usage, was recorded as a clean 200
// and billed bytes/4 of the raw SSE framing (10-20x the real output).
func TestProxyTruncatedStreamIsAFailureAndEstimatedFromText(t *testing.T) {
	h := newHarness(t)
	const tokens = 200
	var frames []string
	var textBytes int
	for i := 0; i < tokens; i++ {
		word := "lorem "
		frames = append(frames, chunkFrame(i, word))
		textBytes += len(word)
	}
	h.streamFrames = frames // no finish_reason, no usage, no [DONE]

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the committed 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"upstream.stream_interrupted"`) {
		t.Fatalf("client was not told the stream is incomplete; tail:\n%s", body[max(0, len(body)-400):])
	}
	if strings.Count(body, `"content":"lorem "`) != tokens {
		t.Errorf("relayed %d of %d content frames", strings.Count(body, `"content":"lorem "`), tokens)
	}

	event := h.waitForUsageEvents(1)[0]
	if event.ErrorCode != CodeUpstreamInterrupted {
		t.Errorf("error_code = %q, want %q", event.ErrorCode, CodeUpstreamInterrupted)
	}
	if event.AccountingMode != usage.AccountingBytes {
		t.Errorf("accounting = %q, want the byte estimate", event.AccountingMode)
	}
	want := usage.EstimateTokensFromBytes(int64(textBytes))
	if event.TokensOut != want {
		t.Errorf("tokens_out = %d, want %d (text bytes/4); raw-frame estimate would be %d",
			event.TokensOut, want, usage.EstimateTokensFromBytes(event.ResponseBytes))
	}
	if event.ResponseBytes <= int64(textBytes)*10 {
		t.Errorf("response bytes %d should still record the raw relayed size", event.ResponseBytes)
	}
}

// TestProxyStreamConnectionLostMidFlight: the upstream's TCP connection dies
// mid-chunk (a crashed engine, a dropped pod). The read error must mark the
// event failed, not just end the loop as if the stream were complete.
func TestProxyStreamConnectionLostMidFlight(t *testing.T) {
	h := newHarness(t)
	dying := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		for i := 0; i < 20; i++ {
			frame := chunkFrame(i, "ipsum ") + "\n\n"
			_, _ = fmt.Fprintf(buf, "%x\r\n%s\r\n", len(frame), frame)
		}
		// Promise a chunk, then vanish.
		_, _ = buf.WriteString("400\r\ndata: {\"choi")
		_ = buf.Flush()
		time.Sleep(20 * time.Millisecond)
	}))
	t.Cleanup(dying.Close)
	if err := h.store.UpdateUpstream(t.Context(), h.model.UpstreamID, dying.URL, true, "", ""); err != nil {
		t.Fatalf("repoint upstream: %v", err)
	}

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}, "stream": true,
	})
	if !strings.Contains(rec.Body.String(), "upstream.stream_interrupted") {
		t.Fatalf("client not told the stream broke:\n%s", rec.Body.String())
	}
	event := h.waitForUsageEvents(1)[0]
	if event.ErrorCode != CodeUpstreamInterrupted {
		t.Errorf("error_code = %q, want %q", event.ErrorCode, CodeUpstreamInterrupted)
	}
	if event.TokensOut > 40 {
		t.Errorf("tokens_out = %d for 20 six-byte tokens; the estimate is counting framing", event.TokensOut)
	}
}

// TestProxyCompleteStreamNotFlagged guards the other direction: a normal
// stream with usage and [DONE], and a stream that finishes (finish_reason)
// but whose engine sends neither usage nor [DONE], are both complete.
func TestProxyCompleteStreamNotFlagged(t *testing.T) {
	for name, frames := range map[string][]string{
		"usage and done": nil, // harness default
		"finish_reason only": {
			chunkFrame(0, "Hello"),
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.streamFrames = frames
			rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
				"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}, "stream": true,
			})
			if strings.Contains(rec.Body.String(), "stream_interrupted") {
				t.Fatalf("complete stream flagged as interrupted:\n%s", rec.Body.String())
			}
			event := h.waitForUsageEvents(1)[0]
			if event.ErrorCode != "" {
				t.Errorf("error_code = %q, want none", event.ErrorCode)
			}
			if frames != nil && event.TokensOut != usage.EstimateTokensFromBytes(int64(len("Hello"))) {
				t.Errorf("tokens_out = %d, want the text estimate", event.TokensOut)
			}
		})
	}
}

func TestStreamTextCounterShapes(t *testing.T) {
	var c streamTextCounter
	in := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"think"}}]}`,
		`data: {"choices":[{"delta":{"content":"héllo"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"text":"legacy"}]}`,
		`data: {"type":"response.output_text.delta","delta":"resp"}`,
		`event: ping`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	sc := bufio.NewScanner(strings.NewReader(in))
	for sc.Scan() {
		c.Feed(append(sc.Bytes(), '\n'))
	}
	want := int64(len("think") + len("héllo") + len("f") + len(`{"a":1}`) + len("legacy") + len("resp"))
	if !c.measured || c.bytes != want {
		t.Fatalf("counted %d (measured=%v), want %d", c.bytes, c.measured, want)
	}
	var unknown streamTextCounter
	unknown.Feed([]byte("data: {\"foo\":1}\n"))
	if unknown.estimateBase(999) != 999 {
		t.Fatal("an unrecognized stream shape must keep the raw-byte estimate")
	}
}
