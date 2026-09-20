package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/reporting"
	"github.com/torvanis/janus/internal/store"
)

func TestReportingWorkerExecutesPersistedRun(t *testing.T) {
	db, _, logger := newPurgeFixture(t)
	ctx := context.Background()
	user, _, err := db.UpsertUserFromIdentity(ctx, "report-worker", "worker@example.test", "Worker", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.InsertUsageEvent(ctx, &store.UsageEvent{UserID: user.ID, GroupIDs: []string{}, ModelName: "test-model", TokensIn: 11, TokensOut: 7, CostNano: 1230000000, HTTPStatus: 200, CreatedAt: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	d := reporting.Definition{Version: 1, Name: "My usage", Template: "usage", Scope: "self", Period: "last_7_days", Timezone: "UTC", GroupMode: "historical", Dimensions: []string{"model"}, Metrics: []string{"requests", "tokens_in", "cost_usd"}}
	queued, err := db.EnqueueReport(ctx, user.ID, "", d)
	if err != nil {
		t.Fatal(err)
	}
	worker := NewReportWorker(db, false, logger)
	worked, err := worker.RunOnce(ctx)
	if err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	result, err := db.ReportRunByID(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "complete" || result.Result == nil {
		t.Fatalf("run not completed: %+v", result)
	}
	if result.Result.Totals["requests"] == nil || *result.Result.Totals["requests"] != 1 {
		t.Fatalf("wrong actual count: %+v", result.Result.Totals)
	}
	notifications, _, err := db.ListNotifications(ctx, user.ID, false, 100)
	if err != nil || len(notifications) != 1 {
		t.Fatalf("notifications=%v err=%v", notifications, err)
	}
	worked, err = worker.RunOnce(ctx)
	if err != nil || worked {
		t.Fatalf("duplicate execution worked=%v err=%v", worked, err)
	}
	notifications, _, err = db.ListNotifications(ctx, user.ID, false, 100)
	if err != nil || len(notifications) != 1 {
		t.Fatalf("duplicate notification: %v %v", notifications, err)
	}
}

func TestReportingWorkerLocalOnlyRedactsMoney(t *testing.T) {
	db, _, logger := newPurgeFixture(t)
	ctx := context.Background()
	user, _, err := db.UpsertUserFromIdentity(ctx, "report-local", "local@example.test", "Local", false, false)
	if err != nil {
		t.Fatal(err)
	}
	err = db.InsertUsageEvent(ctx, &store.UsageEvent{UserID: user.ID, ModelName: "test", HTTPStatus: 200, CostNano: 1230000000, CreatedAt: time.Now().UTC().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	d := reporting.Definition{Version: 1, Name: "Local usage", Template: "usage", Scope: "self", Period: "last_7_days", Timezone: "UTC", GroupMode: "historical", Dimensions: []string{"model"}, Metrics: []string{"requests", "cost_usd"}}
	queued, err := db.EnqueueReport(ctx, user.ID, "", d)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewReportWorker(db, true, logger).RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r, err := db.ReportRunByID(ctx, queued.ID)
	if err != nil || r.Result == nil {
		t.Fatalf("run=%+v err=%v", r, err)
	}
	if _, ok := r.Result.Totals["cost_usd"]; ok {
		t.Fatal("local-only run persisted visible monetary total")
	}
	if r.Result.Totals["requests"] == nil || *r.Result.Totals["requests"] != 1 {
		t.Fatal("redaction lost nonmonetary report")
	}
}
