package reporting

// The PDF is a presentation of the frozen result, never a second reporting
// engine. All totals, confidence and section rows come from that same snapshot.
import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/sfnt"
)

type reportPDF struct {
	p           *fpdf.Fpdf
	font        *sfnt.Font
	orientation string
}

func exportPDF(w io.Writer, r Result) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	p := fpdf.New("P", "mm", "A4", "")
	p.SetCatalogSort(true)
	stamp := r.GeneratedAt
	if stamp.IsZero() {
		stamp = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	p.SetCreationDate(stamp)
	p.SetModificationDate(stamp)
	p.SetTitle(r.Definition.Name, true)
	p.SetAuthor("Janus", true)
	p.AddUTF8FontFromBytes("Go", "", goregular.TTF)
	p.AddUTF8FontFromBytes("Go", "B", gobold.TTF)
	font, err := sfnt.Parse(goregular.TTF)
	if err != nil {
		return err
	}
	d := &reportPDF{p: p, font: font, orientation: "P"}
	p.SetMargins(16, 24, 16)
	p.SetAutoPageBreak(false, 18)
	p.AliasNbPages("")
	p.SetHeaderFunc(func() {
		width, _ := p.GetPageSize()
		p.SetFillColor(79, 70, 229)
		p.Rect(16, 11, 2, 7, "F")
		p.Rect(20, 11, 2, 7, "F")
		d.at(26, 10, 80, 8, "janus", 16, true)
		p.SetTextColor(95, 103, 134)
		p.SetFont("Go", "", 8)
		p.SetXY(width-80, 12)
		p.CellFormat(64, 5, "REPORTS / FROZEN SNAPSHOT", "", 0, "R", false, 0, "")
		p.SetDrawColor(226, 229, 240)
		p.Line(16, 21, width-16, 21)
	})
	p.SetFooterFunc(func() {
		width, height := p.GetPageSize()
		p.SetDrawColor(226, 229, 240)
		p.Line(16, height-16, width-16, height-16)
		p.SetTextColor(95, 103, 134)
		p.SetFont("Go", "", 8)
		p.SetXY(16, height-13)
		p.CellFormat(width-32, 5, fmt.Sprintf("Janus   /   Frozen source data attached as report.json                                  Page %d of {nb}", p.PageNo()), "", 0, "", false, 0, "")
	})
	p.SetAttachments([]fpdf.Attachment{{Content: raw, Filename: "report.json", Description: "Complete frozen UTF-8 report result"}})
	d.page("P")
	d.overview(r)
	d.table("Complete report data", "data", r.Columns, r.Rows, nil)
	for _, s := range r.Sections {
		title := s.Title
		if title == "" {
			title = label(s.ID)
		}
		d.table(title, s.ID, s.Columns, s.Rows, s.Notes)
	}
	d.methodology(r)
	return p.Output(w)
}

// Go's embedded fonts cover Latin, Greek and Cyrillic. Preserve unsupported
// glyph identities visibly instead of silently emitting missing-glyph boxes;
// the attachment always retains the exact original Unicode, including CJK.
func (d *reportPDF) text(s string) string {
	var b strings.Builder
	var buf sfnt.Buffer
	for _, r := range s {
		if r == '\n' {
			b.WriteRune(r)
			continue
		}
		if r == '\t' {
			b.WriteString("    ")
			continue
		}
		idx, err := d.font.GlyphIndex(&buf, r)
		if r < 32 || idx == 0 || err != nil {
			fmt.Fprintf(&b, "[U+%04X]", r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func (d *reportPDF) page(o string) {
	d.orientation = o
	d.p.AddPageFormat(o, fpdf.SizeType{Wd: 210, Ht: 297})
	d.p.SetY(28)
}
func (d *reportPDF) width() float64 { w, _ := d.p.GetPageSize(); return w - 32 }
func (d *reportPDF) room(h float64) {
	_, ph := d.p.GetPageSize()
	if d.p.GetY()+h > ph-21 {
		d.page(d.orientation)
	}
}
func (d *reportPDF) at(x, y, w, h float64, s string, size float64, bold bool) {
	style := ""
	if bold {
		style = "B"
	}
	d.p.SetFont("Go", style, size)
	d.p.SetTextColor(18, 20, 31)
	d.p.SetXY(x, y)
	d.p.CellFormat(w, h, d.text(s), "", 0, "L", false, 0, "")
}
func (d *reportPDF) paragraph(s string, size float64) {
	d.p.SetFont("Go", "", size)
	d.p.SetTextColor(95, 103, 134)
	lines := d.p.SplitText(d.text(s), d.width())
	for _, line := range lines {
		d.room(5)
		d.p.SetX(16)
		d.p.CellFormat(d.width(), 5, line, "", 1, "L", false, 0, "")
	}
	d.p.SetY(d.p.GetY() + 2)
}
func (d *reportPDF) heading(s string) {
	d.p.SetFont("Go", "B", 16)
	lines := d.p.SplitText(d.text(s), d.width())
	d.room(float64(len(lines))*8 + 15)
	for _, line := range lines {
		d.at(16, d.p.GetY(), d.width(), 8, line, 16, true)
		d.p.SetY(d.p.GetY() + 8)
	}
	d.p.SetY(d.p.GetY() + 3)
}
func pdfColumnName(c Column) string {
	if c.Label != "" {
		return c.Label
	}
	return label(c.Key)
}
func pdfNumber(v *float64, unit string) string {
	if v == nil {
		return "Unavailable"
	}
	n := *v
	switch strings.ToLower(unit) {
	case "usd":
		if n != 0 && math.Abs(n) < .01 {
			return fmt.Sprintf("$%.4f", n)
		}
		return fmt.Sprintf("$%.2f", n)
	case "eur":
		return fmt.Sprintf("€%.2f", n)
	case "ratio":
		return fmt.Sprintf("%.1f%%", n*100)
	case "count", "tokens", "requests":
		return pdfGrouped(n)
	default:
		return fmt.Sprintf("%.6g", n)
	}
}
func pdfGrouped(n float64) string {
	if n != math.Trunc(n) {
		return fmt.Sprintf("%.6g", n)
	}
	s := fmt.Sprintf("%.0f", n)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign = "-"
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return sign + s
}
func pdfMetric(r Result, key string) Column {
	for _, c := range r.Columns {
		if c.Key == key {
			return c
		}
	}
	for _, s := range r.Sections {
		for _, c := range s.Columns {
			if c.Key == key {
				return c
			}
		}
	}
	return Column{Key: key, Label: label(key), Unit: resultUnit(r, key)}
}
func pdfKeys(m map[string]*float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func (d *reportPDF) overview(r Result) {
	p := d.p
	d.paragraph(strings.ToUpper(strings.ReplaceAll(r.Definition.Template, "_", " ")), 9)
	d.heading(r.Definition.Name)
	loc, err := time.LoadLocation(r.Definition.Timezone)
	if err != nil {
		loc = time.UTC
	}
	zone := r.Definition.Timezone
	if zone == "" {
		zone = "UTC"
	}
	audience := label(r.Definition.Scope) + " scope"
	if r.Definition.Scope == "" {
		audience = "Report scope"
	}
	if r.Definition.TeamID != "" {
		audience += " · Team " + r.Definition.TeamID
	}
	d.paragraph(audience+" · "+r.Start.In(loc).Format("Jan 2, 2006")+" – "+r.End.In(loc).Format("Jan 2, 2006")+" · "+zone, 10)
	d.paragraph("Generated "+r.GeneratedAt.In(loc).Format("Jan 2, 2006 at 15:04 MST")+" · "+pdfGrouped(float64(r.SourceRows))+" source records", 8)
	// Prefer familiar executive metrics without inventing any missing totals.
	totals := make(map[string]*float64, len(r.Totals))
	for k, v := range r.Totals {
		totals[k] = v
	}
	for _, s := range r.Sections {
		if strings.Contains(s.ID, "executive") && len(s.Rows) == 1 {
			for k, v := range s.Rows[0].Values {
				if _, ok := totals[k]; !ok {
					totals[k] = v
				}
			}
		}
	}
	keys := []string{}
	for _, k := range []string{"requests", "cost_usd", "total_tokens", "success_rate", "success_ratio"} {
		if _, ok := totals[k]; ok {
			keys = append(keys, k)
		}
	}
	for _, k := range pdfKeys(totals) {
		if !containsKey(keys, k) {
			keys = append(keys, k)
		}
	}
	if len(keys) > 4 {
		keys = keys[:4]
	}
	if len(keys) > 0 {
		d.room(31)
		y := p.GetY() + 2
		cw := d.width() / float64(len(keys))
		for i, k := range keys {
			x := 16 + float64(i)*cw
			c := pdfMetric(r, k)
			p.SetFillColor(247, 248, 252)
			p.SetDrawColor(226, 229, 240)
			p.RoundedRect(x, y, cw-2, 27, 2, "1234", "DF")
			d.at(x+4, y+3, cw-10, 5, pdfColumnName(c), 8, false)
			val := pdfNumber(totals[k], c.Unit)
			size := 21.0
			p.SetFont("Go", "B", size)
			for p.GetStringWidth(d.text(val)) > cw-10 && size > 10 {
				size--
				p.SetFont("Go", "B", size)
			}
			d.at(x+4, y+10, cw-10, 11, val, size, true)
		}
		p.SetY(y + 32)
	}
	warnings := append(append([]string{}, r.Warnings...), r.ComparisonWarnings...)
	if r.ComparisonReliable != nil && !*r.ComparisonReliable {
		warnings = append([]string{"Comparison unavailable: coverage is incomplete. Recorded prior totals are retained; changes and outlier flags are unavailable."}, warnings...)
	}
	if len(warnings) > 0 {
		d.paragraph("COVERAGE & CONFIDENCE", 9)
		for _, warning := range warnings {
			d.paragraph(warning, 9)
		}
	}
	d.charts(r)
	d.paragraph("Source cutoff: "+r.DataCutoff.In(loc).Format("Jan 2, 2006 at 15:04 MST")+". Observed records only; no live lookups. Complete tables, interpretation notes and provenance follow.", 8)
}
func containsKey(keys []string, k string) bool {
	for _, v := range keys {
		if v == k {
			return true
		}
	}
	return false
}

// pdfChartMetric skips all-null metrics and never compares unlike units.
func pdfChartMetric(cols []Column, rows []Row) (Column, bool) {
	for _, key := range []string{"requests", "total_tokens", "cost_usd"} {
		for _, c := range cols {
			if c.Key == key {
				for _, r := range rows {
					if r.Values[c.Key] != nil {
						return c, true
					}
				}
			}
		}
	}
	for _, c := range cols {
		for _, r := range rows {
			if r.Values[c.Key] != nil {
				return c, true
			}
		}
	}
	return Column{}, false
}
func pdfDate(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02", "2006-01", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
func pdfDateKey(cols []Column, rows []Row) string {
	for _, c := range cols {
		if !(c.Key == "day" || c.Key == "date" || c.Key == "bucket" || c.Key == "month" || c.Key == "week" || c.Key == "hour" || c.Key == "time") {
			continue
		}
		for _, r := range rows {
			if _, ok := pdfDate(r.Dimensions[c.Key]); ok {
				return c.Key
			}
		}
	}
	return ""
}

// A single time axis cannot distinguish multiple dimensions or duplicate
// buckets. Date-only buckets also lack an offset: do not silently interpret
// local calendar dates as UTC, or extend partial report boundaries to fit them.
// Keep the complete frozen tables rather than inventing an aggregation.
func pdfTimeAxisSafe(r Result, s Section) bool {
	key := pdfDateKey(s.Columns, s.Rows)
	seen := map[time.Time]bool{}
	for _, row := range s.Rows {
		if len(row.Dimensions) != 1 {
			return false
		}
		value := row.Dimensions[key]
		dt, ok := pdfDate(value)
		if !ok || seen[dt] {
			return false
		}
		seen[dt] = true
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			if r.Definition.Timezone != "" && r.Definition.Timezone != "UTC" {
				return false
			}
		}
		if (!r.Start.IsZero() && dt.Before(r.Start)) || (!r.End.IsZero() && !dt.Before(r.End)) {
			return false
		}
	}
	return true
}

func (d *reportPDF) charts(r Result) {
	sets := append([]Section{{ID: "data", Title: "Main data", Columns: r.Columns, Rows: r.Rows}}, r.Sections...)
	timeline, ranking := -1, -1
	for i, s := range sets {
		if _, ok := pdfChartMetric(s.Columns, s.Rows); !ok {
			continue
		}
		if pdfDateKey(s.Columns, s.Rows) != "" {
			if !pdfTimeAxisSafe(r, s) {
				d.paragraph("Time chart unavailable: "+s.Title+" has ambiguous calendar boundaries or multiple series. See complete frozen tables; no aggregation or timezone assumptions applied.", 9)
				continue
			}
			if timeline < 0 {
				timeline = i
			}
		}
		if pdfDateKey(s.Columns, s.Rows) == "" && len(s.Rows) > 0 {
			hasDim := false
			for _, row := range s.Rows {
				if len(row.Dimensions) > 0 {
					hasDim = true
					break
				}
			}
			if hasDim && (ranking < 0 || strings.Contains(s.ID, "model")) {
				ranking = i
			}
		}
	}
	if timeline >= 0 {
		s := sets[timeline]
		c, _ := pdfChartMetric(s.Columns, s.Rows)
		d.timeline(r, s, c)
	}
	if ranking >= 0 {
		s := sets[ranking]
		c, _ := pdfChartMetric(s.Columns, s.Rows)
		d.ranking(s, c)
	}
	if timeline < 0 && ranking < 0 {
		if len(r.Rows) == 0 {
			d.paragraph("Chart unavailable: no main data rows.", 9)
		} else if _, ok := pdfChartMetric(r.Columns, r.Rows); !ok {
			d.paragraph("Chart unavailable: no non-null metric in declared columns.", 9)
		} else {
			d.paragraph("No categorical or time dimension supplied; signed values and zero observations are shown in the complete tables.", 9)
		}
	}
}

type pdfObservation struct {
	date  time.Time
	value *float64
}

func pdfObservations(s Section, c Column) []pdfObservation {
	key := pdfDateKey(s.Columns, s.Rows)
	out := []pdfObservation{}
	for _, row := range s.Rows {
		if dt, ok := pdfDate(row.Dimensions[key]); ok {
			out = append(out, pdfObservation{dt, row.Values[c.Key]})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].date.Before(out[j].date) })
	return out
}
func (d *reportPDF) timeline(r Result, s Section, c Column) {
	points := pdfObservations(s, c)
	if len(points) == 0 {
		return
	}
	d.room(92)
	d.heading("Activity over time")
	d.paragraph(pdfColumnName(c)+" · entire reporting period · "+fmt.Sprint(len(points))+" observations", 9)
	p := d.p
	y := p.GetY()
	x := 33.0
	cw := d.width() - 20
	ch := 40.0
	start, end := r.Start, r.End
	if start.IsZero() || points[0].date.Before(start) {
		start = points[0].date
	}
	if end.IsZero() || !end.After(points[len(points)-1].date) {
		end = points[len(points)-1].date.Add(24 * time.Hour)
	}
	span := end.Sub(start).Seconds()
	if span <= 0 {
		span = 1
	}
	low, high := 0.0, 0.0
	for _, v := range points {
		if v.value != nil {
			low = math.Min(low, *v.value)
			high = math.Max(high, *v.value)
		}
	}
	if high == low {
		high = low + 1
	}
	step := math.Pow(10, math.Floor(math.Log10((high-low)/4)))
	for _, factor := range []float64{1, 2, 5, 10} {
		if step*factor >= (high-low)/4 {
			step *= factor
			break
		}
	}
	if c.Unit == "count" {
		step = math.Max(1, step)
	}
	low = math.Floor(low/step) * step
	high = math.Ceil(high/step) * step
	py := func(v float64) float64 { return y + ch - (v-low)/(high-low)*ch }
	p.SetDrawColor(226, 229, 240)
	p.SetLineWidth(.2)
	for i := 0; i <= int(math.Round((high-low)/step)); i++ {
		v := low + step*float64(i)
		yy := py(v)
		p.Line(x, yy, x+cw, yy)
		d.at(16, yy-2, 16, 4, pdfNumber(&v, c.Unit), 6, false)
	}
	// Calendar-proportional positions; missing observations are never connected
	// or filled as zero. Duplicate timestamps remain independent observations.
	bw := math.Max(.25, math.Min(3, cw/float64(len(points)+1)*.6))
	for _, v := range points {
		if v.value == nil {
			continue
		}
		xx := x + v.date.Sub(start).Seconds()/span*cw
		yy := py(*v.value)
		base := py(0)
		p.SetFillColor(79, 70, 229)
		if *v.value == 0 {
			p.Circle(xx, base, .55, "F")
		} else {
			p.Rect(xx, math.Min(yy, base), math.Min(bw, x+cw-xx), math.Abs(base-yy), "F")
		}
	}
	d.at(x, y+ch+2, 40, 5, start.Format("Jan 2, 2006"), 8, false)
	d.at(x+cw-31, y+ch+2, 33, 5, end.Format("Jan 2, 2006"), 8, false)
	p.SetY(y + ch + 10)
	d.paragraph("Gaps are missing observations, not confirmed zero. Partial boundary periods are included. Each metric uses its own scale; all source rows follow.", 8)
}
func pdfRowLabel(cols []Column, row Row, index int) string {
	parts := []string{}
	// Producers often return both an opaque dimension and its display-name
	// companion. Prefer the companion, while preserving identities in tables.
	for _, c := range tableColumns(cols, []Row{row}) {
		v, ok := row.Dimensions[c.Key]
		if !ok || v == "" {
			continue
		}
		if _, named := row.Dimensions[c.Key+"_name"]; named {
			continue
		}
		if _, named := row.Dimensions[c.Key+"_label"]; named {
			continue
		}
		if strings.HasSuffix(c.Key, "_id") {
			if _, named := row.Dimensions[strings.TrimSuffix(c.Key, "_id")+"_name"]; named {
				continue
			}
		}
		parts = append(parts, v)
	}
	if len(parts) == 0 {
		return fmt.Sprintf("Row %d", index+1)
	}
	return strings.Join(parts, " · ")
}
func (d *reportPDF) ranking(s Section, c Column) {
	indices := make([]int, len(s.Rows))
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(i, j int) bool {
		a, b := s.Rows[indices[i]].Values[c.Key], s.Rows[indices[j]].Values[c.Key]
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		return *a > *b
	})
	shown := len(indices)
	if shown > 8 {
		shown = 8
	}
	d.room(35 + float64(shown)*15)
	d.heading("Category ranking")
	d.paragraph(pdfColumnName(c)+" · "+s.Title+fmt.Sprintf(" · top %d of %d groups; all groups in appendix", shown, len(indices)), 9)
	low, high := 0.0, 0.0
	for _, i := range indices[:shown] {
		if v := s.Rows[i].Values[c.Key]; v != nil {
			low = math.Min(low, *v)
			high = math.Max(high, *v)
		}
	}
	if high == low {
		high = low + 1
	}
	for _, i := range indices[:shown] {
		p := d.p
		name := d.text(pdfRowLabel(s.Columns, s.Rows[i], i))
		p.SetFont("Go", "", 9)
		// Wrap names instead of truncating identities. Label and value share a row.
		lines := p.SplitText(name, d.width()-42)
		h := float64(len(lines)) * 4.5
		d.room(h + 10)
		y := p.GetY()
		for j, line := range lines {
			d.at(16, y+float64(j)*4.5, d.width()-42, 4.5, line, 9, false)
		}
		v := s.Rows[i].Values[c.Key]
		d.at(16+d.width()-39, y, 39, 4.5, pdfNumber(v, c.Unit), 9, true)
		yy := y + h + 2
		cw := d.width()
		p.SetFillColor(238, 237, 252)
		p.Rect(16, yy, cw, 2, "F")
		base := 16 + (0-low)/(high-low)*cw
		if v != nil {
			end := 16 + (*v-low)/(high-low)*cw
			p.SetFillColor(79, 70, 229)
			if *v == 0 {
				p.Circle(base, yy+1, .6, "F")
			} else {
				p.Rect(math.Min(base, end), yy, math.Abs(base-end), 2, "F")
			}
		}
		p.SetY(yy + 7)
	}
	d.paragraph("Descending source values; zero baseline. Unavailable is not zero. Repeated display names may represent distinct identities.", 8)
}

// Tables use real cell geometry. Landscape keeps all columns of a logical row
// together. Extremely wide schemas use a labeled record card, not column bands.
func (d *reportPDF) table(title, id string, cols []Column, rows []Row, notes []string) {
	cols = tableColumns(cols, rows)
	orientation := "P"
	if len(cols) > 5 {
		orientation = "L"
	}
	if id == "data" || d.orientation != orientation {
		d.page(orientation)
	} else {
		d.room(42)
		d.p.SetY(d.p.GetY() + 8)
	}
	d.heading(title)
	d.paragraph(fmt.Sprintf("%d rows · complete frozen detail", len(rows)), 8)
	for _, n := range notes {
		d.paragraph("Note: "+n, 9)
	}
	if len(cols) == 0 {
		d.paragraph("No columns supplied.", 9)
		return
	}
	if len(cols) > 11 {
		d.recordCards(cols, rows)
		return
	}
	p := d.p
	size := 9.5
	if len(cols) > 7 {
		size = 8.0
	}
	widths := make([]float64, len(cols))
	weights := 0.0
	for i, c := range cols {
		w := 1.0
		for _, row := range rows {
			if _, ok := row.Dimensions[c.Key]; ok {
				w = 1.8
				break
			}
		}
		widths[i] = w
		weights += w
	}
	for i := range widths {
		widths[i] = d.width() * widths[i] / weights
	}
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = pdfColumnName(c)
		if c.Unit != "" {
			header[i] += "\n" + c.Unit
		}
	}
	drawHeader := func() { d.cells(header, widths, size, true, 0) }
	// Keep the repeated heading and at least one body row together.
	d.room(28)
	drawHeader()
	if len(rows) == 0 {
		d.paragraph("No rows returned. This is not a confirmed zero.", 9)
	}
	for i, row := range rows {
		cells := make([]string, len(cols))
		for j, c := range cols {
			if v, ok := row.Dimensions[c.Key]; ok {
				cells[j] = v
			} else {
				cells[j] = pdfNumber(row.Values[c.Key], c.Unit)
			}
		}
		p.SetFont("Go", "", size)
		height := d.cellHeight(cells, widths, size)
		_, ph := p.GetPageSize()
		if p.GetY()+height > ph-21 {
			d.page(orientation)
			d.heading(title + " · continued")
			drawHeader()
		}
		// An unusually long single cell can exceed a page. Continue that same
		// logical row with all columns and an explicit continuation label.
		d.rowFragments(cells, widths, size, i, drawHeader, title)
	}
}
func (d *reportPDF) cellHeight(cells []string, widths []float64, size float64) float64 {
	h := 0.0
	for i, c := range cells {
		n := len(d.p.SplitText(d.text(c), widths[i]-4))
		h = math.Max(h, float64(n)*(size*.43))
	}
	return h + 5
}
func (d *reportPDF) cells(cells []string, widths []float64, size float64, header bool, index int) {
	p := d.p
	style := ""
	if header {
		style = "B"
	}
	p.SetFont("Go", style, size)
	h := d.cellHeight(cells, widths, size)
	x, y := 16.0, p.GetY()
	for i, c := range cells {
		p.SetFillColor(255, 255, 255)
		if header {
			p.SetFillColor(238, 237, 252)
		} else if index%2 == 1 {
			p.SetFillColor(247, 248, 252)
		}
		p.SetDrawColor(226, 229, 240)
		p.SetLineWidth(.15)
		p.Rect(x, y, widths[i], h, "DF")
		p.SetTextColor(18, 20, 31)
		p.SetXY(x+2, y+2.5)
		p.MultiCell(widths[i]-4, size*.43, d.text(c), "", "L", false)
		x += widths[i]
	}
	p.SetXY(16, y+h)
}
func (d *reportPDF) rowFragments(cells []string, widths []float64, size float64, index int, header func(), title string) {
	p := d.p
	p.SetFont("Go", "", size)
	lines := make([][]string, len(cells))
	remaining := 0
	for i, c := range cells {
		lines[i] = p.SplitText(d.text(c), widths[i]-4)
		remaining = max(remaining, len(lines[i]))
	}
	for offset := 0; offset < remaining; {
		_, ph := p.GetPageSize()
		capacity := int((ph - 21 - p.GetY() - 5) / (size * .43))
		if capacity < 1 {
			d.page(d.orientation)
			d.heading(title + " · continued")
			header()
			continue
		}
		count := min(capacity, remaining-offset)
		part := make([]string, len(cells))
		for i, ls := range lines {
			if offset < len(ls) {
				part[i] = strings.Join(ls[offset:min(offset+count, len(ls))], "\n")
			}
		}
		d.cells(part, widths, size, false, index)
		offset += count
		if offset < remaining {
			d.page(d.orientation)
			d.heading(title + " · continued")
			d.paragraph(fmt.Sprintf("Row %d continued", index+1), 8)
			header()
		}
	}
}
func (d *reportPDF) recordCards(cols []Column, rows []Row) {
	for i, row := range rows {
		d.room(25)
		d.heading(fmt.Sprintf("Record %d", i+1))
		for _, c := range cols {
			v, ok := row.Dimensions[c.Key]
			if !ok {
				v = pdfNumber(row.Values[c.Key], c.Unit)
			}
			field := pdfColumnName(c) + " (" + c.Unit + ")"
			cells := []string{field, v}
			widths := []float64{d.width() * .3, d.width() * .7}
			title := fmt.Sprintf("Record %d", i+1)
			d.p.SetFont("Go", "", 8)
			_, ph := d.p.GetPageSize()
			if d.p.GetY()+d.cellHeight(cells, widths, 8) > ph-21 {
				d.page(d.orientation)
				d.heading(title + " · continued")
			}
			d.rowFragments(cells, widths, 8, i, func() {
				d.cells([]string{field, ""}, widths, 8, true, i)
			}, title)
		}
	}
}
func (d *reportPDF) methodology(r Result) {
	d.page("P")
	d.heading("Methodology & provenance")
	d.paragraph("Frozen snapshot; half-open [start,end). Null means unavailable, never zero. Ratios are displayed as percentages. Percentiles and distinct counts are not additive. Recorded amounts are not repriced. No estimates or monetization are inferred by this renderer.", 9)
	d.paragraph("Fonts are embedded. Unsupported glyphs are identified as [U+XXXX]; the attached report.json preserves every original UTF-8 character, exact numeric value, definition field, filter and row.", 9)
	// Machine payloads belong in the attachment; their meaning is visible here.
	for _, m := range metadata(r) {
		switch m[0] {
		case "definition", "totals", "previous_totals", "filters", "unit", "section_unit":
			continue
		case "comparison_warnings":
			d.paragraph("Comparison warnings (comparison_warnings)", 9)
			if len(r.ComparisonWarnings) == 0 {
				d.paragraph("None recorded.", 9)
			}
			for _, warning := range r.ComparisonWarnings {
				d.paragraph(warning, 9)
			}
			continue
		}
		d.room(13)
		d.paragraph(strings.Join(m, ": "), 9)
	}
	d.heading("Scope & interpretation")
	d.paragraph("Scope: "+r.Definition.Scope+". Team: "+r.Definition.TeamID+". Group membership: "+r.Definition.GroupMode+". Template: "+r.Definition.Template+". Comparison requested: "+fmt.Sprint(r.Definition.Compare)+".", 9)
	keys := make([]string, 0, len(r.Definition.Filters))
	for k := range r.Definition.Filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		d.paragraph("Filters: none specified.", 9)
	}
	for _, k := range keys {
		d.paragraph("Filter "+k+": "+strings.Join(r.Definition.Filters[k], ", "), 9)
	}
	d.heading("Totals & recorded prior values")
	d.paragraph("previous_totals: recorded source values, not a statement that periods are comparable. No percentage change is inferred.", 9)
	all := map[string]*float64{}
	for k, v := range r.Totals {
		all[k] = v
	}
	for k := range r.PreviousTotals {
		if _, ok := all[k]; !ok {
			all[k] = nil
		}
	}
	for _, k := range pdfKeys(all) {
		c := pdfMetric(r, k)
		d.room(14)
		d.paragraph(pdfColumnName(c)+" ["+k+"] · "+c.Unit+": "+pdfNumber(r.Totals[k], c.Unit)+"; prior: "+pdfNumber(r.PreviousTotals[k], c.Unit), 9)
	}
	d.heading("Field reference")
	sets := append([]Section{{ID: "data", Columns: r.Columns, Rows: r.Rows}}, r.Sections...)
	for _, s := range sets {
		d.room(15)
		d.paragraph(s.ID, 10)
		for _, c := range tableColumns(s.Columns, s.Rows) {
			unit := c.Unit
			if unit == "" {
				unit = "dimension / source value"
			}
			d.paragraph(c.Key+": "+pdfColumnName(c)+" · "+unit, 8)
		}
	}
}
