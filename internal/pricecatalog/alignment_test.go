package pricecatalog_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/torvanis/janus/internal/pricecatalog"
	"github.com/torvanis/janus/internal/store"
)

// undecide makes a model look as it did before "keep aligned" existed.
func undecide(t *testing.T, s *store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	var raw string
	if err := s.DB().QueryRowContext(ctx, `SELECT metadata_json FROM model WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	delete(st, "catalog_aligned")
	b, _ := json.Marshal(st)
	if _, err := s.DB().ExecContext(ctx, `UPDATE model SET metadata_json = ? WHERE id = ?`, string(b), id); err != nil {
		t.Fatal(err)
	}
}

func model(t *testing.T, s *store.Store, id string) *store.Model {
	t.Helper()
	m, err := s.ModelByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// At startup after an upgrade, Reconcile decides "keep aligned" for models
// from before it existed without changing what anything bills, prices
// models discovered before the catalog existed, and is a no-op the second
// time.
func TestReconcileAfterUpgrade(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	gpt4o, _, mini := seedModels(t, r.store)
	up, err := r.store.UpstreamByID(ctx, gpt4o.UpstreamID)
	if err != nil {
		t.Fatal(err)
	}
	// Discovered before the catalog existed: no catalog layer at all.
	if _, err := r.store.UpsertDiscoveredModel(ctx, up.ID, "gpt-4o-2024-08-06", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	early, err := r.store.ModelByUpstreamAndName(ctx, up.ID, "gpt-4o-2024-08-06")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{gpt4o.ID, mini.ID, early.ID} {
		undecide(t, r.store, id)
	}
	before := map[string]int64{gpt4o.ID: model(t, r.store, gpt4o.ID).RateInNano, mini.ID: model(t, r.store, mini.ID).RateInNano}

	m := r.manager(pricecatalog.Config{RequireSignature: true})
	res, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decided < 3 {
		t.Fatalf("decided %+v", res)
	}
	if g := model(t, r.store, gpt4o.ID); !g.CatalogAligned || g.RateInNano != before[gpt4o.ID] {
		t.Fatalf("gpt-4o: aligned=%v in=%d", g.CatalogAligned, g.RateInNano)
	}
	if g := model(t, r.store, mini.ID); g.CatalogAligned || g.RateInNano != before[mini.ID] {
		t.Fatalf("admin-priced model: aligned=%v in=%d", g.CatalogAligned, g.RateInNano)
	}
	// The pre-catalog model was unpriced, so it is not aligned (that
	// would price it), but its catalog layer is filled like every model's.
	if g := model(t, r.store, early.ID); g.CatalogAligned || g.RateInNano != 2.5e9 {
		t.Fatalf("pre-catalog model: aligned=%v in=%d", g.CatalogAligned, g.RateInNano)
	}
	st, err := r.store.PriceCatalogStatus(ctx)
	if err != nil || st.AppliedVersion != m.Current().Version {
		t.Fatalf("status %+v %v", st, err)
	}
	entries, _, err := r.store.ListAudit(ctx, store.AuditFilter{Action: "price_catalog_applied"})
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit %d %v", len(entries), err)
	}
	// Nothing left to do: a second run (another replica, the next tick)
	// changes nothing.
	again, err := r.manager(pricecatalog.Config{RequireSignature: true}).Reconcile(ctx)
	if err != nil || again != (pricecatalog.ApplyResult{}) {
		t.Fatalf("second reconcile %+v %v", again, err)
	}
}

// A catalog update moves an aligned model's prices and keeps an
// administrator's line; a price the provider publishes through its API
// still applies over the catalog on an aligned model.
func TestAlignedModelsFollowCatalogUpdates(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	gpt4o, _, mini := seedModels(t, r.store)
	if !gpt4o.CatalogAligned || mini.CatalogAligned {
		t.Fatalf("seed: gpt-4o aligned=%v, admin-priced aligned=%v", gpt4o.CatalogAligned, mini.CatalogAligned)
	}
	// xAI publishes prices through its API; the catalog has an entry too.
	xai, err := r.store.CreateUpstream(ctx, "xai", "openai_compatible", "https://api.x.ai/v1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ref := map[string]int64{"rate_in_nanousd": 9e9, "rate_out_nanousd": 9e9}
	if _, err := r.store.UpsertDiscoveredMetadata(ctx, xai.ID, "grok-zz", []string{"chat"}, store.AutomaticMetadata{
		Upstream: map[string]int64{"rate_in_nanousd": 1e9}, Reference: ref,
	}); err != nil {
		t.Fatal(err)
	}
	grok, _ := r.store.ModelByUpstreamAndName(ctx, xai.ID, "grok-zz")
	if !grok.CatalogAligned || grok.RateInNano != 1e9 || grok.Metadata["rate_in_nanousd"].Source != "upstream" {
		t.Fatalf("provider API price on an aligned model: %+v", grok.Metadata["rate_in_nanousd"])
	}

	if err := r.store.SetPriceCatalogSettings(ctx, store.PriceCatalogSettings{RefreshEnabled: true, URL: r.url()}); err != nil {
		t.Fatal(err)
	}
	body := r.catalog("9000.1", 3e9)
	r.feed.set(body, pricecatalog.Sign(body, r.priv), "")
	m := r.manager(pricecatalog.Config{RequireSignature: true})
	if st, err := m.Check(ctx, pricecatalog.Actor{}); err != nil || st.Result != "updated" {
		t.Fatalf("check %+v %v", st, err)
	}
	if g := model(t, r.store, gpt4o.ID); g.RateInNano != 3e9 || !g.CatalogAligned {
		t.Fatalf("aligned model did not follow: in=%d aligned=%v", g.RateInNano, g.CatalogAligned)
	}
	if g := model(t, r.store, mini.ID); g.RateInNano != 123 || g.CatalogAligned {
		t.Fatalf("admin line changed: in=%d", g.RateInNano)
	}
	if g := model(t, r.store, grok.ID); g.RateInNano != 1e9 {
		t.Fatalf("provider API price lost: %d", g.RateInNano)
	}
	// Check applied it; Reconcile has nothing left to do.
	if res, err := m.Reconcile(ctx); err != nil || res != (pricecatalog.ApplyResult{}) {
		t.Fatalf("reconcile after check %+v %v", res, err)
	}
}
