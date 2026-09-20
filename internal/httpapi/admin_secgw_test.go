package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

func adminSession(t *testing.T, h *harness) *auth.Session {
	t.Helper()
	session, err := h.server.Sessions.Create(context.Background(), h.user.ID)
	if err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	return session
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
}

func TestAdminSecgwPolicyAndBindingLifecycle(t *testing.T) {
	h := newHarness(t)
	bizLicense(t, h) // enforcing/fallback/capture behaviour is Business
	sess := adminSession(t, h)
	ctx := context.Background()

	// Empty overview.
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/overview", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body.String())
	}

	// Validation errors surface as 400 with the field.
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies", map[string]any{"name": "bad", "checks": []map[string]any{{"kind": "prompt_injection", "mode": "redact"}}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy accepted: %d %s", rec.Code, rec.Body.String())
	}
	// A classifier reference must point at a classifier-role model.
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies", map[string]any{"name": "bad2", "checks": []map[string]any{{"kind": "prompt_injection", "mode": "block", "classifier_model_id": h.model.ID}}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not marked as a classifier") {
		t.Fatalf("non-classifier ref accepted: %d %s", rec.Code, rec.Body.String())
	}

	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/policies", map[string]any{
		"name": "Corporate Default", "enabled": true, "mandatory": true,
		"checks": []map[string]any{{"kind": "secrets", "enabled": true, "mode": "block", "direction": "both"}, {"kind": "pii", "enabled": true, "mode": "redact"}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Policy store.SecgwPolicy `json:"policy"`
	}
	decodeInto(t, rec, &created)
	if created.Policy.ID == "" || created.Policy.Checks[1].Fail != store.SecgwFailClosed {
		t.Fatalf("created: %+v", created.Policy)
	}

	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/bindings", map[string]any{"policy_id": created.Policy.ID, "scope_type": "org"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("bind: %d %s", rec.Code, rec.Body.String())
	}
	var bound struct {
		Binding store.SecgwBinding `json:"binding"`
	}
	decodeInto(t, rec, &bound)
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/bindings", map[string]any{"policy_id": created.Policy.ID, "scope_type": "org"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate binding: %d", rec.Code)
	}

	// The proxy sees it on this replica immediately (cache flushed).
	prox := h.do(http.MethodPost, "/v1/chat/completions", chatBody("key "+testAWSKey, false))
	if prox.Code != http.StatusForbidden {
		t.Fatalf("policy not live after admin write: %d", prox.Code)
	}

	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/bindings", nil)
	if !strings.Contains(rec.Body.String(), `"scope_name":"Everyone"`) || !strings.Contains(rec.Body.String(), `"policy_name":"Corporate Default"`) {
		t.Fatalf("bindings list: %s", rec.Body.String())
	}

	// Delete refused while bound.
	rec = h.doAsSession(sess, http.MethodDelete, "/api/v1/admin/secgw/policies/"+created.Policy.ID, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete bound policy: %d", rec.Code)
	}
	rec = h.doAsSession(sess, http.MethodDelete, "/api/v1/admin/secgw/bindings/"+bound.Binding.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unbind: %d", rec.Code)
	}
	rec = h.doAsSession(sess, http.MethodDelete, "/api/v1/admin/secgw/policies/"+created.Policy.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	prox = h.do(http.MethodPost, "/v1/chat/completions", chatBody("key "+testAWSKey, false))
	if prox.Code != http.StatusOK {
		t.Fatalf("policy still live after delete: %d", prox.Code)
	}

	// Audit trail exists for the writes.
	entries, _, err := h.store.ListAudit(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Action] = true
	}
	for _, want := range []string{"secgw_policy_created", "secgw_binding_created", "secgw_binding_deleted", "secgw_policy_deleted"} {
		if !seen[want] {
			t.Fatalf("audit missing %s: %v", want, seen)
		}
	}

	// A service token is scoped to /v1 and must never reach the admin
	// surface — including this one.
	_, svcPlain, err := h.store.CreateServiceToken(ctx, "integration", "", h.user.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/secgw/policies", nil)
	req.Header.Set("Authorization", "Bearer "+svcPlain)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("service token reached admin secgw API: %d", rec.Code)
	}
}

func TestAdminSecgwTermListsNeverLeakInListingAndAuditReads(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	ctx := context.Background()

	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/term-lists", map[string]any{
		"name": "Codenames", "match_mode": "fuzzy", "severity": "critical", "terms": []string{"Project Halberd", "Orion"}, "allow": []string{"orion nebula"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Halberd") {
		t.Fatal("create response echoed the terms")
	}
	var created struct {
		TermList store.SecgwTermList `json:"term_list"`
	}
	decodeInto(t, rec, &created)
	if created.TermList.TermCount != 2 {
		t.Fatalf("term_count: %+v", created.TermList)
	}

	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/term-lists", nil)
	if strings.Contains(rec.Body.String(), "Halberd") || !strings.Contains(rec.Body.String(), `"term_count":2`) {
		t.Fatalf("listing must carry counts, never terms: %s", rec.Body.String())
	}

	// Single-item read returns terms and is audited.
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/term-lists/"+created.TermList.ID, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Project Halberd") {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	entries, _, _ := h.store.ListAudit(ctx, store.AuditFilter{})
	audited := false
	for _, e := range entries {
		if e.Action == "secgw_term_list_read" && e.ResourceID == created.TermList.ID {
			audited = true
		}
	}
	if !audited {
		t.Fatal("reading the full list must be audited")
	}

	// CSV import appends; first column only; comments skipped.
	csv := "# codenames\nZephyr,owner=alice\n\"Quoted Name\",x\nOrion\n"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/secgw/term-lists/"+created.TermList.ID+"/import?mode=append", strings.NewReader(csv))
	req.Header.Set("Content-Type", "text/csv")
	req.Header.Set(auth.CSRFHeader, sess.CSRFToken)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: sess.ID})
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"term_count":4`) {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	full, _ := h.store.SecgwTermListByID(ctx, created.TermList.ID, h.server.Cipher, true)
	joined := strings.Join(full.Terms, "|")
	for _, want := range []string{"Zephyr", "Quoted Name", "Project Halberd"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("import missing %q: %v", want, full.Terms)
		}
	}
	if strings.Contains(joined, "owner=alice") {
		t.Fatalf("CSV second column imported: %v", full.Terms)
	}

	// Export round-trips as one term per line.
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/term-lists/"+created.TermList.ID+"/export", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Zephyr\n") || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("export: %d %q", rec.Code, rec.Body.String())
	}

	// Metadata-only update keeps the terms.
	rec = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/secgw/term-lists/"+created.TermList.ID, map[string]any{"name": "Codenames v2", "match_mode": "exact"})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	full, _ = h.store.SecgwTermListByID(ctx, created.TermList.ID, h.server.Cipher, true)
	if full.Name != "Codenames v2" || full.MatchMode != store.SecgwTermExact || len(full.Terms) != 4 {
		t.Fatalf("metadata update lost terms: %+v", full)
	}
}

func TestAdminSecgwEffectiveTraceAndDryRun(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	ctx := context.Background()

	// Fresh install: nothing bound. The explainer must answer with empty
	// lists, never null — a null trace blanked the whole admin SPA once.
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/effective?user_id="+h.user.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("effective on empty: %d %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"trace":[]`) || !strings.Contains(body, `"checks":[]`) {
		t.Fatalf("empty effective must serialise lists as [], got %s", body)
	}

	org := store.SecgwPolicy{Name: "org", Enabled: true, Mandatory: true, Checks: []store.SecgwCheck{{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeBlock}}}
	if err := h.store.CreateSecgwPolicy(ctx, &org); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateSecgwBinding(ctx, &store.SecgwBinding{PolicyID: org.ID, ScopeType: store.SecgwScopeOrg}); err != nil {
		t.Fatal(err)
	}
	grp, _ := h.store.CreateGroup(ctx, "Legal")
	_ = h.store.SetGroupMembers(ctx, grp.ID, []string{h.user.ID})
	relaxed := store.SecgwPolicy{Name: "legal", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeObserve},
		{Kind: store.SecgwCheckPII, Enabled: true, Mode: store.SecgwModeRedact},
	}}
	if err := h.store.CreateSecgwPolicy(ctx, &relaxed); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateSecgwBinding(ctx, &store.SecgwBinding{PolicyID: relaxed.ID, ScopeType: store.SecgwScopeGroup, ScopeID: grp.ID}); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()

	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/effective?user_id="+h.user.ID+"&model=test-model", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("effective: %d %s", rec.Code, rec.Body.String())
	}
	var eff struct {
		Checks []struct {
			Kind      string `json:"kind"`
			Mode      string `json:"mode"`
			Floor     bool   `json:"floor"`
			ScopeType string `json:"scope_type"`
		} `json:"checks"`
		Trace []struct {
			Outcome string `json:"outcome"`
			Detail  string `json:"detail"`
		} `json:"trace"`
	}
	decodeInto(t, rec, &eff)
	if len(eff.Checks) != 2 {
		t.Fatalf("checks: %+v", eff.Checks)
	}
	for _, c := range eff.Checks {
		switch c.Kind {
		case "secrets":
			if c.Mode != "block" || !c.Floor || c.ScopeType != "org" {
				t.Fatalf("mandatory floor not shown: %+v", c)
			}
		case "pii":
			if c.Mode != "redact" || c.ScopeType != "group" {
				t.Fatalf("group addition not shown: %+v", c)
			}
		}
	}
	refused := false
	for _, tr := range eff.Trace {
		if tr.Outcome == "overridden_by_mandatory" && strings.Contains(tr.Detail, "secrets") {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("trace must name the refused relaxation: %+v", eff.Trace)
	}

	// Dry run against the live policy set: blocked, no upstream call, no
	// usage event, no violation row.
	before := len(h.upstreamRequests)
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/dry-run", map[string]any{
		"user_id": h.user.ID, "model": "test-model",
		"body": chatBody("my key is "+testAWSKey+" and card 4111 1111 1111 1111", false),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("dry-run: %d %s", rec.Code, rec.Body.String())
	}
	var dry struct {
		Action     string `json:"action"`
		BlockKind  string `json:"block_kind"`
		Violations []struct {
			Kind, RuleID, Action string
		} `json:"violations"`
	}
	decodeInto(t, rec, &dry)
	if dry.Action != "blocked" || dry.BlockKind != "secrets" || len(dry.Violations) != 2 {
		t.Fatalf("dry-run result: %+v", dry)
	}
	if strings.Contains(rec.Body.String(), testAWSKey) {
		t.Fatal("dry-run echoed the secret")
	}
	if len(h.upstreamRequests) != before {
		t.Fatal("dry-run reached the upstream")
	}
	vs, _ := h.store.ListSecgwViolations(ctx, store.SecgwViolationFilter{})
	if len(vs) != 0 {
		t.Fatalf("dry-run persisted violations: %d", len(vs))
	}

	// Dry run against a DRAFT policy id, ignoring bindings.
	draft := store.SecgwPolicy{Name: "draft", Enabled: true, Checks: []store.SecgwCheck{{Kind: store.SecgwCheckPII, Enabled: true, Mode: store.SecgwModeRedact}}}
	if err := h.store.CreateSecgwPolicy(ctx, &draft); err != nil {
		t.Fatal(err)
	}
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/secgw/dry-run", map[string]any{
		"policy_id": draft.ID, "body": chatBody("card 4111 1111 1111 1111 and key "+testAWSKey, false),
	})
	decodeInto(t, rec, &dry)
	if dry.Action != "redacted" || len(dry.Violations) != 1 || dry.Violations[0].Kind != "pii" {
		t.Fatalf("draft dry-run: %+v %s", dry, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[REDACTED:pan]") || !strings.Contains(rec.Body.String(), testAWSKey) {
		t.Fatalf("draft dry-run must show the redacted body and leave secrets alone (not in the draft): %s", rec.Body.String())
	}
}

func TestAdminSecgwViolationsGatedReadAndClassifierRole(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	ctx := context.Background()

	h.secgwPolicy(t, store.SecgwPolicy{Name: "observe", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckPII, Enabled: true, Mode: store.SecgwModeObserve},
	}})
	prox := h.do(http.MethodPost, "/v1/chat/completions", chatBody("mail bob@example.com", false))
	if prox.Code != http.StatusOK {
		t.Fatalf("proxy: %d", prox.Code)
	}
	vs := h.waitForViolations(t, 1)

	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/violations?kind=pii&since=1h", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rule_id":"email"`) || strings.Contains(rec.Body.String(), "bob@example.com") {
		t.Fatalf("list must carry metadata only: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/violations/"+vs[0].ID, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[REDACTED:email]") {
		t.Fatalf("single read: %d %s", rec.Code, rec.Body.String())
	}
	entries, _, _ := h.store.ListAudit(ctx, store.AuditFilter{})
	audited := false
	for _, e := range entries {
		if e.Action == "secgw_violation_text_read" && e.ResourceID == vs[0].ID {
			audited = true
		}
	}
	if !audited {
		t.Fatal("reading captured text must be audited")
	}

	// Classifier role via the admin API.
	if _, err := h.store.UpsertDiscoveredModel(ctx, h.model.UpstreamID, "prompt-guard-2", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	models, _ := h.store.ListModels(ctx, store.ModelFilter{Search: "prompt-guard"})
	guard := models[0]
	rec = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/secgw/classifiers/"+guard.ID, map[string]any{"classifier_role": "text_classification"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"classifier_role":"text_classification"`) {
		t.Fatalf("set role: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/classifiers", nil)
	body := rec.Body.String()
	if !strings.Contains(body, "prompt-guard-2") || !strings.Contains(body, `"protocols":["generative_guard","text_classification"]`) {
		t.Fatalf("classifiers: %s", body)
	}
	// The hazard taxonomy rides along so the policy editor never hardcodes it.
	if !strings.Contains(body, `"code":"S14"`) || !strings.Contains(body, `"default_categories":["S1","S3","S4","S9","S11"]`) {
		t.Fatalf("taxonomy missing from classifiers response: %s", body)
	}
	// The admin model list still shows it; the servable catalog does not.
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/models", nil)
	if strings.Contains(rec.Body.String(), "prompt-guard-2") {
		t.Fatal("classifier offered in the user catalog")
	}
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/admin/secgw/rules", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "aws-access-key-id") || !strings.Contains(rec.Body.String(), `"pan"`) {
		t.Fatalf("rules: %d %s", rec.Code, rec.Body.String())
	}
}
