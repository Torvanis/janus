package adapter

import "testing"

// TestAnthropicMarksCacheDisjoint pins the provider-semantics half of the
// cache-inversion fix. Anthropic reports cache_read_input_tokens DISJOINT from
// input_tokens; if this flag is ever dropped, ComputeCostAll silently clamps
// the cached prefix away and undercharges every cache hit.
func TestAnthropicMarksCacheDisjoint(t *testing.T) {
	a := &Anthropic{}
	body := []byte(`{"type":"message","stop_reason":"end_turn","usage":{
		"input_tokens":16,"output_tokens":4,"cache_read_input_tokens":4183}}`)

	u, ok := a.ExtractUsage(body)
	if !ok {
		t.Fatal("expected usage to be reported")
	}
	if !u.CachedDisjoint {
		t.Fatal("Anthropic usage must set CachedDisjoint: cache reads are additive, not a subset")
	}
	if u.TokensCached != 4183 {
		t.Fatalf("TokensCached = %d, want 4183", u.TokensCached)
	}
	if u.TokensIn != 16 {
		t.Fatalf("TokensIn = %d, want 16 (must not absorb the cached count)", u.TokensIn)
	}
}

// TestAnthropicStreamMarksCacheDisjoint covers the streaming path, where usage
// is split across message_start (inputs) and message_delta (outputs).
func TestAnthropicStreamMarksCacheDisjoint(t *testing.T) {
	a := &Anthropic{}
	c := a.NewStreamCollector()
	c.Feed([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":16,"cache_read_input_tokens":4183,"output_tokens":1}}}`))
	c.Feed([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`))

	u := c.Usage()
	if !u.CachedDisjoint {
		t.Fatal("streamed Anthropic usage must set CachedDisjoint")
	}
	if u.TokensCached != 4183 {
		t.Fatalf("TokensCached = %d, want 4183", u.TokensCached)
	}
	if u.TokensOut != 4 {
		t.Fatalf("TokensOut = %d, want 4", u.TokensOut)
	}
}

// TestOpenAIDoesNotMarkCacheDisjoint pins the other side of the contract: the
// OpenAI convention is subset semantics and must stay the default.
func TestOpenAIDoesNotMarkCacheDisjoint(t *testing.T) {
	a := &OpenAICompatible{}
	body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":50,
		"prompt_tokens_details":{"cached_tokens":800}}}`)

	u, ok := a.ExtractUsage(body)
	if !ok {
		t.Fatal("expected usage to be reported")
	}
	if u.CachedDisjoint {
		t.Fatal("OpenAI cached_tokens is a SUBSET of prompt_tokens; must not be marked disjoint")
	}
}
