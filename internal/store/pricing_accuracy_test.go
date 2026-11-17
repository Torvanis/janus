package store

import (
	"context"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/usage"
)

// accuracyFixture seeds two upstreams and three models and returns their ids.
type accuracyFixture struct {
	upA, upB          string
	chat, image, rout string
}

func seedAccuracyModels(t *testing.T, s *Store) accuracyFixture {
	t.Helper()
	ctx := context.Background()
	a, err := s.CreateUpstream(ctx, "xai-"+NewID()[:6], "openai_compatible", "https://api.x.example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateUpstream(ctx, "openrouter-"+NewID()[:6], "openai_compatible", "https://or.example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	f := accuracyFixture{upA: a.ID, upB: b.ID}
	for _, m := range []struct {
		up, name string
		dst      *string
	}{{a.ID, "grok-chat", &f.chat}, {a.ID, "grok-image", &f.image}, {b.ID, "router", &f.rout}} {
		if _, err := s.UpsertDiscoveredModel(ctx, m.up, m.name, []string{"chat"}); err != nil {
			t.Fatal(err)
		}
		model, err := s.ModelByUpstreamAndName(ctx, m.up, m.name)
		if err != nil {
			t.Fatal(err)
		}
		*m.dst = model.ID
	}
	return f
}

func quotedEvent(at time.Time, up, modelID, name string, quote int64, est *int64) *UsageEvent {
	return &UsageEvent{CreatedAt: at, UserID: "u1", UpstreamID: up, ModelID: modelID, ModelName: name,
		RequestID: "req-" + NewID(), EndpointPath: "/v1/chat/completions", HTTPMethod: "POST", Modality: "chat",
		HTTPStatus: 200, TokensIn: 1000, TokensOut: 100, AccountingMode: usage.AccountingUpstreamCost,
		CostNano: quote, EstimatedCostNano: est}
}

// TestPriceAccuracyReport runs on SQLite and, with
// JANUS_ROLLUP_TEST_DATABASE_URL, on PostgreSQL.
func TestPriceAccuracyReport(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	f := seedAccuracyModels(t, s)
	now := time.Now().UTC()
	in := now.Add(-time.Hour)
	events := []*UsageEvent{
		// grok-chat: exact, +0.5%, +3%, -40%.
		quotedEvent(in, f.upA, f.chat, "grok-chat", 1_000_000, i64(1_000_000)),
		quotedEvent(in, f.upA, f.chat, "grok-chat", 1_000_000, i64(1_005_000)),
		quotedEvent(in, f.upA, f.chat, "grok-chat", 1_000_000, i64(1_030_000)),
		quotedEvent(in.Add(time.Minute), f.upA, f.chat, "grok-chat", 5_000_000, i64(3_000_000)),
		// grok-chat quoted before estimates existed: counted, not compared.
		quotedEvent(in, f.upA, f.chat, "grok-chat", 7_000_000, nil),
		// grok-image: Janus has no price.
		quotedEvent(in, f.upA, f.image, "grok-image", 20_000_000, i64(0)),
		// router: quoted $0 and estimated $0 is a match.
		quotedEvent(in, f.upB, f.rout, "router", 0, i64(0)),
		// Out of range, and an unquoted call: never in the report.
		quotedEvent(now.Add(-48*time.Hour), f.upA, f.chat, "grok-chat", 9, i64(9)),
		{CreatedAt: in, UpstreamID: f.upA, ModelID: f.chat, ModelName: "grok-chat", RequestID: "r-unq", HTTPStatus: 200,
			AccountingMode: usage.AccountingUpstream, CostNano: 123},
	}
	for _, e := range events {
		if err := s.InsertUsageEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.PriceAccuracy(ctx, now.Add(-24*time.Hour), now.Add(time.Minute), 3)
	if err != nil {
		t.Fatal(err)
	}
	tot := r.Totals
	if tot.QuotedCalls != 7 || tot.ComparedCalls != 6 || tot.QuotedNano != 28_000_000 || tot.EstimateNano != 6_035_000 ||
		tot.Within1 != 3 || tot.Within5 != 4 || tot.NoPriceCalls != 1 || tot.DiffNano != 6_035_000-28_000_000 {
		t.Fatalf("totals = %+v", tot)
	}
	if len(r.Models) != 3 || r.Models[0].ModelName != "grok-image" || r.Models[1].ModelName != "grok-chat" {
		t.Fatalf("models (most quoted first) = %+v", r.Models)
	}
	chat := r.Models[1]
	if chat.QuotedCalls != 5 || chat.ComparedCalls != 4 || chat.QuotedNano != 8_000_000 || chat.EstimateNano != 6_035_000 ||
		chat.DiffPercent == nil || *chat.DiffPercent > -24.5 || *chat.DiffPercent < -24.6 || chat.HasPriceNow != nil {
		t.Fatalf("grok-chat = %+v", chat)
	}
	img := r.Models[0]
	if img.NoPriceCalls != 1 || img.HasPriceNow == nil || *img.HasPriceNow || img.UpstreamName == "" {
		t.Fatalf("grok-image (no price, none set now) = %+v", img)
	}
	if router := r.Models[2]; router.DiffPercent != nil || router.Within1 != 1 {
		t.Fatalf("router quoted $0 = %+v", router)
	}
	if len(r.Providers) != 2 || r.Providers[0].UpstreamID != f.upA || r.Providers[0].QuotedCalls != 6 || r.Providers[0].QuotedNano != 28_000_000 {
		t.Fatalf("providers = %+v", r.Providers)
	}
	if len(r.Worst) != 3 || r.Worst[0].ModelName != "grok-image" || r.Worst[0].DiffNano != -20_000_000 ||
		r.Worst[1].DiffNano != -2_000_000 || r.Worst[2].DiffNano != 30_000 || r.Worst[0].RequestID == "" || r.Worst[0].UpstreamName == "" {
		t.Fatalf("worst = %+v", r.Worst)
	}
	// The estimate round-trips through the request log; unquoted rows have none.
	list, _, err := s.ListRequests(ctx, RequestFilter{Limit: 50, IncludeInternal: true})
	if err != nil {
		t.Fatal(err)
	}
	var withEst, unquotedEst int
	for _, e := range list {
		if e.EstimatedCostNano != nil {
			withEst++
		}
		if e.AccountingMode != usage.AccountingUpstreamCost && e.EstimatedCostNano != nil {
			unquotedEst++
		}
	}
	if withEst != 7 || unquotedEst != 0 {
		t.Fatalf("estimates read back: %d (want 7), on unquoted rows: %d", withEst, unquotedEst)
	}
	// One request id narrows the log whatever the range.
	one, total, err := s.ListRequests(ctx, RequestFilter{RequestID: events[3].RequestID, Limit: 10})
	if err != nil || total != 1 || len(one) != 1 || one[0].ID != events[3].ID {
		t.Fatalf("request_id filter: %d rows, total %d, err %v", len(one), total, err)
	}
}

// TestBackfillQuotedEstimates prices older quoted calls from the price
// history, and only those it can price exactly as the proxy would have.
func TestBackfillQuotedEstimates(t *testing.T) {
	s := newRollupStore(t)
	ctx := context.Background()
	f := seedAccuracyModels(t, s)
	now := time.Now().UTC()
	t0 := now.Add(-30 * 24 * time.Hour)
	version := func(modelID string, at time.Time, in, out int64, lines string) {
		t.Helper()
		if err := s.exec(ctx, `INSERT INTO rate_card_version (id, model_id, rate_in_nanousd, rate_out_nanousd, rate_cached_nanousd, lines_json, effective_from, created_at)
			VALUES (?,?,?,?,?,?,?,?)`, NewID(), modelID, in, out, 0, lines, FormatTime(at), FormatTime(at)); err != nil {
			t.Fatal(err)
		}
	}
	// grok-chat: $2/$6 per million, then $3/$9 from day 10, with a
	// long-prompt line ($10/M input past 100k tokens).
	version(f.chat, t0, 2*usage.NanoPerUSD, 6*usage.NanoPerUSD, "{}")
	version(f.chat, t0.Add(10*24*time.Hour), 3*usage.NanoPerUSD, 9*usage.NanoPerUSD, `{"rate_in_nanousd@prompt_over=100000":10000000000}`)
	// router (a decision model) has a price but a per-call fee today.
	version(f.rout, t0, usage.NanoPerUSD, 0, "{}")
	if err := s.exec(ctx, `UPDATE model SET classifier_role = ? WHERE id = ?`, ClassifierRoleDecision, f.rout); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDecisionSetting(ctx, DecisionSetting{ModelID: f.rout, RequestFeeNano: 1000}); err != nil {
		t.Fatal(err)
	}

	early := quotedEvent(t0.Add(-time.Hour), f.upA, f.chat, "grok-chat", 1, nil) // before any price: stays NULL
	first := quotedEvent(t0.Add(24*time.Hour), f.upA, f.chat, "grok-chat", 1, nil)
	second := quotedEvent(t0.Add(11*24*time.Hour), f.upA, f.chat, "grok-chat", 1, nil)
	long := quotedEvent(t0.Add(12*24*time.Hour), f.upA, f.chat, "grok-chat", 1, nil)
	long.TokensIn = 200_000
	media := quotedEvent(t0.Add(24*time.Hour), f.upA, f.image, "grok-image", 1, nil) // no tokens: nothing to price
	media.TokensIn, media.TokensOut, media.Modality = 0, 0, "image"
	decision := quotedEvent(t0.Add(24*time.Hour), f.upB, f.rout, "router", 1, nil)
	decision.Modality = "decision"
	already := quotedEvent(t0.Add(24*time.Hour), f.upA, f.chat, "grok-chat", 1, i64(42)) // untouched
	unquoted := quotedEvent(t0.Add(24*time.Hour), f.upA, f.chat, "grok-chat", 1, nil)
	unquoted.AccountingMode = usage.AccountingUpstream
	for _, e := range []*UsageEvent{early, first, second, long, media, decision, already, unquoted} {
		if err := s.InsertUsageEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.BackfillQuotedEstimates(ctx, 2) // tiny pages: exercise the cursor
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 6 || res.Filled != 3 {
		t.Fatalf("backfill = %+v, want 6 looked at, 3 filled", res)
	}
	want := map[string]*int64{
		early.ID:    nil,
		first.ID:    i64(usage.TokenPrice(usage.TokenCounts{In: 1000, Out: 100}, usage.Rates{InNano: 2 * usage.NanoPerUSD, OutNano: 6 * usage.NanoPerUSD})),
		second.ID:   i64(3_000_000 + 900_000),
		long.ID:     i64(2_000_000_000 + 900_000), // the long-prompt line applies: $10/M in
		media.ID:    nil,
		decision.ID: nil, // a fee today; the history does not keep fees
		already.ID:  i64(42),
		unquoted.ID: nil,
	}
	if *want[first.ID] != 2_000_000+600_000 {
		t.Fatalf("first = %d", *want[first.ID])
	}
	for id, w := range want {
		e, err := s.UsageEventByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case w == nil && e.EstimatedCostNano != nil:
			t.Errorf("%s: estimate %d, want none", id, *e.EstimatedCostNano)
		case w != nil && (e.EstimatedCostNano == nil || *e.EstimatedCostNano != *w):
			t.Errorf("%s: estimate %v, want %d", id, e.EstimatedCostNano, *w)
		}
		if e.CostNano != 1 {
			t.Errorf("%s: billed cost changed to %d", id, e.CostNano)
		}
	}
	// Without the fee, the decision call is priced by its three token rates.
	if err := s.SetDecisionSetting(ctx, DecisionSetting{ModelID: f.rout}); err != nil {
		t.Fatal(err)
	}
	res, err = s.BackfillQuotedEstimates(ctx, 500)
	if err != nil || res.Filled != 1 {
		t.Fatalf("second pass = %+v %v, want only the decision call filled", res, err)
	}
	if e, _ := s.UsageEventByID(ctx, decision.ID); e.EstimatedCostNano == nil || *e.EstimatedCostNano != 1_000_000 {
		t.Fatalf("decision estimate = %v, want 1000000", e.EstimatedCostNano)
	}
}
