package classify

import (
	"strings"
	"testing"
)

func TestWithContextFramesAFollowUp(t *testing.T) {
	earlier := []Turn{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "My landlord in California kept my deposit. What are my rights?"},
		{Role: "assistant", Content: strings.Repeat("In California a landlord must return it within 21 days. ", 20)},
	}
	got := WithContext(earlier, "What about Ohio?", 0)
	want := []string{
		"Earlier in this conversation (context only):\nUser: My landlord in California kept my deposit.",
		"\nAssistant: In California a landlord must return it",
		"\n\nThe latest message, which is the request to judge:\nUser: What about Ohio?",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, "You are helpful") {
		t.Fatalf("system prompt leaked into the context:\n%s", got)
	}
	// The reply is cut to its start.
	line := strings.Split(got, "\n")[2]
	if n := len([]rune(line)); n > len("Assistant: ")+contextReplyChars+1 {
		t.Fatalf("reply not capped (%d): %q", n, line)
	}
}

func TestWithContextFirstTurnIsUnchanged(t *testing.T) {
	if got := WithContext(nil, "  Hello there ", 0); got != "Hello there" {
		t.Fatalf("got %q", got)
	}
	// Only a system prompt before it: still a first turn.
	if got := WithContext([]Turn{{Role: "system", Content: "x"}}, "Hi", 0); got != "Hi" {
		t.Fatalf("got %q", got)
	}
	if got := WithContext([]Turn{{Role: "user", Content: "a"}}, "  ", 0); got != "" {
		t.Fatalf("empty latest: %q", got)
	}
}

func TestWithContextKeepsTheNewestTurnsAndTheLimit(t *testing.T) {
	var earlier []Turn
	for i := 0; i < 10; i++ {
		earlier = append(earlier, Turn{Role: "user", Content: "question " + string(rune('A'+i))}, Turn{Role: "assistant", Content: "answer " + string(rune('A'+i))})
	}
	got := WithContext(earlier, "and then?", 0)
	if strings.Contains(got, "question G") || !strings.Contains(got, "question H") || !strings.Contains(got, "answer J") {
		t.Fatalf("want the last %d messages only:\n%s", ContextTurns, got)
	}
	// A bound: the latest keeps its tail and the context gives way first.
	long := strings.Repeat("x", 900)
	got = WithContext(earlier, long, 1000)
	if n := len([]rune(got)); n > 1000 {
		t.Fatalf("over the bound: %d", n)
	}
	if !strings.HasSuffix(got, long) {
		t.Fatalf("latest message cut to make room for context")
	}
	// No room for useful context: the latest alone.
	if got := WithContext(earlier, strings.Repeat("y", 990), 1000); got != strings.Repeat("y", 990) {
		t.Fatalf("expected the latest alone, got %d runes", len([]rune(got)))
	}
}

func TestParseHistory(t *testing.T) {
	got := ParseHistory("User: hi\nthere\n\nAssistant: hello\nuser: what about Ohio?\nloose line")
	want := []Turn{{"user", "hi\nthere"}, {"assistant", "hello"}, {"user", "what about Ohio?\nloose line"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("turn %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	if got := ParseHistory("no prefix at all"); len(got) != 1 || got[0].Role != "user" {
		t.Fatalf("got %+v", got)
	}
}

func TestExtractTextContextScope(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"Refactor our Go scheduler"},{"role":"assistant","content":"Here is step one"},{"role":"user","content":"Keep going"}]}`)
	got := ExtractText(body, ScopeContext, false)
	if !strings.Contains(got, "User: Refactor our Go scheduler\nAssistant: Here is step one") || !strings.HasSuffix(got, "User: Keep going") {
		t.Fatalf("got:\n%s", got)
	}
	if ExtractText(body, ScopeLastUser, false) != "Keep going" {
		t.Fatalf("last_user changed")
	}
	first := []byte(`{"messages":[{"role":"user","content":"Hello"}]}`)
	if got := ExtractText(first, ScopeContext, false); got != "Hello" {
		t.Fatalf("first turn: %q", got)
	}
}
