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
	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/store"
)

func decodeRec(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %d %s: %v", rec.Code, rec.Body.String(), err)
	}
}

func chatUserID(t *testing.T, h *harness, email string) string {
	t.Helper()
	u, err := h.store.UserByEmail(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func chatAdminPut(t *testing.T, h *harness, sess *auth.Session, mutate func(m map[string]any)) *http.Response {
	t.Helper()
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/chat", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get chat settings: %d %s", rec.Code, rec.Body.String())
	}
	var cur map[string]any
	decodeRec(t, rec, &cur)
	mutate(cur)
	return h.doAsSession(sess, http.MethodPut, "/api/v1/admin/chat", cur).Result()
}

// Restricting who may use Chat is Business; Community admins get a 402 and
// everyone keeps access. With Business, people outside the allowed groups
// are refused with the admin's message; members and admins are not.
func TestChatAccessRestrictionIsBusinessAndApplies(t *testing.T) {
	h, admin := chatHarness(t)
	ctx := context.Background()
	outsider := h.sessionFor(t, "outsider@example.com")
	insider := h.sessionFor(t, "insider@example.com")
	g, err := h.store.CreateGroup(ctx, "chatters")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetGroupMembers(ctx, g.ID, []string{chatUserID(t, h, "insider@example.com")}); err != nil {
		t.Fatal(err)
	}
	restrict := func(m map[string]any) {
		m["access"] = map[string]any{"mode": "restricted", "groups": []string{g.ID}, "teams": []string{}, "users": []string{},
			"message": "Ask IT for Chat access."}
	}
	// Community: refused, nobody loses access.
	if res := chatAdminPut(t, h, admin, restrict); res.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("community restriction = %d, want 402", res.StatusCode)
	}
	if rec := h.doAsSession(outsider, http.MethodGet, "/api/v1/chat/conversations", nil); rec.Code != http.StatusOK {
		t.Fatalf("community outsider = %d", rec.Code)
	}
	// Business: applies.
	withBusinessLicense(t, h)
	if res := chatAdminPut(t, h, admin, restrict); res.StatusCode != http.StatusOK {
		t.Fatalf("business restriction = %d", res.StatusCode)
	}
	rec := h.doAsSession(outsider, http.MethodGet, "/api/v1/chat/conversations", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Ask IT for Chat access.") {
		t.Fatalf("outsider = %d %s", rec.Code, rec.Body.String())
	}
	var me map[string]any
	decodeRec(t, h.doAsSession(outsider, http.MethodGet, "/api/v1/me", nil), &me)
	if me["chat_preview"] != false || me["chat_unavailable_reason"] != "Ask IT for Chat access." {
		t.Fatalf("/me for outsider = %v / %v", me["chat_preview"], me["chat_unavailable_reason"])
	}
	if rec := h.doAsSession(insider, http.MethodGet, "/api/v1/chat/conversations", nil); rec.Code != http.StatusOK {
		t.Fatalf("group member = %d", rec.Code)
	}
	if rec := h.doAsSession(admin, http.MethodGet, "/api/v1/chat/conversations", nil); rec.Code != http.StatusOK {
		t.Fatalf("admin = %d", rec.Code)
	}
	// Past the grace period the saved restriction still applies (expiry
	// never opens Chat to everyone), but changing it waits for a renewal.
	exp := time.Now().Add(-60 * 24 * time.Hour)
	installLicense(t, h, license.Claims{LicenseID: "JNS-CHAT-EXP", Org: "T", IssuedTo: "t@t", Edition: license.EditionBusiness,
		Features: license.BusinessFeatures, Seats: 100, Exp: &exp, GraceDays: 30, Term: license.TermSubscription})
	if rec := h.doAsSession(outsider, http.MethodGet, "/api/v1/chat/conversations", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("expired: outsider = %d, want 403 (restriction kept)", rec.Code)
	}
	if res := chatAdminPut(t, h, admin, func(m map[string]any) {
		m["access"] = map[string]any{"mode": "restricted", "groups": []string{}, "teams": []string{}, "users": []string{chatUserID(t, h, "outsider@example.com")}}
	}); res.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("expired: changing the restriction = %d, want 402", res.StatusCode)
	}
	withBusinessLicense(t, h)
	// Turning Chat off for the instance stops everyone but is Community.
	if res := chatAdminPut(t, h, admin, func(m map[string]any) {
		m["enabled"] = false
		m["access"] = map[string]any{"mode": "everyone"}
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("turn off = %d", res.StatusCode)
	}
	if rec := h.doAsSession(insider, http.MethodGet, "/api/v1/chat/conversations", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("instance off = %d", rec.Code)
	}
}

// Grants carry the chat name/description; the most specific applicable
// grant wins, and the catalog name stays the model name.
func TestChatGrantLabelsMostSpecificWins(t *testing.T) {
	h, admin := chatHarness(t)
	ctx := context.Background()
	other := h.sessionFor(t, "labels@example.com")
	otherID := chatUserID(t, h, "labels@example.com")
	if _, err := h.store.CreateGrant(ctx, h.model.ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	// CreateGrant answers a repeat with an unsaved id: read the stored row.
	var all *store.Grant
	grants, err := h.store.ListGrants(ctx, h.model.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grants {
		if g.GranteeType == store.GranteeAllUsers {
			all = g
		}
	}
	if all == nil {
		t.Fatal("no all-users grant")
	}
	direct, err := h.store.CreateGrant(ctx, h.model.ID, store.ModelKindModel, store.GranteeUser, otherID)
	if err != nil {
		t.Fatal(err)
	}
	label := func(id, name, desc string) {
		rec := h.doAsSession(admin, http.MethodPatch, "/api/v1/admin/grants/"+id+"/chat", map[string]any{"chat_name": name, "chat_description": desc})
		if rec.Code != http.StatusOK {
			t.Fatalf("label grant: %d %s", rec.Code, rec.Body.String())
		}
	}
	label(all.ID, "Everyday", "Good for most questions.")
	label(direct.ID, "Your model", "")
	find := func(sess *auth.Session) map[string]any {
		var cat struct {
			Models []map[string]any `json:"models"`
		}
		decodeRec(t, h.doAsSession(sess, http.MethodGet, "/api/v1/chat/catalog", nil), &cat)
		for _, m := range cat.Models {
			if m["name"] == "test-model" {
				return m
			}
		}
		t.Fatalf("test-model missing from catalog: %v", cat.Models)
		return nil
	}
	if m := find(admin); m["chat_name"] != "Everyday" || m["description"] != "Good for most questions." {
		t.Fatalf("all-users label = %v", m)
	}
	// Direct grant overrides the name; the description still comes from the
	// broader grant (they resolve independently).
	if m := find(other); m["chat_name"] != "Your model" || m["description"] != "Good for most questions." {
		t.Fatalf("direct label = %v", m)
	}
	// Grants list shows the labels; too-long names are refused.
	rec := h.doAsSession(admin, http.MethodPatch, "/api/v1/admin/grants/"+all.ID+"/chat", map[string]any{"chat_name": strings.Repeat("x", 61)})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("long name = %d", rec.Code)
	}
	if rec := h.doAsSession(other, http.MethodPatch, "/api/v1/admin/grants/"+all.ID+"/chat", map[string]any{"chat_name": "x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin label = %d", rec.Code)
	}
}

// Hidden models leave the picker and are refused on send; shares/export
// switches refuse; org instructions lead the system message; categories
// validate.
func TestChatAdminCurationAndSwitches(t *testing.T) {
	h, admin := chatHarness(t)
	conv := chatConversationFor(t, h, admin, "test-model")
	if res := chatAdminPut(t, h, admin, func(m map[string]any) {
		m["models"] = map[string]any{"test-model": map[string]any{"category": "astrology"}}
	}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad category = %d", res.StatusCode)
	}
	if res := chatAdminPut(t, h, admin, func(m map[string]any) {
		m["org_instructions"] = "Always answer in British English."
		m["starter_prompts"] = []string{"Summarize a document", "  "}
		m["shares_enabled"] = false
		m["export_enabled"] = false
		m["models"] = map[string]any{"test-model": map[string]any{"category": "fast", "recommended": true}}
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("save = %d", res.StatusCode)
	}
	var cat map[string]any
	decodeRec(t, h.doAsSession(admin, http.MethodGet, "/api/v1/chat/catalog", nil), &cat)
	if p := cat["starter_prompts"].([]any); len(p) != 1 || p[0] != "Summarize a document" {
		t.Fatalf("starter prompts = %v", p)
	}
	if f := cat["features"].(map[string]any); f["shares"] != false || f["export"] != false {
		t.Fatalf("features = %v", f)
	}
	if rec := h.doAsSession(admin, http.MethodPost, "/api/v1/chat/conversations/"+conv+"/shares", map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("share while off = %d", rec.Code)
	}
	if rec := h.doAsSession(admin, http.MethodGet, "/api/v1/chat/conversations/"+conv+"/export?format=markdown", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("export while off = %d", rec.Code)
	}
	// Org instructions reach the model first.
	sent := chatSend(t, h, admin, conv, map[string]any{"content": "hello", "model": "test-model"})
	if _, done := chatEvents(t, h, admin, sent["run_id"].(string), ""); done["status"] != store.ChatStatusComplete {
		t.Fatalf("send = %v", done)
	}
	var body string
	for _, r := range h.upstreamRequests {
		if strings.Contains(string(r.Body), "hello") {
			body = string(r.Body)
		}
	}
	if !strings.Contains(body, "Always answer in British English.") {
		t.Fatalf("org instructions not sent: %s", body)
	}
	// Hidden: out of the picker, refused on send.
	if res := chatAdminPut(t, h, admin, func(m map[string]any) {
		m["models"] = map[string]any{"test-model": map[string]any{"hidden": true}}
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("hide = %d", res.StatusCode)
	}
	decodeRec(t, h.doAsSession(admin, http.MethodGet, "/api/v1/chat/catalog", nil), &cat)
	if hidden := cat["hidden"].([]any); len(hidden) != 1 || hidden[0] != "test-model" {
		t.Fatalf("hidden = %v", cat["hidden"])
	}
	rec := h.doAsSession(admin, http.MethodPost, "/api/v1/chat/conversations/"+conv+"/messages", map[string]any{"content": "hi", "model": "test-model"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("hidden model send = %d", rec.Code)
	}
	// Personal plans off refuses my/* models in Chat.
	if res := chatAdminPut(t, h, admin, func(m map[string]any) {
		m["models"] = map[string]any{}
		m["personal_plans"] = false
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("plans off = %d", res.StatusCode)
	}
	rec = h.doAsSession(admin, http.MethodPost, "/api/v1/chat/conversations/"+conv+"/messages", map[string]any{"content": "hi", "model": "my/xai/grok-4"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Personal plans") {
		t.Fatalf("plan model with plans off = %d %s", rec.Code, rec.Body.String())
	}
}
