package reporting

// Layout helpers shared by the PDF renderer and the reader-facing exports.
// They hold no PDF library code, so they build in every edition.
import (
	"fmt"
	"sort"
	"strings"
	"time"
)

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
