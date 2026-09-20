package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// TestAdminDiscoveryIntervalEndpoints covers the runtime discovery-interval
// contract: GET reports the environment default as effective; PATCH overrides
// it, persists it, surfaces it in the status document, audit-logs the change
// and makes the discovery scheduler honour it; out-of-range values are
// refused; 0 and DELETE revert to the default.
func TestAdminDiscoveryIntervalEndpoints(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.server.Config.DiscoveryInterval = 60 * time.Minute
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	const path = "/api/v1/admin/system/discovery-interval"
	decode := func(rec *httptest.ResponseRecorder) discoveryIntervalDocument {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", path, rec.Code, rec.Body.String())
		}
		var doc discoveryIntervalDocument
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode document: %v", err)
		}
		return doc
	}

	doc := decode(h.doAsSession(session, http.MethodGet, path, nil))
	if doc.EffectiveMinutes != 60 || doc.DefaultMinutes != 60 || doc.OverrideMinutes != 0 || doc.Source != "default" {
		t.Fatalf("fresh instance doc = %+v, want 60 from default", doc)
	}
	if doc.MinMinutes != store.MinDiscoveryIntervalMinutes || doc.MaxMinutes != store.MaxDiscoveryIntervalMinutes {
		t.Fatalf("bounds = %d..%d", doc.MinMinutes, doc.MaxMinutes)
	}
	if got := h.server.Discovery.EffectiveInterval(ctx); got != 60*time.Minute {
		t.Fatalf("scheduler effective interval = %v, want 60m", got)
	}

	doc = decode(h.doAsSession(session, http.MethodPatch, path, map[string]any{"minutes": 2}))
	if doc.EffectiveMinutes != 2 || doc.OverrideMinutes != 2 || doc.Source != "override" || doc.DefaultMinutes != 60 {
		t.Fatalf("after patch doc = %+v, want effective 2 from override, default still 60", doc)
	}
	if doc.UpdatedAt == nil {
		t.Fatal("updated_at must be set once an override is stored")
	}
	if got := h.server.Discovery.EffectiveInterval(ctx); got != 2*time.Minute {
		t.Fatalf("scheduler must honour the override, got %v", got)
	}
	stored, err := h.store.DiscoveryIntervalOverride(ctx)
	if err != nil || stored.Minutes != 2 {
		t.Fatalf("stored override = %+v (err %v), want 2", stored, err)
	}

	statusRec := h.doAsSession(session, http.MethodGet, "/api/v1/admin/system/status", nil)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status returned %d: %s", statusRec.Code, statusRec.Body.String())
	}
	var status struct {
		Discovery struct {
			IntervalMinutes int                       `json:"interval_minutes"`
			Interval        discoveryIntervalDocument `json:"interval"`
		} `json:"discovery"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Discovery.IntervalMinutes != 2 || status.Discovery.Interval.Source != "override" {
		t.Fatalf("status discovery = %+v, want effective 2 from override", status.Discovery)
	}

	entries, _, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "discovery_interval_changed", Limit: 10})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("PATCH must write one audit entry, got %d", len(entries))
	}
	if !strings.Contains(entries[0].OldValue, `"effective_minutes":60`) || !strings.Contains(entries[0].NewValue, `"effective_minutes":2`) {
		t.Fatalf("audit must record 60 -> 2; old=%s new=%s", entries[0].OldValue, entries[0].NewValue)
	}

	for name, body := range map[string]map[string]any{
		"negative":   {"minutes": -1},
		"past 7d":    {"minutes": store.MaxDiscoveryIntervalMinutes + 1},
		"empty body": {},
		"string":     {"minutes": "two"},
	} {
		rec := h.doAsSession(session, http.MethodPatch, path, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: PATCH %v returned %d, want 400: %s", name, body, rec.Code, rec.Body.String())
		}
	}
	if stored, _ = h.store.DiscoveryIntervalOverride(ctx); stored.Minutes != 2 {
		t.Fatalf("rejected patches must not change the stored value, got %+v", stored)
	}

	doc = decode(h.doAsSession(session, http.MethodPatch, path, map[string]any{"minutes": 0}))
	if doc.Source != "default" || doc.EffectiveMinutes != 60 {
		t.Fatalf("minutes 0 must revert to default, got %+v", doc)
	}
	decode(h.doAsSession(session, http.MethodPatch, path, map[string]any{"minutes": 5}))
	doc = decode(h.doAsSession(session, http.MethodDelete, path, nil))
	if doc.Source != "default" || doc.OverrideMinutes != 0 {
		t.Fatalf("DELETE must clear the override, got %+v", doc)
	}
	if stored, _ = h.store.DiscoveryIntervalOverride(ctx); stored.Minutes != 0 {
		t.Fatalf("override row must be gone after DELETE, got %+v", stored)
	}
}
