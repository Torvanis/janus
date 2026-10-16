package classtrace

import (
	"encoding/json"
	"testing"
)

// A session that does not capture bodies must
// never keep classifier wire bodies (they contain the prompt).
func TestBufferStripsBodiesWhenNotCaptured(t *testing.T) {
	b := &Buffer{Bodies: false}
	b.Record(Call{Feature: "prompt_injection", Request: json.RawMessage(`{"state":"secret prompt"}`), Response: json.RawMessage(`{"a":1}`)})
	var late []Call
	got := b.Drain(func(c Call) { late = append(late, c) })
	b.Record(Call{Feature: "usage_insights", Request: json.RawMessage(`{"state":"secret prompt"}`)})
	for _, c := range append(got, late...) {
		if c.Request != nil || c.Response != nil {
			t.Fatalf("body kept: %+v", c)
		}
	}
	if len(got) != 1 || len(late) != 1 {
		t.Fatalf("got %d buffered, %d late", len(got), len(late))
	}
}
