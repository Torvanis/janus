package chatsearch

import (
	"reflect"
	"strings"
	"testing"
)

func TestStem(t *testing.T) {
	cases := map[string]string{
		"searching": "search",
		"searched":  "search",
		"searches":  "search",
		"search":    "search",
		"quickly":   "quick",
		"boxes":     "box",
		"cats":      "cat",
		"class":     "class", // ss keeps its s
		"glasses":   "glass",
		"is":        "is", // stem would be too short
		"bed":       "bed",
		"sing":      "sing",
		"uses":      "use", // "es" would leave "us" (too short), "s" leaves "use"
		"use":       "use",
		"größes":    "größ",
		"paddle":    "paddl",
		"paddling":  "paddl",
		"planning":  "plan",
		"stopped":   "stop",
		"falling":   "fall",
		"making":    "mak",
		"make":      "mak",
		"trees":     "tre",
	}
	for in, want := range cases {
		if got := Stem(in); got != want {
			t.Errorf("Stem(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTerms(t *testing.T) {
	got := Terms("Searching the Docs: searched DOCS again; x 42 café-crème a")
	want := []string{"search", "the", "doc", "again", "42", "café", "crèm"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Terms = %q, want %q", got, want)
	}
	long := strings.Repeat("a", MaxTermRunes+1)
	if got := Terms(long + " ok " + strings.Repeat("b", MaxTermRunes)); len(got) != 2 || got[0] != "ok" {
		t.Fatalf("length bounds: %q", got)
	}
	var b strings.Builder
	for i := 0; i < MaxTermsPerMessage+50; i++ {
		b.WriteString("w")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString(itoa(i))
		b.WriteString(" ")
	}
	if got := Terms(b.String()); len(got) != MaxTermsPerMessage {
		t.Fatalf("cap: %d terms", len(got))
	}
	if got := Terms("  ,,, !  "); len(got) != 0 {
		t.Fatalf("punctuation only: %q", got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for ; i > 0; i /= 10 {
		s = string(rune('0'+i%10)) + s
	}
	return s
}

func TestHashIsKeyedAndSaltedPerUser(t *testing.T) {
	k1, k2 := []byte("key-one-key-one-key-one-key-one!"), []byte("key-two-key-two-key-two-key-two!")
	a := Hash(k1, "user-a", "secret")
	if len(a) != 32 {
		t.Fatalf("len = %d", len(a))
	}
	if a != Hash(k1, "user-a", "secret") {
		t.Fatal("not deterministic")
	}
	if a == Hash(k1, "user-b", "secret") {
		t.Fatal("same word for two users hashes identically")
	}
	if a == Hash(k2, "user-a", "secret") {
		t.Fatal("hash does not depend on the key")
	}
	if a == Hash(k1, "user-a", "secrets") {
		t.Fatal("different terms collide")
	}
	// The separator keeps (user "ab", term "c") apart from (user "a", term "bc").
	if Hash(k1, "ab", "c") == Hash(k1, "a", "bc") {
		t.Fatal("user/term boundary is ambiguous")
	}
	if strings.Contains(a, "secret") {
		t.Fatal("plaintext in hash")
	}
	if got := Hashes(k1, "user-a", []string{"secret", "x"}); len(got) != 2 || got[0] != a {
		t.Fatalf("Hashes = %v", got)
	}
}

func TestSnippet(t *testing.T) {
	text := strings.Repeat("filler words here. ", 20) + "The Searching party found it." + strings.Repeat(" tail text", 30)
	s := Snippet(text, []string{"search"}, 80)
	if !strings.Contains(s, "Searching party") || !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") {
		t.Fatalf("snippet = %q", s)
	}
	if n := len([]rune(s)); n > 82 {
		t.Fatalf("snippet too long: %d", n)
	}
	// Matches only at word starts: "research" does not match "search".
	s = Snippet("research first, then search", []string{"search"}, 10)
	if !strings.Contains(s, "search") || strings.Contains(s, "research") {
		t.Fatalf("word-start snippet = %q", s)
	}
	// No match: the start of the text.
	if s := Snippet("short text", []string{"zzz"}, 160); s != "short text" {
		t.Fatalf("no-match snippet = %q", s)
	}
}
