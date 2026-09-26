package reporting

// Reader-facing presentation shared by the CSV, XLSX and PDF renderers.
// JSON remains the exact machine snapshot: stable identifiers, field keys and
// raw values live there. Human formats show current display names instead of
// opaque identities whenever the producer supplied a paired name column.
import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// columnTitle turns producer labels such as "team name (current)" or
// "disabled cost requests" into reader headings ("Team", "Disabled cost requests").
func columnTitle(c Column) string {
	s := strings.TrimSpace(c.Label)
	if s == "" {
		s = label(c.Key)
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, " name (current)"))
	if s == "" {
		s = label(strings.TrimSuffix(c.Key, "_label"))
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// headerWithUnit names a column once, adding its unit only when the label does
// not already say it. raw=true describes unformatted values (CSV), where ratios
// are fractions rather than displayed percentages.
func headerWithUnit(c Column, raw bool) string {
	t := columnTitle(c)
	u := strings.TrimSpace(c.Unit)
	lu, lt := strings.ToLower(u), strings.ToLower(t)
	switch {
	case u == "" || lu == "count" || lu == "number" || lu == "requests":
		return t
	case lu == "ratio":
		if raw {
			return t + " (ratio)"
		}
		return t
	case lu == "percent" || lu == "%":
		if raw {
			return t + " (%)"
		}
		return t
	case lu == "tokens":
		if strings.Contains(lt, "token") {
			return t
		}
		return t + " (tokens)"
	case strings.Contains(lt, lu):
		return t
	}
	return t + " (" + u + ")"
}

// hiddenIdentity reports whether key is an opaque identity with a paired
// display-name column in these rows.
func hiddenIdentity(key string, rows []Row) bool {
	if strings.HasSuffix(key, "_label") || strings.HasSuffix(key, "_name") {
		return false
	}
	for _, row := range rows {
		if _, ok := row.Dimensions[key+"_label"]; ok {
			return true
		}
		if _, ok := row.Dimensions[key+"_name"]; ok {
			return true
		}
		if strings.HasSuffix(key, "_id") {
			if _, ok := row.Dimensions[strings.TrimSuffix(key, "_id")+"_name"]; ok {
				return true
			}
		}
	}
	return false
}

// displayColumns is tableColumns without identities that have a display name.
func displayColumns(cols []Column, rows []Row) []Column {
	out := []Column{}
	for _, c := range tableColumns(cols, rows) {
		if !hiddenIdentity(c.Key, rows) {
			out = append(out, c)
		}
	}
	return out
}

func isDimension(c Column, rows []Row) bool {
	for _, row := range rows {
		if _, ok := row.Dimensions[c.Key]; ok {
			return true
		}
	}
	return false
}

// factSection is a single aggregate row with no grouping (e.g. cost coverage).
func factSection(s Section) bool {
	return len(s.Rows) == 1 && len(s.Rows[0].Dimensions) == 0 && len(s.Rows[0].Values) > 0
}

type comparisonLine struct {
	Key     string
	Label   string
	Change  *float64
	Outlier *float64
}

// comparisonLines reshapes the rule-based comparison section's paired
// <metric>_relative_change / <metric>_outlier values into one line per metric.
func comparisonLines(r Result, s Section) []comparisonLine {
	if s.ID != "comparison" || len(s.Rows) != 1 {
		return nil
	}
	v := s.Rows[0].Values
	seen := map[string]bool{}
	out := []comparisonLine{}
	for _, c := range tableColumns(s.Columns, s.Rows) {
		base := strings.TrimSuffix(strings.TrimSuffix(c.Key, "_relative_change"), "_outlier")
		if base == c.Key || seen[base] {
			continue
		}
		seen[base] = true
		out = append(out, comparisonLine{base, columnTitle(pdfMetric(r, base)), v[base+"_relative_change"], v[base+"_outlier"]})
	}
	return out
}

// readerNotices merges warnings that repeat for the prior period, so a caveat
// is stated once with the periods it applies to.
func readerNotices(r Result) []string {
	type notice struct {
		text           string
		current, prior bool
	}
	order := []*notice{}
	byText := map[string]*notice{}
	add := func(s string) {
		base := strings.TrimPrefix(s, "Previous period: ")
		n := byText[base]
		if n == nil {
			n = &notice{text: base}
			byText[base] = n
			order = append(order, n)
		}
		if base == s {
			n.current = true
		} else {
			n.prior = true
		}
	}
	for _, s := range r.Warnings {
		add(s)
	}
	for _, s := range r.ComparisonWarnings {
		add(s)
	}
	out := []string{}
	if r.ComparisonReliable != nil && !*r.ComparisonReliable {
		out = append(out, "Comparison unavailable: coverage is incomplete. Prior-period totals are shown for reference, but changes are not assessed.")
	}
	for _, n := range order {
		switch {
		case n.current && n.prior:
			out = append(out, n.text+" (Applies to this and the prior period.)")
		case n.prior:
			out = append(out, "Prior period: "+n.text)
		default:
			out = append(out, n.text)
		}
	}
	return out
}

func reportLocation(r Result) *time.Location {
	if loc, err := time.LoadLocation(r.Definition.Timezone); err == nil && r.Definition.Timezone != "" {
		return loc
	}
	return time.UTC
}

func scopeText(d Definition) string {
	switch d.Scope {
	case "organization":
		return "Organization"
	case "team":
		return "Team"
	case "self":
		return "Personal"
	case "":
		return "Report"
	}
	return label(d.Scope)
}

func groupModeText(mode string) string {
	switch mode {
	case "historical":
		return "Team membership at the time of each request"
	case "current":
		return "Current team membership"
	}
	return label(mode)
}

// periodText renders [start,end) for readers. When the end is a local
// midnight, the last included day is shown instead of the exclusive bound.
func periodText(r Result) string {
	loc := reportLocation(r)
	s, e := r.Start.In(loc), r.End.In(loc)
	if s.IsZero() || e.IsZero() {
		return "Unavailable"
	}
	midnight := func(t time.Time) bool {
		return t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0
	}
	if midnight(s) && midnight(e) {
		last := e.AddDate(0, 0, -1)
		if s.Year() == last.Year() {
			return s.Format("Jan 2") + " – " + last.Format("Jan 2, 2006")
		}
		return s.Format("Jan 2, 2006") + " – " + last.Format("Jan 2, 2006")
	}
	return s.Format("Jan 2, 2006 15:04") + " – " + e.Format("Jan 2, 2006 15:04")
}

// filterText resolves filter values to the display names present in the
// snapshot where possible; otherwise the recorded value is shown.
func filterText(r Result) []string {
	names := map[string]map[string]string{}
	collect := func(rows []Row) {
		for _, row := range rows {
			for k, v := range row.Dimensions {
				if l, ok := row.Dimensions[k+"_label"]; ok && l != "" {
					if names[k] == nil {
						names[k] = map[string]string{}
					}
					names[k][v] = l
				}
			}
		}
	}
	collect(r.Rows)
	for _, s := range r.Sections {
		collect(s.Rows)
	}
	keys := make([]string, 0, len(r.Definition.Filters))
	for k := range r.Definition.Filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []string{}
	for _, k := range keys {
		vals := []string{}
		for _, v := range r.Definition.Filters[k] {
			if n, ok := names[k][v]; ok {
				v = n
			}
			vals = append(vals, v)
		}
		out = append(out, label(k)+": "+strings.Join(vals, ", "))
	}
	return out
}

// DownloadName is the attachment filename for an export of r.
func DownloadName(r Result, format string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(r.Definition.Name) {
		if c < 128 && (unicode.IsLetter(c) || unicode.IsDigit(c)) {
			b.WriteRune(c)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	if slug == "" {
		slug = "report"
	}
	name := "janus-" + slug
	if !r.GeneratedAt.IsZero() {
		name += "-" + r.GeneratedAt.In(reportLocation(r)).Format("2006-01-02")
	}
	return name + Extension(format)
}

// grouped formats n with thousands separators and a fixed number of decimals.
func grouped(n float64, decimals int) string {
	s := strconv.FormatFloat(math.Abs(n), 'f', decimals, 64)
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	for i := len(intPart) - 3; i > 0; i -= 3 {
		intPart = intPart[:i] + "," + intPart[i:]
	}
	sign := ""
	if n < 0 && strings.Trim(intPart+frac, "0.,") != "" {
		sign = "-"
	}
	return sign + intPart + frac
}

// trimmed formats with up to max decimals, dropping trailing zeros.
func trimmed(n float64, max int) string {
	s := grouped(n, max)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func formatValue(v *float64, unit string) string {
	if v == nil {
		return "Unavailable"
	}
	n := *v
	switch strings.ToLower(unit) {
	case "usd":
		if n != 0 && math.Abs(n) < .01 {
			return signed(n, "$"+grouped(math.Abs(n), 4))
		}
		return signed(n, "$"+grouped(math.Abs(n), 2))
	case "eur":
		return signed(n, "€"+grouped(math.Abs(n), 2))
	case "ratio":
		return trimmed(n*100, 1) + "%"
	case "percent", "%":
		return trimmed(n, 1) + "%"
	case "ms":
		return grouped(n, 0) + " ms"
	case "count", "tokens", "requests":
		if n == math.Trunc(n) {
			return grouped(n, 0)
		}
		return trimmed(n, 2)
	}
	return trimmed(n, 2)
}

func signed(n float64, abs string) string {
	if n < 0 {
		return "-" + abs
	}
	return abs
}

// compactValue shortens very large headline values (tables keep full values).
func compactValue(v *float64, unit string) string {
	if v == nil {
		return "Unavailable"
	}
	n, lu := *v, strings.ToLower(unit)
	if math.Abs(n) >= 1e6 && (lu == "count" || lu == "tokens" || lu == "requests" || lu == "usd" || lu == "") {
		suffix, div := "M", 1e6
		if math.Abs(n) >= 1e9 {
			suffix, div = "B", 1e9
		}
		s := trimmed(n/div, 1) + suffix
		if lu == "usd" {
			return signed(n, "$"+strings.TrimPrefix(s, "-"))
		}
		return s
	}
	return formatValue(v, unit)
}

// higherIsWorse decides the color of a change; it never alters the value.
func higherIsWorse(key string) bool {
	for _, w := range []string{"cost", "error", "denial", "block", "latency", "ttfb", "unpriced", "estimated", "unknown", "_ms"} {
		if strings.Contains(key, w) {
			return true
		}
	}
	return false
}

// changeSummary describes current versus prior for headline cards.
func changeSummary(current, prior *float64, unit string) (text string, direction int) {
	if current == nil || prior == nil {
		return "No prior-period value", 0
	}
	diff := *current - *prior
	switch {
	case *prior == 0 && diff == 0:
		return "No change vs prior period", 0
	case *prior == 0:
		return "New this period (prior 0)", 1
	}
	pct := diff / math.Abs(*prior) * 100
	dir := 0
	if diff > 0 {
		dir = 1
	} else if diff < 0 {
		dir = -1
	}
	sign := "+"
	if pct < 0 {
		sign = "-"
	}
	if dir == 0 || math.Abs(pct) < .05 {
		return "No change vs prior (" + compactValue(prior, unit) + ")", 0
	}
	return fmt.Sprintf("%s%s%% vs prior (%s)", sign, trimmed(math.Abs(pct), 1), compactValue(prior, unit)), dir
}

// headlineKeys picks up to max familiar headline metrics without inventing any.
func headlineKeys(totals map[string]*float64, max int) []string {
	keys := []string{}
	for _, k := range []string{"requests", "cost_usd", "total_tokens", "tokens_in", "tokens_out", "success_rate", "success_ratio", "active_users"} {
		if _, ok := totals[k]; ok {
			keys = append(keys, k)
		}
	}
	for _, k := range pdfKeys(totals) {
		if !containsKey(keys, k) {
			keys = append(keys, k)
		}
	}
	if len(keys) > max {
		keys = keys[:max]
	}
	return keys
}

// headlineTotals merges engine totals with a single-row executive summary.
func headlineTotals(r Result) map[string]*float64 {
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
	return totals
}

// orderedSections keeps producer order but moves bookkeeping sections
// (rule-based comparison, cost coverage) after the analytical ones.
func orderedSections(r Result) []Section {
	out := append([]Section(nil), r.Sections...)
	rank := func(id string) int {
		switch id {
		case "comparison":
			return 1
		case "cost_coverage":
			return 2
		}
		return 0
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i].ID) < rank(out[j].ID) })
	return out
}

func sectionTitle(s Section) string {
	if s.Title != "" {
		return s.Title
	}
	return label(s.ID)
}
