package ratecards

import "testing"

func TestReferenceSeedParses(t *testing.T) {
	entries, updated, err := Reference()
	if err != nil {
		t.Fatalf("bundled seed does not parse: %v", err)
	}
	if len(entries) < 10 {
		t.Fatalf("expected a useful seed, got %d entries", len(entries))
	}
	if updated == "" {
		t.Error("seed has no updated date")
	}
	for _, e := range entries {
		if e.Model == "" {
			t.Error("entry with empty model name")
		}
		if e.InUSDPerMTok < 0 || e.OutUSDPerMTok < 0 || e.CachedUSDPerMTok < 0 {
			t.Errorf("%s has a negative rate", e.Model)
		}
		if e.InUSDPerMTok == 0 && e.OutUSDPerMTok == 0 {
			t.Errorf("%s has no positive rate — an all-zero card cannot gate model enablement", e.Model)
		}
		if e.ContextWindowTokens <= 0 {
			t.Errorf("%s has no context_window_tokens — every bundled entry must carry the provider's published context window", e.Model)
		}
	}
}

// TestReferenceSeedContextWindows pins the context windows the seed ships for
// representative models — the values apply copies onto matching models — and
// that alias matching resolves to the same entry.
func TestReferenceSeedContextWindows(t *testing.T) {
	cases := []struct {
		model  string
		tokens int64
	}{
		{"gpt-4o", 128000},
		{"gpt-4o-mini", 128000},
		{"gpt-4.1", 1047576},
		{"o3-mini", 200000},
		{"claude-sonnet-5", 200000},
		{"claude-fable-5-latest", 200000}, // alias resolution
		{"mistral-small-latest", 32000},
		{"text-embedding-3-small", 8191},
	}
	for _, tc := range cases {
		e := Match(tc.model)
		if e == nil {
			t.Errorf("seed has no entry for %s", tc.model)
			continue
		}
		if e.ContextWindowTokens != tc.tokens {
			t.Errorf("%s context_window_tokens = %d, want %d", tc.model, e.ContextWindowTokens, tc.tokens)
		}
	}
}

func TestMatch(t *testing.T) {
	if Match("gpt-4o") == nil {
		t.Error("exact match failed")
	}
	if Match("GPT-4O") == nil {
		t.Error("case-insensitive match failed")
	}
	if Match("claude-3-5-sonnet-latest") == nil {
		t.Error("alias match failed")
	}
	if Match("totally-unknown-model") != nil {
		t.Error("unknown model must not match")
	}
}
