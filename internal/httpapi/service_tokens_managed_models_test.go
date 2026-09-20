package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// doAsServiceToken issues a request authenticated with a service credential.
func (h *harness) doAsServiceToken(plaintext, method, path string, raw []byte) *httptest.ResponseRecorder {
	h.t.Helper()
	var body *strings.Reader
	if raw != nil {
		body = strings.NewReader(string(raw))
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// newServiceToken mints a credential directly in the store and grants it the
// harness model unless grant is false.
func (h *harness) newServiceToken(t *testing.T, name string, grant bool) (*store.ServiceToken, string) {
	t.Helper()
	ctx := context.Background()
	tok, plaintext, err := h.store.CreateServiceToken(ctx, name, "", h.user.ID, time.Time{})
	if err != nil {
		t.Fatalf("create service token: %v", err)
	}
	if grant {
		if _, err := h.store.CreateGrant(ctx, h.model.ID, store.ModelKindModel, store.GranteeServiceToken, tok.ID); err != nil {
			t.Fatalf("grant service token: %v", err)
		}
	}
	h.server.InvalidateConfigCache()
	return tok, plaintext
}

// TestServiceTokenCanProxyAndIsMeteredSeparately is the end-to-end proof of the
// feature: an integration credential can run inference, its usage is recorded
// against the token rather than a user, and it never lands in a user report.
func TestServiceTokenCanProxyAndIsMeteredSeparately(t *testing.T) {
	h := newHarness(t)
	tok, plaintext := h.newServiceToken(t, "nightly-bot", true)

	rec := h.doAsServiceToken(plaintext, http.MethodPost, "/v1/chat/completions",
		[]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("service token proxy returned %d: %s", rec.Code, rec.Body.String())
	}

	events := h.waitForUsageEvents(1)
	event := events[0]
	if event.ServiceTokenID != tok.ID {
		t.Errorf("service_token_id = %q, want %q", event.ServiceTokenID, tok.ID)
	}
	// The structural guarantee: no user id, so people-oriented reports cannot
	// pick this event up no matter how they are written.
	if event.UserID != "" {
		t.Errorf("user_id = %q, want empty — service-token traffic belongs to no user", event.UserID)
	}
	if event.TokensOut == 0 {
		t.Error("service-token usage must still be fully metered")
	}

	// Org-wide totals include it; the top-users breakdown does not.
	ctx := context.Background()
	orgTotals, err := h.store.AggregateUsage(ctx, store.UsageScope{})
	if err != nil {
		t.Fatalf("org totals: %v", err)
	}
	if orgTotals.Requests != 1 {
		t.Errorf("org-wide requests = %d, want 1 (service tokens count toward the org pulse)", orgTotals.Requests)
	}
	byUser, err := h.store.BreakdownUsage(ctx, store.UsageScope{}, "user")
	if err != nil {
		t.Fatalf("user breakdown: %v", err)
	}
	if len(byUser) != 0 {
		t.Errorf("top-users breakdown = %+v, want empty — only service-token traffic exists", byUser)
	}
}

// TestServiceTokenIsProxyOnly locks the scope decision: an unattended
// credential embedded in a website must not be able to read dashboards or
// enumerate the organisation.
func TestServiceTokenIsProxyOnly(t *testing.T) {
	h := newHarness(t)
	_, plaintext := h.newServiceToken(t, "scoped-bot", true)

	for _, path := range []string{
		"/api/v1/me",
		"/api/v1/requests",
		"/api/v1/dashboard/personal",
		"/api/v1/admin/users",
		"/api/v1/admin/service-tokens",
		"/api/v1/tokens",
	} {
		rec := h.doAsServiceToken(plaintext, http.MethodGet, path, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with a service token returned %d, want 403", path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), CodeServiceTokenScope) {
			t.Errorf("GET %s should explain the scope limit, got %s", path, rec.Body.String())
		}
	}

	// The proxy surface, by contrast, works.
	if rec := h.doAsServiceToken(plaintext, http.MethodGet, "/v1/models", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models with a service token returned %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestServiceTokenRequiresItsOwnGrant proves an all_users grant does not carry
// over to integrations at the HTTP layer, not just in the store.
func TestServiceTokenRequiresItsOwnGrant(t *testing.T) {
	h := newHarness(t)
	// The harness already granted the model to all_users. This token gets no
	// grant of its own.
	_, plaintext := h.newServiceToken(t, "ungranted-bot", false)

	rec := h.doAsServiceToken(plaintext, http.MethodPost, "/v1/chat/completions",
		[]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted service token returned %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), CodeModelNotGranted) {
		t.Errorf("expected a model_not_granted error, got %s", rec.Body.String())
	}
}

// TestRevokedServiceTokenIsRefused covers immediate revocation through the
// admin endpoint, including the credential-cache flush.
func TestRevokedServiceTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	tok, plaintext := h.newServiceToken(t, "doomed-bot", true)
	session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Warm the credential cache with a successful call.
	if rec := h.doAsServiceToken(plaintext, http.MethodGet, "/v1/models", nil); rec.Code != http.StatusOK {
		t.Fatalf("pre-revocation call returned %d", rec.Code)
	}
	if rec := h.doAsSession(session, http.MethodDelete, "/api/v1/admin/service-tokens/"+tok.ID, nil); rec.Code != http.StatusOK {
		t.Fatalf("revoke returned %d: %s", rec.Code, rec.Body.String())
	}
	rec := h.doAsServiceToken(plaintext, http.MethodGet, "/v1/models", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token returned %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

// TestExpiredServiceTokenIsRefused proves expiry is enforced on the hot path
// even while a cache entry is live — expiry is a clock event with no write to
// invalidate against, so it must be re-checked on every use.
func TestExpiredServiceTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tok, plaintext, err := h.store.CreateServiceToken(ctx, "expiring-bot", "", h.user.ID, time.Now().UTC().Add(750*time.Millisecond))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := h.store.CreateGrant(ctx, h.model.ID, store.ModelKindModel, store.GranteeServiceToken, tok.ID); err != nil {
		t.Fatalf("grant: %v", err)
	}
	h.server.InvalidateConfigCache()

	// Warm the cache while still valid.
	if rec := h.doAsServiceToken(plaintext, http.MethodGet, "/v1/models", nil); rec.Code != http.StatusOK {
		t.Fatalf("pre-expiry call returned %d: %s", rec.Code, rec.Body.String())
	}
	time.Sleep(900 * time.Millisecond)
	rec := h.doAsServiceToken(plaintext, http.MethodGet, "/v1/models", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token returned %d, want 401 even on a warm cache: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), CodeServiceTokenExpired) {
		t.Errorf("expiry must be distinguishable from revocation, got %s", rec.Body.String())
	}
}

// TestServiceTokenAdminCRUD walks the admin surface: issue (value shown once),
// list, rename, and the usage detail page.
func TestServiceTokenAdminCRUD(t *testing.T) {
	h := newHarness(t)
	session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	rec := h.doAsSession(session, http.MethodPost, "/api/v1/admin/service-tokens", map[string]any{
		"name": "checkout-widget", "description": "the storefront assistant",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ServiceToken struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"service_token"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Value == "" || !strings.HasPrefix(created.Value, store.ServiceTokenPrefix) {
		t.Fatalf("create must return the plaintext once, got %q", created.Value)
	}
	if created.ServiceToken.Status != "active" {
		t.Errorf("status = %q, want active", created.ServiceToken.Status)
	}

	// A duplicate name is a 400 naming the field, not a 500.
	dup := h.doAsSession(session, http.MethodPost, "/api/v1/admin/service-tokens", map[string]any{"name": "checkout-widget"})
	if dup.Code != http.StatusBadRequest {
		t.Errorf("duplicate name returned %d, want 400: %s", dup.Code, dup.Body.String())
	}

	// Rename.
	if rec := h.doAsSession(session, http.MethodPatch, "/api/v1/admin/service-tokens/"+created.ServiceToken.ID, map[string]any{
		"name": "storefront-assistant",
	}); rec.Code != http.StatusOK {
		t.Fatalf("rename returned %d: %s", rec.Code, rec.Body.String())
	}

	// List reflects it.
	list := h.doAsSession(session, http.MethodGet, "/api/v1/admin/service-tokens", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list returned %d", list.Code)
	}
	if !strings.Contains(list.Body.String(), "storefront-assistant") {
		t.Errorf("renamed token missing from list: %s", list.Body.String())
	}

	// The usage detail page exists and is scoped to the token.
	usage := h.doAsSession(session, http.MethodGet, "/api/v1/admin/service-tokens/"+created.ServiceToken.ID+"/usage", nil)
	if usage.Code != http.StatusOK {
		t.Fatalf("usage detail returned %d: %s", usage.Code, usage.Body.String())
	}
	if !strings.Contains(usage.Body.String(), "totals") {
		t.Errorf("usage detail should carry totals: %s", usage.Body.String())
	}
}

// --- Managed models ----------------------------------------------------------

// TestManagedModelProxyRecordsUnderlyingModel is the end-to-end proof of the
// reporting rule: the caller addresses the alias, the upstream receives the
// real model, and the usage event reports the underlying model.
func TestManagedModelProxyRecordsUnderlyingModel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mm, err := h.store.CreateManagedModel(ctx, "current-best", "the good one", h.model.ID, h.user.ID)
	if err != nil {
		t.Fatalf("create managed model: %v", err)
	}
	if _, err := h.store.CreateGrant(ctx, mm.ID, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant alias: %v", err)
	}
	h.server.InvalidateConfigCache()

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "current-best", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("alias request returned %d: %s", rec.Code, rec.Body.String())
	}

	// The upstream must have been asked for the NATIVE model name — it has
	// never heard of "current-best".
	last := h.upstreamRequests[len(h.upstreamRequests)-1]
	if !strings.Contains(string(last.Body), `"model":"test-model"`) {
		t.Errorf("upstream received %s, want the native model name", last.Body)
	}

	events := h.waitForUsageEvents(1)
	event := events[0]
	if event.ModelName != h.model.PublicName() {
		t.Errorf("model_name = %q, want the UNDERLYING model %q", event.ModelName, h.model.PublicName())
	}
	if event.ModelID != h.model.ID {
		t.Errorf("model_id = %q, want the underlying model id", event.ModelID)
	}
	if event.RequestedModelName != "current-best" {
		t.Errorf("requested_model_name = %q, want the alias", event.RequestedModelName)
	}
}

// TestManagedModelSwapIsInvisibleToCallers is the feature's headline promise.
func TestManagedModelSwapIsInvisibleToCallers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	// A second real model to swap to.
	if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, "text-embedding-3-small", []string{"chat"}); err != nil {
		t.Fatalf("seed second model: %v", err)
	}
	models, err := h.store.ListModels(ctx, store.ModelFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var second *store.Model
	for _, m := range models {
		if m.ID != h.model.ID {
			second = m
		}
	}
	if second == nil {
		t.Fatal("expected a second model")
	}
	if err := h.store.SetModelStatus(ctx, second.ID, store.ModelEnabled); err != nil {
		t.Fatalf("enable second: %v", err)
	}

	mm, err := h.store.CreateManagedModel(ctx, "best-coder", "", h.model.ID, h.user.ID)
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}
	if _, err := h.store.CreateGrant(ctx, mm.ID, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	h.server.InvalidateConfigCache()

	body := map[string]any{"model": "best-coder", "messages": []map[string]string{{"role": "user", "content": "hi"}}}
	if rec := h.do(http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK {
		t.Fatalf("first call returned %d: %s", rec.Code, rec.Body.String())
	}

	// The admin repoints the alias through the API.
	if rec := h.doAsSession(session, http.MethodPatch, "/api/v1/admin/managed-models/"+mm.ID, map[string]any{
		"target_model_id": second.ID,
	}); rec.Code != http.StatusOK {
		t.Fatalf("repoint returned %d: %s", rec.Code, rec.Body.String())
	}

	// The caller's request is byte-identical and still succeeds — but now
	// reaches the other model.
	if rec := h.do(http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK {
		t.Fatalf("post-swap call returned %d: %s", rec.Code, rec.Body.String())
	}
	last := h.upstreamRequests[len(h.upstreamRequests)-1]
	if !strings.Contains(string(last.Body), `"model":"text-embedding-3-small"`) {
		t.Errorf("after the swap the upstream received %s, want the new target", last.Body)
	}

	events := h.waitForUsageEvents(2)
	// Newest first: the two calls must report DIFFERENT underlying models
	// while the caller sent the same alias both times.
	if events[0].ModelName == events[1].ModelName {
		t.Errorf("both events report %q; the swap should change the underlying model", events[0].ModelName)
	}
	for _, e := range events {
		if e.RequestedModelName != "best-coder" {
			t.Errorf("requested_model_name = %q, want the alias on both calls", e.RequestedModelName)
		}
	}
}

// TestManagedModelIsTransparentInCatalog locks the user's explicit ask: the
// model card tells the user what the alias currently resolves to.
func TestManagedModelIsTransparentInCatalog(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mm, err := h.store.CreateManagedModel(ctx, "current-best", "pick of the week", h.model.ID, h.user.ID)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := h.store.CreateGrant(ctx, mm.ID, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	h.server.InvalidateConfigCache()

	// /v1/models discloses the target.
	rec := h.do(http.MethodGet, "/v1/models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models returned %d", rec.Code)
	}
	var listing struct {
		Data []struct {
			ID    string `json:"id"`
			Janus struct {
				Managed      bool `json:"managed"`
				ManagedModel struct {
					ResolvesTo string `json:"resolves_to"`
					MayChange  bool   `json:"may_change"`
				} `json:"managed_model"`
			} `json:"janus"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, m := range listing.Data {
		if m.ID != "current-best" {
			continue
		}
		found = true
		if !m.Janus.Managed {
			t.Error("the alias must be flagged as managed")
		}
		if m.Janus.ManagedModel.ResolvesTo != h.model.PublicName() {
			t.Errorf("resolves_to = %q, want %q — we help, we do not hide",
				m.Janus.ManagedModel.ResolvesTo, h.model.PublicName())
		}
		if !m.Janus.ManagedModel.MayChange {
			t.Error("the card should say the target may change")
		}
	}
	if !found {
		t.Fatalf("managed model missing from /v1/models: %s", rec.Body.String())
	}

	// The web catalog carries it too, with the target resolved.
	web := h.do(http.MethodGet, "/api/v1/models", nil)
	if web.Code != http.StatusOK {
		t.Fatalf("/api/v1/models returned %d", web.Code)
	}
	if !strings.Contains(web.Body.String(), "managed_models") ||
		!strings.Contains(web.Body.String(), "target_public_name") {
		t.Errorf("web catalog should expose managed models and their targets: %s", web.Body.String())
	}
}

// TestManagedModelBrokenTargetGivesConfigError proves a caller with a correct
// configuration is told the fault is the gateway's, not theirs.
func TestManagedModelBrokenTargetGivesConfigError(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mm, err := h.store.CreateManagedModel(ctx, "will-break", "", h.model.ID, h.user.ID)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := h.store.CreateGrant(ctx, mm.ID, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := h.store.SetModelStatus(ctx, h.model.ID, store.ModelDisabled); err != nil {
		t.Fatalf("disable target: %v", err)
	}
	h.server.InvalidateConfigCache()

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "will-break", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("broken alias returned %d, want 503: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, CodeManagedModelBroken) {
		t.Errorf("expected the managed-model error code, got %s", body)
	}
	if !strings.Contains(body, "administrator") {
		t.Errorf("the message should point at an administrator, not blame the caller: %s", body)
	}
}

// TestDisablingModelWithDependentAliasIsRefused covers the blast-radius guard:
// an admin cannot silently break "current-best" for the whole organisation.
func TestDisablingModelWithDependentAliasIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if _, err := h.store.CreateManagedModel(ctx, "depends-on-it", "", h.model.ID, h.user.ID); err != nil {
		t.Fatalf("create alias: %v", err)
	}

	rec := h.doAsSession(session, http.MethodPatch, "/api/v1/admin/models/"+h.model.ID, map[string]any{
		"status": store.ModelDisabled,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("disabling a managed target returned %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "depends-on-it") {
		t.Errorf("the refusal must name the affected alias so the admin can act: %s", rec.Body.String())
	}

	// The explicit override goes through: the guard informs, it does not trap.
	forced := h.doAsSession(session, http.MethodPatch,
		"/api/v1/admin/models/"+h.model.ID+"?force=true", map[string]any{"status": store.ModelDisabled})
	if forced.Code != http.StatusOK {
		t.Fatalf("forced disable returned %d, want 200: %s", forced.Code, forced.Body.String())
	}
}

// TestDeleteUpstreamGuardsManagedModelDependents proves that deleting an
// upstream follows the same rule as disabling one of its models: the
// preflight lists the blast radius (models, aliases, grants), the delete is
// refused with 409 while an enabled alias still targets a hosted model, and
// ?force=true goes through — optionally purging the direct grants that would
// otherwise dangle on disabled models.
func TestDeleteUpstreamGuardsManagedModelDependents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	alias, err := h.store.CreateManagedModel(ctx, "depends-on-upstream", "", h.model.ID, h.user.ID)
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}
	upstreamID := h.model.UpstreamID

	// Preflight names everything the delete would touch.
	pre := h.doAsSession(session, http.MethodGet, "/api/v1/admin/upstreams/"+upstreamID+"/dependents", nil)
	if pre.Code != http.StatusOK {
		t.Fatalf("dependents returned %d: %s", pre.Code, pre.Body.String())
	}
	var deps upstreamDependents
	if err := json.Unmarshal(pre.Body.Bytes(), &deps); err != nil {
		t.Fatalf("decode dependents: %v", err)
	}
	if len(deps.Models) == 0 || len(deps.ManagedModels) != 1 || deps.ManagedModels[0].Name != "depends-on-upstream" {
		t.Fatalf("dependents = %+v, want the hosted models and the one alias", deps)
	}
	if deps.BlockingManagedModels != 1 {
		t.Fatalf("blocking_managed_models = %d, want 1 (the alias is enabled)", deps.BlockingManagedModels)
	}
	if deps.GrantCount != 1 {
		t.Fatalf("grant_count = %d, want the harness's all-users grant", deps.GrantCount)
	}

	// Plain delete is a conflict that names the alias.
	rec := h.doAsSession(session, http.MethodDelete, "/api/v1/admin/upstreams/"+upstreamID, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting an upstream with a dependent alias returned %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "depends-on-upstream") {
		t.Errorf("the refusal must name the affected alias: %s", rec.Body.String())
	}
	if up, err := h.store.UpstreamByID(ctx, upstreamID); err != nil || !up.Enabled {
		t.Fatalf("a refused delete must leave the upstream untouched (err=%v up=%+v)", err, up)
	}

	// Disabling the alias unblocks the delete — the two-step the message asks for.
	if err := h.store.SetManagedModelStatus(ctx, alias.ID, store.ManagedModelDisabled); err != nil {
		t.Fatalf("disable alias: %v", err)
	}
	pre = h.doAsSession(session, http.MethodGet, "/api/v1/admin/upstreams/"+upstreamID+"/dependents", nil)
	_ = json.Unmarshal(pre.Body.Bytes(), &deps)
	if deps.BlockingManagedModels != 0 || len(deps.ManagedModels) != 1 {
		t.Fatalf("a disabled alias must still be listed but not block: %+v", deps)
	}
	// Re-enable so we exercise the force path instead.
	if err := h.store.SetManagedModelStatus(ctx, alias.ID, store.ManagedModelEnabled); err != nil {
		t.Fatalf("re-enable alias: %v", err)
	}

	forced := h.doAsSession(session, http.MethodDelete, "/api/v1/admin/upstreams/"+upstreamID+"?force=true&purge_grants=true", nil)
	if forced.Code != http.StatusOK {
		t.Fatalf("forced delete returned %d, want 200: %s", forced.Code, forced.Body.String())
	}
	var result struct {
		ModelsDisabled      int      `json:"models_disabled"`
		ManagedModelsBroken []string `json:"managed_models_broken"`
		GrantsRemoved       int      `json:"grants_removed"`
	}
	if err := json.Unmarshal(forced.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode delete result: %v", err)
	}
	if result.GrantsRemoved != 1 || len(result.ManagedModelsBroken) != 1 || result.ModelsDisabled == 0 {
		t.Fatalf("delete result = %+v, want 1 grant removed, 1 alias broken, models disabled", result)
	}
	grants, err := h.store.ListGrants(ctx, h.model.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("purge_grants must remove the direct grants on the upstream's models, %d left", len(grants))
	}
	m, err := h.store.ModelByID(ctx, h.model.ID)
	if err != nil || m.Status != store.ModelDisabled {
		t.Fatalf("hosted models must be disabled by the delete (err=%v status=%q)", err, m.Status)
	}
	entries, _, err := h.store.ListAudit(ctx, store.AuditFilter{Action: "upstream_deleted", Limit: 5})
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one upstream_deleted audit entry, got %d (err %v)", len(entries), err)
	}
	if !strings.Contains(entries[0].OldValue, "depends-on-upstream") || !strings.Contains(entries[0].OldValue, `"forced":true`) {
		t.Fatalf("audit must record the broken alias and that the delete was forced: %s", entries[0].OldValue)
	}
}

// TestManagedModelNameCannotCollideViaAPI locks the shared namespace at the
// HTTP layer in both directions.
func TestManagedModelNameCannotCollideViaAPI(t *testing.T) {
	h := newHarness(t)
	session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	// An alias may not take the enabled model's name.
	rec := h.doAsSession(session, http.MethodPost, "/api/v1/admin/managed-models", map[string]any{
		"name": h.model.Name, "target_model_id": h.model.ID,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("alias shadowing a real model returned %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// Create a legitimate alias, then try to rename the model onto it.
	if rec := h.doAsSession(session, http.MethodPost, "/api/v1/admin/managed-models", map[string]any{
		"name": "taken-name", "target_model_id": h.model.ID,
	}); rec.Code != http.StatusCreated {
		t.Fatalf("create alias returned %d: %s", rec.Code, rec.Body.String())
	}
	rename := h.doAsSession(session, http.MethodPatch, "/api/v1/admin/models/"+h.model.ID, map[string]any{
		"display_name": "taken-name",
	})
	if rename.Code != http.StatusBadRequest {
		t.Fatalf("renaming a model onto an alias name returned %d, want 400: %s", rename.Code, rename.Body.String())
	}
}

// TestManagedModelRequiresItsOwnGrant proves alias access is an independent
// control from access to the underlying model.
func TestManagedModelRequiresItsOwnGrant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// The harness grants the underlying model to all_users; the alias gets no
	// grant of its own.
	if _, err := h.store.CreateManagedModel(ctx, "ungranted-alias", "", h.model.ID, h.user.ID); err != nil {
		t.Fatalf("create: %v", err)
	}
	h.server.InvalidateConfigCache()

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "ungranted-alias", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted alias returned %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

// TestServiceTokenQuotaIsEnforced proves the runaway-integration control works
// end to end: a service-token quota stops the credential at its limit.
func TestServiceTokenQuotaIsEnforced(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tok, plaintext := h.newServiceToken(t, "capped-bot", true)
	if err := h.store.CreateQuota(ctx, &store.Quota{
		SubjectType: "service_token", SubjectID: tok.ID, Metric: store.MetricRequests,
		Limit: 1, Window: store.WindowDaily, BreachBehavior: store.BreachLetFinish,
	}); err != nil {
		t.Fatalf("create quota: %v", err)
	}
	h.server.InvalidateConfigCache()

	body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec := h.doAsServiceToken(plaintext, http.MethodPost, "/v1/chat/completions", body); rec.Code != http.StatusOK {
		t.Fatalf("first call returned %d: %s", rec.Code, rec.Body.String())
	}
	// Quota accrual happens in the post-response background writer, AFTER the
	// usage insert. Waiting only for the event would race the ledger update,
	// so drain the writers the way shutdown does.
	h.waitForUsageEvents(1)
	drainCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := h.server.Drain(drainCtx); err != nil {
		t.Fatalf("drain background writers: %v", err)
	}

	rec := h.doAsServiceToken(plaintext, http.MethodPost, "/v1/chat/completions", body)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second call returned %d, want 429 (the quota should bound the integration): %s", rec.Code, rec.Body.String())
	}
}

// TestServiceTokenListCarriesUsageAggregate covers the admin table contract:
// a row has to answer "is this integration expensive, busy, or failing"
// without the admin opening anything, because a service token has no human
// owner to ask.
func TestServiceTokenListCarriesUsageAggregate(t *testing.T) {
	h := newHarness(t)
	session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, plaintext := h.newServiceToken(t, "busy-bot", true)
	rec := h.doAsServiceToken(plaintext, http.MethodPost, "/v1/chat/completions",
		[]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	h.waitForUsageEvents(1)
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDrain()
	if err := h.server.Drain(drainCtx); err != nil {
		t.Fatalf("drain background writers: %v", err)
	}

	list := h.doAsSession(session, http.MethodGet, "/api/v1/admin/service-tokens", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list returned %d: %s", list.Code, list.Body.String())
	}
	var payload struct {
		ServiceTokens []map[string]any `json:"service_tokens"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.ServiceTokens) != 1 {
		t.Fatalf("want 1 row, got %d", len(payload.ServiceTokens))
	}
	row := payload.ServiceTokens[0]
	for _, key := range []string{
		"spend_30d_usd", "requests_30d", "tokens_in_30d", "tokens_out_30d",
		"errors_30d", "top_model_30d", "grant_count",
	} {
		if _, ok := row[key]; !ok {
			t.Errorf("row is missing %q — the admin table cannot render without it", key)
		}
	}
	if got, _ := row["requests_30d"].(float64); got != 1 {
		t.Errorf("requests_30d = %v, want 1", row["requests_30d"])
	}
	// The row must name the underlying model that actually ran.
	if got, _ := row["top_model_30d"].(string); got != "test-model" {
		t.Errorf("top_model_30d = %q, want test-model", got)
	}
}

// TestServiceTokenListSortIsHonoured checks the table's sort contract rather
// than trusting the UI to re-sort a page of rows client-side.
func TestServiceTokenListSortIsHonoured(t *testing.T) {
	h := newHarness(t)
	session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	for _, name := range []string{"zebra", "alpha", "middle"} {
		h.newServiceToken(t, name, false)
	}

	names := func(sort string) []string {
		rec := h.doAsSession(session, http.MethodGet, "/api/v1/admin/service-tokens?sort="+sort, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %s returned %d", sort, rec.Code)
		}
		var payload struct {
			ServiceTokens []struct {
				Name string `json:"name"`
			} `json:"service_tokens"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := make([]string, 0, len(payload.ServiceTokens))
		for _, tok := range payload.ServiceTokens {
			out = append(out, tok.Name)
		}
		return out
	}

	asc := names("name_asc")
	if len(asc) != 3 || asc[0] != "alpha" || asc[2] != "zebra" {
		t.Errorf("name_asc = %v, want alpha…zebra", asc)
	}
	desc := names("name_desc")
	if len(desc) != 3 || desc[0] != "zebra" || desc[2] != "alpha" {
		t.Errorf("name_desc = %v, want zebra…alpha", desc)
	}
}

// TestManagedModelListReportsAliasUsage is the one place alias-keyed usage is
// correct: model reporting must show the underlying model, but an admin
// judging whether an alias earns its keep needs traffic for the alias itself.
func TestManagedModelListReportsAliasUsage(t *testing.T) {
	h := newHarness(t)
	session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	ctx := context.Background()
	alias, err := h.store.CreateManagedModel(ctx, "current-best", "", h.model.ID, h.user.ID)
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}
	if _, err := h.store.CreateGrant(ctx, alias.ID, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	h.server.InvalidateConfigCache()

	rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "current-best",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("alias proxy returned %d: %s", rec.Code, rec.Body.String())
	}
	h.waitForUsageEvents(1)
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDrain()
	if err := h.server.Drain(drainCtx); err != nil {
		t.Fatalf("drain background writers: %v", err)
	}

	list := h.doAsSession(session, http.MethodGet, "/api/v1/admin/managed-models", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list returned %d: %s", list.Code, list.Body.String())
	}
	var payload struct {
		ManagedModels []map[string]any `json:"managed_models"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.ManagedModels) != 1 {
		t.Fatalf("want 1 alias row, got %d", len(payload.ManagedModels))
	}
	row := payload.ManagedModels[0]
	if got, _ := row["requests_30d"].(float64); got != 1 {
		t.Errorf("requests_30d = %v, want 1 — alias traffic must be attributed to the alias here", row["requests_30d"])
	}
	if got, _ := row["distinct_principals_30d"].(float64); got != 1 {
		t.Errorf("distinct_principals_30d = %v, want 1", row["distinct_principals_30d"])
	}
	// The row still reports what it actually resolves to.
	if got, _ := row["target_model_name"].(string); got != h.model.Name {
		t.Errorf("target_model_name = %q, want %q", got, h.model.Name)
	}

	// And the model report is unchanged: the underlying model, not the alias.
	byModel, err := h.store.BreakdownUsage(context.Background(), store.UsageScope{}, "model")
	if err != nil {
		t.Fatalf("model breakdown: %v", err)
	}
	if len(byModel) != 1 || byModel[0].Key != h.model.Name {
		t.Errorf("model breakdown = %+v, want the underlying model %q", byModel, h.model.Name)
	}
}

// TestClientAppIdentifiesTheAgentHarness locks the attribution contract: the
// User-Agent only ever names the SDK ("OpenAI/Python 2.24.0"), so without an
// explicit header every agent harness looks identical in the request log.
func TestClientAppIdentifiesTheAgentHarness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			"explicit Janus header wins",
			map[string]string{"X-Janus-Agent": "hermes", "X-Title": "something-else"},
			"hermes",
		},
		{
			"OpenRouter current spelling",
			map[string]string{"X-OpenRouter-Title": "opencode"},
			"opencode",
		},
		{
			"OpenRouter legacy spelling",
			map[string]string{"X-Title": "opencode"},
			"opencode",
		},
		{
			"HTTP-Referer as a last resort",
			map[string]string{"HTTP-Referer": "https://my-agent.example"},
			"https://my-agent.example",
		},
		{
			// The common case: a bare SDK sends no attribution at all, and an
			// honest blank beats inventing an identity from the user agent.
			"no attribution header leaves it empty",
			map[string]string{"User-Agent": "OpenAI/Python 2.24.0"},
			"",
		},
		{
			"whitespace-only header is treated as absent",
			map[string]string{"X-Title": "   "},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if got := clientAppName(req); got != tc.want {
				t.Errorf("clientAppName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientAppIsRecordedOnTheUsageEvent proves the label survives the proxy
// path onto the stored event, which is what the request log reads.
func TestClientAppIsRecordedOnTheUsageEvent(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OpenAI/Python 2.24.0")
	req.Header.Set("X-Janus-Agent", "hermes")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
	}

	events := h.waitForUsageEvents(1)
	if events[0].ClientApp != "hermes" {
		t.Errorf("client_app = %q, want hermes", events[0].ClientApp)
	}
	// The SDK user agent is still captured separately — the two answer
	// different questions and neither replaces the other.
	if events[0].UserAgent != "OpenAI/Python 2.24.0" {
		t.Errorf("client_user_agent = %q, want the SDK string", events[0].UserAgent)
	}
}

// TestLeadCanManageOwnTeamMembers locks the roster-management boundary: a lead
// manages their OWN team, is refused on someone else's, and — critically — is
// NOT gated on the quota-delegation flag. Deciding who is on the team is a
// different decision from controlling its budget, and conflating them would
// force an admin to hand over spending authority just to let a lead add a
// teammate.
func TestLeadCanManageOwnTeamMembers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	member, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-member", "member@example.com", "Member", false, false)
	if err != nil {
		t.Fatalf("seed member: %v", err)
	}
	other, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-other", "other@example.com", "Other", false, false)
	if err != nil {
		t.Fatalf("seed other: %v", err)
	}

	// h.user leads this team; quota delegation is deliberately OFF.
	team, err := h.store.CreateTeam(ctx, "Platform", h.user.ID)
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	// A second team led by somebody else.
	foreign, err := h.store.CreateTeam(ctx, "Other Team", other.ID)
	if err != nil {
		t.Fatalf("create foreign team: %v", err)
	}

	session, err := h.server.Sessions.Create(ctx, h.user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	// The lead sets their roster with delegation off, retaining the required leader.
	rec := h.doAsSession(session, http.MethodPut, "/api/v1/lead/teams/"+team.ID+"/members",
		map[string]any{"member_ids": []string{h.user.ID, member.ID}})
	if rec.Code != http.StatusOK {
		t.Fatalf("lead roster edit returned %d, want 200 (must not require quota delegation): %s", rec.Code, rec.Body.String())
	}
	ids, err := h.store.TeamMemberIDs(ctx, team.ID)
	if err != nil {
		t.Fatalf("read members: %v", err)
	}
	if len(ids) != 2 || !slices.Contains(ids, member.ID) || !slices.Contains(ids, h.user.ID) {
		t.Errorf("members = %v, want leader %s and member %s", ids, h.user.ID, member.ID)
	}

	// Reading back through the API returns the resolved roster.
	rec = h.doAsSession(session, http.MethodGet, "/api/v1/lead/teams/"+team.ID+"/members", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("roster read returned %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "member@example.com") {
		t.Errorf("roster should name the member: %s", rec.Body.String())
	}

	// An admin may manage ANY team — that is the admin surface, and the
	// harness user is a bootstrap admin.
	rec = h.doAsSession(session, http.MethodPut, "/api/v1/lead/teams/"+foreign.ID+"/members",
		map[string]any{"member_ids": []string{other.ID, h.user.ID}})
	if rec.Code != http.StatusOK {
		t.Errorf("an admin should manage any team, got %d: %s", rec.Code, rec.Body.String())
	}

	// A plain user who leads NOTHING is refused on someone else's team. This
	// is the boundary that matters: leadership is per-team, not a global role.
	plainSession, err := h.server.Sessions.Create(ctx, member.ID)
	if err != nil {
		t.Fatalf("member session: %v", err)
	}
	rec = h.doAsSession(plainSession, http.MethodPut, "/api/v1/lead/teams/"+foreign.ID+"/members",
		map[string]any{"member_ids": []string{member.ID}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("a non-lead editing another team returned %d, want 403: %s", rec.Code, rec.Body.String())
	}
	// ...and also refused on a team they merely BELONG to but do not lead.
	rec = h.doAsSession(plainSession, http.MethodPut, "/api/v1/lead/teams/"+team.ID+"/members",
		map[string]any{"member_ids": []string{}})
	if rec.Code != http.StatusForbidden {
		t.Errorf("a member (not lead) editing their own team returned %d, want 403: %s", rec.Code, rec.Body.String())
	}

	// A non-existent member id is a field-specific 400, not a dangling row.
	rec = h.doAsSession(session, http.MethodPut, "/api/v1/lead/teams/"+team.ID+"/members",
		map[string]any{"member_ids": []string{"no-such-user"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown member id returned %d, want 400: %s", rec.Code, rec.Body.String())
	}
}
