package reporting

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func ptr(v float64) *float64 { return &v }
func fixture() Result {
	return Result{Version: 1, Definition: GetCatalog().Templates[0], GeneratedAt: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC), DataCutoff: time.Date(2026, 3, 10, 11, 0, 0, 0, time.UTC), Start: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), Columns: []Column{{"user", "User", ""}, {"requests", "Requests", "count"}, {"cost_usd", "Cost", "USD"}}, Rows: []Row{{Dimensions: map[string]string{"user": "\t=HYPERLINK(\"evil\")"}, Values: map[string]*float64{"requests": ptr(7), "cost_usd": nil}}}, Totals: map[string]*float64{"requests": ptr(7)}, PreviousTotals: map[string]*float64{"requests": ptr(4)}, Warnings: []string{"warning-one", "warning-two"}, Sections: []Section{{ID: "usage", Title: "Usage", Columns: []Column{{"model", "Model", ""}, {"requests", "Requests", "count"}}, Rows: []Row{{Dimensions: map[string]string{"model": "模型"}, Values: map[string]*float64{"requests": ptr(7)}}}, Notes: []string{"section-note"}}}, SourceRows: 7}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken") }
func TestJSONAndCSVExportFrozenResult(t *testing.T) {
	r := fixture()
	before, _ := json.Marshal(r)
	var b bytes.Buffer
	if err := Export(&b, r, "json"); err != nil {
		t.Fatal(err)
	}
	var got Result
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, got) {
		t.Fatal("JSON is not exact full result")
	}
	b.Reset()
	if err := Export(&b, r, "csv"); err != nil {
		t.Fatal(err)
	}
	reader := csv.NewReader(&b)
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	all := fmtRecords(records)
	for _, s := range []string{"'\t=HYPERLINK", "warning-one", "warning-two", "section-note", "模型", "USD", "data_cutoff", "timezone", "filters", "previous_totals", "2026-02-01"} {
		if !strings.Contains(all, s) {
			t.Errorf("missing %q in %s", s, all)
		}
	}
	after, _ := json.Marshal(r)
	if !bytes.Equal(before, after) {
		t.Fatal("mutated frozen result")
	}
	for _, f := range []string{"csv", "json"} {
		if Export(brokenWriter{}, r, f) == nil {
			t.Errorf("%s swallowed write error", f)
		}
	}
	if Export(&b, r, "bogus") == nil || ContentType("bogus") != "" || Extension("bogus") != "" {
		t.Fatal("unknown format accepted")
	}
	if ContentType("csv") != "text/csv; charset=utf-8" || Extension("json") != ".json" {
		t.Fatal("format mapping")
	}
}
func TestXLSXRealWorkbookAllData(t *testing.T) {
	r := fixture()
	r.Sections = append(r.Sections, r.Sections[0])
	r.Sections[1].Title = "Usage[]:/\\?*"
	var b bytes.Buffer
	if err := Export(&b, r, "xlsx"); err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	texts := ""
	sheets := 0
	workbook := ""
	for _, f := range z.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		if strings.HasSuffix(f.Name, ".xml") || strings.HasSuffix(f.Name, ".rels") {
			d := xml.NewDecoder(bytes.NewReader(data))
			for {
				_, err := d.Token()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("%s: %v", f.Name, err)
				}
			}
		}
		if f.Name == "xl/workbook.xml" {
			workbook = string(data)
		}
		if strings.HasPrefix(f.Name, "xl/worksheets/") {
			sheets++
			texts += string(data)
			if strings.Contains(string(data), "<f>") {
				t.Fatal("formula emitted")
			}
		}
	}
	if sheets != 5 {
		t.Fatalf("sheets %d", sheets)
	}
	for _, s := range []string{"Overview", "Data", "Methodology"} {
		if !strings.Contains(workbook, s) {
			t.Fatal("missing sheet", s)
		}
	}
	for _, s := range []string{"模型", "HYPERLINK", "section-note", "data_cutoff", "timezone", "USD", "inlineStr"} {
		if !strings.Contains(texts, s) {
			t.Fatal("missing", s)
		}
	}
	if strings.Count(texts, "warning-one") != 1 || strings.Count(texts, "warning-two") != 1 {
		t.Fatal("warnings lost or duplicated")
	}
	if Export(brokenWriter{}, r, "xlsx") == nil {
		t.Fatal("swallowed error")
	}
}

func TestSummaryUsesDeclaredMonetaryUnits(t *testing.T) {
	r := fixture()
	r.Columns = append(r.Columns, Column{"remaining", "Remaining", "USD"})
	r.Totals["remaining"] = ptr(12)
	text := exportText(t, r, "pdf")
	for _, want := range []string{"Remaining", "$12.00", "USD"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing declared monetary presentation %q", want)
		}
	}
}

func TestPDFMainDataChartPreservesCompleteDetail(t *testing.T) {
	r := fixture()
	r.Rows = nil
	r.Columns = []Column{{"user", "User", ""}, {"missing", "Missing", "count"}, {"requests", "Requests", "count"}}
	for i := 1; i <= 160; i++ {
		r.Rows = append(r.Rows, Row{Dimensions: map[string]string{"user": fmt.Sprintf("group-%03d", i)}, Values: map[string]*float64{"missing": nil, "requests": ptr(float64(i))}})
	}
	before, _ := json.Marshal(r)
	text := exportText(t, r, "pdf")
	end := strings.Index(text, "Complete report data")
	if end < 0 {
		t.Fatal("missing complete appendix")
	}
	chart := text[:end]
	for _, want := range []string{"Category ranking", "top 8 of 160", "group-160", "group-153"} {
		if !strings.Contains(chart, want) {
			t.Fatalf("ranking missing %q", want)
		}
	}
	if strings.Contains(chart, "group-001") {
		t.Fatal("ranking used report order instead of value order")
	}
	for i := 1; i <= 160; i++ {
		if !strings.Contains(text[end:], fmt.Sprintf("group-%03d", i)) {
			t.Fatalf("detail lost group %d", i)
		}
	}
	var a, b bytes.Buffer
	if err := Export(&a, r, "pdf"); err != nil {
		t.Fatal(err)
	}
	if err := Export(&b, r, "pdf"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("PDF bytes are nondeterministic")
	}
	after, _ := json.Marshal(r)
	if !bytes.Equal(before, after) {
		t.Fatal("chart mutated frozen result")
	}
}

func TestPDFMainChartUnavailableAndZeroValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []Row
		want string
	}{
		{"empty", nil, "Chart unavailable: no main data rows."},
		{"nil", []Row{{Values: map[string]*float64{"requests": nil}}}, "Chart unavailable: no non-null metric in declared columns."},
		{"zero", []Row{{Dimensions: map[string]string{"user": "zero-row"}, Values: map[string]*float64{"requests": ptr(0)}}}, "zero-row"},
		{"negative", []Row{{Dimensions: map[string]string{"user": "negative-row"}, Values: map[string]*float64{"requests": ptr(-3)}}}, "-3"},
		{"mixed-null", []Row{{Dimensions: map[string]string{"user": "valid-row"}, Values: map[string]*float64{"requests": ptr(3)}}, {Dimensions: map[string]string{"user": "null-row"}, Values: map[string]*float64{"requests": nil}}}, "Unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixture()
			r.Sections = nil
			r.Rows = tc.rows
			text := exportText(t, r, "pdf")
			if !strings.Contains(text, tc.want) {
				t.Fatalf("missing %q", tc.want)
			}
		})
	}
}

func TestPDFTableHeadersAlignWithData(t *testing.T) {
	var b bytes.Buffer
	if err := Export(&b, fixture(), "pdf"); err != nil {
		t.Fatal(err)
	}
	text := pdfVisibleText(t, b.Bytes())
	for _, want := range []string{"User", "Requests", "Cost", "HYPERLINK", "Unavailable"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing cell %q", want)
		}
	}
	streams := pdfStreams(t, b.Bytes())
	rects := 0
	for _, s := range streams {
		rects += strings.Count(string(s), " re ")
	}
	if rects < 10 {
		t.Fatal("tables are not native rectangular cells")
	}
	if strings.Contains(text, "columns 1-4") || strings.Contains(text, "-----") {
		t.Fatal("legacy split-band text dump")
	}
}

func TestPDFPaginatedValidXrefAndComplete(t *testing.T) {
	r := fixture()
	for i := 0; i < 160; i++ {
		r.Rows = append(r.Rows, Row{Dimensions: map[string]string{"user": fmt.Sprintf("person-%03d (test) \\ end", i)}, Values: map[string]*float64{"requests": ptr(float64(i))}})
	}
	var b bytes.Buffer
	if err := Export(&b, r, "pdf"); err != nil {
		t.Fatal(err)
	}
	s := b.String()
	if !strings.HasPrefix(s, "%PDF-1.") || !strings.HasSuffix(s, "%%EOF\n") {
		t.Fatal("not PDF")
	}
	if strings.Count(s, "/Type /Page\n") < 3 {
		t.Fatal("not paginated")
	}
	text := pdfVisibleText(t, b.Bytes())
	for _, want := range []string{"person-159", "warning-one", "warning-two", "section-note", "U+6A21", "data_cutoff", "timezone", "USD", "Page 1 of"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing visible %s", want)
		}
	}
	start := strings.LastIndex(s, "startxref\n")
	offset, err := strconv.Atoi(strings.Split(s[start+10:], "\n")[0])
	if err != nil || !strings.HasPrefix(s[offset:], "xref\n") {
		t.Fatal("invalid xref pointer")
	}
	lines := strings.Split(s[offset:], "\n")
	var first, count int
	fmt.Sscanf(lines[1], "%d %d", &first, &count)
	for i := 1; i < count; i++ {
		var off int
		fmt.Sscanf(lines[i+2], "%d", &off)
		if !strings.HasPrefix(s[off:], fmt.Sprintf("%d 0 obj\n", i)) {
			t.Fatalf("bad object offset %d", i)
		}
	}
	raw, _ := json.Marshal(r)
	found := false
	for _, stream := range pdfStreams(t, b.Bytes()) {
		if bytes.Equal(bytes.TrimSpace(stream), raw) {
			found = true
		}
	}
	if !found || !strings.Contains(s, "/EmbeddedFile") {
		t.Fatal("missing byte-exact lossless source attachment")
	}
	if Export(brokenWriter{}, r, "pdf") == nil {
		t.Fatal("swallowed write error")
	}
}

func TestExportRejectsLossyInputs(t *testing.T) {
	for _, format := range []string{"csv", "xlsx", "pdf", "json"} {
		t.Run(format, func(t *testing.T) {
			r := fixture()
			r.Totals["requests"] = ptr(math.NaN())
			var b bytes.Buffer
			if Export(&b, r, format) == nil {
				t.Fatal("non-finite result accepted")
			}
			r = fixture()
			r.Rows[0].Dimensions["user"] = string([]byte{0xff})
			if Export(&b, r, format) == nil {
				t.Fatal("invalid UTF-8 silently replaced")
			}
			if Export(shortWriter{}, fixture(), format) == nil {
				t.Fatal("short write swallowed")
			}
		})
	}
	r := fixture()
	r.Rows[0].Dimensions["requests"] = "conflicting"
	var b bytes.Buffer
	for _, format := range []string{"csv", "xlsx", "pdf"} {
		if Export(&b, r, format) == nil {
			t.Fatal("ambiguous row silently loses values", format)
		}
	}
	r = fixture()
	r.Rows[0].Dimensions["user"] = "NUL\x00text"
	if Export(&b, r, "xlsx") == nil {
		t.Fatal("XML NUL silently replaced")
	}
	r = fixture()
	r.Rows[0].Dimensions["user"] = strings.Repeat("😀", 20000)
	if Export(&b, r, "xlsx") == nil {
		t.Fatal("UTF-16 Excel cell limit not enforced")
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestRedactCostsDeepCopyAndNoMonetaryLeak(t *testing.T) {
	scope := Scope{HideCosts: true}
	if !scope.HideCosts {
		t.Fatal("scope flag")
	}
	r := fixture()
	r.Definition.Name = "Savings 987.65"
	r.Definition.ScenarioDiscountPercent = 20
	r.Totals["cost_usd"] = ptr(987.65)
	r.PreviousTotals["cost_usd"] = ptr(987.65)
	r.Rows[0].Values["cost_usd"] = ptr(987.65)
	r.Sections[0].Columns = append(r.Sections[0].Columns, Column{"remaining", "Remaining", "USD"}, Column{"budget_percent", "Budget used", "ratio"})
	r.Sections[0].Rows[0].Values["remaining"] = ptr(987.65)
	r.Sections[0].Rows[0].Values["budget_percent"] = ptr(20)
	r.Sections[0].Notes = []string{"987.65 remaining", "Scenario saves 987.65"}
	r.Warnings = append(r.Warnings, "987.65 outstanding")
	before, _ := json.Marshal(r)
	got := RedactCosts(r)
	out, _ := json.Marshal(got)
	for _, s := range []string{"987.65", "cost_usd", "remaining", "budget_percent", "Savings", "Scenario saves"} {
		if bytes.Contains(out, []byte(s)) {
			t.Errorf("leaked %s: %s", s, out)
		}
	}
	if got.Definition.ScenarioDiscountPercent != 0 || len(got.Warnings) == 0 || got.Warnings[len(got.Warnings)-1] != "Cost reporting disabled for this deployment" {
		t.Fatal("redaction warning/discount")
	}
	*got.Rows[0].Values["requests"] = 90
	got.Sections[0].Rows[0].Dimensions["model"] = "mutated"
	got.Definition.Metrics[0] = "mutated"
	after, _ := json.Marshal(r)
	if !bytes.Equal(before, after) {
		t.Fatal("redaction aliases source")
	}
	if _, ok := got.Rows[0].Values["cost_usd"]; ok {
		t.Fatal("must omit, not zero")
	}
}

// Optional independent-reader integration; the package has no Python dependency.
// Set REPORTING_EXTERNAL_READERS=1 in an environment with pypdf/openpyxl/PyMuPDF.
func TestIndependentOfficeAndPDFReaders(t *testing.T) {
	if os.Getenv("REPORTING_EXTERNAL_READERS") != "1" {
		t.Skip("optional external readers")
	}
	for _, format := range []string{"xlsx", "pdf"} {
		t.Run(format, func(t *testing.T) {
			var b bytes.Buffer
			r := fixture()
			if format == "pdf" {
				for i := 0; i < 160; i++ {
					r.Rows = append(r.Rows, Row{Dimensions: map[string]string{"user": fmt.Sprintf("person-%03d", i)}, Values: map[string]*float64{"requests": ptr(float64(i))}})
				}
			}
			if err := Export(&b, r, format); err != nil {
				t.Fatal(err)
			}
			script := `import sys,io,os
raw=sys.stdin.buffer.read()
if sys.argv[1]=='xlsx':
 import openpyxl
 w=openpyxl.load_workbook(io.BytesIO(raw))
 assert w.sheetnames==['Overview','Data','Usage 1','Methodology'],w.sheetnames
 assert w['Data']['A2'].value=='\t=HYPERLINK("evil")'
 assert w['Data']['A2'].data_type=='s'
 assert w['Usage 1']['A3'].value=='模型'
 assert w['Data']['B2'].value=='7'
 print('openpyxl: 4 sheets, safe formula text, Unicode and numeric text verified')
else:
 from pypdf import PdfReader
 import pymupdf,json
 p=PdfReader(io.BytesIO(raw),strict=True)
 assert len(p.pages)>=3
 text='\n'.join(page.extract_text() for page in p.pages)
 assert 'person-159' in text
 for expected in ['warning-one','warning-two','section-note','U+6A21','Requests','USD']:
  assert expected in text,expected
 doc=pymupdf.open(stream=raw,filetype='pdf')
 assert doc.embfile_info(0)['ufilename']=='report.json'
 attached=json.loads(doc.embfile_get(0))
 assert attached['sections'][0]['rows'][0]['dimensions']['model']=='模型'
 if os.getenv('REPORTING_PREVIEW_DIR'):
  os.makedirs(os.environ['REPORTING_PREVIEW_DIR'],exist_ok=True)
  doc[0].get_pixmap(matrix=pymupdf.Matrix(1.5,1.5)).save(os.path.join(os.environ['REPORTING_PREVIEW_DIR'],'report.png'))
 print('pypdf strict + PyMuPDF: %d pages; text, source attachment and rendering verified'%len(p.pages))
`
			cmd := exec.Command("python3", "-c", script, format)
			cmd.Stdin = &b
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, output)
			}
			t.Log(string(output))
		})
	}
}

// exportText inspects actual CSV records, XLSX XML text and PDF visible streams
// (not the attached JSON, which could conceal missing PDF methodology fields).
func exportText(t *testing.T, r Result, format string) string {
	t.Helper()
	var b bytes.Buffer
	if err := Export(&b, r, format); err != nil {
		t.Fatal(err)
	}
	switch format {
	case "csv":
		rd := csv.NewReader(&b)
		rd.FieldsPerRecord = -1
		records, err := rd.ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		return fmtRecords(records)
	case "xlsx":
		z, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		for _, f := range z.File {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			decoder := xml.NewDecoder(rc)
			for {
				token, err := decoder.Token()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if text, ok := token.(xml.CharData); ok {
					out.Write(text)
					out.WriteByte('\n')
				}
			}
			rc.Close()
		}
		return out.String()
	case "pdf":
		return pdfVisibleText(t, b.Bytes())
	default:
		return b.String()
	}
}

func TestComparisonConfidenceAllExports(t *testing.T) {
	for _, state := range []string{"unknown", "true", "false"} {
		for _, format := range []string{"json", "csv", "xlsx", "pdf"} {
			t.Run(state+"/"+format, func(t *testing.T) {
				r := fixture()
				r.ComparisonWarnings = []string{"Prior interval is incomplete"}
				if state != "unknown" {
					v := state == "true"
					r.ComparisonReliable = &v
				}
				r.PreviousTotals["cost_usd"] = ptr(987.65)
				before, _ := json.Marshal(r)
				text := exportText(t, r, format)
				for _, want := range []string{"comparison_warnings", "Prior interval is incomplete", "previous_totals", "987.65"} {
					if !strings.Contains(text, want) {
						t.Fatalf("missing %s", want)
					}
				}
				if state != "unknown" && !strings.Contains(text, "comparison_reliable") {
					t.Fatal("comparison confidence missing")
				}
				if state == "false" && format != "json" && !strings.Contains(text, "Comparison unavailable") {
					t.Fatal("missing readable comparison unavailability")
				}
				after, _ := json.Marshal(r)
				if !bytes.Equal(before, after) {
					t.Fatal("export changed frozen snapshot")
				}
			})
		}
	}
}

func TestLocalOnlyOperationalFrozenExports(t *testing.T) {
	for _, template := range GetCatalog().Templates {
		for _, format := range []string{"json", "csv", "xlsx", "pdf"} {
			t.Run(template.Template+"/"+format, func(t *testing.T) {
				original := operationalFixture()
				original.Definition = template
				for _, dim := range []string{"modality", "model_family", "provider", "hosting", "model", "client_app", "service_token", "endpoint"} {
					family := "portfolio_"
					if contains("client_app service_token endpoint", dim) {
						family = "integrations_"
					}
					original.Sections = append(original.Sections, Section{ID: family + dim, Columns: []Column{{dim, label(dim), ""}, {"requests", "Requests", "count"}, {"cost_usd", "Cost", "USD"}}, Rows: []Row{{Dimensions: map[string]string{dim: "entity-" + dim}, Values: map[string]*float64{"requests": ptr(7), "cost_usd": ptr(987.65)}}}})
				}
				original.Sections = append(original.Sections, Section{ID: "adoption", Columns: []Column{{"eligible_users_current", "Eligible", "count"}, {"active_eligible_users", "Active", "count"}, {"adoption_ratio_current_census", "Adoption", "ratio"}}, Rows: []Row{{Values: map[string]*float64{"eligible_users_current": ptr(10), "active_eligible_users": ptr(7), "adoption_ratio_current_census": ptr(.7)}}}})
				before, _ := json.Marshal(original)
				r := RedactCosts(original)
				text := exportText(t, r, format)
				for _, want := range []string{"portfolio_modality", "portfolio_model_family", "portfolio_provider", "portfolio_hosting", "portfolio_model", "integrations_client_app", "integrations_service_token", "integrations_endpoint", "entity-modality", "entity-client_app", "entity-service_token", "entity-endpoint", "eligible_users_current", "active_eligible_users", "adoption_ratio_current_census", "quota_windows", "q-tokens", "data_quality", "cost_coverage", "quota_history_coverage", "efficiency_ratios", "comparison_reliable", "comparison_warnings", "Comparison unavailable"} {
					if !strings.Contains(text, want) {
						t.Fatalf("operational information missing: %s", want)
					}
				}
				for _, secret := range []string{"987.65", "98765", "98.765", "q-money", "cost_usd", "USD", "EUR", "portfolio_secret"} {
					if strings.Contains(text, secret) {
						t.Fatalf("financial leak: %s", secret)
					}
				}
				if format == "json" {
					var roundtrip Result
					if err := json.Unmarshal([]byte(text), &roundtrip); err != nil || !reflect.DeepEqual(r, roundtrip) {
						t.Fatal("snapshot did not roundtrip")
					}
				}
				after, _ := json.Marshal(original)
				if !bytes.Equal(before, after) {
					t.Fatal("redaction/export mutated original")
				}
			})
		}
	}
}

func fmtRecords(v [][]string) string {
	var b strings.Builder
	for _, r := range v {
		b.WriteString(strings.Join(r, "|"))
		b.WriteByte('\n')
	}
	return b.String()
}
