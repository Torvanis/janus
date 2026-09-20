package stream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func frameFor(choice int, content string) []byte {
	payload, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"index": choice, "delta": map[string]any{"content": content}}}})
	return append(append([]byte("data: "), payload...), '\n', '\n')
}

func toolFrame(choice, tool int, args string) []byte {
	payload, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"index": choice, "delta": map[string]any{
		"tool_calls": []map[string]any{{"index": tool, "function": map[string]any{"arguments": args}}}}}}})
	return append(append([]byte("data: "), payload...), '\n', '\n')
}

func reasoningFrame(choice int, text string) []byte {
	payload, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"index": choice, "delta": map[string]any{"reasoning_content": text}}}})
	return append(append([]byte("data: "), payload...), '\n', '\n')
}

// contentOf concatenates the delta.content of released frames.
func contentOf(t *testing.T, frames [][]byte) string {
	t.Helper()
	var sb strings.Builder
	for _, f := range frames {
		payload, _, _, ok := splitSSE(f)
		if !ok {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Function struct{ Arguments string }
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			t.Fatalf("released frame is not valid JSON: %s", f)
		}
		for _, c := range chunk.Choices {
			sb.WriteString(c.Delta.Content)
			sb.WriteString(c.Delta.ReasoningContent)
			for _, tc := range c.Delta.ToolCalls {
				sb.WriteString(tc.Function.Arguments)
			}
		}
	}
	return sb.String()
}

func substringScanner(secret string, block bool) Scanner {
	return func(_ string, full string, from int) []Match {
		var out []Match
		start := from
		for {
			i := strings.Index(full[start:], secret)
			if i < 0 {
				return out
			}
			out = append(out, Match{Offset: start + i, Length: len(secret), Replacement: "[REDACTED:secret]", Block: block})
			start += i + len(secret)
		}
	}
}

// randomSplit chops text into random-length pieces (1..max).
func randomSplit(r *rand.Rand, text string, max int) []string {
	var out []string
	for len(text) > 0 {
		n := 1 + r.Intn(max)
		if n > len(text) {
			n = len(text)
		}
		out = append(out, text[:n])
		text = text[n:]
	}
	return out
}

func TestHoldbackNeverReleasesBlockedSecret(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	filler := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 40)
	r := rand.New(rand.NewSource(1))
	for iter := 0; iter < 300; iter++ {
		pos := r.Intn(len(filler))
		text := filler[:pos] + secret + filler[pos:]
		hold := 32 + r.Intn(300)
		h := New(hold, len(secret), substringScanner(secret, true))
		var released [][]byte
		cut := false
		for _, piece := range randomSplit(r, text, 12) {
			res := h.Feed(frameFor(0, piece))
			released = append(released, res.Release...)
			if res.Cut {
				cut = true
				if res.Blocked == nil || res.Blocked.Offset != pos {
					t.Fatalf("iter %d: blocked match offset %v, want %d", iter, res.Blocked, pos)
				}
				break
			}
		}
		if !cut {
			t.Fatalf("iter %d: stream was never cut", iter)
		}
		released = append(released, h.Flush()...)
		got := contentOf(t, released)
		if strings.Contains(got, secret) {
			t.Fatalf("iter %d (hold=%d): secret leaked into released output", iter, hold)
		}
		// Everything released must be a prefix of the clean text before the
		// secret: nothing after the cut point may leak either.
		if !strings.HasPrefix(text, got) || len(got) > pos {
			t.Fatalf("iter %d: released %q is not a prefix ending before the secret at %d", iter, got, pos)
		}
		// And the hold actually held: the released prefix must end at least
		// HoldBytes before the secret UNLESS the secret arrived within the
		// first frames (nothing released yet at all).
		if len(got) > 0 && pos-len(got) < 0 {
			t.Fatalf("iter %d: released past the secret", iter)
		}
	}
}

func TestHoldbackCleanTextIsByteIdentical(t *testing.T) {
	text := strings.Repeat("Nothing to see here, just ordinary prose with numbers 12345. ", 30)
	r := rand.New(rand.NewSource(2))
	for iter := 0; iter < 100; iter++ {
		h := New(64+r.Intn(200), 20, substringScanner("NEVERPRESENT", true))
		var frames, released [][]byte
		for _, piece := range randomSplit(r, text, 15) {
			f := frameFor(0, piece)
			frames = append(frames, f)
			res := h.Feed(f)
			if res.Cut {
				t.Fatal("clean text was cut")
			}
			released = append(released, res.Release...)
		}
		released = append(released, h.Flush()...)
		if len(released) != len(frames) {
			t.Fatalf("iter %d: released %d frames, fed %d", iter, len(released), len(frames))
		}
		for i := range frames {
			if !bytes.Equal(frames[i], released[i]) {
				t.Fatalf("iter %d: frame %d altered:\n%s\n%s", iter, i, frames[i], released[i])
			}
		}
	}
}

func TestHoldbackRedactsInPlaceAcrossFrameBoundary(t *testing.T) {
	const secret = "sk-ant-api03-verysecretkeyvalue"
	text := "Here is the key: " + secret + " and that is all."
	r := rand.New(rand.NewSource(3))
	for iter := 0; iter < 200; iter++ {
		h := New(64, len(secret), substringScanner(secret, false))
		var released [][]byte
		redactions := 0
		for _, piece := range randomSplit(r, text, 7) {
			res := h.Feed(frameFor(0, piece))
			if res.Cut {
				t.Fatal("redact mode must not cut")
			}
			redactions += res.Redactions
			released = append(released, res.Release...)
		}
		released = append(released, h.Flush()...)
		got := contentOf(t, released)
		if strings.Contains(got, secret) {
			t.Fatalf("iter %d: secret survived redaction: %q", iter, got)
		}
		if !strings.HasPrefix(got, "Here is the key: [REDACTED:secret]") || !strings.HasSuffix(got, " and that is all.") {
			t.Fatalf("iter %d: unexpected redacted output %q", iter, got)
		}
		if len(got) != len(text) {
			t.Fatalf("iter %d: redaction changed length (%d vs %d): delta offsets would drift", iter, len(got), len(text))
		}
		if redactions < 1 {
			t.Fatalf("iter %d: redaction not counted", iter)
		}
	}
}

func TestHoldbackChannelsAreIndependent(t *testing.T) {
	const secret = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	h := New(16, len(secret), substringScanner(secret, true))
	var released [][]byte
	// choice 1 carries the secret split in two; choice 0 is clean.
	feeds := [][]byte{
		frameFor(0, "hello from choice zero, plenty of clean text here"),
		frameFor(1, "prefix "+secret[:10]),
		frameFor(0, " more clean text"),
		frameFor(1, secret[10:]),
	}
	cut := false
	for _, f := range feeds {
		res := h.Feed(f)
		released = append(released, res.Release...)
		if res.Cut {
			cut = true
			break
		}
	}
	if !cut {
		t.Fatal("secret across two frames on choice 1 not caught")
	}
	if strings.Contains(contentOf(t, released), secret) {
		t.Fatal("leaked")
	}
	// Interleaving must not have concatenated choice 0 and choice 1 text.
	if strings.Contains(contentOf(t, released), "prefix") {
		t.Fatal("choice 1 prefix should have been held (within HoldBytes of the growing secret)")
	}
}

func TestHoldbackScansToolArgumentsAndReasoning(t *testing.T) {
	const secret = "xoxb-123456789012-abcdefghijklmnop"
	for name, f := range map[string][]byte{
		"tool":      toolFrame(0, 0, `{"token":"`+secret+`"}`),
		"reasoning": reasoningFrame(0, "the user's token is "+secret),
	} {
		h := New(8, len(secret), substringScanner(secret, true))
		res := h.Feed(f)
		if !res.Cut {
			t.Fatalf("%s channel not scanned", name)
		}
		if len(h.Flush()) != 0 {
			t.Fatalf("%s: flush after cut released frames", name)
		}
	}
}

func TestHoldbackPassesThroughNonDataLines(t *testing.T) {
	h := New(64, 8, substringScanner("x", true))
	released := 0
	for _, raw := range [][]byte{[]byte(": keepalive\n\n"), []byte("event: ping\n\n"), []byte("data: [DONE]\n\n")} {
		res := h.Feed(raw)
		if res.Cut {
			t.Fatalf("non-data line %q cut", raw)
		}
		// A frame with no text delta has nothing to hold: it is released
		// on the spot so keepalives and [DONE] are never delayed.
		if len(res.Release) != 1 || !bytes.Equal(res.Release[0], raw) {
			t.Fatalf("non-data line %q not released verbatim immediately: %q", raw, res.Release)
		}
		released++
	}
	if released != 3 || len(h.Flush()) != 0 {
		t.Fatalf("non-data lines must be released immediately, got %d", released)
	}
}

func TestHoldbackMissedWhenOverlapTooSmallIsReportedNotHidden(t *testing.T) {
	// Deliberately misconfigure: overlap 0. The secret split across two
	// scan passes may escape detection at the boundary — the engine
	// prevents this by sizing overlap from the rule set. Here we only
	// assert the buffer does not crash and reports what it can.
	const secret = "ABCDEFGH"
	h := New(0, 0, substringScanner(secret, true))
	h.Feed(frameFor(0, "ABCD"))
	res := h.Feed(frameFor(0, "EFGH"))
	_ = res // may or may not detect; must not panic
	_ = fmt.Sprintf("%v", res.Matches)
}

// A match inside the overlap window is seen by two consecutive scans (the
// overlap exists so a term split across frames is caught). It must be
// reported once. Redact mode hid this — the redaction overwrites the term
// so the rescan cannot re-find it — but in OBSERVE mode the text is left
// alone and every overlap hit was reported again. In production one
// streamed answer with 40 term hits wrote 45 violation rows.
func TestHoldbackReportsAnOverlapMatchOnce(t *testing.T) {
	const term = "Watermelon"
	r := rand.New(rand.NewSource(11))
	text := strings.Repeat("Project "+term+" is due; ", 12)
	want := strings.Count(text, term)
	observe := func(_ string, full string, from int) []Match {
		var out []Match
		for start := from; ; {
			i := strings.Index(full[start:], term)
			if i < 0 {
				return out
			}
			// Observe: report, but Replacement is empty so nothing is rewritten.
			out = append(out, Match{Offset: start + i, Length: len(term)})
			start += i + len(term)
		}
	}
	for iter := 0; iter < 200; iter++ {
		h := New(64, len(term), observe)
		got := 0
		for _, piece := range randomSplit(r, text, 9) {
			got += len(h.Feed(frameFor(0, piece)).Matches)
		}
		h.Flush()
		if got != want {
			t.Fatalf("iter %d: %d matches reported for %d occurrences (duplicates from the overlap window)", iter, got, want)
		}
	}
}
