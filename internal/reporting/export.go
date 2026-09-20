package reporting

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

func ContentType(format string) string {
	switch strings.ToLower(format) {
	case "json":
		return "application/json"
	case "csv":
		return "text/csv; charset=utf-8"
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "pdf":
		return "application/pdf"
	}
	return ""
}
func Extension(format string) string {
	if ContentType(format) == "" {
		return ""
	}
	return "." + strings.ToLower(format)
}

// Export serializes only the supplied frozen Result, with no external lookups.
func Export(w io.Writer, r Result, format string) error {
	if ContentType(format) == "" {
		return fmt.Errorf("unsupported export format %q", format)
	}
	if err := validText(reflect.ValueOf(r), strings.EqualFold(format, "xlsx")); err != nil {
		return err
	}
	// Marshal preflight rejects nonfinite numbers instead of lossy metadata.
	if _, err := json.Marshal(r); err != nil {
		return fmt.Errorf("invalid report result: %w", err)
	}
	if !strings.EqualFold(format, "json") {
		sets := append([]Section{{Columns: r.Columns, Rows: r.Rows}}, r.Sections...)
		for _, s := range sets {
			seen := map[string]bool{}
			for _, c := range s.Columns {
				if seen[c.Key] {
					return fmt.Errorf("duplicate column %q", c.Key)
				}
				seen[c.Key] = true
			}
			for _, row := range s.Rows {
				for k := range row.Dimensions {
					if _, ok := row.Values[k]; ok {
						return fmt.Errorf("ambiguous dimension/value key %q", k)
					}
				}
			}
		}
	}
	w = checkedWriter{w}
	switch strings.ToLower(format) {
	case "json":
		return json.NewEncoder(w).Encode(r)
	case "csv":
		return exportCSV(w, r)
	case "xlsx":
		return exportXLSX(w, r)
	case "pdf":
		return exportPDF(w, r)
	default:
		return fmt.Errorf("unsupported export format %q", format)
	}
}

type checkedWriter struct{ io.Writer }

func (w checkedWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}
func validText(v reflect.Value, xmlOnly bool) error {
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		if !utf8.ValidString(s) {
			return fmt.Errorf("report contains invalid UTF-8")
		}
		if xmlOnly {
			for _, r := range s {
				if (r < 32 && r != '\t' && r != '\n' && r != '\r') || r == 0xFFFE || r == 0xFFFF {
					return fmt.Errorf("report contains character U+%04X unsupported by XML", r)
				}
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				if err := validText(v.Field(i), xmlOnly); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := validText(v.Index(i), xmlOnly); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if err := validText(iter.Key(), xmlOnly); err != nil {
				return err
			}
			if err := validText(iter.Value(), xmlOnly); err != nil {
				return err
			}
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return validText(v.Elem(), xmlOnly)
		}
	}
	return nil
}
func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }
func resultUnit(r Result, k string) string {
	for _, c := range r.Columns {
		if c.Key == k && c.Unit != "" {
			return c.Unit
		}
	}
	for _, s := range r.Sections {
		for _, c := range s.Columns {
			if c.Key == k && c.Unit != "" {
				return c.Unit
			}
		}
	}
	if contains(metricIDs, k) {
		return metricUnit(k)
	}
	return ""
}
func metadata(r Result) [][]string {
	m := [][]string{{"result_version", strconv.Itoa(r.Version)}, {"definition_version", strconv.Itoa(r.Definition.Version)}, {"name", r.Definition.Name}, {"timeframe", "[" + r.Start.Format(time.RFC3339Nano) + ", " + r.End.Format(time.RFC3339Nano) + ")"}, {"timezone", r.Definition.Timezone}, {"filters", jsonText(r.Definition.Filters)}, {"generated_at", r.GeneratedAt.Format(time.RFC3339Nano)}, {"data_cutoff", r.DataCutoff.Format(time.RFC3339Nano)}, {"source_rows", strconv.FormatInt(r.SourceRows, 10)}, {"definition", jsonText(r.Definition)}, {"totals", jsonText(r.Totals)}, {"previous_totals", jsonText(r.PreviousTotals)}, {"methodology", "Frozen snapshot; half-open [start,end); null means unavailable, never zero. Rates are ratios, not percentages. Historical/current grouping follows definition.group_mode. Percentiles and distinct counts are not additive."}}
	m = append(m, []string{"comparison_reliable", jsonText(r.ComparisonReliable)}, []string{"comparison_warnings", jsonText(r.ComparisonWarnings)})
	if r.ComparisonReliable != nil && !*r.ComparisonReliable {
		m = append(m, []string{"comparison_status", "Comparison unavailable: coverage is incomplete; recorded prior totals remain visible, but changes and outlier flags are unavailable."})
	}
	for _, c := range r.Columns {
		m = append(m, []string{"unit", c.Key, c.Unit})
	}
	for _, s := range r.Sections {
		for _, c := range s.Columns {
			m = append(m, []string{"section_unit", s.ID, c.Key, c.Unit})
		}
	}
	for _, s := range r.Warnings {
		m = append(m, []string{"warning", s})
	}
	return m
}

// Spreadsheet applications may ignore initial whitespace before formulas.
func safeCSV(s string) string {
	trim := strings.TrimLeftFunc(s, unicode.IsSpace)
	if strings.ContainsAny(s[:len(s)-len(trim)], "\t\r\n") || (len(trim) > 0 && strings.ContainsRune("=+-@", rune(trim[0]))) {
		return "'" + s
	}
	return s
}
func value(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'g', -1, 64)
}

// tableColumns preserves declared order and appends undeclared row keys, ensuring
// a producer's extra dimensions/values cannot silently disappear in an export.
func tableColumns(cols []Column, rows []Row) []Column {
	out := append([]Column(nil), cols...)
	seen := map[string]bool{}
	for _, c := range cols {
		seen[c.Key] = true
	}
	extra := map[string]bool{}
	for _, r := range rows {
		for k := range r.Dimensions {
			if !seen[k] {
				extra[k] = true
			}
		}
		for k := range r.Values {
			if !seen[k] {
				extra[k] = true
			}
		}
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		unit := ""
		if contains(metricIDs, k) {
			unit = metricUnit(k)
		}
		out = append(out, Column{k, label(k), unit})
	}
	return out
}
func tableRecords(cols []Column, rows []Row) [][]string {
	cols = tableColumns(cols, rows)
	head := make([]string, len(cols))
	for i, c := range cols {
		head[i] = c.Label
		if head[i] == "" {
			head[i] = c.Key
		}
		if c.Unit != "" {
			head[i] += " (" + c.Unit + ")"
		}
	}
	out := [][]string{head}
	for _, r := range rows {
		line := make([]string, len(cols))
		for i, c := range cols {
			if s, ok := r.Dimensions[c.Key]; ok {
				line[i] = s
			} else {
				line[i] = value(r.Values[c.Key])
			}
		}
		out = append(out, line)
	}
	return out
}

const spreadsheetNS = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func sheetName(s string, index int) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune("[]:*?/\\", r) || r < 32 {
			return '_'
		}
		return r
	}, s)
	s = strings.Trim(s, "'")
	if s == "" {
		s = "Section"
	}
	rr := []rune(s)
	if len(rr) > 22 {
		s = string(rr[:22])
	}
	return fmt.Sprintf("%s %d", s, index)
}
func worksheet(records [][]string) (string, error) {
	if len(records) > 1048576 {
		return "", fmt.Errorf("XLSX row limit exceeded")
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><worksheet xmlns="` + spreadsheetNS + `"><sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" state="frozen"/></sheetView></sheetViews><sheetData>`)
	for i, record := range records {
		if len(record) > 16384 {
			return "", fmt.Errorf("XLSX column limit exceeded")
		}
		fmt.Fprintf(&b, `<row r="%d">`, i+1)
		for _, s := range record {
			if len(utf16.Encode([]rune(s))) > 32767 {
				return "", fmt.Errorf("XLSX cell exceeds 32767 characters")
			}
			b.WriteString(`<c t="inlineStr"><is><t xml:space="preserve">` + xmlText(s) + `</t></is></c>`)
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String(), nil
}
func exportXLSX(w io.Writer, r Result) error {
	names := []string{"Overview", "Data"}
	records := [][][]string{{{"Report", r.Definition.Name}, {"Summary metric", "Value", "Unit"}}, tableRecords(r.Columns, r.Rows)}
	keys := make([]string, 0, len(r.Totals))
	for k := range r.Totals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		records[0] = append(records[0], []string{k, value(r.Totals[k]), resultUnit(r, k)})
	}
	for i, s := range r.Sections {
		names = append(names, sheetName(s.Title, i+1))
		rec := [][]string{{"Section", s.ID, s.Title}}
		rec = append(rec, tableRecords(s.Columns, s.Rows)...)
		for _, n := range s.Notes {
			rec = append(rec, []string{"Note", n})
		}
		records = append(records, rec)
	}
	names = append(names, "Methodology")
	records = append(records, metadata(r))
	// Build fully before writing, so limit errors cannot produce a partial artifact.
	files := map[string]string{}
	order := []string{}
	add := func(n, s string) { files[n] = s; order = append(order, n) }
	content := `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`
	workbook := `<?xml version="1.0"?><workbook xmlns="` + spreadsheetNS + `" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`
	rels := `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`
	for i, name := range names {
		num := i + 1
		path := fmt.Sprintf("xl/worksheets/sheet%d.xml", num)
		data, err := worksheet(records[i])
		if err != nil {
			return err
		}
		add(path, data)
		content += fmt.Sprintf(`<Override PartName="/%s" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, path)
		workbook += fmt.Sprintf(`<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlText(name), num, num)
		rels += fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, num, num)
	}
	add("[Content_Types].xml", content+`</Types>`)
	add("xl/workbook.xml", workbook+`</sheets></workbook>`)
	add("xl/_rels/workbook.xml.rels", rels+`</Relationships>`)
	add("_rels/.rels", `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`)
	z := zip.NewWriter(w)
	for _, path := range order {
		f, err := z.Create(path)
		if err != nil {
			return err
		}
		if _, err = io.WriteString(f, files[path]); err != nil {
			return err
		}
	}
	return z.Close()
}

func exportCSV(w io.Writer, r Result) error {
	c := csv.NewWriter(w)
	write := func(rec []string) error {
		safe := make([]string, len(rec))
		for i, s := range rec {
			safe[i] = safeCSV(s)
		}
		return c.Write(safe)
	}
	for _, m := range metadata(r) {
		if err := write(append([]string{"#"}, m...)); err != nil {
			return err
		}
	}
	for _, rec := range tableRecords(r.Columns, r.Rows) {
		if err := write(rec); err != nil {
			return err
		}
	}
	for _, s := range r.Sections {
		if err := write([]string{"#", "section", s.ID, s.Title}); err != nil {
			return err
		}
		for _, n := range s.Notes {
			if err := write([]string{"#", "note", n}); err != nil {
				return err
			}
		}
		for _, rec := range tableRecords(s.Columns, s.Rows) {
			if err := write(rec); err != nil {
				return err
			}
		}
	}
	c.Flush()
	return c.Error()
}
