package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/balancer"
	"github.com/torvanis/janus/internal/store"
)

// chatUsageRows lists the usage events of Chat calls, oldest first.
func chatUsageRows(t *testing.T, h *harness, want int) []*store.UsageEvent {
	t.Helper()
	var out []*store.UsageEvent
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out = out[:0]
		for _, e := range h.waitForUsageEvents(1) {
			if strings.HasPrefix(e.ClientApp, "janus-chat") {
				out = append(out, e)
			}
		}
		if len(out) >= want {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// One Chat reply that makes several calls (here: a tool round and the
// answer) counts once toward per-minute rate limits: with 1 request a
// minute the reply still finishes instead of refusing its own second call.
func TestChatReplyCountsOnceTowardRateLimits(t *testing.T) {
	h, sess := chatHarness(t)
	f := newFakeToolUpstream(t, h)
	f.call = []struct{ name, args string }{{"create_file", `{"filename":"a.txt","content":"hi"}`}}
	f.reply = "Saved."
	ctx := context.Background()
	if err := h.store.CreateRateLimitRule(ctx, &store.RateLimitRule{Endpoint: "/v1/chat/completions", RequestsPerMinute: 1}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetFeatureFlag(ctx, "per_user_rate_limits_enabled", true); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	_, done := toolChat(t, h, sess, map[string]any{"content": "save a file"})
	if done["status"] != store.ChatStatusComplete || done["content"] != "Saved." {
		t.Fatalf("a reply's own tool round was rate limited: status %v error %v", done["status"], done["error_message"])
	}
	// The next message is a new request and is limited as before.
	_, done = toolChat(t, h, sess, map[string]any{"content": "again"})
	if done["status"] == store.ChatStatusComplete {
		t.Fatal("a second message within the minute was not rate limited")
	}
}

// Request-count quotas count a Chat reply once; tokens and cost still
// count every call.
func TestChatReplyCountsOnceTowardRequestQuotas(t *testing.T) {
	h, sess := chatHarness(t)
	f := newFakeToolUpstream(t, h)
	f.call = []struct{ name, args string }{{"create_file", `{"filename":"a.txt","content":"hi"}`}}
	f.reply = "Saved."
	q := &store.Quota{SubjectType: "user", SubjectID: h.user.ID, Metric: store.MetricRequests, Limit: 2, Window: store.WindowDaily, BreachBehavior: store.BreachLetFinish}
	if err := h.store.CreateQuota(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	h.server.invalidateQuotaRules()
	_, done := toolChat(t, h, sess, map[string]any{"content": "save a file"})
	if done["status"] != store.ChatStatusComplete {
		t.Fatalf("first reply = %v %v", done["status"], done["error_message"])
	}
	rows := chatUsageRows(t, h, 2)
	if len(rows) != 2 || rows[0].ChatSubcall || !rows[1].ChatSubcall {
		t.Fatalf("want the first call counted and the tool round a sub-call: %d rows", len(rows))
	}
	n, err := h.store.SumMetricSince(context.Background(), "user", h.user.ID, "", store.MetricRequests, time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("requests counted for one reply = %d (%v), want 1", n, err)
	}
	time.Sleep(100 * time.Millisecond)
	_, done = toolChat(t, h, sess, map[string]any{"content": "again"})
	if done["status"] != store.ChatStatusComplete {
		t.Fatalf("second message refused by a 2-request quota after one reply: %v %v", done["status"], done["error_message"])
	}
}

// sendChatFrom posts a Chat message as a browser at peer (with the given
// extra headers) would, and returns the finished reply.
func sendChatFrom(t *testing.T, h *harness, sess *auth.Session, convID, peer string, hdr map[string]string, content string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"content": content})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/conversations/"+convID+"/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: sess.ID})
	req.Header.Set(auth.CSRFHeader, sess.CSRFToken)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = peer
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	decodeInto(t, rec, &out)
	_, done := chatEvents(t, h, sess, out["run_id"].(string), "")
	return done
}

// Chat's calls carry the browser's address (X-Forwarded-For only through a
// trusted proxy), user agent and headers, so IP and header policy rules
// apply to Chat as to API traffic and the request log shows the address.
func TestChatCallsCarryTheClientAddress(t *testing.T) {
	h, sess := chatHarness(t)
	_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
	h.server.Config.TrustedProxies = []*net.IPNet{trusted}
	ctx := context.Background()
	// "Only from the office network": everything else is blocked.
	if err := h.store.CreateBlockingRule(ctx, &store.BlockingRule{Name: "office-only", Combinator: "and", Enabled: true, Reason: "Use the office network.",
		Clauses: []store.RuleClause{{Type: store.ClauseSourceIP, Pattern: "203.0.113.0/24", Negate: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateBlockingRule(ctx, &store.BlockingRule{Name: "no-legacy-device", Combinator: "and", Enabled: true, Reason: "Legacy device.",
		Clauses: []store.RuleClause{{Type: store.ClauseHeader, Header: "X-Corp-Device", Pattern: "legacy"}}}); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	conv := chatConversationFor(t, h, sess, "test-model")
	office := map[string]string{"X-Forwarded-For": "203.0.113.9", "User-Agent": "Mozilla/5.0 Test", "Referer": "https://janus.example/chat"}
	done := sendChatFrom(t, h, sess, conv, "10.1.2.3:5000", office, "hello from the office")
	if done["status"] != store.ChatStatusComplete {
		t.Fatalf("office user blocked in Chat: %v %v", done["status"], done["error_message"])
	}
	rows := chatUsageRows(t, h, 1)
	ev := rows[len(rows)-1]
	if ev.ClientIP != "203.0.113.9" || ev.XForwardedFor != "203.0.113.9" || ev.UserAgent != "Mozilla/5.0 Test" || ev.Referer != "https://janus.example/chat" {
		t.Fatalf("request log identity = ip %q xff %q ua %q referer %q", ev.ClientIP, ev.XForwardedFor, ev.UserAgent, ev.Referer)
	}
	// From outside the office: the rule blocks Chat too.
	done = sendChatFrom(t, h, sess, conv, "198.51.100.7:5000", map[string]string{"X-Forwarded-For": "203.0.113.9"}, "hello from home")
	if done["status"] == store.ChatStatusComplete {
		t.Fatal("a client outside the allowed network (spoofing X-Forwarded-For) used Chat")
	}
	// A header rule sees the browser's headers.
	office["X-Corp-Device"] = "legacy"
	done = sendChatFrom(t, h, sess, conv, "10.1.2.3:5000", office, "hello from an old laptop")
	if done["status"] == store.ChatStatusComplete {
		t.Fatal("a header blocking rule did not apply to Chat")
	}
}

// The footer shows the whole reply: tokens and cost of every call it made
// (here the tool-calling round, 30 in / 9 out, and the answer, 20 in / 3
// out), not only the first call's.
func TestChatFooterSumsTheWholeReply(t *testing.T) {
	h, sess := chatHarness(t)
	f := newFakeToolUpstream(t, h)
	f.call = []struct{ name, args string }{{"create_file", `{"filename":"a.txt","content":"hi"}`}}
	f.reply = "Saved."
	_, done := toolChat(t, h, sess, map[string]any{"content": "save a file"})
	if done["status"] != store.ChatStatusComplete {
		t.Fatalf("reply = %v", done)
	}
	if done["tokens_in"].(float64) != 50 || done["tokens_out"].(float64) != 12 {
		t.Fatalf("footer tokens = %v in / %v out, want 50 / 12 (both calls)", done["tokens_in"], done["tokens_out"])
	}
	rows := chatUsageRows(t, h, 2)
	var cost int64
	for _, e := range rows {
		if e.ChatMessageID != done["id"] {
			t.Fatalf("call %s (%s) not tied to the reply: %q", e.RequestID, e.ClientApp, e.ChatMessageID)
		}
		cost += e.CostNano
	}
	if got := done["cost_usd"].(float64); cost == 0 || got < float64(cost)/1e9*0.999 || got > float64(cost)/1e9*1.001 {
		t.Fatalf("footer cost = %v, want the sum %v", got, float64(cost)/1e9)
	}
	// The reply's first call is janus-chat, the tool round janus-chat-tool;
	// both carry the conversation as the auto-routing session.
	if rows[0].ClientApp != chatClientApp || rows[1].ClientApp != chatToolClientApp {
		t.Fatalf("client apps = %q, %q", rows[0].ClientApp, rows[1].ClientApp)
	}
}

// Every Chat call names the conversation as its auto-routing session, so a
// router keeps one conversation on one route even as tools and memories
// change between rounds and turns.
func TestChatSendsTheConversationAsTheRoutingSession(t *testing.T) {
	h, sess := chatHarness(t)
	conv := chatConversationFor(t, h, sess, "test-model")
	for _, msg := range []string{"first", "second"} {
		chatEvents(t, h, sess, chatSend(t, h, sess, conv, map[string]any{"content": msg})["run_id"].(string), "")
	}
	if len(h.upstreamRequests) < 2 {
		t.Fatalf("upstream saw %d requests", len(h.upstreamRequests))
	}
	var keys []string
	for _, r := range h.upstreamRequests {
		if !strings.Contains(string(r.Body), `"messages"`) {
			continue
		}
		key, source := balancer.SessionKey(r.Header, r.Body, h.user.ID)
		if source != "header" {
			t.Fatalf("sticky key source = %q, want the conversation (header)", source)
		}
		keys = append(keys, key)
	}
	if keys[0] != keys[len(keys)-1] || keys[0] != balancerKey(h.user.ID, conv) {
		t.Fatalf("sticky keys %v differ or are not the conversation's", keys)
	}
}

func balancerKey(caller, conv string) string {
	h := http.Header{}
	h.Set("X-Janus-Session", conv)
	k, _ := balancer.SessionKey(h, nil, caller)
	return k
}

// Usage insights classifies the person's message, not Chat's auto-title.
func TestChatTitleIsNotClassified(t *testing.T) {
	h, sess := chatHarness(t)
	f := newFakeDecisionServer(t)
	h.enableClassification(t, f, store.ClassificationModeAsync, nil)
	h.server.Config.ChatAutoTitle = true
	conv := chatConversationFor(t, h, sess, "test-model")
	chatEvents(t, h, sess, chatSend(t, h, sess, conv, map[string]any{"content": "why does the borrow checker reject this"})["run_id"].(string), "")
	// Wait for the title call to be metered, then for classification.
	chatUsageRows(t, h, 2)
	h.waitForClassifications(t, 1)
	time.Sleep(300 * time.Millisecond)
	for _, seen := range f.seen() {
		if strings.Contains(seen, "Title for the conversation above") {
			t.Fatalf("the auto-title was classified as a request: %q", seen)
		}
	}
	rows := chatUsageRows(t, h, 2)
	var title *store.UsageEvent
	for _, e := range rows {
		if e.ClientApp == chatTitleClientApp {
			title = e
		}
	}
	if title == nil || !title.ChatSubcall || title.ChatMessageID == "" {
		t.Fatalf("title call not tagged as a sub-call of the reply: %+v", title)
	}
}

// Chat access restriction × shares: a person outside the restriction can
// still open a link someone shared (a read-only view), and an owner who
// lost Chat access can still list and revoke the links they made, but can
// neither make new ones nor use Chat.
func TestChatSharesAcrossTheAccessRestriction(t *testing.T) {
	h, admin := chatHarness(t)
	owner := h.sessionFor(t, "owner@example.com")
	viewer := h.sessionFor(t, "viewer@example.com")
	conv := chatConversationFor(t, h, owner, "test-model")
	chatEvents(t, h, owner, chatSend(t, h, owner, conv, map[string]any{"content": "Pancakes for four?"})["run_id"].(string), "")
	rec := h.doAsSession(owner, http.MethodPost, "/api/v1/chat/conversations/"+conv+"/shares", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("share: %d %s", rec.Code, rec.Body.String())
	}
	var sh chatShareView
	decodeInto(t, rec, &sh)
	// Restrict Chat to someone else.
	withBusinessLicense(t, h)
	h.sessionFor(t, "insider@example.com")
	if res := chatAdminPut(t, h, admin, func(m map[string]any) {
		m["access"] = map[string]any{"mode": "restricted", "groups": []string{}, "teams": []string{}, "users": []string{chatUserID(t, h, "insider@example.com")}}
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("restrict: %d", res.StatusCode)
	}
	if rec := h.doAsSession(owner, http.MethodGet, "/api/v1/chat/conversations", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("restricted owner still uses Chat: %d", rec.Code)
	}
	if rec := h.doAsSession(owner, http.MethodPost, "/api/v1/chat/conversations/"+conv+"/shares", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("restricted owner made a new link: %d", rec.Code)
	}
	if rec := h.doAsSession(viewer, http.MethodGet, "/api/v1/chat/shared/"+sh.ID, nil); rec.Code != http.StatusOK {
		t.Fatalf("restricted viewer could not open a shared link: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(owner, http.MethodGet, "/api/v1/chat/conversations/"+conv+"/shares", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), sh.ID) {
		t.Fatalf("restricted owner cannot list their links: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(viewer, http.MethodDelete, "/api/v1/chat/conversations/"+conv+"/shares/"+sh.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("another person revoked the link: %d", rec.Code)
	}
	if rec := h.doAsSession(owner, http.MethodDelete, "/api/v1/chat/conversations/"+conv+"/shares/"+sh.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("restricted owner cannot revoke their link: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.doAsSession(viewer, http.MethodGet, "/api/v1/chat/shared/"+sh.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("revoked link still opens: %d", rec.Code)
	}
}

// Local-only mode tracks no cost, so a Chat export prints none.
func TestChatExportLeavesCostOutInLocalOnlyMode(t *testing.T) {
	h, sess := chatHarness(t)
	conv := chatConversationFor(t, h, sess, "test-model")
	chatEvents(t, h, sess, chatSend(t, h, sess, conv, map[string]any{"content": "hello"})["run_id"].(string), "")
	h.server.Config.LocalOnly = true
	md := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+conv+"/export?format=markdown", nil).Body.String()
	if strings.Contains(md, "$") || !strings.Contains(md, "11 in / 7 out") {
		t.Fatalf("local-only markdown export:\n%s", md)
	}
	js := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+conv+"/export?format=json", nil).Body.String()
	if strings.Contains(js, "cost_usd") {
		t.Fatalf("local-only JSON export has cost: %s", js)
	}
}
