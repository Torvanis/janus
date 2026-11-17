package store

import (
	"testing"
	"unicode/utf8"
)

// Usage rows carry text a caller chose (path, headers, model name). A value
// PostgreSQL refuses (invalid UTF-8, a NUL byte) would lose the record of a
// request that was already served, so every text argument is made storable.
func TestUsageArgsAreStorable(t *testing.T) {
	e := &UsageEvent{
		ID: "e1", EndpointPath: "/v1/\xff\xfe", ModelName: "m\x00odel", UserAgent: "ok agent",
		Referer: "caf\xc3", ClientApp: "naïve", RequestID: "r\x00\xff",
	}
	seen := map[string]bool{}
	for _, a := range usageArgs(e) {
		s, ok := a.(string)
		if !ok {
			continue
		}
		seen[s] = true
		if !utf8.ValidString(s) {
			t.Errorf("argument is not valid UTF-8: %q", s)
		}
		for i := 0; i < len(s); i++ {
			if s[i] == 0 {
				t.Errorf("argument holds a NUL byte: %q", s)
			}
		}
	}
	for _, want := range []string{"ok agent", "naïve", "model", "/v1/\uFFFD"} {
		if !seen[want] {
			t.Errorf("expected stored value %q, have %v", want, seen)
		}
	}
}
