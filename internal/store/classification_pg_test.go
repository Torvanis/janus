package store

// the classification store on PostgreSQL. Runs
// only with JANUS_ROLLUP_TEST_DATABASE_URL set to a loopback database.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestClassificationStoreOnPostgres(t *testing.T) {
	if os.Getenv("JANUS_ROLLUP_TEST_DATABASE_URL") == "" {
		t.Skip("JANUS_ROLLUP_TEST_DATABASE_URL not set")
	}
	s := newRollupStore(t)
	ctx := context.Background()

	got, err := s.ClassificationSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got.Enabled, got.UpstreamID, got.Model = true, "u1", "nimble"
	if err := s.SetClassificationSettings(ctx, got); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateClassificationCategory(ctx, ClassificationCategoryInput{Name: "Rust", Description: "Rust code.", ParentSlug: "coding"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateClassificationCategory(ctx, c.ID, ClassificationCategoryInput{Description: "Rust code and crates."}); err != nil {
		t.Fatal(err)
	}
	var v *ValidationError
	if _, err := s.CreateClassificationCategory(ctx, ClassificationCategoryInput{Name: "Rust", Description: "dup", ParentSlug: "coding"}); !errors.As(err, &v) {
		t.Fatalf("duplicate: %v", err)
	}

	now := time.Now().UTC()
	user, _, err := s.UpsertUserFromIdentity(ctx, "sub", "a@example.com", "Ada", true, false)
	if err != nil {
		t.Fatal(err)
	}
	add := func(cat, sub, status, errText string, ago time.Duration, svc, teams string) error {
		r := &ClassificationResult{CreatedAt: now.Add(-ago), RequestID: NewID(), UserID: user.ID, ModelName: "gpt",
			Category: cat, Subcategory: sub, Confidence: 0.8, Status: status, Error: errText, Mode: "async", Classifier: "N / n", LatencyMs: 120,
			Probabilities: map[string]float64{cat: 0.8}, TeamIDs: teams}
		if svc != "" {
			r.UserID, r.ServiceTokenID = "", svc
		}
		return s.InsertClassificationResult(ctx, r)
	}
	for _, e := range []error{
		add("coding", "rust", "ok", "", time.Hour, "", "t1,t2"),
		add("legal", "", "ok", "", 2*time.Hour, "svc-1", ""),
		add("", "", "error", "boom", time.Hour, "", ""),
	} {
		if e != nil {
			t.Fatal(e)
		}
	}
	// A cut error with a half rune: must not lose the row.
	if err := add("", "", "error", strings.Repeat("a", 299)+"\xc3", time.Hour, "", ""); err != nil {
		t.Errorf("Postgres refused a classification row whose error was cut mid-rune: %v", err)
	}
	for _, span := range []time.Duration{24 * time.Hour, 7 * 24 * time.Hour} {
		rep, err := s.ClassificationReport(ctx, now.Add(-span), now)
		if err != nil {
			t.Fatalf("report %v on postgres: %v", span, err)
		}
		if rep.Classified != 2 || len(rep.ByTeam) != 2 {
			t.Fatalf("report %v: classified=%d teams=%d", span, rep.Classified, len(rep.ByTeam))
		}
		people := 0
		for _, row := range rep.Categories {
			people += len(row.TopPeople)
		}
		if people != 2 {
			t.Fatalf("top people: %d", people)
		}
	}
	if _, err := s.ListClassificationResults(ctx, ClassificationResultFilter{Category: "coding", From: now.Add(-24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClassificationTagsForRequests(ctx, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"*", "-", "coding", "coding/rust"} {
		if _, _, err := s.ListRequests(ctx, RequestFilter{Classified: f, IncludeInternal: true, Limit: 10}); err != nil {
			t.Fatalf("request log classified=%s on postgres: %v", f, err)
		}
	}
	if _, err := s.PruneClassificationResults(ctx, now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteClassificationCategory(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
}
