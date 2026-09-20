package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReportClassificationValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, stmt := range reportingClassificationMigration.stmt {
		if err := s.exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	seedCatalogModels(t, s, "validation")
	models, err := s.ListModels(ctx, ModelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	id := models[0].ID
	for _, v := range []ReportModelClassification{
		{ModelID: id, Family: strings.Repeat("a", 101)}, {ModelID: id, Provider: strings.Repeat("é", 51)},
		{ModelID: id, Family: "bad\nvalue"}, {ModelID: id, Provider: "bad\x00value"},
		{ModelID: id, Family: "hidden\u200bvalue"}, {ModelID: id, Provider: string([]byte{255})},
		{ModelID: id, Hosting: "cloud"},
	} {
		if err := s.SaveReportModelClassification(ctx, &v); err == nil {
			t.Errorf("accepted invalid taxonomy: %+v", v)
		}
	}
	for _, hosting := range []string{"", "self_hosted", "external", "hybrid"} {
		v := ReportModelClassification{ModelID: id, Family: strings.Repeat("é", 50), Hosting: hosting}
		if err := s.SaveReportModelClassification(ctx, &v); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveReportModelClassification(ctx, &ReportModelClassification{ModelID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing model: %v", err)
	}
}

func TestReportClassificationHistoryAndPortfolio(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, stmt := range reportingClassificationMigration.stmt {
		if err := s.exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	seedCatalogModels(t, s, "old-name", "unassigned")
	models, err := s.ListModels(ctx, ModelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	v := ReportModelClassification{ModelID: models[0].ID, Family: "before", Provider: "vendor", Hosting: "external"}
	if err := s.SaveReportModelClassification(ctx, &v); err != nil {
		t.Fatal(err)
	}
	event := UsageEvent{ID: NewID(), CreatedAt: time.Now(), ModelFamily: v.Family, ModelProvider: v.Provider, ModelHosting: v.Hosting}
	if err := s.InsertUsageEvent(ctx, &event); err != nil {
		t.Fatal(err)
	}
	v.Family = "after"
	if err := s.SaveReportModelClassification(ctx, &v); err != nil {
		t.Fatal(err)
	}
	var family string
	if err := s.queryRow(ctx, `SELECT model_family FROM reporting_usage_snapshot WHERE usage_id=?`, event.ID).Scan(&family); err != nil {
		t.Fatal(err)
	}
	if family != "before" {
		t.Fatalf("history rewritten: %s", family)
	}
	if err := s.exec(ctx, `UPDATE model SET display_name='Current Name' WHERE id=?`, v.ModelID); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListReportModelClassifications(ctx)
	if err != nil || len(all) != 2 || all[0].ModelName != "Current Name" {
		t.Fatalf("portfolio: %+v %v", all, err)
	}
	if err := s.exec(ctx, `DELETE FROM model WHERE id=?`, v.ModelID); err != nil {
		t.Fatal(err)
	}
	all, err = s.ListReportModelClassifications(ctx)
	if err != nil || len(all) != 1 || all[0].ModelName != "unassigned" {
		t.Fatalf("deleted model listed: %+v %v", all, err)
	}
}

func TestReportClassificationRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Exercise this migration independently while the parent registers it.
	for _, stmt := range reportingClassificationMigration.stmt {
		if err := s.exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	seedCatalogModels(t, s, "opaque-model")
	models, err := s.ListModels(ctx, ModelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	id := models[0].ID
	if _, err = s.GetReportModelClassification(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unset: %v", err)
	}
	all, err := s.ListReportModelClassifications(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("list: %+v %v", all, err)
	}
	if all[0].ModelName != "opaque-model" || all[0].Family != "" || all[0].Provider != "" || all[0].Hosting != "" {
		t.Fatalf("inferred taxonomy: %+v", all[0])
	}
	v := ReportModelClassification{ModelID: id, Family: "Family A", Provider: "Provider A", Hosting: "self_hosted"}
	if err = s.SaveReportModelClassification(ctx, &v); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetReportModelClassification(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Family != v.Family || got.Provider != v.Provider || got.Hosting != v.Hosting || got.UpdatedAt.IsZero() || got.ModelName != "opaque-model" {
		t.Fatalf("round trip: %+v", got)
	}
	v.Family = "Family B"
	if err = s.SaveReportModelClassification(ctx, &v); err != nil {
		t.Fatal(err)
	}
	latest, err := s.GetReportModelClassification(ctx, id)
	if err != nil || latest.Family != "Family B" || got.Family != "Family A" {
		t.Fatalf("future assignment: %+v %v", latest, err)
	}
	v.Family = ""
	v.Provider = ""
	v.Hosting = ""
	if err = s.SaveReportModelClassification(ctx, &v); err != nil {
		t.Fatal(err)
	}
	all, err = s.ListReportModelClassifications(ctx)
	if err != nil || len(all) != 1 || all[0].Family != "" || all[0].Hosting != "" {
		t.Fatalf("clear: %+v %v", all, err)
	}
}
