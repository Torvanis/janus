package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

const namingReply = `{"id":"t","choices":[{"message":{"role":"assistant","content":"\"Garden Planning: Spring Tomatoes!\""},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":30,"completion_tokens":5}}`

func chatTurn(t *testing.T, h *harness, sess *auth.Session, convID, text string) {
	t.Helper()
	chatEvents(t, h, sess, chatSend(t, h, sess, convID, map[string]any{"content": text})["run_id"].(string), "")
}

func titleCalls(h *harness) []string {
	var out []string
	for _, r := range h.upstreamRequests {
		if strings.Contains(string(r.Body), `"max_tokens":1024`) {
			out = append(out, string(r.Body))
		}
	}
	return out
}

func setNaming(t *testing.T, h *harness, mode, model string) {
	t.Helper()
	cfg, _ := h.store.ChatAdminSettings(context.Background())
	cfg.Naming, cfg.NamingModel = mode, model
	if err := h.store.SetChatAdminSettings(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	h.server.invalidateChatAdmin()
}

// TestChatNamingAfterAFewReplies: the chat is named by its own model after
// the third reply (not the first), as a separate metered request; a second
// round never renames it.
func TestChatNamingAfterAFewReplies(t *testing.T) {
	h, sess := chatHarness(t)
	h.server.Config.ChatAutoTitle = true
	h.upstreamReply = namingReply
	conv := chatNewConversation(t, h, sess)
	chatTurn(t, h, sess, conv.ID, "when should I plant tomatoes")
	chatTurn(t, h, sess, conv.ID, "and peppers?")
	time.Sleep(200 * time.Millisecond)
	if n := len(titleCalls(h)); n != 0 {
		t.Fatalf("named after %d replies, want after %d", 2, chatNamingAfterReplies)
	}
	chatTurn(t, h, sess, conv.ID, "what about basil")
	title := waitForChatTitle(t, h, sess, conv.ID, func(s string) bool { return s == "Garden Planning: Spring Tomatoes" })
	if title != "Garden Planning: Spring Tomatoes" {
		t.Fatalf("title = %q", title)
	}
	calls := titleCalls(h)
	if len(calls) != 1 || !strings.Contains(calls[0], `"model":"test-model"`) || strings.Contains(calls[0], `"stream":true`) {
		t.Fatalf("title calls = %v", calls)
	}
	// It saw the first question, the first answer and the latest question.
	if !strings.Contains(calls[0], "plant tomatoes") || !strings.Contains(calls[0], "basil") {
		t.Errorf("title call missing the excerpt: %s", calls[0])
	}
	events := h.waitForUsageEvents(4)
	found := false
	for _, e := range events {
		found = found || e.ClientApp == chatTitleClientApp && e.UserID == h.user.ID
	}
	if !found {
		t.Error("the naming call was not metered as janus-chat-title")
	}
	if res := chatSearchAs(t, h, sess, "garden"); len(res) != 1 || res[0].ConversationID != conv.ID {
		t.Fatalf("title search = %+v", res)
	}
	chatTurn(t, h, sess, conv.ID, "one more")
	time.Sleep(200 * time.Millisecond)
	if n := len(titleCalls(h)); n != 1 {
		t.Errorf("renamed again: %d naming calls", n)
	}
}

// TestChatNamingWhenIdle: a one-question chat is named by the sweep once
// idle; a chat a person renamed, and a temporary chat, are not.
func TestChatNamingWhenIdle(t *testing.T) {
	h, sess := chatHarness(t)
	h.server.Config.ChatAutoTitle = true
	h.upstreamReply = namingReply
	quiet := chatNewConversation(t, h, sess)
	chatTurn(t, h, sess, quiet.ID, "when should I plant tomatoes")
	renamed := chatNewConversation(t, h, sess)
	chatTurn(t, h, sess, renamed.ID, "something else entirely")
	if rec := h.doAsSession(sess, http.MethodPatch, "/api/v1/chat/conversations/"+renamed.ID, map[string]any{"title": "My own name"}); rec.Code != http.StatusOK {
		t.Fatalf("rename: %d", rec.Code)
	}
	// Not idle yet: nothing.
	h.server.chatSweepNaming(time.Now())
	waitSweep()
	if n := len(titleCalls(h)); n != 0 {
		t.Fatalf("named before idle: %d", n)
	}
	h.server.chatSweepNaming(time.Now().Add(chatNamingIdle + time.Minute))
	waitSweep()
	if got := waitForChatTitle(t, h, sess, quiet.ID, func(s string) bool { return s != "when should I plant tomatoes" }); got != "Garden Planning: Spring Tomatoes" {
		t.Fatalf("idle chat title = %q", got)
	}
	if got := waitForChatTitle(t, h, sess, renamed.ID, func(string) bool { return true }); got != "My own name" {
		t.Fatalf("renamed chat = %q", got)
	}
	if n := len(titleCalls(h)); n != 1 {
		t.Fatalf("naming calls = %d, want 1", n)
	}
	// Swept once: never again.
	h.server.chatSweepNaming(time.Now().Add(chatNamingIdle + 2*time.Minute))
	waitSweep()
	if n := len(titleCalls(h)); n != 1 {
		t.Fatalf("swept twice: %d", n)
	}
}

func waitSweep() {
	for i := 0; i < 200 && chatNamingSweeping.Load(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
}

// TestChatNamingOff: the admin can turn naming off; the first message stays.
func TestChatNamingOff(t *testing.T) {
	h, sess := chatHarness(t)
	h.server.Config.ChatAutoTitle = true
	setNaming(t, h, store.ChatNamingOff, "")
	conv := chatNewConversation(t, h, sess)
	for _, q := range []string{"a question about soil", "two", "three"} {
		chatTurn(t, h, sess, conv.ID, q)
	}
	h.server.chatSweepNaming(time.Now().Add(chatNamingIdle + time.Minute))
	waitSweep()
	if n := len(titleCalls(h)); n != 0 {
		t.Fatalf("naming off made %d calls", n)
	}
	if got := waitForChatTitle(t, h, sess, conv.ID, func(string) bool { return true }); got != "a question about soil" {
		t.Fatalf("title = %q", got)
	}
}

// TestChatNamingByAnotherModel: a model the admin chose sees only what the
// person typed (never a reply), trimmed; a chat the Security Gateway had
// any finding on is never sent to it.
func TestChatNamingByAnotherModel(t *testing.T) {
	h, sess := chatHarness(t)
	namer := newRoutedEngine(t, "namer-model")
	h.addModel(t, namer, 0)
	setNaming(t, h, store.ChatNamingModel, "namer-model")
	h.upstreamReply = `{"id":"r","choices":[{"message":{"role":"assistant","content":"SECRET-REPLY-TEXT"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":3}}`

	clean := chatNewConversation(t, h, sess)
	chatTurn(t, h, sess, clean.ID, "first question "+strings.Repeat("x", 3000))
	chatTurn(t, h, sess, clean.ID, "second")
	// Checked by a policy that found nothing: still a clean chat.
	if _, err := h.store.DB().ExecContext(context.Background(), `UPDATE usage_event SET secgw_action = 'checked'
		WHERE chat_message_id IN (SELECT id FROM chat_message WHERE conversation_id = ?)`, clean.ID); err != nil {
		t.Fatal(err)
	}
	chatTurn(t, h, sess, clean.ID, "latest question about tomatoes")
	waitForChatTitle(t, h, sess, clean.ID, func(s string) bool { return s == "answer from namer-model" })
	if namer.count() != 1 {
		t.Fatalf("namer calls = %d", namer.count())
	}
	namer.mu.Lock()
	body := namer.bodies[0]
	namer.mu.Unlock()
	msgs, _ := body["messages"].([]any)
	var all strings.Builder
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" {
			t.Errorf("another model was sent a reply: %v", mm)
		}
		all.WriteString(mm["content"].(string))
	}
	text := all.String()
	if strings.Contains(text, "SECRET-REPLY-TEXT") {
		t.Error("the chat's replies reached the naming model")
	}
	if !strings.Contains(text, "latest question about tomatoes") || len(text) > 1200 {
		t.Errorf("excerpt (%d chars) = %.200q", len(text), text)
	}

	// A chat with any Security Gateway finding is not named by another model.
	flagged := chatNewConversation(t, h, sess)
	for _, q := range []string{"one", "two", "three"} {
		chatTurn(t, h, sess, flagged.ID, q)
	}
	// Let the third reply's background naming finish first; otherwise a slow
	// run lands its title after the reset below.
	waitForChatTitle(t, h, sess, flagged.ID, func(s string) bool { return s == "answer from namer-model" })
	before := namer.count()
	// Mark one of its requests as observed by the Security Gateway, reset
	// the claim, and let the sweep try again.
	ctx := context.Background()
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE usage_event SET secgw_action = 'observed', secgw_violations = 1
		WHERE chat_message_id IN (SELECT id FROM chat_message WHERE conversation_id = ?)`, flagged.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE chat_conversation SET title = 'one', title_named_at = '' WHERE id = ?`, flagged.ID); err != nil {
		t.Fatal(err)
	}
	h.server.chatSweepNaming(time.Now().Add(chatNamingIdle + time.Minute))
	waitSweep()
	if namer.count() != before {
		t.Fatalf("a flagged chat was sent to the naming model")
	}
	if got := waitForChatTitle(t, h, sess, flagged.ID, func(string) bool { return true }); got != "one" {
		t.Fatalf("flagged chat title = %q", got)
	}
}

func TestChatNamingSettingsValidate(t *testing.T) {
	c := store.DefaultChatAdminSettings()
	c.Naming = store.ChatNamingModel
	if err := c.Validate(); err == nil {
		t.Fatal("model naming without a model was accepted")
	}
	c.Naming = "sometimes"
	c.NamingModel = "x"
	if err := c.Validate(); err == nil {
		t.Fatal("unknown naming mode accepted")
	}
}

// TestChatNamingModelNotGranted: the administrator's naming model is a
// system task; it names chats for people who are not granted it, and the
// call is still metered to them.
func TestChatNamingModelNotGranted(t *testing.T) {
	h, sess := chatHarness(t)
	namer := newRoutedEngine(t, "namer-model")
	h.addModel(t, namer, 0)
	setNaming(t, h, store.ChatNamingModel, "namer-model")
	conv := chatNewConversation(t, h, sess)
	for _, q := range []string{"one", "two", "three"} {
		chatTurn(t, h, sess, conv.ID, q)
	}
	if got := waitForChatTitle(t, h, sess, conv.ID, func(s string) bool { return s != "one" }); got != "answer from namer-model" {
		t.Fatalf("title = %q", got)
	}
	if namer.count() != 1 {
		t.Fatalf("namer calls = %d", namer.count())
	}
	found := false
	for _, e := range h.waitForUsageEvents(4) {
		found = found || e.ClientApp == chatTitleClientApp && e.ModelName == "namer-model" && e.UserID == h.user.ID && e.HTTPStatus == 200
	}
	if !found {
		t.Error("the naming call was not metered to the person")
	}
}

// TestChatNamingGrantFreeIsInternalOnly: the grant exception is the
// in-process naming task only. A caller sending the naming agent header
// itself is still refused a model it is not granted.
func TestChatNamingGrantFreeIsInternalOnly(t *testing.T) {
	h, _ := chatHarness(t)
	namer := newRoutedEngine(t, "namer-model")
	h.addModel(t, namer, 0)
	setNaming(t, h, store.ChatNamingModel, "namer-model")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"namer-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Janus-Agent", chatTitleClientApp)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), CodeModelNotGranted) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if namer.count() != 0 {
		t.Fatal("the ungranted model was called")
	}
}

// TestChatNamingAsksForNoReasoning: naming sends reasoning_effort "none";
// a model that refuses the setting gets the request again without it.
func TestChatNamingAsksForNoReasoning(t *testing.T) {
	h, sess := chatHarness(t)
	var bodies []map[string]any
	e := &routedEngine{name: "plain-model"}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"plain-model"}]}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		if _, ok := b["reasoning_effort"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Argument not supported on this model: reasoning_effort","type":"invalid_request_error","param":"reasoning_effort"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"Plain Title"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	}))
	t.Cleanup(e.srv.Close)
	h.addModel(t, e, 0)
	setNaming(t, h, store.ChatNamingModel, "plain-model")
	conv := chatNewConversation(t, h, sess)
	for _, q := range []string{"one", "two", "three"} {
		chatTurn(t, h, sess, conv.ID, q)
	}
	if got := waitForChatTitle(t, h, sess, conv.ID, func(s string) bool { return s == "Plain Title" }); got != "Plain Title" {
		t.Fatalf("title = %q (calls %d)", got, len(bodies))
	}
	if len(bodies) != 2 || bodies[0]["reasoning_effort"] != "none" || bodies[1]["reasoning_effort"] != nil {
		t.Fatalf("calls = %v", bodies)
	}
}

// TestChatNamingRetriesMaxCompletionTokens: OpenAI's reasoning models
// refuse max_tokens; naming retries once with max_completion_tokens.
func TestChatNamingRetriesMaxCompletionTokens(t *testing.T) {
	h, sess := chatHarness(t)
	var bodies []map[string]any
	e := &routedEngine{name: "gpt-5.6-sol"}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.6-sol"}]}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		if _, ok := b["max_tokens"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"max_tokens","code":"unsupported_parameter"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"Boiling Water At Altitude"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	t.Cleanup(e.srv.Close)
	h.addModel(t, e, 0)
	setNaming(t, h, store.ChatNamingModel, "gpt-5.6-sol")
	conv := chatNewConversation(t, h, sess)
	for _, q := range []string{"boiling point at 2000 m", "and 4000 m", "why for pasta"} {
		chatTurn(t, h, sess, conv.ID, q)
	}
	if got := waitForChatTitle(t, h, sess, conv.ID, func(s string) bool { return s == "Boiling Water At Altitude" }); got != "Boiling Water At Altitude" {
		t.Fatalf("title = %q (calls %d)", got, len(bodies))
	}
	if len(bodies) != 2 || bodies[1]["max_completion_tokens"] == nil || bodies[1]["reasoning_effort"] != "none" {
		t.Fatalf("calls = %v", bodies)
	}
}
