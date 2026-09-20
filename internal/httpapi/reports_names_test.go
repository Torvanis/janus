package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/reporting"
)

func TestLocalOnlyReportNamesCatalogAndGeneratedResults(t *testing.T) {
	h := newHarness(t)
	h.server.Config.LocalOnly = true
	rr := h.do("GET", "/api/v1/reports/catalog", nil)
	if rr.Code != 200 {
		t.Fatalf("catalog: %d %s", rr.Code, rr.Body)
	}
	var catalog reporting.Catalog
	if err := json.Unmarshal(rr.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	wants := map[string]string{
		"team_usage": "Team usage",
		"executive":  "Executive overview", "usage": "Usage and allocation", "adoption": "Adoption and engagement", "portfolio": "Model portfolio", "quotas": "Quotas", "reliability": "Reliability and performance", "efficiency": "Efficiency and optimization", "integrations": "Applications and integrations", "governance": "Governance and security", "data_quality": "Data quality and reconciliation",
	}
	if len(catalog.Templates) != 11 {
		t.Fatalf("templates: %d", len(catalog.Templates))
	}
	seen := map[string]bool{}
	for _, d := range catalog.Templates {
		t.Run(d.Template, func(t *testing.T) {
			want := wants[d.Template]
			if want == "" || d.Name != want || seen[d.Name] {
				t.Fatalf("catalog name %q want %q (duplicate=%v)", d.Name, want, seen[d.Name])
			}
			seen[d.Name] = true
			scope, err := h.store.ResolveReportScope(context.Background(), h.user.ID, d)
			if err != nil {
				t.Fatal(err)
			}
			scope.HideCosts = true
			result, err := h.store.QueryReport(context.Background(), d, scope)
			if err != nil {
				t.Fatal(err)
			}
			if result.Definition.Name != want {
				t.Fatalf("generated name %q want %q", result.Definition.Name, want)
			}
			resultCopy := reporting.RedactCosts(*result)
			if resultCopy.Definition.Name != want {
				t.Fatal("repeated redaction lost name")
			}
			raw, err := json.Marshal(resultCopy)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "cost_usd") {
				t.Fatal("generated costs leaked")
			}
			t.Logf("HTTP catalog and generated result: %s = %s", d.Template, want)
		})
	}
	if strings.Contains(rr.Body.String(), "cost_usd") {
		t.Fatal("catalog costs leaked")
	}
}
