package httpapi

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// TestTokenUsageDashboardIsTokenScoped locks the documented contract
// for the per-token dashboard (regression: totals were computed by summing the
// 25 most recent events — so the Requests/Spend tiles could never exceed 25 —
// and per_model used a user-wide scope, listing models the token never called).
func TestTokenUsageDashboardIsTokenScoped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tokens, err := h.store.ListTokens(ctx, h.user.ID)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("list tokens: %v (%d rows)", err, len(tokens))
	}
	tokenA := tokens[0]
	tokenB, _, err := h.store.CreateToken(ctx, h.user.ID, "other token")
	if err != nil {
		t.Fatalf("create second token: %v", err)
	}
	// Model breakdowns only surface catalogued models, so the names used in
	// the seeded events must exist in the catalog.
	for _, name := range []string{"model-a", "model-b"} {
		if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, name, []string{"chat"}); err != nil {
			t.Fatalf("seed catalog model %q: %v", name, err)
		}
	}

	now := time.Now().UTC()
	// 30 events on token A (more than the 25-row recent page) and 5 events on
	// token B under a different model, all inside the default day range.
	for i := 0; i < 30; i++ {
		if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
			CreatedAt: now.Add(-time.Duration(i) * time.Minute), UserID: h.user.ID, TokenID: tokenA.ID,
			ModelName: "model-a", Modality: "chat", TokensIn: 10, TokensOut: 5, CostNano: 100, HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("insert token-A event: %v", err)
		}
	}
	for i := 0; i < 5; i++ {
		if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
			CreatedAt: now.Add(-time.Duration(i) * time.Minute), UserID: h.user.ID, TokenID: tokenB.ID,
			ModelName: "model-b", Modality: "chat", TokensIn: 1000, TokensOut: 500, CostNano: 9999, HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("insert token-B event: %v", err)
		}
	}

	rec := h.do(http.MethodGet, "/api/v1/dashboard/tokens/"+tokenA.ID+"?range=day", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("token usage returned %d: %s", rec.Code, rec.Body.String())
	}
	payload := decodeBody(t, rec)

	totals := payload["totals"].(map[string]any)
	if got := totals["request_count"].(float64); got != 30 {
		t.Errorf("totals.request_count = %v, want 30 (all events in range, not the 25-row recent page)", got)
	}
	if got := totals["tokens_in"].(float64); got != 300 {
		t.Errorf("totals.tokens_in = %v, want 300 (token A only)", got)
	}
	if got := totals["cost_nanousd"].(float64); got != 3000 {
		t.Errorf("totals.cost_nanousd = %v, want 3000 (token A only)", got)
	}

	perModel := payload["per_model"].([]any)
	if len(perModel) != 1 {
		t.Fatalf("per_model has %d rows, want 1 (token-scoped, not user-scoped): %v", len(perModel), perModel)
	}
	row := perModel[0].(map[string]any)
	if row["key"] != "model-a" {
		t.Errorf("per_model row = %v, want model-a (model-b belongs to another token)", row["key"])
	}

	// The recent-requests page stays a page.
	if got := len(payload["recent_requests"].([]any)); got != 25 {
		t.Errorf("recent_requests has %d rows, want the 25-row page", got)
	}
}

// TestPersonalDashboardBreakdowns covers the documented personal dashboard
// contract: totals plus per-model, per-token and modality breakdowns scoped to
// the authenticated user only.
func TestPersonalDashboardBreakdowns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tokens, err := h.store.ListTokens(ctx, h.user.ID)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("list tokens: %v (%d rows)", err, len(tokens))
	}
	// Model breakdowns only surface catalogued models, so the names used in
	// the seeded events must exist in the catalog.
	for _, name := range []string{"gpt-4o", "text-embedding-3"} {
		if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, name, []string{"chat"}); err != nil {
			t.Fatalf("seed catalog model %q: %v", name, err)
		}
	}
	now := time.Now().UTC()
	insert := func(userID, tokenID, model, modality string, tokensIn, costNano int64) {
		t.Helper()
		if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
			CreatedAt: now.Add(-time.Minute), UserID: userID, TokenID: tokenID,
			ModelName: model, Modality: modality, TokensIn: tokensIn, CostNano: costNano, HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("insert usage event: %v", err)
		}
	}
	insert(h.user.ID, tokens[0].ID, "gpt-4o", "chat", 100, 1000)
	insert(h.user.ID, tokens[0].ID, "text-embedding-3", "embedding", 50, 10)
	insert("someone-else", "tok-x", "claude", "chat", 9999, 99999) // must not leak in

	rec := h.do(http.MethodGet, "/api/v1/dashboard/personal?range=day", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("personal dashboard returned %d: %s", rec.Code, rec.Body.String())
	}
	payload := decodeBody(t, rec)

	totals := payload["totals"].(map[string]any)
	if got := totals["request_count"].(float64); got != 2 {
		t.Errorf("totals.request_count = %v, want 2 (other users' events excluded)", got)
	}
	if got := totals["tokens_in"].(float64); got != 150 {
		t.Errorf("totals.tokens_in = %v, want 150", got)
	}

	if got := len(payload["per_model"].([]any)); got != 2 {
		t.Errorf("per_model rows = %d, want 2", got)
	}
	if got := len(payload["per_modality"].([]any)); got != 2 {
		t.Errorf("per_modality rows = %d, want 2", got)
	}
	perToken := payload["per_token"].([]any)
	if len(perToken) != 1 {
		t.Fatalf("per_token rows = %d, want 1", len(perToken))
	}
	if key := perToken[0].(map[string]any)["key"]; key != tokens[0].ID {
		t.Errorf("per_token key = %v, want %s", key, tokens[0].ID)
	}
}

// TestPersonalDashboardWireShapeInLocalOnly locks the wire-compatibility
// decision behind local-only mode (JANUS_LOCAL_ONLY): dashboard payloads keep
// their cost fields PRESENT (the SPA hides cost surfaces via the /api/v1/me
// flag, not via field absence), token/request metrics are untouched, and
// historical cost values recorded before the mode was enabled are neither
// rewritten nor suppressed.
func TestPersonalDashboardWireShapeInLocalOnly(t *testing.T) {
	h := newHarness(t)
	h.server.Config.LocalOnly = true
	ctx := context.Background()

	tokens, err := h.store.ListTokens(ctx, h.user.ID)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("list tokens: %v (%d rows)", err, len(tokens))
	}
	// A historical event with a nonzero cost, recorded before the mode was
	// switched on. It must survive verbatim — no backfill, no suppression.
	if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
		CreatedAt: time.Now().UTC().Add(-time.Minute), UserID: h.user.ID, TokenID: tokens[0].ID,
		ModelName: "gpt-4o", Modality: "chat", TokensIn: 100, TokensOut: 40, CostNano: 5000, HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("insert usage event: %v", err)
	}

	rec := h.do(http.MethodGet, "/api/v1/dashboard/personal?range=day", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("personal dashboard returned %d: %s", rec.Code, rec.Body.String())
	}
	totals := decodeBody(t, rec)["totals"].(map[string]any)

	if got := totals["tokens_in"].(float64); got != 100 {
		t.Errorf("totals.tokens_in = %v, want 100 (token metrics unaffected by local-only)", got)
	}
	if got := totals["request_count"].(float64); got != 1 {
		t.Errorf("totals.request_count = %v, want 1", got)
	}
	cost, present := totals["cost_nanousd"]
	if !present {
		t.Fatal("totals.cost_nanousd must stay present in local-only mode (wire compatibility)")
	}
	if cost.(float64) != 5000 {
		t.Errorf("totals.cost_nanousd = %v, want 5000 (historical values untouched)", cost)
	}
}

// doAsToken sends a request authenticated with the given bearer token, so a
// test can act as a user other than the harness's bootstrapped admin.
func (h *harness) doAsToken(token, method, path string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// TestAdminRequestLogCrossUserScope locks the admin log-viewer contract on
// GET /api/v1/requests: an admin using scope=all sees every user's events with
// the owning user's identity attached, user_id narrows to one user even when
// scope=all is also present, and a non-admin sending the same overrides still
// receives only their own events (server-enforced, not UI-hidden).
func TestAdminRequestLogCrossUserScope(t *testing.T) {
	h := newHarness(t) // h.user is bootstrapped as admin
	ctx := context.Background()

	other, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-other", "other@example.com", "Other Person", false, false)
	if err != nil {
		t.Fatalf("create second user: %v", err)
	}
	_, otherToken, err := h.store.CreateToken(ctx, other.ID, "other token")
	if err != nil {
		t.Fatalf("create other token: %v", err)
	}
	now := time.Now().UTC()
	insert := func(userID, model string) {
		t.Helper()
		if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
			CreatedAt: now.Add(-time.Minute), UserID: userID, ModelName: model,
			Modality: "chat", TokensIn: 10, TokensOut: 5, CostNano: 100, HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
	insert(h.user.ID, "model-mine")
	insert(h.user.ID, "model-mine")
	insert(other.ID, "model-theirs")

	// Admin + scope=all: all three events, each carrying its owner identity.
	rec := h.do(http.MethodGet, "/api/v1/requests?scope=all&range=day", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin scope=all returned %d: %s", rec.Code, rec.Body.String())
	}
	payload := decodeBody(t, rec)
	if got := payload["total_count"].(float64); got != 3 {
		t.Errorf("admin scope=all total_count = %v, want 3", got)
	}
	foreign := 0
	for _, raw := range payload["requests"].([]any) {
		event := raw.(map[string]any)
		if event["user_email"] == nil || event["user_email"] == "" {
			t.Errorf("admin scope=all event lacks user_email: %v", event["id"])
		}
		if event["user_id"] == other.ID {
			foreign++
			if event["user_email"] != "other@example.com" || event["user_name"] != "Other Person" {
				t.Errorf("foreign event identity = %v/%v, want other@example.com/Other Person",
					event["user_email"], event["user_name"])
			}
		}
	}
	if foreign != 1 {
		t.Errorf("admin scope=all returned %d foreign events, want 1", foreign)
	}

	// user_id narrows even alongside scope=all (the UI keeps both in the URL).
	rec = h.do(http.MethodGet, "/api/v1/requests?scope=all&user_id="+other.ID+"&range=day", nil)
	payload = decodeBody(t, rec)
	if got := payload["total_count"].(float64); got != 1 {
		t.Errorf("admin user_id filter total_count = %v, want 1", got)
	}

	// A non-admin gets only their own events regardless of the overrides.
	for _, path := range []string{
		"/api/v1/requests?scope=all&range=day",
		"/api/v1/requests?user_id=" + h.user.ID + "&range=day",
	} {
		rec = h.doAsToken(otherToken, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("non-admin %s returned %d: %s", path, rec.Code, rec.Body.String())
		}
		payload = decodeBody(t, rec)
		if got := payload["total_count"].(float64); got != 1 {
			t.Errorf("non-admin %s total_count = %v, want 1 (own events only)", path, got)
		}
		for _, raw := range payload["requests"].([]any) {
			event := raw.(map[string]any)
			if event["user_id"] != other.ID {
				t.Errorf("non-admin %s leaked event owned by %v", path, event["user_id"])
			}
			if _, present := event["user_email"]; present {
				t.Errorf("non-admin %s response carries user_email", path)
			}
		}
	}
}

// TestExportRequestsAdminScopeIncludesUserIdentity locks the admin CSV export:
// under scope=all the file gains user identity columns and spans users, while
// the user-scoped export keeps its historical header untouched.
func TestExportRequestsAdminScopeIncludesUserIdentity(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	other, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-other", "other@example.com", "Other Person", false, false)
	if err != nil {
		t.Fatalf("create second user: %v", err)
	}
	now := time.Now().UTC()
	for _, userID := range []string{h.user.ID, other.ID} {
		if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
			CreatedAt: now.Add(-time.Minute), UserID: userID, ModelName: "model-a",
			Modality: "chat", TokensIn: 1, HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}

	rec := h.do(http.MethodGet, "/api/v1/requests.csv?scope=all&range=day", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin export returned %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	// The admin cross-user export carries principal identity columns:
	// user_email/user_name for human traffic and service_token for
	// integration traffic, so every row in a scope=all export says who ran it.
	if want := "timestamp,user_email,user_name,service_token,model"; !strings.HasPrefix(lines[0], want) {
		t.Errorf("admin export header = %q, want prefix %q", lines[0], want)
	}
	if !strings.Contains(body, "other@example.com") || !strings.Contains(body, "tester@example.com") {
		t.Errorf("admin export does not span users:\n%s", body)
	}

	// The user-scoped export is unchanged: no identity columns, own rows only.
	rec = h.do(http.MethodGet, "/api/v1/requests.csv?range=day", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("user export returned %d: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	lines = strings.Split(strings.TrimSpace(body), "\n")
	if strings.Contains(lines[0], "user_email") {
		t.Errorf("user-scoped export header gained identity columns: %q", lines[0])
	}
	if strings.Contains(body, "other@example.com") {
		t.Errorf("user-scoped export leaked another user's row:\n%s", body)
	}
}

// TestRequestsTokenSizeFiltersAndSortViaHTTP proves the token-size threshold
// parameters and token sort keys are honoured end to end by both the JSON
// request log and the CSV export — the same filter the UI builds for the
// list must narrow the download, or the export silently lies about the view.
func TestRequestsTokenSizeFiltersAndSortViaHTTP(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i, pair := range [][2]int64{{500, 50}, {1000, 20_000}, {15_000, 900}, {600_000, 120_000}} {
		if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
			CreatedAt: now.Add(-time.Duration(i+1) * time.Minute), UserID: h.user.ID, ModelName: "model-a",
			Modality: "chat", TokensIn: pair[0], TokensOut: pair[1], HTTPStatus: 200,
		}); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}

	// JSON list: strict greater-than on tokens_in excludes the 1,000 row.
	rec := h.do(http.MethodGet, "/api/v1/requests?range=day&tokens_in_gt=1000", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list returned %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Requests []*store.UsageEvent `json:"requests"`
		Total    int                 `json:"total_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Fatalf("tokens_in_gt=1000 total = %d, want 2", page.Total)
	}
	for _, e := range page.Requests {
		if e.TokensIn <= 1000 {
			t.Fatalf("tokens_in_gt=1000 returned tokens_in=%d", e.TokensIn)
		}
	}

	// Both directions combine, and sort=tokens_out_asc orders the survivors.
	rec = h.do(http.MethodGet, "/api/v1/requests?range=day&tokens_in_lt=100000&tokens_out_gt=100&sort=tokens_out_asc", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || page.Requests[0].TokensOut != 900 || page.Requests[1].TokensOut != 20_000 {
		t.Fatalf("combined filter + tokens_out_asc = total %d, outs %v", page.Total,
			func() []int64 {
				var outs []int64
				for _, e := range page.Requests {
					outs = append(outs, e.TokensOut)
				}
				return outs
			}())
	}

	// CSV export: the same parameters narrow the file, and the sort key
	// orders its rows. Header + 2 data rows, largest tokens_in first.
	rec = h.do(http.MethodGet, "/api/v1/requests.csv?range=day&tokens_in_gt=1000&sort=tokens_in", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("csv export returned %d: %s", rec.Code, rec.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("csv export has %d lines, want header + 2 rows:\n%s", len(lines), rec.Body.String())
	}
	if !strings.Contains(lines[0], "tokens_in") || !strings.Contains(lines[0], "tokens_out") {
		t.Fatalf("csv header must carry separate tokens_in / tokens_out columns: %q", lines[0])
	}
	cols := strings.Split(lines[0], ",")
	idx := -1
	for i, c := range cols {
		if c == "tokens_in" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("tokens_in column not found in %q", lines[0])
	}
	first, second := strings.Split(lines[1], ",")[idx], strings.Split(lines[2], ",")[idx]
	if first != "600000" || second != "15000" {
		t.Fatalf("csv rows sorted by tokens_in desc = %s, %s; want 600000, 15000", first, second)
	}
	if strings.Contains(rec.Body.String(), ",1000,") || strings.Contains(rec.Body.String(), ",500,") {
		t.Fatalf("csv export must not include rows excluded by tokens_in_gt=1000:\n%s", rec.Body.String())
	}

	// Smaller-than on tokens_out in the CSV path, independent of tokens_in.
	rec = h.do(http.MethodGet, "/api/v1/requests.csv?range=day&tokens_out_lt=1000", nil)
	lines = strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("tokens_out_lt=1000 csv has %d lines, want header + 2 rows:\n%s", len(lines), rec.Body.String())
	}
}

// TestExportRequestsSignalsTruncation locks the documented export contract: when
// more rows match than the 5000-row cap, the CSV must not look complete — the
// response carries X-Janus-Truncated/X-Janus-Total-Rows headers and the file
// ends with an explicit TRUNCATED marker row.
func TestExportRequestsSignalsTruncation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	insertMany := func(n int) {
		t.Helper()
		tx, err := h.store.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		now := time.Now().UTC()
		for i := 0; i < n; i++ {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO usage_event (id, created_at, user_id, model_name, modality, tokens_in, http_status)
				 VALUES (?,?,?,?,?,?,?)`,
				fmt.Sprintf("evt-%06d", i), store.FormatTime(now.Add(-time.Duration(i)*time.Second)),
				h.user.ID, "model-a", "chat", 1, 200); err != nil {
				t.Fatalf("insert event %d: %v", i, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	insertMany(5003)

	rec := h.do(http.MethodGet, "/api/v1/requests.csv?range=day", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export returned %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Janus-Truncated"); got != "true" {
		t.Errorf("X-Janus-Truncated = %q, want true", got)
	}
	if got := rec.Header().Get("X-Janus-Total-Rows"); got != "5003" {
		t.Errorf("X-Janus-Total-Rows = %q, want 5003", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "# TRUNCATED: 5000 of 5003") {
		tail := body
		if len(tail) > 200 {
			tail = tail[len(tail)-200:]
		}
		t.Errorf("CSV lacks the truncation marker row; tail: %q", tail)
	}
	// 1 header + 5000 data rows + 1 marker row.
	if got := strings.Count(body, "\n"); got != 5002 {
		t.Errorf("CSV has %d lines, want 5002", got)
	}
}

// TestGlobalDashboardLeaderboardsRankByTokensOut locks the leaderboard metric
// contract: top_users and top_teams on the global dashboard rank by output
// token volume (tokens_out DESC), not by spend.
func TestGlobalDashboardLeaderboardsRankByTokensOut(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.store.SetFeatureFlag(ctx, "leaderboards_enabled", true); err != nil {
		t.Fatalf("enable leaderboards: %v", err)
	}

	now := time.Now().UTC()
	seed := func(userID string, n int, tokensOut, costNano int64) {
		t.Helper()
		for i := 0; i < n; i++ {
			if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
				CreatedAt: now.Add(-time.Duration(i) * time.Minute), UserID: userID, TokenID: "tok-" + userID,
				ModelName: "gpt-4o", Modality: "chat", TokensIn: 10, TokensOut: tokensOut, CostNano: costNano, HTTPStatus: 200,
			}); err != nil {
				t.Fatalf("insert usage event: %v", err)
			}
		}
	}
	// u-volume emits the most tokens; u-spend costs the most. Token volume
	// must win the ranking.
	seed("u-volume", 2, 9000, 10)
	seed("u-spend", 3, 100, 500000)

	rec := h.do(http.MethodGet, "/api/v1/dashboard/global", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("global dashboard returned %d: %s", rec.Code, rec.Body.String())
	}
	payload := decodeBody(t, rec)

	topUsers, ok := payload["top_users"].([]any)
	if !ok || len(topUsers) < 2 {
		t.Fatalf("top_users = %v, want at least the two seeded users", payload["top_users"])
	}
	first := topUsers[0].(map[string]any)
	second := topUsers[1].(map[string]any)
	if first["key"] != "u-volume" || second["key"] != "u-spend" {
		t.Errorf("top_users order = [%v %v], want [u-volume u-spend] (ranked by tokens_out, not cost)",
			first["key"], second["key"])
	}

	// Descending tokens_out across the whole leaderboard.
	prev := int64(1<<62 - 1)
	for i, row := range topUsers {
		totals := row.(map[string]any)["totals"].(map[string]any)
		out := int64(totals["tokens_out"].(float64))
		if out > prev {
			t.Errorf("top_users[%d].totals.tokens_out = %d, want descending order", i, out)
		}
		prev = out
	}
}

// TestExportRequestsIncludesCacheWriteColumns locks the CSV shape for the
// prompt-cache dimensions: the export carries tokens_cache_write_5m and
// tokens_cache_write_1h columns alongside tokens_cached, populated per row
// (and honestly zero for cache-free requests).
func TestExportRequestsIncludesCacheWriteColumns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	now := time.Now().UTC()
	if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
		CreatedAt: now.Add(-time.Minute), UserID: h.user.ID, ModelName: "claude-cache", Modality: "chat",
		TokensIn: 1000, TokensOut: 500, TokensCached: 50, TokensCacheWrite5m: 100, TokensCacheWrite1h: 200,
		HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("insert cache event: %v", err)
	}
	if err := h.store.InsertUsageEvent(ctx, &store.UsageEvent{
		CreatedAt: now, UserID: h.user.ID, ModelName: "model-plain", Modality: "chat",
		TokensIn: 10, TokensOut: 5, HTTPStatus: 200,
	}); err != nil {
		t.Fatalf("insert plain event: %v", err)
	}

	rec := h.do(http.MethodGet, "/api/v1/requests.csv?range=day", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export returned %d: %s", rec.Code, rec.Body.String())
	}
	rows, err := csv.NewReader(rec.Body).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("CSV rows = %d, want header + 2 data rows", len(rows))
	}
	col := map[string]int{}
	for i, name := range rows[0] {
		col[name] = i
	}
	for _, name := range []string{"tokens_cached", "tokens_cache_write_5m", "tokens_cache_write_1h"} {
		if _, ok := col[name]; !ok {
			t.Fatalf("CSV header lacks %q: %v", name, rows[0])
		}
	}
	byModel := map[string][]string{}
	for _, row := range rows[1:] {
		byModel[row[col["model"]]] = row
	}
	cacheRow, ok := byModel["claude-cache"]
	if !ok {
		t.Fatalf("no row for claude-cache: %v", rows)
	}
	if got := cacheRow[col["tokens_cached"]]; got != "50" {
		t.Errorf("tokens_cached = %q, want 50", got)
	}
	if got := cacheRow[col["tokens_cache_write_5m"]]; got != "100" {
		t.Errorf("tokens_cache_write_5m = %q, want 100", got)
	}
	if got := cacheRow[col["tokens_cache_write_1h"]]; got != "200" {
		t.Errorf("tokens_cache_write_1h = %q, want 200", got)
	}
	plainRow, ok := byModel["model-plain"]
	if !ok {
		t.Fatalf("no row for model-plain: %v", rows)
	}
	if got5, got1 := plainRow[col["tokens_cache_write_5m"]], plainRow[col["tokens_cache_write_1h"]]; got5 != "0" || got1 != "0" {
		t.Errorf("cache-free row cache-write columns = (%q, %q), want (0, 0)", got5, got1)
	}
}

// TestTokenEndpointsRevokedAtWireFormat locks the wire contract the tokens UI
// depends on: GET /api/v1/tokens and GET /api/v1/tokens/{id} must return
// revoked_at as an empty string for active tokens and as an RFC 3339
// timestamp for revoked ones. Regression: the zero time.Time serialized to
// "0001-01-01T00:00:00Z" — truthy in JS — so every active token rendered with
// a "revoked" badge and vanished from the status=active filter.
func TestTokenEndpointsRevokedAtWireFormat(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// The harness seeds one active token; add a second and revoke it.
	tokens, err := h.store.ListTokens(ctx, h.user.ID)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("list seeded tokens: %v (%d rows)", err, len(tokens))
	}
	activeID := tokens[0].ID
	revokedToken, _, err := h.store.CreateToken(ctx, h.user.ID, "revoked token")
	if err != nil {
		t.Fatalf("create second token: %v", err)
	}
	if err := h.store.RevokeToken(ctx, revokedToken.ID); err != nil {
		t.Fatalf("revoke token: %v", err)
	}

	rec := h.do(http.MethodGet, "/api/v1/tokens", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list tokens returned %d: %s", rec.Code, rec.Body.String())
	}
	rows := decodeBody(t, rec)["tokens"].([]any)
	if len(rows) != 2 {
		t.Fatalf("list tokens returned %d rows, want 2", len(rows))
	}
	byID := map[string]map[string]any{}
	for _, row := range rows {
		entry := row.(map[string]any)
		byID[entry["id"].(string)] = entry
	}
	if got := byID[activeID]["revoked_at"]; got != "" {
		t.Errorf("active token revoked_at = %q, want empty string", got)
	}
	revokedAt, ok := byID[revokedToken.ID]["revoked_at"].(string)
	if !ok || revokedAt == "" {
		t.Fatalf("revoked token revoked_at = %v, want RFC 3339 string", byID[revokedToken.ID]["revoked_at"])
	}
	if _, err := time.Parse(time.RFC3339Nano, revokedAt); err != nil {
		t.Errorf("revoked token revoked_at %q is not RFC 3339: %v", revokedAt, err)
	}
	if strings.HasPrefix(revokedAt, "0001-01-01") {
		t.Errorf("revoked token revoked_at %q is the zero time", revokedAt)
	}

	// The detail endpoint (drawer status badge) uses the same encoding.
	rec = h.do(http.MethodGet, "/api/v1/tokens/"+activeID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("token detail returned %d: %s", rec.Code, rec.Body.String())
	}
	detail := decodeBody(t, rec)["token"].(map[string]any)
	if got := detail["revoked_at"]; got != "" {
		t.Errorf("detail active token revoked_at = %q, want empty string", got)
	}
}
