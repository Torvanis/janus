package reporting

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
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
	if math.Abs(*v) >= 1e21 || (*v != 0 && math.Abs(*v) < 1e-6) {
		return strconv.FormatFloat(*v, 'g', -1, 64)
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
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

// csvTable picks the report's primary table: the main grouped result, or the
// first grouped section when a template has no main rows.
func csvTable(r Result) ([]Column, []Row) {
	if len(r.Columns) > 0 {
		return r.Columns, r.Rows
	}
	for _, s := range orderedSections(r) {
		if !factSection(s) && s.ID != "comparison" && len(s.Columns) > 0 {
			return s.Columns, s.Rows
		}
	}
	return r.Columns, r.Rows
}

// exportCSV writes one rectangular table: a single header row followed by data
// rows. Display names replace opaque identities; numbers are unformatted so
// spreadsheets and data tools parse them. Ratios are written as percentages.
// Context, notices and every section live in the XLSX, PDF and JSON exports.
func exportCSV(w io.Writer, r Result) error {
	cols, rows := csvTable(r)
	cols = displayColumns(cols, rows)
	c := csv.NewWriter(w)
	head := make([]string, len(cols))
	for i, col := range cols {
		head[i] = safeCSV(csvHeader(col))
	}
	if err := c.Write(head); err != nil {
		return err
	}
	for _, row := range rows {
		line := make([]string, len(cols))
		for i, col := range cols {
			if s, ok := row.Dimensions[col.Key]; ok {
				line[i] = safeCSV(s)
				continue
			}
			v := row.Values[col.Key]
			if v != nil && strings.EqualFold(col.Unit, "ratio") {
				pct := *v * 100
				v = &pct
			}
			line[i] = value(v)
		}
		if err := c.Write(line); err != nil {
			return err
		}
	}
	c.Flush()
	return c.Error()
}

func csvHeader(c Column) string {
	if strings.EqualFold(c.Unit, "ratio") {
		return columnTitle(c) + " (%)"
	}
	return headerWithUnit(c, true)
}

const spreadsheetNS = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Cell styles (indices into cellXfs in xlsxStyles).
const (
	xsText = iota
	xsHeader
	xsInt
	xsUSD
	xsPercent
	xsDecimal
	xsMillis
	xsTitle
	xsWrap
	xsLabel
	xsDate
	xsMuted
	xsSubtitle
)

const xlsxStyles = `<?xml version="1.0" encoding="UTF-8"?><styleSheet xmlns="` + spreadsheetNS + `">` +
	`<numFmts count="6"><numFmt numFmtId="164" formatCode="#,##0"/><numFmt numFmtId="165" formatCode="&quot;$&quot;#,##0.00"/><numFmt numFmtId="166" formatCode="0.0%"/><numFmt numFmtId="167" formatCode="#,##0.00"/><numFmt numFmtId="168" formatCode="#,##0&quot; ms&quot;"/><numFmt numFmtId="169" formatCode="mmm d, yyyy"/></numFmts>` +
	`<fonts count="5"><font><sz val="11"/><name val="Calibri"/><family val="2"/></font><font><b/><sz val="11"/><color rgb="FF1F2937"/><name val="Calibri"/><family val="2"/></font><font><b/><sz val="16"/><color rgb="FF312E81"/><name val="Calibri"/><family val="2"/></font><font><sz val="10"/><color rgb="FF5F6786"/><name val="Calibri"/><family val="2"/></font><font><b/><sz val="12"/><color rgb="FF312E81"/><name val="Calibri"/><family val="2"/></font></fonts>` +
	`<fills count="3"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill><fill><patternFill patternType="solid"><fgColor rgb="FFEEEDFC"/><bgColor indexed="64"/></patternFill></fill></fills>` +
	`<borders count="2"><border><left/><right/><top/><bottom/><diagonal/></border><border><left/><right/><top/><bottom style="thin"><color rgb="FFA5A1F0"/></bottom><diagonal/></border></borders>` +
	`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
	`<cellXfs count="13">` +
	`<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"><alignment vertical="top"/></xf>` +
	`<xf numFmtId="0" fontId="1" fillId="2" borderId="1" xfId="0" applyFont="1" applyFill="1" applyBorder="1" applyAlignment="1"><alignment vertical="center" wrapText="1"/></xf>` +
	`<xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>` +
	`<xf numFmtId="165" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>` +
	`<xf numFmtId="166" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>` +
	`<xf numFmtId="167" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>` +
	`<xf numFmtId="168" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>` +
	`<xf numFmtId="0" fontId="2" fillId="0" borderId="0" xfId="0" applyFont="1"/>` +
	`<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyAlignment="1"><alignment vertical="top" wrapText="1"/></xf>` +
	`<xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1" applyAlignment="1"><alignment vertical="top"/></xf>` +
	`<xf numFmtId="169" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1" applyAlignment="1"><alignment horizontal="left"/></xf>` +
	`<xf numFmtId="0" fontId="3" fillId="0" borderId="0" xfId="0" applyFont="1" applyAlignment="1"><alignment vertical="top" wrapText="1"/></xf>` +
	`<xf numFmtId="0" fontId="4" fillId="0" borderId="0" xfId="0" applyFont="1"/>` +
	`</cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`

type xcell struct {
	text  string
	num   *float64
	style int
}

type xsheet struct {
	name        string
	rows        [][]xcell
	widths      []float64
	freezeRow   int    // rows above the split; 0 = no freeze
	filter      string // autoFilter range
	merges      []string
	tableHeader int // 1-based row of the table header (0 = none)
}

func txt(s string, style int) xcell  { return xcell{text: s, style: style} }
func num(v float64, style int) xcell { return xcell{num: &v, style: style} }

func unitStyle(unit string, integral bool) int {
	switch strings.ToLower(unit) {
	case "usd", "eur":
		return xsUSD
	case "ratio":
		return xsPercent
	case "ms":
		return xsMillis
	case "count", "tokens", "requests":
		if integral {
			return xsInt
		}
		return xsDecimal
	}
	if integral {
		return xsInt
	}
	return xsDecimal
}

// numberCell stores the exact value with a display format. Percent-unit
// values are stored as fractions so the spreadsheet's % format is truthful.
func numberCell(v *float64, unit string, integral bool) xcell {
	if v == nil {
		return xcell{}
	}
	n := *v
	style := unitStyle(unit, integral)
	switch strings.ToLower(unit) {
	case "percent", "%":
		n /= 100
		style = xsPercent
	case "eur":
		return xcell{text: formatValue(v, unit), style: xsText}
	}
	return num(n, style)
}

func excelDate(s string) (float64, bool) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, false
	}
	return float64(t.Sub(time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)) / (24 * time.Hour)), true
}

func dateDimension(key string) bool {
	switch key {
	case "day", "date", "week", "month":
		return true
	}
	return false
}

func columnIntegral(key string, rows []Row) bool {
	for _, row := range rows {
		if v := row.Values[key]; v != nil && *v != math.Trunc(*v) {
			return false
		}
	}
	return true
}

// tableSheet renders a grouped table with names, typed numbers and a filter.
func tableSheet(name string, cols []Column, rows []Row, notes []string) xsheet {
	cols = displayColumns(cols, rows)
	sh := xsheet{name: name, freezeRow: 1, tableHeader: 1}
	head := []xcell{}
	for _, c := range cols {
		head = append(head, txt(headerWithUnit(c, false), xsHeader))
	}
	sh.rows = append(sh.rows, head)
	integral := map[string]bool{}
	for _, c := range cols {
		integral[c.Key] = columnIntegral(c.Key, rows)
	}
	for _, row := range rows {
		line := make([]xcell, len(cols))
		for i, c := range cols {
			if s, ok := row.Dimensions[c.Key]; ok {
				if n, ok := excelDate(s); ok && dateDimension(c.Key) {
					line[i] = num(n, xsDate)
				} else {
					line[i] = txt(s, xsText)
				}
				continue
			}
			line[i] = numberCell(row.Values[c.Key], c.Unit, integral[c.Key])
		}
		sh.rows = append(sh.rows, line)
	}
	if len(cols) > 0 {
		sh.filter = "A1:" + cellRef(len(cols)-1, len(rows)+1)
	}
	if len(rows) == 0 {
		sh.rows = append(sh.rows, []xcell{txt("No rows returned for this period. This is not a confirmed zero.", xsMuted)})
		sh.filter = ""
	}
	if len(notes) > 0 {
		sh.rows = append(sh.rows, nil)
		for _, n := range notes {
			sh.rows = append(sh.rows, []xcell{txt(n, xsMuted)})
		}
	}
	sh.widths = autoWidths(sh.rows, 10, 48)
	return sh
}

// factSheet shows single-row sections (e.g. cost confidence) as Measure/Value.
func factSheet(name string, s Section) xsheet {
	sh := xsheet{name: name, freezeRow: 1, tableHeader: 1}
	sh.rows = append(sh.rows, []xcell{txt("Measure", xsHeader), txt("Value", xsHeader)})
	row := s.Rows[0]
	for _, c := range tableColumns(s.Columns, s.Rows) {
		v := row.Values[c.Key]
		sh.rows = append(sh.rows, []xcell{txt(columnTitle(c), xsText), numberCell(v, c.Unit, v == nil || *v == math.Trunc(*v))})
	}
	sh.rows = append(sh.rows, nil)
	for _, n := range s.Notes {
		sh.rows = append(sh.rows, []xcell{txt(n, xsMuted)})
		sh.merges = append(sh.merges, fmt.Sprintf("A%d:B%d", len(sh.rows), len(sh.rows)))
	}
	sh.widths = []float64{40, 22}
	return sh
}

func comparisonSheet(name string, r Result, s Section) xsheet {
	sh := xsheet{name: name, freezeRow: 1, tableHeader: 1}
	sh.rows = append(sh.rows, []xcell{txt("Metric", xsHeader), txt("Change vs prior period", xsHeader), txt("Large change (±50%)", xsHeader)})
	for _, l := range comparisonLines(r, s) {
		flag := txt("Unavailable", xsMuted)
		if l.Outlier != nil {
			flag = txt("No", xsText)
			if *l.Outlier != 0 {
				flag = txt("Yes", xsLabel)
			}
		}
		change := xcell{}
		if l.Change != nil {
			change = num(*l.Change, xsPercent)
		}
		sh.rows = append(sh.rows, []xcell{txt(l.Label, xsText), change, flag})
	}
	sh.rows = append(sh.rows, nil)
	for _, n := range s.Notes {
		sh.rows = append(sh.rows, []xcell{txt(n, xsMuted)})
		sh.merges = append(sh.merges, fmt.Sprintf("A%d:C%d", len(sh.rows), len(sh.rows)))
	}
	sh.widths = []float64{34, 24, 22}
	return sh
}

func summarySheet(r Result) xsheet {
	sh := xsheet{name: "Summary"}
	add := func(cells ...xcell) { sh.rows = append(sh.rows, cells) }
	add(txt(r.Definition.Name, xsTitle))
	add()
	loc := reportLocation(r)
	zone := r.Definition.Timezone
	if zone == "" {
		zone = "UTC"
	}
	add(txt("Period", xsLabel), txt(periodText(r), xsText))
	add(txt("Time zone", xsLabel), txt(zone, xsText))
	add(txt("Scope", xsLabel), txt(scopeText(r.Definition), xsText))
	if r.Definition.GroupMode != "" {
		add(txt("Team grouping", xsLabel), txt(groupModeText(r.Definition.GroupMode), xsText))
	}
	filters := filterText(r)
	if len(filters) == 0 {
		filters = []string{"None"}
	}
	add(txt("Filters", xsLabel), txt(strings.Join(filters, "; "), xsText))
	add(txt("Generated", xsLabel), txt(r.GeneratedAt.In(loc).Format("Jan 2, 2006 15:04 MST"), xsText))
	add(txt("Data through", xsLabel), txt(r.DataCutoff.In(loc).Format("Jan 2, 2006 15:04 MST"), xsText))
	add(txt("Source records", xsLabel), num(float64(r.SourceRows), xsInt))
	add()
	add(txt("Headline totals", xsSubtitle))
	sh.tableHeader = len(sh.rows) + 1
	add(txt("Metric", xsHeader), txt("This period", xsHeader), txt("Prior period", xsHeader), txt("Change", xsHeader))
	totals := headlineTotals(r)
	keys := headlineKeys(totals, len(totals))
	for k := range r.PreviousTotals {
		if !containsKey(keys, k) {
			keys = append(keys, k)
		}
	}
	unreliable := r.ComparisonReliable != nil && !*r.ComparisonReliable
	for _, k := range keys {
		c := pdfMetric(r, k)
		cur, prev := totals[k], r.PreviousTotals[k]
		integral := (cur == nil || *cur == math.Trunc(*cur)) && (prev == nil || *prev == math.Trunc(*prev))
		change := txt("", xsText)
		switch {
		case unreliable:
			change = txt("Not assessed", xsMuted)
		case cur != nil && prev != nil && *prev != 0:
			change = num((*cur-*prev)/math.Abs(*prev), xsPercent)
		case cur != nil && prev != nil && *cur == *prev:
			change = num(0, xsPercent)
		}
		add(txt(headerWithUnit(c, false), xsText), numberCell(cur, c.Unit, integral), numberCell(prev, c.Unit, integral), change)
	}
	if notices := readerNotices(r); len(notices) > 0 {
		add()
		add(txt("Notices", xsSubtitle))
		for _, n := range notices {
			add(txt(n, xsWrap))
			sh.merges = append(sh.merges, fmt.Sprintf("A%d:D%d", len(sh.rows), len(sh.rows)))
		}
	}
	sh.widths = []float64{34, 26, 18, 14}
	return sh
}

func aboutSheet(r Result) xsheet {
	sh := xsheet{name: "About"}
	add := func(style int, s string) { sh.rows = append(sh.rows, []xcell{txt(s, style)}) }
	add(xsTitle, "About this report")
	add(xsText, "")
	for _, s := range aboutLines(r) {
		add(xsWrap, s)
	}
	for _, s := range orderedSections(r) {
		if len(s.Notes) == 0 {
			continue
		}
		add(xsText, "")
		add(xsSubtitle, sectionTitle(s))
		for _, n := range s.Notes {
			add(xsWrap, n)
		}
	}
	sh.widths = []float64{110}
	return sh
}

// aboutLines is the plain-English reading guide shared by XLSX and PDF.
func aboutLines(r Result) []string {
	lines := []string{
		"Figures are a snapshot taken when the report ran. Downloading again returns the same numbers.",
		"The period includes its start and excludes its end. Days are calendar days in the report time zone.",
		"Blank or \"Unavailable\" means the value was not recorded. It is never the same as zero.",
		"Costs are the amounts recorded at request time. They are not repriced at today's rates.",
		"Rates such as success rate and cache hit rate are shown as percentages.",
		"Percentiles and distinct counts (such as active users) cannot be added across rows.",
	}
	switch r.Definition.GroupMode {
	case "historical":
		lines = append(lines, "Teams are grouped by membership at the time of each request.")
	case "current":
		lines = append(lines, "Teams are grouped by current membership.")
	}
	if r.Definition.Compare {
		lines = append(lines, "Prior-period values cover the same length of time immediately before this period.")
	}
	return lines
}

func cellRef(col, row int) string {
	name := ""
	for col++; col > 0; col = (col - 1) / 26 {
		name = string(rune('A'+(col-1)%26)) + name
	}
	return name + strconv.Itoa(row)
}

func autoWidths(rows [][]xcell, min, max float64) []float64 {
	w := []float64{}
	for _, row := range rows {
		for i, c := range row {
			for len(w) <= i {
				w = append(w, min)
			}
			n := float64(utf8.RuneCountInString(c.text))
			if c.num != nil {
				n = 14
			}
			if c.style == xsMuted && len(row) == 1 {
				continue
			}
			w[i] = math.Max(w[i], math.Min(max, n+2))
		}
	}
	return w
}

// sheetName makes Excel-safe unique names without truncation noise.
func sheetName(s string, used map[string]bool) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune("[]:*?/\\", r) || r < 32 {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(strings.Trim(s, "'")), " ")
	if s == "" {
		s = "Section"
	}
	if strings.EqualFold(s, "history") {
		s = "History data"
	}
	rr := []rune(s)
	if len(rr) > 31 {
		s = strings.TrimSpace(string(rr[:31]))
	}
	base := s
	for i := 2; used[strings.ToLower(s)]; i++ {
		suffix := fmt.Sprintf(" (%d)", i)
		br := []rune(base)
		if len(br)+len(suffix) > 31 {
			br = br[:31-len(suffix)]
		}
		s = strings.TrimSpace(string(br)) + suffix
	}
	used[strings.ToLower(s)] = true
	return s
}

func worksheet(sh xsheet) (string, error) {
	if len(sh.rows) > 1048576 {
		return "", fmt.Errorf("XLSX row limit exceeded")
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><worksheet xmlns="` + spreadsheetNS + `"><sheetViews><sheetView workbookViewId="0" showGridLines="0">`)
	if sh.freezeRow > 0 {
		fmt.Fprintf(&b, `<pane ySplit="%d" topLeftCell="A%d" activePane="bottomLeft" state="frozen"/>`, sh.freezeRow, sh.freezeRow+1)
	}
	b.WriteString(`</sheetView></sheetViews><sheetFormatPr defaultRowHeight="15"/>`)
	if len(sh.widths) > 0 {
		b.WriteString(`<cols>`)
		for i, w := range sh.widths {
			fmt.Fprintf(&b, `<col min="%d" max="%d" width="%.1f" customWidth="1"/>`, i+1, i+1, w)
		}
		b.WriteString(`</cols>`)
	}
	b.WriteString(`<sheetData>`)
	for i, record := range sh.rows {
		if len(record) > 16384 {
			return "", fmt.Errorf("XLSX column limit exceeded")
		}
		if i+1 == sh.tableHeader {
			fmt.Fprintf(&b, `<row r="%d" ht="20" customHeight="1">`, i+1)
		} else {
			fmt.Fprintf(&b, `<row r="%d">`, i+1)
		}
		for j, c := range record {
			ref := cellRef(j, i+1)
			switch {
			case c.num != nil:
				fmt.Fprintf(&b, `<c r="%s" s="%d"><v>%s</v></c>`, ref, c.style, strconv.FormatFloat(*c.num, 'g', -1, 64))
			case c.text != "":
				if len(utf16.Encode([]rune(c.text))) > 32767 {
					return "", fmt.Errorf("XLSX cell exceeds 32767 characters")
				}
				fmt.Fprintf(&b, `<c r="%s" s="%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, c.style, xmlText(c.text))
			case c.style != xsText:
				fmt.Fprintf(&b, `<c r="%s" s="%d"/>`, ref, c.style)
			}
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData>`)
	if sh.filter != "" {
		b.WriteString(`<autoFilter ref="` + sh.filter + `"/>`)
	}
	if len(sh.merges) > 0 {
		fmt.Fprintf(&b, `<mergeCells count="%d">`, len(sh.merges))
		for _, m := range sh.merges {
			b.WriteString(`<mergeCell ref="` + m + `"/>`)
		}
		b.WriteString(`</mergeCells>`)
	}
	b.WriteString(`<pageMargins left="0.5" right="0.5" top="0.6" bottom="0.6" header="0.3" footer="0.3"/><pageSetup orientation="landscape" fitToWidth="1" fitToHeight="0"/></worksheet>`)
	return b.String(), nil
}

func exportXLSX(w io.Writer, r Result) error {
	used := map[string]bool{"summary": true, "data": true, "about": true}
	sheets := []xsheet{summarySheet(r)}
	cols, rows := r.Columns, r.Rows
	sheets = append(sheets, tableSheet("Data", cols, rows, nil))
	for _, s := range orderedSections(r) {
		title := sectionTitle(s)
		if s.ID == "comparison" && len(s.Rows) == 1 {
			title = "Change vs prior period"
		}
		name := sheetName(title, used)
		switch {
		case s.ID == "comparison" && len(s.Rows) == 1:
			sheets = append(sheets, comparisonSheet(name, r, s))
		case factSection(s):
			sheets = append(sheets, factSheet(name, s))
		default:
			sheets = append(sheets, tableSheet(name, s.Columns, s.Rows, s.Notes))
		}
	}
	sheets = append(sheets, aboutSheet(r))
	// Build fully before writing, so limit errors cannot produce a partial artifact.
	files := map[string]string{}
	order := []string{}
	add := func(n, s string) { files[n] = s; order = append(order, n) }
	content := `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/><Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>`
	workbook := `<?xml version="1.0" encoding="UTF-8"?><workbook xmlns="` + spreadsheetNS + `" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`
	defined := ""
	rels := `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`
	for i, sh := range sheets {
		n := i + 1
		path := fmt.Sprintf("xl/worksheets/sheet%d.xml", n)
		data, err := worksheet(sh)
		if err != nil {
			return err
		}
		add(path, data)
		content += fmt.Sprintf(`<Override PartName="/%s" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, path)
		workbook += fmt.Sprintf(`<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlText(sh.name), n, n)
		rels += fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, n, n)
		if sh.filter != "" {
			ref := strings.ReplaceAll(sh.filter, ":", ":$")
			defined += fmt.Sprintf(`<definedName name="_xlnm._FilterDatabase" localSheetId="%d" hidden="1">'%s'!$%s</definedName>`, i, xmlText(strings.ReplaceAll(sh.name, "'", "''")), dollarRef(ref))
		}
	}
	workbook += `</sheets>`
	if defined != "" {
		workbook += `<definedNames>` + defined + `</definedNames>`
	}
	rels += fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, len(sheets)+1)
	stamp := r.GeneratedAt.UTC().Format("2006-01-02T15:04:05Z")
	add("[Content_Types].xml", content+`</Types>`)
	add("xl/workbook.xml", workbook+`</workbook>`)
	add("xl/styles.xml", xlsxStyles)
	add("xl/_rels/workbook.xml.rels", rels+`</Relationships>`)
	add("docProps/core.xml", `<?xml version="1.0" encoding="UTF-8"?><cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><dc:title>`+xmlText(r.Definition.Name)+`</dc:title><dc:creator>Janus</dc:creator><dcterms:created xsi:type="dcterms:W3CDTF">`+stamp+`</dcterms:created></cp:coreProperties>`)
	add("_rels/.rels", `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/></Relationships>`)
	z := zip.NewWriter(w)
	for _, path := range order {
		f, err := z.CreateHeader(&zip.FileHeader{Name: path, Method: zip.Deflate, Modified: r.GeneratedAt.UTC()})
		if err != nil {
			return err
		}
		if _, err = io.WriteString(f, files[path]); err != nil {
			return err
		}
	}
	return z.Close()
}

// dollarRef converts "A1:$D9" (from the filter) into absolute "A$1:$D$9" form.
func dollarRef(ref string) string {
	parts := strings.Split(strings.ReplaceAll(ref, "$", ""), ":")
	for i, p := range parts {
		j := strings.IndexFunc(p, unicode.IsDigit)
		if j > 0 {
			parts[i] = p[:j] + "$" + p[j:]
		}
	}
	return strings.Join(parts, ":$")
}
