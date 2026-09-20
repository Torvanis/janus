package store

import "testing"

func TestReportingDerivedMetricsKeepDimensionlessUnits(t *testing.T) {
	v := 1.0
	section := reportStatSection("comparison", "Comparison", map[string]*float64{"cost_usd_relative_change": &v, "cost_usd_outlier": &v, "cost_usd": &v})
	want := map[string]string{"cost_usd_relative_change": "ratio", "cost_usd_outlier": "count", "cost_usd": "USD"}
	for _, column := range section.Columns {
		if column.Unit != want[column.Key] {
			t.Errorf("%s unit=%s want %s", column.Key, column.Unit, want[column.Key])
		}
	}
}
