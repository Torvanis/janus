package discovery_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// An upstream that is restarting can answer {"data":[]}. That must not mark
// its enabled models stale: the run counts as failed and the rows stay.
func TestEmptyListingWithEnabledModelsIsAFailedRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	up := h.createUpstream(t, "vllm-local")
	h.provider.set(http.StatusOK, "m1", "m2")
	if res := h.svc.RunFor(ctx, up); res.Error != "" {
		t.Fatalf("seed run failed: %s", res.Error)
	}
	for _, n := range []string{"m1", "m2"} {
		if err := h.store.SetModelStatus(ctx, modelID(t, h, up.ID, n), store.ModelEnabled); err != nil {
			t.Fatal(err)
		}
	}
	h.provider.set(http.StatusOK)
	res := h.svc.RunFor(ctx, up)
	if res.Error == "" || res.Stale != 0 {
		t.Fatalf("empty listing = error %q stale %d, want a failed run that marks nothing stale", res.Error, res.Stale)
	}
	st := h.modelStatuses(t, up.ID)
	if st["m1"] != store.ModelEnabled || st["m2"] != store.ModelEnabled {
		t.Fatalf("an empty listing changed enabled models: %v", st)
	}
	reloaded, err := h.store.UpstreamByID(ctx, up.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LastError == "" {
		t.Fatal("the failed empty listing was not recorded on the upstream")
	}
}

// An upstream with no enabled models may legitimately list nothing.
func TestEmptyListingWithoutEnabledModelsStillMarksStale(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	up := h.createUpstream(t, "empty-ok")
	h.provider.set(http.StatusOK, "m1")
	_ = h.svc.RunFor(ctx, up)
	h.provider.set(http.StatusOK)
	res := h.svc.RunFor(ctx, up)
	if res.Error != "" || res.Stale != 1 {
		t.Fatalf("empty listing without enabled models = error %q stale %d, want ok/1", res.Error, res.Stale)
	}
}

// An enabled model that drops out of a good listing goes stale, raises an
// admin notification, and resumes "enabled" when it is listed again.
func TestStaleEnabledModelAlertsAndRestoresPreviousStatus(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin, _, err := h.store.UpsertUserFromIdentity(ctx, "admin-sub", "admin@example.com", "Admin", true, false)
	if err != nil {
		t.Fatal(err)
	}
	up := h.createUpstream(t, "prod")
	h.provider.set(http.StatusOK, "m1", "m2", "m3")
	_ = h.svc.RunFor(ctx, up)
	_ = h.store.SetModelStatus(ctx, modelID(t, h, up.ID, "m1"), store.ModelEnabled)
	_ = h.store.SetModelStatus(ctx, modelID(t, h, up.ID, "m2"), store.ModelDisabled)

	h.provider.set(http.StatusOK, "m3")
	if res := h.svc.RunFor(ctx, up); res.Error != "" || res.Stale != 2 {
		t.Fatalf("partial listing = error %q stale %d, want ok/2", res.Error, res.Stale)
	}
	notes, _, err := h.store.ListNotifications(ctx, admin.ID, false, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n.Title, "no longer listed") && strings.Contains(n.Body, "m1") && !strings.Contains(n.Body, "m2") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no admin notification naming the enabled model that went stale; inbox: %v", titles(notes))
	}

	h.provider.set(http.StatusOK, "m1", "m2", "m3")
	_ = h.svc.RunFor(ctx, up)
	st := h.modelStatuses(t, up.ID)
	if st["m1"] != store.ModelEnabled || st["m2"] != store.ModelDisabled {
		t.Fatalf("models back from stale = %v, want m1 enabled and m2 disabled (their previous state)", st)
	}
}
