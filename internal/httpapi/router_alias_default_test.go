package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// A router whose default is an alias resolves that alias at request time,
// not from the snapshot taken when the router was saved: the alias's
// fallback, a repoint and the catalogs behave exactly as calling the alias.
func TestRouterDefaultAliasResolvesAtRequestTime(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	x := newRoutedEngine(t, "x-model")
	f := newRoutedEngine(t, "f-model")
	y := newRoutedEngine(t, "y-model")
	mx, mf, my := h.addModel(t, x, 0), h.addModel(t, f, 0), h.addModel(t, y, 0)

	safe, err := h.store.CreateManagedModel(ctx, "safe", "", mx.ID, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetManagedModelFallback(ctx, safe.ID, mf.ID, []string{store.FallbackTriggerTargetUnavailable}); err != nil {
		t.Fatal(err)
	}
	r, err := h.store.CreateRouter(ctx, store.RouterInput{Name: "auto/test", Status: store.RouterStatusLive, Config: store.RouterConfig{
		Default: store.RouterDefault{ModelID: safe.ID},
	}}, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{safe.ID, r.ID} {
		if _, err := h.store.CreateGrant(ctx, id, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
			t.Fatal(err)
		}
	}
	call := func(model string) *httptest.ResponseRecorder {
		h.server.InvalidateConfigCache()
		h.server.InvalidateResolvedModelCache("safe", "auto/test")
		return h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	}
	listed := func() bool {
		h.server.InvalidateConfigCache()
		return strings.Contains(h.do(http.MethodGet, "/v1/models", nil).Body.String(), `"auto/test"`)
	}

	// 1. The alias's target goes out: the alias serves from its fallback,
	// and so does the router.
	if err := h.store.SetModelStatus(ctx, mx.ID, store.ModelDisabled); err != nil {
		t.Fatal(err)
	}
	if rec := call("safe"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from f-model") {
		t.Fatalf("alias with its target out = %d %s, want the fallback", rec.Code, rec.Body.String())
	}
	if rec := call("auto/test"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from f-model") {
		t.Fatalf("router whose default alias falls back = %d %s, want the alias's fallback", rec.Code, rec.Body.String())
	}
	if !listed() {
		t.Fatal("the router dropped out of /v1/models although its default alias serves")
	}
	if mm, err := h.store.ManagedModelByID(ctx, r.ID); err != nil || !mm.Servable || mm.Broken {
		t.Fatalf("router reported broken while its default alias serves: %+v %v", mm, err)
	}

	// 2. The alias is repointed: the router follows without being re-saved.
	if err := h.store.SetModelStatus(ctx, mx.ID, store.ModelEnabled); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetManagedModelTarget(ctx, safe.ID, my.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelStatus(ctx, mx.ID, store.ModelDisabled); err != nil {
		t.Fatal(err)
	}
	if rec := call("auto/test"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from y-model") {
		t.Fatalf("router after its default alias was repointed = %d %s, want the new target", rec.Code, rec.Body.String())
	}

	// 3. The alias truly cannot serve: the router is broken, says why, and
	// leaves the catalog.
	if err := h.store.SetModelStatus(ctx, my.ID, store.ModelDisabled); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelStatus(ctx, mf.ID, store.ModelDisabled); err != nil {
		t.Fatal(err)
	}
	if rec := call("auto/test"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("router whose default alias cannot serve = %d %s, want 503", rec.Code, rec.Body.String())
	}
	if listed() {
		t.Fatal("a router whose default cannot serve is still offered in /v1/models")
	}
	rt, err := h.store.RouterByID(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Problems) == 0 {
		t.Fatal("the router page shows no problem although the default alias cannot serve")
	}
}

// A router whose default is a pool keeps serving while the pool's primary
// member is out and another member serves.
func TestRouterDefaultPoolServesFromOtherMembers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := newRoutedEngine(t, "pool-a"), newRoutedEngine(t, "pool-b")
	ma, mb := h.addModel(t, a, 0), h.addModel(t, b, 0)
	pool, err := h.store.CreateManagedModel(ctx, "pooled", "", ma.ID, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	norm, err := h.store.NormalizePool(ctx, store.PoolInput{Policy: store.PoolPolicyFailover, Members: []store.PoolMemberInput{
		{ModelID: ma.ID, Priority: 1, Enabled: &on}, {ModelID: mb.ID, Priority: 2, Enabled: &on},
	}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetManagedModelPool(ctx, pool.ID, norm); err != nil {
		t.Fatal(err)
	}
	r, err := h.store.CreateRouter(ctx, store.RouterInput{Name: "auto/pool", Status: store.RouterStatusLive, Config: store.RouterConfig{
		Default: store.RouterDefault{ModelID: pool.ID},
	}}, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateGrant(ctx, r.ID, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetModelStatus(ctx, ma.ID, store.ModelDisabled); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	h.server.InvalidateResolvedModelCache("auto/pool")
	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "auto/pool", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "answer from pool-b") {
		t.Fatalf("router whose default pool lost its primary = %d %s, want the other member", rec.Code, rec.Body.String())
	}
}
