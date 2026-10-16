package httpapi

// Regression tests for the release review of the classifier benchmark and
// the classifier resolver (area B). Each one failed on the reviewed
// release candidate.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/classify"
	"github.com/torvanis/janus/internal/store"
)

// B8: a run left "running" by a replica that stopped stayed running
// forever: only starting a new run failed it, and the page disables the
// Run button while any run is running. Listing (and reading) runs now fails
// stale ones.
func TestStaleBenchmarkIsFailedWhenListed(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	ctx := context.Background()
	stale := &store.ClassifierBenchmark{CreatedAt: time.Now().UTC().Add(-20 * time.Minute), CreatedBy: "someone"}
	if err := h.store.CreateClassifierBenchmark(ctx, stale); err != nil {
		t.Fatal(err)
	}
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/classification/benchmarks", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Benchmarks []store.ClassifierBenchmark `json:"benchmarks"`
	}
	decodeInto(t, rec, &out)
	if len(out.Benchmarks) != 1 || out.Benchmarks[0].Status != store.BenchmarkFailed {
		t.Fatalf("stale run = %+v, want failed", out.Benchmarks)
	}
}

// B8: the one-run-at-a-time rule was per replica (a package-level slot), so
// two replicas could benchmark at once and measure each other. A run that
// another replica started (a fresh running row) now refuses the start.
func TestBenchmarkRefusedWhileAnotherReplicaRuns(t *testing.T) {
	h := newHarness(t)
	f := newFakeDecider(t)
	m := h.decisionGuard(t, f, "nimble", "llama_cpp")
	sess := adminSession(t, h)
	other := &store.ClassifierBenchmark{CreatedBy: "replica-2"}
	if err := h.store.CreateClassifierBenchmark(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/classification/benchmarks", map[string]any{
		"injection_model_id": m.ID, "levels": []int{1}, "requests_per_level": 2, "budget_ms": 5000,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("start while another replica runs: %d %s, want 409", rec.Code, rec.Body.String())
	}
	list, _ := h.store.ListClassifierBenchmarks(context.Background(), 10)
	if len(list) != 1 {
		t.Fatalf("%d runs stored, want only the other replica's", len(list))
	}
}

// B10: a running benchmark (up to ten minutes, context.Background) held
// Drain until its deadline and stayed "running". Drain now stops it and the
// run is recorded as failed because the gateway stopped.
func TestDrainStopsARunningBenchmark(t *testing.T) {
	h := newHarness(t)
	f := newFakeDecider(t)
	f.delay = 150 * time.Millisecond
	m := h.decisionGuard(t, f, "nimble", "llama_cpp")
	sess := adminSession(t, h)
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/classification/benchmarks", map[string]any{
		"injection_model_id": m.ID, "levels": []int{1, 2, 4, 8}, "requests_per_level": 40, "budget_ms": 5000,
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	var b store.ClassifierBenchmark
	decodeInto(t, rec, &b)
	time.Sleep(200 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	if err := h.server.Drain(ctx); err != nil {
		t.Fatalf("Drain waited for the benchmark: %v after %v", err, time.Since(started))
	}
	got, err := h.store.ClassifierBenchmarkByID(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.BenchmarkFailed || !strings.Contains(got.Error, "gateway stopped") {
		t.Fatalf("after drain: status=%s error=%q, want failed because the gateway stopped", got.Status, got.Error)
	}
}

// B9: every classifier resolution read the model and its call profile from
// the database (two queries per check per request). Lookups are now cached
// against the model revision; a role change still takes effect at once.
func TestClassifierLookupIsCachedUntilTheCatalogChanges(t *testing.T) {
	h := newHarness(t)
	f := newFakeDecider(t)
	m := h.decisionGuard(t, f, "nimble", "llama_cpp")
	ctx := context.Background()
	if _, err := h.server.secgwClassifiers().For(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	// A write behind the store's back (another replica) is not seen until
	// the cache expires: proof the hot path no longer reads the database.
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE model SET classifier_role = '' WHERE id = ?`, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.secgwClassifiers().For(ctx, m.ID); err != nil {
		t.Fatalf("second resolution read the database again: %v", err)
	}
	// A role change through the store bumps the revision and is seen.
	if err := h.store.SetModelClassifierRole(ctx, m.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.secgwClassifiers().For(ctx, m.ID); err == nil || !strings.Contains(err.Error(), "no classifier role") {
		t.Fatalf("after a role change: err=%v, want the change seen", err)
	}
}

// B3 (end to end): failed auto-routing questions on the guard's decision
// model do not stop the guard from judging the next request.
func TestRoutingFailuresDoNotOpenTheGuardBreaker(t *testing.T) {
	h := newHarness(t)
	f := newFakeDecider(t)
	m := h.decisionGuard(t, f, "nimble", "llama_cpp")
	ctx := context.Background()
	cl, err := h.server.secgwClassifiers().For(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := cl.(routingClassifier)
	if !ok {
		t.Fatal("not a decision classifier")
	}
	f.Close() // the classifier is down for routing's calls
	q := []classify.Question{{ID: "route_category", Options: []classify.Option{{Slug: "a", Name: "a"}, {Slug: "b", Name: "b"}}}}
	for i := 0; i < 5; i++ {
		if _, err := d.Decide(ctx, "route me", q); err == nil {
			t.Fatal("routing call succeeded against a closed server")
		}
	}
	if br, ok := cl.(interface{ Open() bool }); !ok || br.Open() {
		t.Fatal("routing failures opened the breaker the guards use")
	}
}
