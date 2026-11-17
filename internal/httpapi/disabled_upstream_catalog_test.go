package httpapi

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// A model whose upstream is turned off leaves every user-facing list even
// though it is granted: /v1/models, the Models page API and the Chat picker.
// A managed model leaves them once none of its targets can serve; a pool
// with a live member, an alias with a live fallback and a router with a live
// route stay. Requests still fail as before (503 upstream.unavailable, now
// saying the upstream is off), admin lists keep everything, and turning the
// upstream back on restores the lot with its grants.
func TestDisabledUpstreamHidesModelsFromUserLists(t *testing.T) {
	h, admin := chatHarness(t)
	ctx := context.Background()
	user := h.sessionFor(t, "lists@example.com")
	x, y, f := newRoutedEngine(t, "x-model"), newRoutedEngine(t, "y-model"), newRoutedEngine(t, "f-model")
	mx, my, mf := h.addModel(t, x, 0), h.addModel(t, y, 0), h.addModel(t, f, 0)

	solo, err := h.store.CreateManagedModel(ctx, "solo", "", mx.ID, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	safe, err := h.store.CreateManagedModel(ctx, "safe", "", mx.ID, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetManagedModelFallback(ctx, safe.ID, mf.ID, []string{store.FallbackTriggerTargetUnavailable}); err != nil {
		t.Fatal(err)
	}
	duo, err := h.store.CreateManagedModel(ctx, "duo", "", mx.ID, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := h.store.NormalizePool(ctx, store.PoolInput{Members: []store.PoolMemberInput{{ModelID: mx.ID}, {ModelID: my.ID}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetManagedModelPool(ctx, duo.ID, pool); err != nil {
		t.Fatal(err)
	}
	router, err := h.store.CreateRouter(ctx, store.RouterInput{Name: "auto/test", Status: store.RouterStatusLive, Config: store.RouterConfig{
		Default: store.RouterDefault{ModelID: mx.ID},
		Routes:  []store.RouterRoute{{ID: "y", Name: "Y", ModelID: my.ID, Categories: []string{"coding"}}},
	}}, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{mx.ID, my.ID, mf.ID} {
		if _, err := h.store.CreateGrant(ctx, id, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{solo.ID, safe.ID, duo.ID, router.ID} {
		if _, err := h.store.CreateGrant(ctx, id, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
			t.Fatal(err)
		}
	}
	setUpstream := func(m *store.Model, e *routedEngine, enabled bool) {
		t.Helper()
		if err := h.store.UpdateUpstream(ctx, m.UpstreamID, e.srv.URL, enabled, "", ""); err != nil {
			t.Fatal(err)
		}
		h.server.InvalidateConfigCache()
		h.server.InvalidateResolvedModelCache("x-model", "y-model", "solo", "safe", "duo", "auto/test")
	}

	// lists returns what each user-facing list offers, by name.
	lists := func() map[string][]string {
		t.Helper()
		h.server.InvalidateConfigCache()
		out := map[string][]string{}
		var openai struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		decodeRec(t, h.do(http.MethodGet, "/v1/models", nil), &openai)
		for _, d := range openai.Data {
			out["/v1/models"] = append(out["/v1/models"], d.ID)
		}
		var page struct {
			Models  []struct{ Name string } `json:"models"`
			Managed []struct{ Name string } `json:"managed_models"`
		}
		decodeRec(t, h.doAsSession(user, http.MethodGet, "/api/v1/models", nil), &page)
		for _, m := range page.Models {
			out["/api/v1/models"] = append(out["/api/v1/models"], m.Name)
		}
		for _, m := range page.Managed {
			out["/api/v1/models"] = append(out["/api/v1/models"], m.Name)
		}
		var picker struct {
			Models []struct{ Name string } `json:"models"`
		}
		decodeRec(t, h.doAsSession(user, http.MethodGet, "/api/v1/chat/catalog", nil), &picker)
		for _, m := range picker.Models {
			out["/api/v1/chat/catalog"] = append(out["/api/v1/chat/catalog"], m.Name)
		}
		return out
	}
	expect := func(step string, shown, hidden []string) {
		t.Helper()
		got := lists()
		for list, names := range got {
			for _, n := range shown {
				// The picker lists chat models by catalog name; managed
				// models appear there too, under their own name.
				if !slices.Contains(names, n) {
					t.Errorf("%s: %s lacks %q: %v", step, list, n, names)
				}
			}
			for _, n := range hidden {
				if slices.Contains(names, n) {
					t.Errorf("%s: %s still offers %q: %v", step, list, n, names)
				}
			}
		}
		if len(got) != 3 {
			t.Fatalf("%s: lists read = %v", step, got)
		}
	}
	chat := func(model string) (int, string) {
		h.server.InvalidateResolvedModelCache(model)
		rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
		return rec.Code, rec.Body.String()
	}

	all := []string{"x-model", "y-model", "f-model", "solo", "safe", "duo", "auto/test"}
	expect("all upstreams on", all, nil)

	// x's upstream off: x and the alias that only serves from x go; the
	// alias with a live fallback, the pool with a live member and the
	// router with a live route stay.
	setUpstream(mx, x, false)
	expect("x upstream off", []string{"y-model", "f-model", "safe", "duo", "auto/test"}, []string{"x-model", "solo"})
	for _, name := range []string{"x-model", "solo"} {
		code, body := chat(name)
		if code != http.StatusServiceUnavailable || !strings.Contains(body, `"upstream.unavailable"`) || !strings.Contains(body, "is turned off") {
			t.Fatalf("request to hidden %s = %d %s, want 503 upstream.unavailable saying the upstream is off", name, code, body)
		}
	}
	if code, body := chat("safe"); code != http.StatusOK || !strings.Contains(body, "answer from f-model") {
		t.Fatalf("alias with a live fallback = %d %s, want the fallback", code, body)
	}
	if code, body := chat("duo"); code != http.StatusOK || !strings.Contains(body, "answer from y-model") {
		t.Fatalf("pool with a live member = %d %s, want y", code, body)
	}

	// y's upstream off too: the pool and the router have nothing live.
	setUpstream(my, y, false)
	expect("x and y upstreams off", []string{"f-model", "safe"}, []string{"x-model", "y-model", "solo", "duo", "auto/test"})

	// Admin lists keep everything, with the upstream state visible.
	var adminModels struct {
		Models []struct {
			Name            string `json:"name"`
			Status          string `json:"status"`
			UpstreamEnabled bool   `json:"upstream_enabled"`
		} `json:"models"`
	}
	decodeRec(t, h.doAsSession(admin, http.MethodGet, "/api/v1/admin/models", nil), &adminModels)
	seen := map[string]bool{}
	for _, m := range adminModels.Models {
		seen[m.Name] = true
		if m.Name == "x-model" && (m.UpstreamEnabled || m.Status != store.ModelEnabled) {
			t.Fatalf("admin x-model = %+v, want enabled on a disabled upstream", m)
		}
	}
	if !seen["x-model"] || !seen["y-model"] {
		t.Fatalf("admin models lost the disabled upstream's models: %+v", adminModels.Models)
	}
	var adminManaged struct {
		Models []struct{ Name string } `json:"managed_models"`
	}
	decodeRec(t, h.doAsSession(admin, http.MethodGet, "/api/v1/admin/managed-models", nil), &adminManaged)
	for _, n := range []string{"solo", "duo"} {
		if !slices.ContainsFunc(adminManaged.Models, func(m struct{ Name string }) bool { return m.Name == n }) {
			t.Fatalf("admin managed models lost %q: %+v", n, adminManaged.Models)
		}
	}

	// Both back on: everything returns, grants untouched.
	setUpstream(mx, x, true)
	setUpstream(my, y, true)
	expect("upstreams back on", all, nil)
	if code, body := chat("solo"); code != http.StatusOK || !strings.Contains(body, "answer from x-model") {
		t.Fatalf("alias after re-enabling = %d %s", code, body)
	}
}
