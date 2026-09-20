package reporting

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestPDFComparisonWarningsAreProseNotJSON(t *testing.T) {
	r := fixture()
	r.ComparisonWarnings = []string{"Prior interval is incomplete"}
	text := exportText(t, r, "pdf")
	if strings.Contains(text, `["Prior interval is incomplete"]`) {
		t.Fatal("serialized warning JSON printed in reader-facing PDF")
	}
	if !strings.Contains(text, "Prior interval is incomplete") {
		t.Fatal("comparison caveat lost")
	}
}

func TestPDFSmallSectionsFlowWithoutBlankPages(t *testing.T) {
	r := fixture()
	r.Sections = nil
	for i := 0; i < 6; i++ {
		r.Sections = append(r.Sections, Section{ID: strconv.Itoa(i), Title: "Related summary", Columns: []Column{{"requests", "Requests", "count"}}, Rows: []Row{{Values: map[string]*float64{"requests": ptr(float64(i))}}}})
	}
	var b bytes.Buffer
	if err := Export(&b, r, "pdf"); err != nil {
		t.Fatal(err)
	}
	pages := len(regexp.MustCompile(`/Type /Page\b`).FindAll(b.Bytes(), -1))
	if pages > 5 {
		t.Fatalf("six tiny related sections should flow, got %d pages", pages)
	}
}

func TestPDFEmbedsNativeUnicodeFonts(t *testing.T) {
	r := fixture()
	r.Definition.Name = "Équipe — usage"
	var b bytes.Buffer
	if err := Export(&b, r, "pdf"); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"/FontFile2", "/ToUnicode", "/EmbeddedFile"} {
		if !bytes.Contains(b.Bytes(), []byte(token)) {
			t.Errorf("native Unicode PDF missing %s", token)
		}
	}
	if !strings.Contains(pdfVisibleText(t, b.Bytes()), r.Definition.Name) {
		t.Fatal("supported Unicode name was not rendered")
	}
}

// Decode only page text operators, never source attachments. This deliberately
// small test reader is supplemented by the independent PyMuPDF integration.
func pdfStreams(t *testing.T, raw []byte) [][]byte {
	t.Helper()
	out := [][]byte{}
	lengthRE := regexp.MustCompile(`/Length (\d+)`)
	for offset := 0; offset < len(raw); {
		pos := bytes.Index(raw[offset:], []byte("\nstream\n"))
		if pos < 0 {
			break
		}
		pos += offset
		header := raw[offset:pos]
		matches := lengthRE.FindAllSubmatch(header, -1)
		if len(matches) == 0 {
			t.Fatal("stream has no direct length")
		}
		n, err := strconv.Atoi(string(matches[len(matches)-1][1]))
		if err != nil {
			t.Fatal(err)
		}
		start := pos + 8
		if start+n > len(raw) {
			t.Fatal("stream length exceeds file")
		}
		s := raw[start : start+n]
		offset = start + n
		if z, err := zlib.NewReader(bytes.NewReader(s)); err == nil {
			decoded, e := io.ReadAll(z)
			z.Close()
			if e != nil {
				t.Fatal(e)
			}
			s = decoded
		}
		out = append(out, s)
	}
	return out
}
func pdfVisibleText(t *testing.T, raw []byte) string {
	t.Helper()
	var out strings.Builder
	re := regexp.MustCompile(`(?s)\(((?:\\.|[^\\)])*)\)\s*Tj`)
	for _, s := range pdfStreams(t, raw) {
		if !bytes.Contains(s, []byte("BT ")) {
			continue
		}
		for _, m := range re.FindAllSubmatch(s, -1) {
			b := []byte{}
			for i := 0; i < len(m[1]); i++ {
				v := m[1][i]
				if v == '\\' && i+1 < len(m[1]) {
					i++
					v = m[1][i]
					switch v {
					case 'n':
						v = '\n'
					case 'r':
						v = '\r'
					case 't':
						v = '\t'
					}
				}
				b = append(b, v)
			}
			if len(b)%2 != 0 {
				t.Fatal("odd Unicode text length")
			}
			u := make([]uint16, len(b)/2)
			for i := range u {
				u[i] = binary.BigEndian.Uint16(b[i*2:])
			}
			out.WriteString(string(utf16.Decode(u)))
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func TestPDFWideRecordCardsPaginateLongField(t *testing.T) {
	r := fixture()
	r.Columns, r.Rows, r.Sections = nil, nil, nil
	row := Row{Dimensions: map[string]string{}}
	for i := 0; i < 12; i++ {
		key := "field_" + strconv.Itoa(i)
		r.Columns = append(r.Columns, Column{Key: key, Label: "Field " + strconv.Itoa(i)})
		row.Dimensions[key] = "value_" + strconv.Itoa(i)
	}
	lines := make([]string, 120)
	for i := range lines {
		lines[i] = "detail_line_" + strconv.Itoa(i) + "_end"
	}
	row.Dimensions["field_10"] = strings.Join(lines, "\n")
	r.Rows = []Row{row}
	before, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := Export(&b, r, "pdf"); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(r)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("PDF export changed the frozen result")
	}
	python := os.Getenv("REPORTING_PDF_PYTHON")
	if python != "" {
		dir := t.TempDir()
		pdfPath, source := filepath.Join(dir, "wide.pdf"), filepath.Join(dir, "expected.json")
		if err := os.WriteFile(pdfPath, b.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, before, 0600); err != nil {
			t.Fatal(err)
		}
		script := `import pymupdf,sys,pathlib
p=pymupdf.open(sys.argv[1])
assert p.embfile_count()==1
assert p.embfile_get(0)==pathlib.Path(sys.argv[2]).read_bytes(),'frozen attachment changed'
text='\n'.join(page.get_text() for page in p)
for i in range(120):
 token='detail_line_%d_end'%i
 assert text.count(token)==1,('missing or duplicated visible detail',token)
for i in range(12):
 if i!=10: assert text.count('value_%d\n'%i)==1,('lost neighboring field',i)
detail_pages=[]
for page in p:
 if 'detail_line_' not in page.get_text(): continue
 detail_pages.append(page.number)
 assert 'Record 1' in page.get_text(),('record identity missing',page.number)
 assert 'Field 10' in page.get_text(),('field identity missing',page.number)
 for block in page.get_text('dict')['blocks']:
  for line in block.get('lines',[]):
   for span in line['spans']:
    if not span['text'].startswith('detail_line_'): continue
    x0,y0,x1,y1=span['bbox']
    assert x0>=16*72/25.4 and x1<page.rect.width-16*72/25.4,('horizontal clipping',span)
    assert y0>=28*72/25.4 and y1<(page.rect.height-21*72/25.4),('footer overlap',page.number,span)
assert len(detail_pages)>=3,('field did not continue across pages',detail_pages)
print('PyMuPDF verified %d detail pages: all 120 lines exactly once, neighboring fields, continuation identities, footer-safe bounds, byte-exact attachment'%len(detail_pages))
`
		out, err := exec.Command(python, "-c", script, pdfPath, source).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		t.Log(string(out))
	}
	text := pdfVisibleText(t, b.Bytes())
	if !strings.Contains(text, "Record 1 · continued") || strings.Count(text, "Field 10") < 3 {
		t.Fatal("long record field missing record/field continuation labels")
	}
}

func TestPDFRankingPrefersPairedDisplayLabel(t *testing.T) {
	row := Row{Dimensions: map[string]string{"model": "opaque-id-123", "model_label": "Équipe model"}, Values: map[string]*float64{"requests": ptr(1)}}
	got := pdfRowLabel([]Column{{"model", "Model", ""}, {"model_label", "Model name", ""}}, row, 0)
	if got != "Équipe model" {
		t.Fatalf("rank label = %q; opaque identity should remain in appendix only", got)
	}
}
func TestPDFChartSelectionDoesNotDependOnSectionOrder(t *testing.T) {
	r := fixture()
	r.Rows = nil
	r.Sections = []Section{
		{ID: "portfolio_model", Title: "Models", Columns: []Column{{"model", "Model", ""}, {"requests", "Requests", "count"}}, Rows: []Row{{Dimensions: map[string]string{"model": "named model"}, Values: map[string]*float64{"requests": ptr(8)}}}},
		{ID: "daily", Title: "Daily", Columns: []Column{{"day", "Day", ""}, {"requests", "Requests", "count"}}, Rows: []Row{{Dimensions: map[string]string{"day": "2026-02-20"}, Values: map[string]*float64{"requests": ptr(8)}}}},
	}
	text := exportText(t, r, "pdf")
	if !strings.Contains(text, "Activity over time") {
		t.Fatal("a preceding model section suppressed the complete-period timeline")
	}
}

func TestPDFPartialClockBoundariesKeepInRangeTimeline(t *testing.T) {
	r := fixture()
	r.Start = r.Start.Add(10 * time.Hour)
	r.End = r.End.Add(10 * time.Hour)
	r.Sections = nil
	r.Columns = []Column{{"day", "Day", ""}, {"requests", "Requests", "count"}}
	r.Rows = []Row{{Dimensions: map[string]string{"day": "2026-02-02"}, Values: map[string]*float64{"requests": ptr(3)}}, {Dimensions: map[string]string{"day": "2026-02-28"}, Values: map[string]*float64{"requests": ptr(7)}}}
	text := exportText(t, r, "pdf")
	if !strings.Contains(text, "Activity over time") || !strings.Contains(text, "2 observations") {
		t.Fatal("in-range daily observations lost solely because range endpoints have a clock time")
	}
	if strings.Contains(text, "Time chart unavailable:") {
		t.Fatal("unambiguous in-range UTC dates incorrectly rejected")
	}
}

func TestPDFTimeChartRejectsAmbiguousAxes(t *testing.T) {
	for _, kind := range []string{"nonUTC", "partial", "multidimensional"} {
		t.Run(kind, func(t *testing.T) {
			r := fixture()
			r.Sections = nil
			r.Columns = []Column{{"day", "Day", ""}, {"requests", "Requests", "count"}}
			r.Rows = []Row{{Dimensions: map[string]string{"day": "2026-02-01"}, Values: map[string]*float64{"requests": ptr(3)}}}
			switch kind {
			case "nonUTC":
				r.Definition.Timezone = "America/Los_Angeles"
			case "partial":
				r.Start = r.Start.Add(12 * time.Hour)
			case "multidimensional":
				r.Rows[0].Dimensions["model"] = "model-a"
				r.Rows = append(r.Rows, Row{Dimensions: map[string]string{"day": "2026-02-01", "model": "model-b"}, Values: map[string]*float64{"requests": ptr(7)}})
			}
			text := exportText(t, r, "pdf")
			if strings.Contains(text, "Activity over time") || !strings.Contains(text, "Time chart unavailable:") {
				t.Fatal("ambiguous calendar/multidimensional axis rendered without an honest table fallback")
			}
		})
	}
}

func TestPDFWeeklyDateDimension(t *testing.T) {
	r := fixture()
	r.Sections = nil
	r.Columns = []Column{{"week", "Week", ""}, {"requests", "Requests", "count"}}
	r.Rows = []Row{{Dimensions: map[string]string{"week": "2026-02-02"}, Values: map[string]*float64{"requests": ptr(3)}}}
	if text := exportText(t, r, "pdf"); !strings.Contains(text, "Activity over time") {
		t.Fatal("weekly calendar dimension was rendered as a categorical ranking")
	}
}

func TestPDFCountChartUsesWholeNumberTicks(t *testing.T) {
	r := fixture()
	r.Sections = nil
	r.Columns = []Column{{"day", "Day", ""}, {"requests", "Requests", "count"}}
	r.Rows = []Row{{Dimensions: map[string]string{"day": "2026-02-20"}, Values: map[string]*float64{"requests": ptr(3)}}}
	text := strings.Split(exportText(t, r, "pdf"), "Complete report data")[0]
	if strings.Contains(text, "2.25") || strings.Contains(text, "0.75") {
		t.Fatal("fractional request counts on chart axis")
	}
}

func TestPDFFullPeriodCalendarObservations(t *testing.T) {
	r := fixture()
	r.Sections = nil
	r.Rows = nil
	r.Columns = []Column{{"day", "Day", ""}, {"requests", "Requests", "count"}}
	for i := 0; i < 28; i++ {
		if i == 7 {
			continue
		}
		dt := r.Start.AddDate(0, 0, i)
		r.Rows = append(r.Rows, Row{Dimensions: map[string]string{"day": dt.Format("2006-01-02")}, Values: map[string]*float64{"requests": ptr(float64(i + 1))}})
	}
	s := Section{Columns: r.Columns, Rows: r.Rows}
	points := pdfObservations(s, r.Columns[1])
	if len(points) != 27 {
		t.Fatalf("lost observations: %d", len(points))
	}
	if points[7].date.Sub(points[6].date) != 48*time.Hour {
		t.Fatal("calendar gap collapsed")
	}
	text := exportText(t, r, "pdf")
	chart := strings.Split(text, "Complete report data")[0]
	for _, want := range []string{"entire reporting period", "27 observations", "Feb 1, 2026", "Mar 1, 2026", "Gaps are missing observations"} {
		if !strings.Contains(chart, want) {
			t.Fatalf("missing chart semantics %q", want)
		}
	}
	if strings.Contains(chart, "First 12") {
		t.Fatal("time series truncated")
	}
}

// REPORTING_PDF_PYTHON points at any interpreter with PyMuPDF. No runtime or
// production Python dependency. REPORTING_PREVIEW_DIR retains PDF + PNG files.
// REPORTING_FROZEN_JSON optionally renders a real frozen result for inspection.
func TestPDFIndependentNativeLayout(t *testing.T) {
	python := os.Getenv("REPORTING_PDF_PYTHON")
	if python == "" {
		t.Skip("set REPORTING_PDF_PYTHON for independent reader")
	}
	r := fixture()
	r.Definition.Name = "Équipe — usage"
	if path := os.Getenv("REPORTING_FROZEN_JSON"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		r = Result{} // Unmarshal must not merge maps from the synthetic fixture.
		if err = json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
	} else {
		for i := 0; i < 160; i++ {
			r.Rows = append(r.Rows, Row{Dimensions: map[string]string{"user": strings.Repeat("long identity ", 3) + time.Unix(int64(i), 0).Format("150405")}, Values: map[string]*float64{"requests": ptr(float64(i))}})
		}
	}
	var b bytes.Buffer
	if err := Export(&b, r, "pdf"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r)
	dir := t.TempDir()
	if preview := os.Getenv("REPORTING_PREVIEW_DIR"); preview != "" {
		dir = preview
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	pdfPath := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(pdfPath, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "expected.json")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	script := `import pymupdf,sys,pathlib,json
p=pymupdf.open(sys.argv[1]);expected=pathlib.Path(sys.argv[2]).read_bytes()
assert p.embfile_count()==1
assert p.embfile_info(0)['ufilename']=='report.json'
assert p.embfile_get(0)==expected,'attachment differs'
r=json.loads(expected);text='\n'.join(page.get_text() for page in p)
assert r['definition']['name'] in text
assert 'Page 1 of '+str(len(p)) in text
assert '[U+6A21]' in text or '模型' not in expected.decode()
assert len(p)>=3
for page in p:
 assert page.get_drawings(),'native vector objects absent'
 assert not page.get_images(),'unexpected raster layout'
 for block in page.get_text('dict')['blocks']:
  if 'lines' not in block: continue
  for line in block['lines']:
   for span in line['spans']:
    x0,y0,x1,y1=span['bbox']
    assert x0>=30 and x1<=page.rect.width-25,(page.number,'horizontal clipping',span)
    assert y0>=15 and y1<=page.rect.height-15,(page.number,'vertical clipping',span)
for i in range(min(3,len(p))):p[i].get_pixmap(matrix=pymupdf.Matrix(1.5,1.5)).save(str(pathlib.Path(sys.argv[1]).with_name('page-%d.png'%(i+1))))
print('PyMuPDF verified %d pages: Unicode, page counts, vector geometry, no clipping, byte-exact attachment'%len(p))
`
	cmd := exec.Command(python, "-c", script, pdfPath, source)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	t.Log(string(out))
	t.Log("Rendered", pdfPath)
}
