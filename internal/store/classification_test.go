package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestClassificationTaxonomySeedAndEdits(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMigratedSQLite(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	cats, err := s.ListClassificationCategories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cats) != len(BuiltinClassificationCategories) {
		t.Fatalf("seeded %d categories, want %d", len(cats), len(BuiltinClassificationCategories))
	}
	if cats[len(cats)-1].Slug != ClassificationOtherSlug {
		t.Fatalf("Other must sort last, got %s", cats[len(cats)-1].Slug)
	}

	rust, err := s.CreateClassificationCategory(ctx, ClassificationCategoryInput{Name: "Rust", Description: "Rust code.", ParentSlug: "coding"})
	if err != nil || rust.Slug != "rust" || rust.Builtin {
		t.Fatalf("create rust: %+v %v", rust, err)
	}
	if c, err := s.CreateClassificationCategory(ctx, ClassificationCategoryInput{Name: "C++", Description: "C++ code.", ParentSlug: "coding"}); err != nil || c.Slug != "cplusplus" {
		t.Fatalf("create c++: %+v %v", c, err)
	}
	var v *ValidationError
	for name, in := range map[string]ClassificationCategoryInput{
		"duplicate":      {Name: "Rust", Description: "again", ParentSlug: "coding"},
		"no description": {Name: "Kotlin", ParentSlug: "coding"},
		"nested":         {Name: "Async Rust", Description: "x", ParentSlug: "rust"},
		"under other":    {Name: "Misc", Description: "x", ParentSlug: ClassificationOtherSlug},
		"missing parent": {Name: "Orphan", Description: "x", ParentSlug: "nope"},
		"reserved slug":  {Name: "None", Description: "x"},
	} {
		if _, err := s.CreateClassificationCategory(ctx, in); !errors.As(err, &v) {
			t.Errorf("%s: want a validation error, got %v", name, err)
		}
	}

	cats, _ = s.ListClassificationCategories(ctx)
	for i, c := range cats {
		if c.Slug == "coding" && (cats[i+1].Slug != "cplusplus" || cats[i+2].Slug != "rust") {
			t.Fatalf("children must follow their parent alphabetically: %s, %s", cats[i+1].Slug, cats[i+2].Slug)
		}
	}

	// Built-ins: editable, not deletable; Other cannot be disabled.
	var coding, other *ClassificationCategory
	for i := range cats {
		switch cats[i].Slug {
		case "coding":
			coding = &cats[i]
		case ClassificationOtherSlug:
			other = &cats[i]
		}
	}
	off := false
	if _, err := s.UpdateClassificationCategory(ctx, other.ID, ClassificationCategoryInput{Enabled: &off}); !errors.As(err, &v) {
		t.Fatalf("disabling Other must be refused, got %v", err)
	}
	if err := s.DeleteClassificationCategory(ctx, coding.ID); !errors.As(err, &v) {
		t.Fatalf("deleting a built-in must be refused, got %v", err)
	}
	upd, err := s.UpdateClassificationCategory(ctx, coding.ID, ClassificationCategoryInput{Description: "Software engineering.", Enabled: &off})
	if err != nil || upd.Enabled || upd.Description != "Software engineering." || upd.Slug != "coding" {
		t.Fatalf("update built-in: %+v %v", upd, err)
	}

	// Deleting a custom parent takes its children with it.
	sec, err := s.CreateClassificationCategory(ctx, ClassificationCategoryInput{Name: "OT security", Description: "Industrial control systems."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateClassificationCategory(ctx, ClassificationCategoryInput{Name: "PLC", Description: "PLC code.", ParentSlug: sec.Slug}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteClassificationCategory(ctx, sec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.categoryBySlug(ctx, "plc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("child of a deleted category must go too, got %v", err)
	}
}

func TestClassificationTopLevelLimit(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMigratedSQLite(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	room := MaxTopLevelCategories - len(BuiltinClassificationCategories)
	for i := 0; i < room; i++ {
		if _, err := s.CreateClassificationCategory(ctx, ClassificationCategoryInput{Name: "Extra " + string(rune('a'+i)), Description: "x"}); err != nil {
			t.Fatalf("category %d of %d: %v", i+1, room, err)
		}
	}
	var v *ValidationError
	if _, err := s.CreateClassificationCategory(ctx, ClassificationCategoryInput{Name: "One too many", Description: "x"}); !errors.As(err, &v) || v.Field != "enabled" {
		t.Fatalf("want the top-level limit, got %v", err)
	}
	off := false
	if c, err := s.CreateClassificationCategory(ctx, ClassificationCategoryInput{Name: "Parked", Description: "x", Enabled: &off}); err != nil || c.Enabled {
		t.Fatalf("a disabled category does not count against the limit: %+v %v", c, err)
	}
}

func TestClassificationSettingsAndReport(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMigratedSQLite(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	got, err := s.ClassificationSettings(ctx)
	if err != nil || got.Enabled || got.Mode != ClassificationModeAsync {
		t.Fatalf("defaults: %+v %v", got, err)
	}
	got.Enabled = true
	var v *ValidationError
	if err := s.SetClassificationSettings(ctx, got); !errors.As(err, &v) || v.Field != "model" {
		t.Fatalf("enabling without a model must be refused, got %v", err)
	}
	got.UpstreamID, got.Model, got.Mode, got.SampleRate = "u1", "nimble", ClassificationModeInline, 25
	if err := s.SetClassificationSettings(ctx, got); err != nil {
		t.Fatal(err)
	}
	if back, _ := s.ClassificationSettings(ctx); back.Model != "nimble" || back.SampleRate != 25 || back.Mode != ClassificationModeInline || back.UpdatedAt.IsZero() {
		t.Fatalf("round trip: %+v", back)
	}

	now := time.Now().UTC()
	user, _, err := s.UpsertUserFromIdentity(ctx, "sub", "a@example.com", "Ada", true, false)
	if err != nil {
		t.Fatal(err)
	}
	add := func(cat, sub string, conf float64, status string, ms int, ago time.Duration) {
		t.Helper()
		if err := s.InsertClassificationResult(ctx, &ClassificationResult{CreatedAt: now.Add(-ago), RequestID: NewID(), UserID: user.ID, ModelName: "gpt",
			Category: cat, Subcategory: sub, Confidence: conf, Status: status, Mode: "async", Classifier: "Nimble / nimble", LatencyMs: ms,
			Probabilities: map[string]float64{cat: conf}}); err != nil {
			t.Fatal(err)
		}
	}
	add("coding", "rust", 0.9, "ok", 100, time.Hour)
	add("coding", "", 0.4, "ok", 200, 2*time.Hour)
	add("legal", "", 0.95, "ok", 300, 3*time.Hour)
	add("", "", 0, "timeout", 5000, time.Hour)
	add("", "", 0, "dropped", 0, time.Hour)
	add("coding", "python", 0.9, "ok", 100, 40*24*time.Hour) // outside the range

	rep, err := s.ClassificationReport(ctx, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Classified != 3 || len(rep.Categories) != 2 || rep.Categories[0].Category != "coding" || rep.Categories[0].Count != 2 {
		t.Fatalf("report categories: %+v", rep.Categories)
	}
	c := rep.Categories[0]
	if c.LowConfidence != 1 || len(c.Subcategories) != 1 || c.Subcategories[0].Key != "rust" || c.TopPeople[0].Label != "Ada" || c.TopModels[0].Key != "gpt" {
		t.Fatalf("coding row: %+v", c)
	}
	if rep.Bucket != "hour" || len(rep.Series) < 24 {
		t.Fatalf("a one-day range buckets by hour, got %s x %d", rep.Bucket, len(rep.Series))
	}
	h := rep.Health
	if h.Attempts != 5 || h.OK != 3 || h.Timeouts != 1 || h.Dropped != 1 || h.MaxMs != 5000 || h.P50Ms != 200 {
		t.Fatalf("health: %+v", h)
	}
	if len(h.ByClassifier) != 1 || h.ByClassifier[0].Count != 4 {
		t.Fatalf("by classifier: %+v", h.ByClassifier)
	}

	list, err := s.ListClassificationResults(ctx, ClassificationResultFilter{Category: "coding", From: now.Add(-24 * time.Hour)})
	if err != nil || len(list) != 2 || list[0].UserName != "Ada" || list[0].Probabilities["coding"] != 0.9 {
		t.Fatalf("results: %+v %v", list, err)
	}
	n, err := s.PruneClassificationResults(ctx, now.Add(-30*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
}

// Deleting an insights category that an Auto-routing category follows is
// refused (the routing category would otherwise lose its description).
func TestClassificationDeleteRefusedWhileRoutingFollowsIt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sec, err := s.CreateClassificationCategory(ctx, ClassificationCategoryInput{Name: "Clinical", Description: "Clinical trials."})
	if err != nil {
		t.Fatal(err)
	}
	kid, err := s.CreateClassificationCategory(ctx, ClassificationCategoryInput{Name: "Oncology", Description: "Cancer trials.", ParentSlug: sec.Slug})
	if err != nil {
		t.Fatal(err)
	}
	name, link := "Trials", kid.Slug
	rc, err := s.CreateRoutingCategory(ctx, RoutingCategoryInput{Name: &name, LinkedSlug: &link})
	if err != nil {
		t.Fatal(err)
	}
	var v *ValidationError
	if err := s.DeleteClassificationCategory(ctx, sec.ID); !errors.As(err, &v) || !strings.Contains(v.Message, "Trials") {
		t.Fatalf("deleting the parent of a followed subcategory: %v", err)
	}
	if err := s.DeleteClassificationCategory(ctx, kid.ID); !errors.As(err, &v) {
		t.Fatalf("deleting a followed category: %v", err)
	}
	empty := ""
	inc := "Clinical trial work."
	if _, err := s.UpdateRoutingCategory(ctx, rc.ID, RoutingCategoryInput{LinkedSlug: &empty, Includes: &inc}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteClassificationCategory(ctx, sec.ID); err != nil {
		t.Fatalf("delete after unlinking: %v", err)
	}
}

func TestClassificationErrorIsStoredAsValidUTF8(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r := &ClassificationResult{RequestID: "r", Status: ClassificationStatusError, Error: strings.Repeat("é", 400)}
	if err := s.InsertClassificationResult(ctx, r); err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(r.Error) || len(r.Error) > 500 {
		t.Fatalf("stored error len=%d valid=%v", len(r.Error), utf8.ValidString(r.Error))
	}
}
