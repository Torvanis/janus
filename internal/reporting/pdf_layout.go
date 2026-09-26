package reporting

// The PDF is a presentation of the frozen result, never a second reporting
// engine. All totals, confidence and section rows come from that same snapshot,
// which is also attached byte-for-byte as report.json.
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
	aligns      []string // per-column alignment for the table being drawn
}

type rgb struct{ r, g, b int }

var (
	pdfInk     = rgb{18, 20, 31}
	pdfMuted   = rgb{95, 103, 134}
	pdfLine    = rgb{226, 229, 240}
	pdfAccent  = rgb{79, 70, 229}
	pdfAccent2 = rgb{13, 148, 136}
	pdfTint    = rgb{238, 237, 252}
	pdfZebra   = rgb{248, 249, 252}
	pdfGood    = rgb{21, 128, 61}
	pdfBad     = rgb{185, 28, 28}
	pdfWarnInk = rgb{146, 64, 14}
)

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
	loc := reportLocation(r)
	p.SetHeaderFunc(func() {
		width, _ := p.GetPageSize()
		d.fill(pdfAccent)
		p.Rect(16, 11, 2, 7, "F")
		p.Rect(20, 11, 2, 7, "F")
		d.at(26, 10, 40, 8, "janus", 16, true)
		if p.PageNo() > 1 {
			p.SetFont("Go", "", 8)
			d.atc(70, 12, width-86, 5, d.fit(r.Definition.Name, width-86), 8, false, pdfMuted, "R")
		}
		d.draw(pdfLine)
		p.SetLineWidth(.2)
		p.Line(16, 21, width-16, 21)
	})
	p.SetFooterFunc(func() {
		width, height := p.GetPageSize()
		d.draw(pdfLine)
		p.SetLineWidth(.2)
		p.Line(16, height-16, width-16, height-16)
		left := "Janus report"
		if !r.GeneratedAt.IsZero() {
			left += " · Generated " + r.GeneratedAt.In(loc).Format("Jan 2, 2006")
		}
		d.atc(16, height-13, width-72, 5, left, 8, false, pdfMuted, "L")
		d.atc(width-56, height-13, 40, 5, fmt.Sprintf("Page %d of {nb}", p.PageNo()), 8, false, pdfMuted, "R")
	})
	p.SetAttachments([]fpdf.Attachment{{Content: raw, Filename: "report.json", Description: "Complete machine-readable report"}})
	d.page("P")
	d.cover(r)
	d.table("Complete report data", "data", r.Columns, r.Rows, nil)
	for _, s := range orderedSections(r) {
		d.section(r, s)
	}
	d.about(r)
	return p.Output(w)
}

func (d *reportPDF) color(c rgb) { d.p.SetTextColor(c.r, c.g, c.b) }
func (d *reportPDF) fill(c rgb)  { d.p.SetFillColor(c.r, c.g, c.b) }
func (d *reportPDF) draw(c rgb)  { d.p.SetDrawColor(c.r, c.g, c.b) }

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

// fit shortens s with an ellipsis to width using the current font.
func (d *reportPDF) fit(s string, width float64) string {
	s = d.text(s)
	if d.p.GetStringWidth(s) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 1 && d.p.GetStringWidth(string(r)+"…") > width {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
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
	d.atc(x, y, w, h, s, size, bold, pdfInk, "L")
}
func (d *reportPDF) atc(x, y, w, h float64, s string, size float64, bold bool, c rgb, align string) {
	style := ""
	if bold {
		style = "B"
	}
	d.p.SetFont("Go", style, size)
	d.color(c)
	d.p.SetXY(x, y)
	d.p.CellFormat(w, h, d.text(s), "", 0, align, false, 0, "")
}
func (d *reportPDF) paragraph(s string, size float64) { d.paragraphc(s, size, pdfMuted, false) }
func (d *reportPDF) paragraphc(s string, size float64, c rgb, bold bool) {
	style := ""
	if bold {
		style = "B"
	}
	d.p.SetFont("Go", style, size)
	d.color(c)
	lh := size * .5
	for _, line := range d.p.SplitText(d.text(s), d.width()) {
		d.room(lh)
		d.p.SetX(16)
		d.p.CellFormat(d.width(), lh, line, "", 1, "L", false, 0, "")
	}
	d.p.SetY(d.p.GetY() + 1.5)
}
func (d *reportPDF) heading(s string) { d.headingSized(s, 15) }
func (d *reportPDF) headingSized(s string, size float64) {
	d.p.SetFont("Go", "B", size)
	lh := size * .5
	lines := d.p.SplitText(d.text(s), d.width())
	d.room(float64(len(lines))*lh + 15)
	for _, line := range lines {
		d.at(16, d.p.GetY(), d.width(), lh, line, size, true)
		d.p.SetY(d.p.GetY() + lh)
	}
	d.p.SetY(d.p.GetY() + 2)
}

// pdfNumber is the reader format for one value (grouped, currency, percent).
func pdfNumber(v *float64, unit string) string { return formatValue(v, unit) }

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
func containsKey(keys []string, k string) bool {
	for _, v := range keys {
		if v == k {
			return true
		}
	}
	return false
}

func (d *reportPDF) cover(r Result) {
	p := d.p
	if t := strings.TrimSpace(strings.ReplaceAll(r.Definition.Template, "_", " ")); t != "" {
		d.paragraphc(strings.ToUpper(t)+" REPORT", 8, pdfAccent, true)
	}
	d.headingSized(r.Definition.Name, 20)
	zone := r.Definition.Timezone
	if zone == "" {
		zone = "UTC"
	}
	d.paragraphc(scopeText(r.Definition)+" · "+periodText(r)+" · "+zone, 10, pdfInk, false)
	loc := reportLocation(r)
	meta := []string{}
	if !r.DataCutoff.IsZero() {
		meta = append(meta, "Data through "+r.DataCutoff.In(loc).Format("Jan 2, 2006 15:04 MST"))
	}
	meta = append(meta, grouped(float64(r.SourceRows), 0)+" source records")
	if f := filterText(r); len(f) > 0 {
		meta = append(meta, "Filters: "+strings.Join(f, "; "))
	}
	d.paragraph(strings.Join(meta, " · "), 8)
	totals := headlineTotals(r)
	keys := headlineKeys(totals, 4)
	unreliable := r.ComparisonReliable != nil && !*r.ComparisonReliable
	if len(keys) > 0 {
		d.room(34)
		y := p.GetY() + 2
		cw := d.width() / float64(len(keys))
		for i, k := range keys {
			x := 16 + float64(i)*cw
			c := pdfMetric(r, k)
			d.fill(rgb{247, 248, 252})
			d.draw(pdfLine)
			p.SetLineWidth(.2)
			p.RoundedRect(x, y, cw-3, 29, 2, "1234", "DF")
			d.fill(pdfAccent)
			p.Rect(x, y+4, .9, 21, "F")
			p.SetFont("Go", "", 8)
			d.atc(x+4, y+3, cw-10, 5, d.fit(headerWithUnit(c, false), cw-10), 8, false, pdfMuted, "L")
			val := compactValue(totals[k], c.Unit)
			size := 20.0
			p.SetFont("Go", "B", size)
			for p.GetStringWidth(d.text(val)) > cw-10 && size > 10 {
				size--
				p.SetFont("Go", "B", size)
			}
			d.at(x+4, y+10, cw-10, 10, val, size, true)
			if len(r.PreviousTotals) > 0 || r.Definition.Compare {
				text, dir := changeSummary(totals[k], r.PreviousTotals[k], c.Unit)
				col := pdfMuted
				if unreliable {
					text, dir = "Change not assessed", 0
				}
				if dir != 0 {
					good := dir > 0
					if higherIsWorse(k) {
						good = !good
					}
					col = pdfBad
					if good {
						col = pdfGood
					}
					arrow := "▲ "
					if dir < 0 {
						arrow = "▼ "
					}
					text = arrow + text
				}
				p.SetFont("Go", "B", 7.5)
				if p.GetStringWidth(d.text(text)) > cw-9 {
					if i := strings.Index(text, " ("); i > 0 {
						text = text[:i]
					}
				}
				d.atc(x+4, y+21.5, cw-8, 4, d.fit(text, cw-9), 7.5, dir != 0, col, "L")
			}
		}
		p.SetY(y + 34)
	}
	d.notices(readerNotices(r))
	d.charts(r)
}

// notices draws every reader caveat once, in a single callout.
func (d *reportPDF) notices(list []string) {
	if len(list) == 0 {
		return
	}
	p := d.p
	p.SetFont("Go", "", 8.5)
	lines := [][]string{}
	h := 9.5
	for _, n := range list {
		l := p.SplitText(d.text(n), d.width()-14)
		lines = append(lines, l)
		h += float64(len(l))*4.2 + 1.2
	}
	d.room(h + 4)
	y := p.GetY()
	p.SetFillColor(255, 250, 240)
	p.SetDrawColor(240, 205, 140)
	p.SetLineWidth(.25)
	p.RoundedRect(16, y, d.width(), h, 2, "1234", "DF")
	d.atc(21, y+2.5, d.width()-10, 4, "Notices", 8.5, true, pdfWarnInk, "L")
	yy := y + 7.5
	for _, l := range lines {
		d.atc(21, yy, 4, 4.2, "•", 8.5, false, pdfWarnInk, "L")
		for _, line := range l {
			p.SetFont("Go", "", 8.5)
			d.color(rgb{87, 54, 20})
			p.SetXY(25, yy)
			p.CellFormat(d.width()-14, 4.2, line, "", 0, "L", false, 0, "")
			yy += 4.2
		}
		yy += 1.2
	}
	p.SetY(y + h + 5)
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
// buckets. Date-only buckets lack an offset, so they are only interpreted in
// UTC reports. A day bucket may overlap a partial first or last day (rolling
// periods start mid-day); it is drawn and the partial boundary is disclosed.
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
		span := time.Duration(0)
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			if r.Definition.Timezone != "" && r.Definition.Timezone != "UTC" {
				return false
			}
			span = 24 * time.Hour
		}
		if (!r.Start.IsZero() && !dt.Add(span).After(r.Start) && !(span == 0 && dt.Equal(r.Start))) || (!r.End.IsZero() && !dt.Before(r.End)) {
			return false
		}
	}
	return true
}

func partialBoundary(r Result) bool {
	midnight := func(t time.Time) bool {
		if t.IsZero() {
			return true
		}
		t = t.In(reportLocation(r))
		return t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0
	}
	return !midnight(r.Start) || !midnight(r.End)
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
				d.paragraph("Time chart unavailable: "+sectionTitle(s)+" has more than one series per date or local dates outside UTC. The complete table follows.", 8.5)
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
		metrics := []Column{}
		for _, key := range []string{"requests", "cost_usd", "total_tokens"} {
			for _, c := range s.Columns {
				if c.Key != key || len(metrics) >= 2 {
					continue
				}
				for _, row := range s.Rows {
					if row.Values[c.Key] != nil {
						metrics = append(metrics, c)
						break
					}
				}
			}
		}
		if len(metrics) == 0 {
			c, _ := pdfChartMetric(s.Columns, s.Rows)
			metrics = append(metrics, c)
		}
		d.room(60)
		d.p.SetY(d.p.GetY() + 1)
		d.headingSized("Activity over time", 13)
		for i, c := range metrics {
			d.timeline(r, s, c, []rgb{pdfAccent, pdfAccent2}[i%2])
		}
		note := "Each bar is one day. Gaps are days with no recorded data, not confirmed zeros."
		if partialBoundary(r) {
			note += " The first and last days are partial because the period starts and ends mid-day."
		}
		d.paragraph(note, 7.5)
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
			d.paragraph("No category or date breakdown to chart. All values, including zeros and negatives, are in the tables that follow.", 9)
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

// niceStep picks a 1/2/5×10^n tick step giving about four intervals.
func niceStep(span float64, whole bool) float64 {
	if span <= 0 {
		span = 1
	}
	step := math.Pow(10, math.Floor(math.Log10(span/4)))
	for _, f := range []float64{1, 2, 2.5, 5, 10} {
		if step*f >= span/4 {
			step *= f
			break
		}
	}
	if whole {
		step = math.Max(1, math.Round(step))
	}
	return step
}

func axisValue(v float64, unit string) string {
	c := v
	switch strings.ToLower(unit) {
	case "usd":
		if math.Abs(v) >= 1000 {
			return "$" + trimmed(v/1000, 1) + "k"
		}
		return "$" + trimmed(v, 0)
	case "ratio":
		return trimmed(v*100, 0) + "%"
	}
	if math.Abs(c) >= 1e6 {
		return trimmed(c/1e6, 1) + "M"
	}
	if math.Abs(c) >= 1e4 {
		return trimmed(c/1e3, 0) + "k"
	}
	return grouped(c, 0)
}

func (d *reportPDF) timeline(r Result, s Section, c Column, col rgb) {
	points := pdfObservations(s, c)
	if len(points) == 0 {
		return
	}
	var total float64
	have := 0
	for _, v := range points {
		if v.value != nil {
			total += *v.value
			have++
		}
	}
	d.room(46)
	p := d.p
	title := columnTitle(c) + " per day"
	sub := fmt.Sprintf("%d observations", len(points))
	if have > 0 && !strings.Contains(c.Key, "active") && !strings.Contains(c.Key, "rate") && !strings.EqualFold(c.Unit, "ratio") {
		v := total
		sub = "Total " + formatValue(&v, c.Unit) + " · " + sub
	}
	d.atc(16, p.GetY(), d.width()/2, 5, title, 9.5, true, pdfInk, "L")
	d.atc(16+d.width()/2, p.GetY(), d.width()/2, 5, sub, 8, false, pdfMuted, "R")
	y := p.GetY() + 7
	x := 30.0
	cw := d.width() - 14
	ch := 20.0
	start, end := r.Start, r.End
	day := time.Duration(0)
	if _, err := time.Parse(time.RFC3339Nano, s.Rows[0].Dimensions[pdfDateKey(s.Columns, s.Rows)]); err != nil {
		day = 24 * time.Hour
	}
	// Axis runs over whole calendar days that the observations occupy.
	if start.IsZero() || points[0].date.Before(start) {
		start = points[0].date
	}
	if day > 0 {
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	}
	last := points[len(points)-1].date.Add(day)
	if end.IsZero() || end.Before(last) {
		end = last
	}
	if day > 0 && !end.Equal(end.Truncate(day)) {
		end = end.Truncate(day).Add(day)
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
	whole := strings.EqualFold(c.Unit, "count") || strings.EqualFold(c.Unit, "tokens")
	step := niceStep(high-low, whole)
	low = math.Floor(low/step) * step
	high = math.Ceil(high/step) * step
	py := func(v float64) float64 { return y + ch - (v-low)/(high-low)*ch }
	p.SetLineWidth(.15)
	for i := 0; i <= int(math.Round((high-low)/step)); i++ {
		v := low + step*float64(i)
		yy := py(v)
		d.draw(pdfLine)
		p.Line(x, yy, x+cw, yy)
		d.atc(16, yy-2, 12.5, 4, axisValue(v, c.Unit), 6.5, false, pdfMuted, "R")
	}
	slot := cw / math.Max(1, span/math.Max(1, day.Seconds()))
	bw := math.Max(.4, slot*.72)
	if day == 0 {
		bw = math.Max(.4, math.Min(3, cw/float64(len(points)+1)*.6))
	}
	d.fill(col)
	for _, v := range points {
		if v.value == nil {
			continue
		}
		xx := x + v.date.Sub(start).Seconds()/span*cw + (slot-bw)/2
		if day == 0 {
			xx = x + v.date.Sub(start).Seconds()/span*cw
		}
		yy, base := py(*v.value), py(0)
		if *v.value == 0 {
			p.Circle(xx+bw/2, base, .5, "F")
		} else {
			p.Rect(xx, math.Min(yy, base), math.Min(bw, x+cw-xx), math.Abs(base-yy), "F")
		}
	}
	d.draw(rgb{190, 194, 210})
	p.SetLineWidth(.25)
	p.Line(x, py(0), x+cw, py(0))
	// Date ticks: first day, then every Monday (or month start for long spans).
	days := int(math.Round(span / 86400))
	lastLabel := -100.0
	for i := 0; i <= days; i++ {
		t := start.AddDate(0, 0, i)
		first := i == 0
		weekly := days <= 70 && i%7 == 0
		monthly := days > 70 && t.Day() == 1
		if !first && !weekly && !monthly {
			continue
		}
		xx := x + t.Sub(start).Seconds()/span*cw
		if xx-lastLabel < 16 || xx > x+cw-8 {
			continue
		}
		lastLabel = xx
		d.draw(rgb{190, 194, 210})
		p.Line(xx, y+ch, xx, y+ch+1)
		label := t.Format("Jan 2")
		if first || (monthly && t.Month() == time.January) {
			label = t.Format("Jan 2, 2006")
		}
		d.atc(xx-1, y+ch+1.2, 24, 3.5, label, 6.5, false, pdfMuted, "L")
	}
	p.SetY(y + ch + 7)
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
	shown := min(len(indices), 8)
	var total float64
	for _, i := range indices {
		if v := s.Rows[i].Values[c.Key]; v != nil {
			total += *v
		}
	}
	d.room(20 + float64(shown)*5.6)
	d.p.SetY(d.p.GetY() + 2)
	d.headingSized("Category ranking", 13)
	d.paragraph(columnTitle(c)+" · "+sectionTitle(s)+fmt.Sprintf(" · top %d of %d; every group is in the tables that follow", shown, len(indices)), 8)
	high := 0.0
	for _, i := range indices[:shown] {
		if v := s.Rows[i].Values[c.Key]; v != nil {
			high = math.Max(high, math.Abs(*v))
		}
	}
	if high == 0 {
		high = 1
	}
	p := d.p
	rankW, labelW, valueW, shareW := 7.0, 56.0, 28.0, 14.0
	barW := d.width() - rankW - labelW - valueW - shareW - 6
	head := func() {
		y := p.GetY()
		d.atc(16+rankW, y, labelW, 4, columnTitle(pdfRowColumn(s)), 7.5, true, pdfMuted, "L")
		d.atc(16+d.width()-valueW-shareW, y, valueW, 4, headerWithUnit(c, false), 7.5, true, pdfMuted, "R")
		d.atc(16+d.width()-shareW, y, shareW, 4, "Share", 7.5, true, pdfMuted, "R")
		d.draw(pdfLine)
		p.SetLineWidth(.15)
		p.Line(16, y+5, 16+d.width(), y+5)
		p.SetY(y + 6)
	}
	head()
	for n, i := range indices[:shown] {
		name := pdfRowLabel(s.Columns, s.Rows[i], i)
		p.SetFont("Go", "", 8.5)
		lines := p.SplitText(d.text(name), labelW-2)
		h := math.Max(5.6, float64(len(lines))*4+1.6)
		if _, ph := p.GetPageSize(); p.GetY()+h > ph-21 {
			d.page(d.orientation)
			head()
		}
		y := p.GetY()
		d.atc(16, y+1, rankW-1, 4, fmt.Sprint(n+1), 8, false, pdfMuted, "L")
		for j, line := range lines {
			d.at(16+rankW, y+1+float64(j)*4, labelW-2, 4, line, 8.5, false)
		}
		v := s.Rows[i].Values[c.Key]
		bx := 16 + rankW + labelW
		d.fill(pdfTint)
		p.Rect(bx, y+1.5, barW, 3.2, "F")
		if v != nil {
			d.fill(pdfAccent)
			if *v == 0 {
				p.Circle(bx, y+3.1, .6, "F")
			} else if *v > 0 {
				p.Rect(bx, y+1.5, math.Max(.4, *v/high*barW), 3.2, "F")
			}
		}
		d.atc(16+d.width()-valueW-shareW, y+1, valueW, 4, pdfNumber(v, c.Unit), 8.5, true, pdfInk, "R")
		share := ""
		if v != nil && total > 0 && *v >= 0 && !strings.EqualFold(c.Unit, "ratio") {
			share = trimmed(*v/total*100, 1) + "%"
		}
		d.atc(16+d.width()-shareW, y+1, shareW, 4, share, 8.5, false, pdfMuted, "R")
		p.SetY(y + h)
	}
	p.SetY(p.GetY() + 1)
	d.paragraph("Bars are scaled to the largest value shown. Share is of all groups in this breakdown. Unavailable is not zero.", 7.5)
}

// pdfRowColumn is the column that names ranking rows (display name preferred).
func pdfRowColumn(s Section) Column {
	for _, c := range displayColumns(s.Columns, s.Rows) {
		if isDimension(c, s.Rows) {
			return c
		}
	}
	return Column{Key: "group", Label: "Group"}
}

// section routes one result section to the right reader layout.
func (d *reportPDF) section(r Result, s Section) {
	switch {
	case s.ID == "comparison" && len(s.Rows) == 1:
		d.comparison(r, s)
	case factSection(s):
		d.facts(s)
	default:
		d.table(sectionTitle(s), s.ID, s.Columns, s.Rows, s.Notes)
	}
}

func (d *reportPDF) blockStart(need float64) {
	if d.orientation != "P" {
		d.page("P")
		return
	}
	d.room(need)
	if d.p.GetY() > 30 {
		d.p.SetY(d.p.GetY() + 6)
	}
}

// facts shows a single-row aggregate section as a two-column measure list.
func (d *reportPDF) facts(s Section) {
	cols := tableColumns(s.Columns, s.Rows)
	d.blockStart(24 + float64(len(cols))*6.5)
	d.headingSized(sectionTitle(s), 13)
	widths := []float64{d.width() * .62, d.width() * .38}
	d.aligns = []string{"L", "R"}
	d.cells([]string{"Measure", "Value"}, widths, 8.5, true, 0)
	for i, c := range cols {
		d.rowFragments([]string{columnTitle(c), pdfNumber(s.Rows[0].Values[c.Key], c.Unit)}, widths, 8.5, i, func() {
			d.cells([]string{"Measure", "Value"}, widths, 8.5, true, 0)
		}, sectionTitle(s))
	}
	d.aligns = nil
	d.p.SetY(d.p.GetY() + 2)
	for _, n := range s.Notes {
		d.paragraph(n, 8)
	}
}

func (d *reportPDF) comparison(r Result, s Section) {
	lines := comparisonLines(r, s)
	d.blockStart(26 + float64(len(lines))*6.5)
	d.headingSized("Change vs prior period", 13)
	header := []string{"Metric", "This period", "Prior period", "Change", "Large change"}
	widths := []float64{d.width() * .3, d.width() * .19, d.width() * .19, d.width() * .14, d.width() * .18}
	d.aligns = []string{"L", "R", "R", "R", "C"}
	d.cells(header, widths, 8.5, true, 0)
	totals := headlineTotals(r)
	for i, l := range lines {
		c := pdfMetric(r, l.Key)
		change := "Unavailable"
		if l.Change != nil {
			change = signedPercent(*l.Change)
		}
		flag := "Unavailable"
		if l.Outlier != nil {
			flag = "No"
			if *l.Outlier != 0 {
				flag = "Yes"
			}
		}
		cur := totals[l.Key]
		d.rowFragments([]string{l.Label, pdfNumber(cur, c.Unit), pdfNumber(r.PreviousTotals[l.Key], c.Unit), change, flag}, widths, 8.5, i, func() {
			d.cells(header, widths, 8.5, true, 0)
		}, "Change vs prior period")
	}
	d.aligns = nil
	d.p.SetY(d.p.GetY() + 2)
	for _, n := range s.Notes {
		d.paragraph(n, 8)
	}
}

func signedPercent(v float64) string {
	if math.Abs(v) < .0005 {
		if v == 0 {
			return "0%"
		}
		return "<0.1%"
	}
	s := trimmed(math.Abs(v)*100, 1) + "%"
	switch {
	case v > 0:
		return "+" + s
	case v < 0:
		return "-" + s
	}
	return s
}

// Tables use real cell geometry with right-aligned numbers. Opaque identities
// are replaced by their display names; the attached report.json keeps both.
// Extremely wide schemas use labeled record cards rather than column bands.
func (d *reportPDF) table(title, id string, cols []Column, rows []Row, notes []string) {
	cols = displayColumns(cols, rows)
	orientation := "P"
	if len(cols) > 6 {
		orientation = "L"
	}
	if d.orientation != orientation {
		d.page(orientation)
	} else {
		d.room(42)
		if d.p.GetY() > 30 {
			d.p.SetY(d.p.GetY() + 6)
		}
	}
	d.headingSized(title, 13)
	noun := "rows"
	if len(rows) == 1 {
		noun = "row"
	}
	if len(rows) > 12 {
		d.paragraph(fmt.Sprintf("%s %s", grouped(float64(len(rows)), 0), noun), 8)
	}
	for _, n := range notes {
		d.paragraph(n, 8)
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
	size := 8.5
	if len(cols) > 7 {
		size = 7.5
	}
	widths := make([]float64, len(cols))
	d.aligns = make([]string, len(cols))
	weights := 0.0
	p.SetFont("Go", "", size)
	for i, c := range cols {
		w, align := 1.0, "R"
		if isDimension(c, rows) {
			// Size text columns to their longest value (bounded) so names wrap rarely.
			longest := 0.0
			for _, row := range rows {
				longest = math.Max(longest, p.GetStringWidth(d.text(row.Dimensions[c.Key])))
			}
			w, align = math.Min(3.2, math.Max(1.2, (longest+5)/24)), "L"
		}
		widths[i] = w
		d.aligns[i] = align
		weights += w
	}
	for i := range widths {
		widths[i] = d.width() * widths[i] / weights
	}
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = headerWithUnit(c, false)
	}
	drawHeader := func() { d.cells(header, widths, size, true, 0) }
	d.room(22)
	drawHeader()
	if len(rows) == 0 {
		d.aligns = nil
		d.paragraph("No rows returned for this period. This is not a confirmed zero.", 9)
		return
	}
	for i, row := range rows {
		cells := make([]string, len(cols))
		for j, c := range cols {
			if v, ok := row.Dimensions[c.Key]; ok {
				if t, err := time.Parse("2006-01-02", v); err == nil && dateDimension(c.Key) {
					v = t.Format("Mon, Jan 2, 2006")
				}
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
			d.headingSized(title+" · continued", 13)
			drawHeader()
		}
		d.rowFragments(cells, widths, size, i, drawHeader, title)
	}
	d.aligns = nil
}
func (d *reportPDF) cellHeight(cells []string, widths []float64, size float64) float64 {
	h := 0.0
	for i, c := range cells {
		n := len(d.p.SplitText(d.text(c), widths[i]-4))
		h = math.Max(h, float64(n)*(size*.43))
	}
	return h + 3.6
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
	total := 0.0
	for _, w := range widths {
		total += w
	}
	switch {
	case header:
		d.fill(pdfTint)
		p.Rect(x, y, total, h, "F")
	case index%2 == 1:
		d.fill(pdfZebra)
		p.Rect(x, y, total, h, "F")
	}
	d.draw(pdfLine)
	p.SetLineWidth(.15)
	p.Line(x, y+h, x+total, y+h)
	for i, c := range cells {
		// A thin cell outline keeps native table geometry for readers/tools.
		p.SetDrawColor(255, 255, 255)
		p.SetLineWidth(.05)
		p.Rect(x, y, widths[i], h, "D")
		align := "L"
		if i < len(d.aligns) {
			align = d.aligns[i]
		}
		if header {
			d.color(rgb{49, 46, 129})
		} else {
			d.color(pdfInk)
		}
		p.SetFont("Go", style, size)
		p.SetXY(x+2, y+1.8)
		p.MultiCell(widths[i]-4, size*.43, d.text(c), "", align, false)
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
	saved := d.aligns
	d.aligns = nil
	defer func() { d.aligns = saved }()
	for i, row := range rows {
		d.room(25)
		d.headingSized(fmt.Sprintf("Record %d", i+1), 12)
		for _, c := range cols {
			v, ok := row.Dimensions[c.Key]
			if !ok {
				v = pdfNumber(row.Values[c.Key], c.Unit)
			}
			field := headerWithUnit(c, false)
			cells := []string{field, v}
			widths := []float64{d.width() * .3, d.width() * .7}
			title := fmt.Sprintf("Record %d", i+1)
			d.p.SetFont("Go", "", 8)
			_, ph := d.p.GetPageSize()
			if d.p.GetY()+d.cellHeight(cells, widths, 8) > ph-21 {
				d.page(d.orientation)
				d.headingSized(title+" · continued", 12)
			}
			d.rowFragments(cells, widths, 8, i, func() {
				d.cells([]string{field, ""}, widths, 8, true, i)
			}, title)
		}
	}
}

// about is a short plain-language reading guide, plus totals with prior values.
func (d *reportPDF) about(r Result) {
	d.page("P")
	d.headingSized("About this report", 15)
	for _, s := range aboutLines(r) {
		d.room(6)
		y := d.p.GetY()
		d.atc(16, y, 4, 4.5, "•", 9, false, pdfMuted, "L")
		d.p.SetFont("Go", "", 9)
		d.color(rgb{55, 60, 80})
		for _, line := range d.p.SplitText(d.text(s), d.width()-6) {
			d.p.SetXY(21, y)
			d.p.CellFormat(d.width()-6, 4.5, line, "", 0, "L", false, 0, "")
			y += 4.5
		}
		d.p.SetY(y + 1.2)
	}
	d.paragraph("A machine-readable copy of this report (report.json) is attached to this PDF.", 8.5)
	all := headlineTotals(r)
	for k := range r.PreviousTotals {
		if _, ok := all[k]; !ok {
			all[k] = nil
		}
	}
	if len(all) == 0 {
		return
	}
	d.p.SetY(d.p.GetY() + 4)
	d.headingSized("Totals", 13)
	header := []string{"Metric", "This period", "Prior period"}
	widths := []float64{d.width() * .46, d.width() * .27, d.width() * .27}
	d.aligns = []string{"L", "R", "R"}
	d.cells(header, widths, 8.5, true, 0)
	for i, k := range headlineKeys(all, len(all)) {
		c := pdfMetric(r, k)
		prior := pdfNumber(r.PreviousTotals[k], c.Unit)
		if len(r.PreviousTotals) == 0 {
			prior = "—"
		}
		d.rowFragments([]string{headerWithUnit(c, false), pdfNumber(all[k], c.Unit), prior}, widths, 8.5, i, func() {
			d.cells(header, widths, 8.5, true, 0)
		}, "Totals")
	}
	d.aligns = nil
	zone := r.Definition.Timezone
	if zone == "" {
		zone = "UTC"
	}
	loc := reportLocation(r)
	d.p.SetY(d.p.GetY() + 4)
	d.headingSized("Report details", 13)
	details := [][2]string{{"Period", periodText(r)}, {"Time zone", zone}, {"Scope", scopeText(r.Definition)}}
	if r.Definition.GroupMode != "" {
		details = append(details, [2]string{"Team grouping", groupModeText(r.Definition.GroupMode)})
	}
	f := filterText(r)
	if len(f) == 0 {
		f = []string{"None"}
	}
	details = append(details, [2]string{"Filters", strings.Join(f, "; ")})
	if !r.GeneratedAt.IsZero() {
		details = append(details, [2]string{"Generated", r.GeneratedAt.In(loc).Format("Jan 2, 2006 15:04 MST")})
	}
	if !r.DataCutoff.IsZero() {
		details = append(details, [2]string{"Data through", r.DataCutoff.In(loc).Format("Jan 2, 2006 15:04 MST")})
	}
	details = append(details, [2]string{"Source records", grouped(float64(r.SourceRows), 0)})
	widths = []float64{d.width() * .3, d.width() * .7}
	for i, kv := range details {
		d.rowFragments([]string{kv[0], kv[1]}, widths, 8.5, i, func() {}, "Report details")
	}
}
