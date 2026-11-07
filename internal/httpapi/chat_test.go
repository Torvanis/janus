package httpapi

import (
	"bufio"
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

func chatHarness(t *testing.T) (*harness, *auth.Session) {
	t.Helper()
	h := newHarness(t)
	h.server.Config.ChatPreview = true
	return h, adminSession(t, h)
}

// chatEvents reads a run's SSE stream to its done frame.
func chatEvents(t *testing.T, h *harness, sess *auth.Session, runID, query string) (deltas []map[string]any, done map[string]any) {
	t.Helper()
	// Generous: a deep-research run is several model rounds, and the race
	// detector on a shared CI runner is slow. A finished run returns at once.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/runs/"+runID+"/events"+query, nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: sess.ID})
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var v map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
				t.Fatalf("bad frame %q: %v", line, err)
			}
			if event == "delta" {
				deltas = append(deltas, v)
			} else if event == "done" {
				done = v
			}
		}
	}
	if done == nil {
		t.Fatalf("no done frame; body:\n%s", rec.Body.String())
	}
	return deltas, done
}

func chatSend(t *testing.T, h *harness, sess *auth.Session, convID string, body map[string]any) map[string]any {
	t.Helper()
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations/"+convID+"/messages", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	decodeInto(t, rec, &out)
	return out
}

// TestChatRunGoesThroughTheProxyAndMeters is the Phase 0 core claim: a chat
// message is a normal proxied, metered request attributed to the user via a
// hidden chat token, and the reply is persisted encrypted.
func TestChatRunGoesThroughTheProxyAndMeters(t *testing.T) {
	h, sess := chatHarness(t)
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "test-model"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var conv store.ChatConversation
	decodeInto(t, rec, &conv)

	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": "Say hello"})
	runID := sent["run_id"].(string)
	_, done := chatEvents(t, h, sess, runID, "")
	if done["content"] != "Hello" || done["status"] != store.ChatStatusComplete {
		t.Fatalf("done = %v", done)
	}
	if done["tokens_in"].(float64) != 11 || done["tokens_out"].(float64) != 7 {
		t.Errorf("usage not attached: %v", done)
	}
	if done["cost_usd"].(float64) <= 0 {
		t.Errorf("cost not attached: %v", done)
	}

	// The upstream saw a normal streaming chat request with the history.
	last := h.upstreamRequests[len(h.upstreamRequests)-1]
	if !strings.Contains(string(last.Body), `"content":"Say hello"`) || !strings.Contains(string(last.Body), `"stream":true`) {
		t.Errorf("upstream body = %s", last.Body)
	}

	// Metered as janus-chat, against the user's hidden chat token.
	ev := h.waitForUsageEvents(1)[0]
	if ev.ClientApp != chatClientApp || ev.UserID != h.user.ID || ev.TokenID == "" {
		t.Errorf("usage event = client_app %q user %q token %q", ev.ClientApp, ev.UserID, ev.TokenID)
	}
	tok, err := h.store.TokenByID(context.Background(), ev.TokenID)
	if err != nil || tok.Kind != store.TokenKindChat {
		t.Fatalf("token = %+v err %v", tok, err)
	}
	// ...which is never listed on the Tokens page.
	if strings.Contains(h.doAsSession(sess, http.MethodGet, "/api/v1/tokens", nil).Body.String(), tok.ID) {
		t.Error("hidden chat token listed on /api/v1/tokens")
	}
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/tokens/" + tok.ID},
		{http.MethodDelete, "/api/v1/tokens/" + tok.ID},
		{http.MethodGet, "/api/v1/dashboard/tokens/" + tok.ID},
	} {
		if rec := h.doAsSession(sess, probe.method, probe.path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the hidden chat token = %d, want 404", probe.method, probe.path, rec.Code)
		}
	}
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/tokens/"+tok.ID+"/team", map[string]any{"team_id": "", "move_history": false}); rec.Code != http.StatusNotFound {
		t.Errorf("re-teaming the hidden chat token = %d, want 404", rec.Code)
	}

	// Stored encrypted.
	m, err := h.store.ChatMessage(context.Background(), done["id"].(string))
	if err != nil || m.ContentEnc == "" || strings.Contains(m.ContentEnc, "Hello") {
		t.Errorf("content stored as %q (err %v)", m.ContentEnc, err)
	}

	// A follow-up carries the whole branch as context.
	sent = chatSend(t, h, sess, conv.ID, map[string]any{"content": "Again"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	last = h.upstreamRequests[len(h.upstreamRequests)-1]
	for _, want := range []string{`"Say hello"`, `"Hello"`, `"Again"`} {
		if !strings.Contains(string(last.Body), want) {
			t.Errorf("follow-up history missing %s: %s", want, last.Body)
		}
	}

	// Reconnect resumes from an offset without repeating text.
	deltas, _ := chatEvents(t, h, sess, sent["run_id"].(string), "?content_from=3")
	if len(deltas) != 1 || deltas[0]["content"] != "lo" {
		t.Errorf("resume deltas = %v", deltas)
	}
}

// TestChatBillsTheWorkingUnderTeam: chat bills whatever the "Working under"
// selector (session active team) says at send time, and the model list the
// picker reads follows the same selection.
func TestChatBillsTheWorkingUnderTeam(t *testing.T) {
	h, sess := chatHarness(t)
	ctx := context.Background()
	team, err := h.store.CreateTeam(ctx, "Household", "")
	if err != nil {
		t.Fatalf("team: %v", err)
	}
	if err := h.store.SetTeamMembers(ctx, team.ID, []string{h.user.ID}); err != nil {
		t.Fatalf("member: %v", err)
	}
	models := func() int {
		var cat struct {
			Models []map[string]any `json:"models"`
		}
		decodeInto(t, h.doAsSession(sess, http.MethodGet, "/api/v1/models", nil), &cat)
		return len(cat.Models)
	}
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "test-model"})
	var conv store.ChatConversation
	decodeInto(t, rec, &conv)

	// Personal: the all-users grant applies.
	if models() != 1 {
		t.Fatalf("personal picker = %d models", models())
	}
	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": "hi"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")

	// Switch "Working under" to the team: the model is not granted to
	// teams, so the picker offers nothing and a send names the blocker.
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/me/active-team", map[string]any{"team_id": team.ID}); rec.Code != http.StatusOK {
		t.Fatalf("set active team: %d %s", rec.Code, rec.Body.String())
	}
	if models() != 0 {
		t.Errorf("team picker offered %d models the team is not granted", models())
	}
	if _, err := h.store.CreateGrant(ctx, h.model.ID, store.ModelKindModel, store.GranteeTeam, team.ID); err != nil {
		t.Fatal(err)
	}
	h.server.configCache.invalidate()
	if models() != 1 {
		t.Fatalf("team picker after grant = %d", models())
	}
	sent = chatSend(t, h, sess, conv.ID, map[string]any{"content": "again"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["team_id"] != team.ID {
		t.Errorf("message billed to %v, want %s", done["team_id"], team.ID)
	}
	events := h.waitForUsageEvents(2)
	if events[0].TeamIDs != team.ID || events[1].TeamIDs != "" {
		t.Errorf("usage team_ids = [%q %q], want [team, personal] newest first", events[0].TeamIDs, events[1].TeamIDs)
	}
}

// TestChatErrorsNameTheBlocker: an ungranted model is a stored error with
// the proxy's code, not a silent empty reply.
func TestChatErrorsNameTheBlocker(t *testing.T) {
	h, sess := chatHarness(t)
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "no-such-model"})
	var conv store.ChatConversation
	decodeInto(t, rec, &conv)
	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": "hi"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["status"] != store.ChatStatusError || done["error_code"] == "" || done["error_message"] == "" {
		t.Errorf("done = %v", done)
	}
}

// TestChatEditForksABranch: sending with an earlier parent creates a
// sibling and the context excludes the abandoned branch.
func TestChatEditForksABranch(t *testing.T) {
	h, sess := chatHarness(t)
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "test-model"})
	var conv store.ChatConversation
	decodeInto(t, rec, &conv)
	first := chatSend(t, h, sess, conv.ID, map[string]any{"content": "original question"})
	chatEvents(t, h, sess, first["run_id"].(string), "")
	edited := chatSend(t, h, sess, conv.ID, map[string]any{"content": "edited question", "parent_id": ""})
	chatEvents(t, h, sess, edited["run_id"].(string), "")
	last := string(h.upstreamRequests[len(h.upstreamRequests)-1].Body)
	if strings.Contains(last, "original question") || !strings.Contains(last, "edited question") {
		t.Errorf("edited branch context = %s", last)
	}
	rec = h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+conv.ID, nil)
	var got struct {
		Messages []chatMessageView `json:"messages"`
	}
	decodeInto(t, rec, &got)
	if len(got.Messages) != 4 {
		t.Errorf("messages = %d, want 4 (two branches)", len(got.Messages))
	}
}

// TestChatReaperInterruptsOrphanedRuns: a run whose owner stopped
// heart-beating (pod died) becomes interrupted with a named reason.
func TestChatReaperInterruptsOrphanedRuns(t *testing.T) {
	h, _ := chatHarness(t)
	ctx := context.Background()
	c := &store.ChatConversation{UserID: h.user.ID, ModelName: "test-model"}
	if err := h.store.CreateChatConversation(ctx, c); err != nil {
		t.Fatal(err)
	}
	m := &store.ChatMessage{ConversationID: c.ID, Role: store.ChatRoleAssistant, Status: store.ChatStatusStreaming}
	if err := h.store.InsertChatMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	run := &store.ChatRun{MessageID: m.ID, ConversationID: c.ID, UserID: h.user.ID, Status: store.ChatStatusStreaming}
	if err := h.store.InsertChatRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if n, err := h.store.ReapChatRuns(ctx, time.Now().Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("reaped %d err %v", n, err)
	}
	got, _ := h.store.ChatMessage(ctx, m.ID)
	if got.Status != store.ChatStatusInterrupted || got.ErrorMessage == "" {
		t.Errorf("message = %+v", got)
	}
}

// TestChatNeedsSessionAndFlag: bearer tokens cannot drive chat, and the
// whole surface 404s while the preview is off.
func TestChatNeedsSessionAndFlag(t *testing.T) {
	h, sess := chatHarness(t)
	if rec := h.do(http.MethodGet, "/api/v1/chat/conversations", nil); rec.Code != http.StatusForbidden {
		t.Errorf("bearer = %d", rec.Code)
	}
	h.server.Config.ChatPreview = false
	if rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations", nil); rec.Code != http.StatusNotFound {
		t.Errorf("flag off = %d", rec.Code)
	}
}
