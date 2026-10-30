package stream

import (
	"strings"
	"testing"
)

func frameOf(text string) []byte {
	return []byte(`data: {"choices":[{"index":0,"delta":{"content":` + quote(text) + `}}]}` + "\n")
}

func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func TestParagraphGateReleasesOnlyJudgedParagraphs(t *testing.T) {
	var judged []string
	g := NewParagraphGate(func(_, prior, chunk string) (bool, any) {
		judged = append(judged, chunk)
		return strings.Contains(chunk, "BAD"), "tag"
	})
	var out []string
	feed := func(raw []byte) GateResult {
		r := g.Feed(raw)
		for _, f := range r.Release {
			out = append(out, string(f))
		}
		return r
	}
	feed(frameOf("First para"))
	feed([]byte("\n")) // SSE blank line queued behind it
	if len(out) != 0 {
		t.Fatalf("unjudged text released: %q", out)
	}
	feed(frameOf("graph done.\n\nSecond "))
	// The first paragraph is judged; its frame is released. The frame
	// carrying "Second " straddles the boundary and must wait.
	if len(judged) != 1 || judged[0] != "First paragraph done.\n\n" {
		t.Fatalf("judged %q", judged)
	}
	if len(out) != 2 || !strings.Contains(out[0], "First para") || out[1] != "\n" {
		t.Fatalf("released %q", out)
	}
	r := feed(frameOf("is BAD.\n\n"))
	if !r.Cut || r.Tag != "tag" {
		t.Fatalf("block not reported: %+v", r)
	}
	for _, f := range out {
		if strings.Contains(f, "Second") || strings.Contains(f, "BAD") {
			t.Fatalf("blocked paragraph leaked: %q", out)
		}
	}
	if f := g.Flush(); !f.Cut || len(f.Release) != 0 {
		t.Fatalf("flush after cut released %+v", f)
	}
}

func TestParagraphGateFlushJudgesTheTail(t *testing.T) {
	calls := 0
	g := NewParagraphGate(func(_, prior, chunk string) (bool, any) { calls++; return false, nil })
	if r := g.Feed(frameOf("no breaks at all")); len(r.Release) != 0 {
		t.Fatal("released before judged")
	}
	g.Feed([]byte("data: [DONE]\n"))
	r := g.Flush()
	if calls != 1 || len(r.Release) != 2 || !strings.Contains(string(r.Release[1]), "[DONE]") {
		t.Fatalf("flush: calls=%d release=%q", calls, r.Release)
	}
}

func TestParagraphGateChunksLongUnbrokenText(t *testing.T) {
	var sizes []int
	g := NewParagraphGate(func(_, _, chunk string) (bool, any) { sizes = append(sizes, len(chunk)); return false, nil })
	for i := 0; i < 40; i++ {
		g.Feed(frameOf(strings.Repeat("word ", 20) + "end. "))
	}
	if len(sizes) == 0 {
		t.Fatal("a long reply without paragraph breaks was never judged before the end")
	}
	for _, n := range sizes {
		if n > DefaultHardBytes+200 {
			t.Fatalf("chunk of %d bytes", n)
		}
	}
	// Prior context is passed.
	var prior string
	g2 := NewParagraphGate(func(_, p, _ string) (bool, any) { prior = p; return false, nil })
	g2.Feed(frameOf("one\n\n"))
	g2.Feed(frameOf("two\n\n"))
	if prior != "one\n\n" {
		t.Fatalf("prior %q", prior)
	}
}
