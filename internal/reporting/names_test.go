package reporting

import "testing"

func TestRedactionNamesFailClosed(t *testing.T) {
	for _, d := range GetCatalog().Templates {
		original := d.Name
		for _, name := range []string{"Revenue $987.65", original + " $987.65", "My report", "", " " + original} {
			d.Name = name
			got := RedactCosts(Result{Definition: d})
			if got.Definition.Name != "Usage report (costs disabled)" {
				t.Fatalf("%s: arbitrary name %q retained as %q", d.Template, name, got.Definition.Name)
			}
			if d.Name != name {
				t.Fatal("source mutated")
			}
		}
	}
	for _, template := range []string{"", "unknown", "usage $987.65"} {
		got := RedactCosts(Result{Definition: Definition{Template: template, Name: "Usage and allocation"}})
		if got.Definition.Name != "Usage report (costs disabled)" {
			t.Fatalf("unknown template %q bypassed redaction", template)
		}
	}
}
