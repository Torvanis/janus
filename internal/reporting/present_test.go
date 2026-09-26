package reporting

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func namedFixture() Result {
	r := fixture()
	r.Definition.Name = "Demo — Executive overview"
	r.Columns = []Column{{"team", "team", ""}, {"team_label", "team name (current)", ""}, {"requests", "Requests", "count"}, {"tokens_out", "Output tokens", "tokens"}, {"cost_usd", "Recorded cost (USD)", "USD"}, {"success_rate", "Success rate", "ratio"}}
	r.Rows = []Row{{Dimensions: map[string]string{"team": "0b6f1c9e-opaque", "team_label": "Platform"}, Values: map[string]*float64{"requests": ptr(1500), "tokens_out": ptr(2637931), "cost_usd": ptr(278.650296), "success_rate": ptr(.975)}}}
	r.Sections = []Section{{ID: "cost_coverage", Title: "Recorded cost confidence", Columns: []Column{{"priced_requests", "priced requests", "count"}}, Rows: []Row{{Values: map[string]*float64{"priced_requests": ptr(5)}}}}}
	return r
}

func TestCSVReaderTable(t *testing.T) {
	var b bytes.Buffer
	if err := Export(&b, namedFixture(), "csv"); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(&b).ReadAll() // strict: all rows same width
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"Team", "Requests", "Output tokens", "Recorded cost (USD)", "Success rate (%)"}, {"Platform", "1500", "2637931", "278.650296", "97.5"}}
	if len(records) != 2 || strings.Join(records[0], "|") != strings.Join(want[0], "|") || strings.Join(records[1], "|") != strings.Join(want[1], "|") {
		t.Fatalf("got %q", records)
	}
	if strings.Contains(b.String(), "0b6f1c9e") {
		t.Fatal("opaque identity in CSV")
	}
}

func TestXLSXReaderWorkbook(t *testing.T) {
	text := exportText(t, namedFixture(), "xlsx")
	for _, want := range []string{"Platform", "Team", "Output tokens", "Recorded cost (USD)", "Success rate", "Priced requests", "Recorded cost confidence", "Headline totals", "Prior period"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q", want)
		}
	}
	for _, bad := range []string{"0b6f1c9e", "(USD) (USD)", "team name (current)", "cost_usd", "Output tokens (tokens)"} {
		if strings.Contains(text, bad) {
			t.Fatalf("workbook shows %q", bad)
		}
	}
}

func TestPresentationFormatting(t *testing.T) {
	cases := map[string]string{
		formatValue(ptr(3909.0827), "USD"):                                               "$3,909.08",
		formatValue(ptr(-1234.5), "USD"):                                                 "-$1,234.50",
		formatValue(ptr(.0042), "USD"):                                                   "$0.0042",
		formatValue(ptr(2637931), "tokens"):                                              "2,637,931",
		formatValue(ptr(.975), "ratio"):                                                  "97.5%",
		formatValue(nil, "USD"):                                                          "Unavailable",
		compactValue(ptr(2637931), "count"):                                              "2.6M",
		headerWithUnit(Column{"cost_usd", "Recorded cost (USD)", "USD"}, false):          "Recorded cost (USD)",
		headerWithUnit(Column{"tokens_out", "Output tokens", "tokens"}, false):           "Output tokens",
		headerWithUnit(Column{"x", "disabled cost requests", "count"}, false):            "Disabled cost requests",
		headerWithUnit(Column{"team_label", "team name (current)", ""}, false):           "Team",
		headerWithUnit(Column{"latency_p95_ms", "95th-percentile latency", "ms"}, false): "95th-percentile latency (ms)",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
	text, dir := changeSummary(ptr(3909.08), ptr(1952.65), "USD")
	if dir != 1 || !strings.HasPrefix(text, "+100.2% vs prior") {
		t.Fatalf("change %q %d", text, dir)
	}
}

func TestDownloadName(t *testing.T) {
	r := namedFixture()
	r.GeneratedAt = time.Date(2026, 9, 20, 2, 0, 0, 0, time.UTC)
	if got := DownloadName(r, "pdf"); got != "janus-demo-executive-overview-2026-09-20.pdf" {
		t.Fatal(got)
	}
	r.Definition.Name = "Équipe / 模型"
	if got := DownloadName(r, "xlsx"); got != "janus-quipe-2026-09-20.xlsx" {
		t.Fatal(got)
	}
	r.Definition.Name = "模型"
	if got := DownloadName(r, "csv"); got != "janus-report-2026-09-20.csv" {
		t.Fatal(got)
	}
}

func TestReaderNoticesMergePriorPeriodDuplicates(t *testing.T) {
	r := fixture()
	r.Warnings = []string{"A.", "Previous period: A.", "B."}
	r.ComparisonWarnings = []string{"Previous period: A.", "Previous period: C."}
	got := strings.Join(readerNotices(r), "|")
	if got != "A. (Applies to this and the prior period.)|B.|Prior period: C." {
		t.Fatal(got)
	}
}
