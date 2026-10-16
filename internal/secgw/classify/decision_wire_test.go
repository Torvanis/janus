package classify

import "testing"

func TestDefaultWireModel(t *testing.T) {
	cases := []struct{ name, base, want string }{
		{"typesafe/jev-router", "https://openrouter.ai/api", "~typesafe/jev-latest"},
		{"typesafe/jev-router", "https://example.com/v1", "typesafe/jev-router"},
		{"clef-flash", "https://openrouter.ai/api", "clef-flash"},
	}
	for _, c := range cases {
		if got := defaultWireModel(c.name, c.base); got != c.want {
			t.Errorf("defaultWireModel(%q, %q) = %q, want %q", c.name, c.base, got, c.want)
		}
	}
}
