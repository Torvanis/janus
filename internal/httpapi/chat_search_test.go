package httpapi

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/store"
)

func chatNewConversation(t *testing.T, h *harness, sess *auth.Session) store.ChatConversation {
	t.Helper()
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "test-model"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var conv store.ChatConversation
	decodeInto(t, rec, &conv)
	return conv
}

type chatSearchResponse struct {
	Results []chatSearchResult `json:"results"`
}

func chatSearchAs(t *testing.T, h *harness, sess *auth.Session, q string) []chatSearchResult {
	t.Helper()
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/search?q="+strings.ReplaceAll(q, " ", "+"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search %q: %d %s", q, rec.Code, rec.Body.String())
	}
	var out chatSearchResponse
	decodeInto(t, rec, &out)
	return out.Results
}

// TestChatSearchFindsOwnMessagesOnly: whole-word (stemmed) AND search over
// encrypted chats returns a decrypted snippet, never another user's chat,
// and nothing readable is stored in the index or the message rows.
func TestChatSearchFindsOwnMessagesOnly(t *testing.T) {
	h, sess := chatHarness(t)
	ctx := context.Background()
	conv := chatNewConversation(t, h, sess)
	question := "How do I repot an orchid without damaging the aerial roots?"
	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": question})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")

	res := chatSearchAs(t, h, sess, "Orchids roots")
	if len(res) != 1 || res[0].ConversationID != conv.ID || !strings.Contains(res[0].Snippet, "orchid") || res[0].Title == "" {
		t.Fatalf("search = %+v", res)
	}
	// AND: a word that is nowhere in the chat empties the result.
	if res := chatSearchAs(t, h, sess, "orchid volcano"); len(res) != 0 {
		t.Fatalf("AND search = %+v", res)
	}
	// Whole words only: a fragment does not match.
	if res := chatSearchAs(t, h, sess, "orch"); len(res) != 0 {
		t.Fatalf("substring matched: %+v", res)
	}
	// The finished reply is indexed too.
	if res := chatSearchAs(t, h, sess, "hello"); len(res) != 1 || !strings.Contains(res[0].Snippet, "Hello") {
		t.Fatalf("reply search = %+v", res)
	}
	if res := chatSearchAs(t, h, sess, "a"); len(res) != 0 {
		t.Fatalf("too-short query = %+v", res)
	}

	// Another user with the same words finds only their own chat.
	other, _, err := h.store.UpsertUserFromIdentity(ctx, "sub-other", "other@example.com", "Other", true, false)
	if err != nil {
		t.Fatal(err)
	}
	otherSess, err := h.server.Sessions.Create(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res := chatSearchAs(t, h, otherSess, "orchid roots"); len(res) != 0 {
		t.Fatalf("user B sees user A's chat: %+v", res)
	}
	oc := chatNewConversation(t, h, otherSess)
	chatEvents(t, h, otherSess, chatSend(t, h, otherSess, oc.ID, map[string]any{"content": "orchid roots are aerial"})["run_id"].(string), "")
	res = chatSearchAs(t, h, otherSess, "orchid roots")
	if len(res) != 1 || res[0].ConversationID != oc.ID {
		t.Fatalf("user B search = %+v", res)
	}
	if res := chatSearchAs(t, h, sess, "orchid roots"); len(res) != 1 || res[0].ConversationID != conv.ID {
		t.Fatalf("user A search after B wrote = %+v", res)
	}

	// Nothing readable at rest: message bodies are envelopes and the
	// index holds only hashes, which differ per user for the same word.
	m, err := h.store.ChatMessage(ctx, done["id"].(string))
	if err != nil || strings.Contains(m.ContentEnc, "Hello") {
		t.Fatalf("reply stored as %q (%v)", m.ContentEnc, err)
	}
	all, _ := h.store.ListChatMessages(ctx, conv.ID)
	for _, msg := range all {
		if strings.Contains(strings.ToLower(msg.ContentEnc), "orchid") {
			t.Fatalf("plaintext in content_enc: %q", msg.ContentEnc)
		}
	}
	terms := chatSearchTermRows(t, h)
	if len(terms) == 0 {
		t.Fatal("no index rows written")
	}
	byUser := map[string]map[string]bool{}
	for _, r := range terms {
		for _, word := range []string{"orchid", "root", "aerial", "hello", "repot"} {
			if strings.Contains(r.hash, word) {
				t.Fatalf("plaintext in chat_search_term: %+v", r)
			}
		}
		if byUser[r.user] == nil {
			byUser[r.user] = map[string]bool{}
		}
		byUser[r.user][r.hash] = true
	}
	for hsh := range byUser[h.user.ID] {
		if byUser[other.ID][hsh] {
			t.Fatalf("the same hash %s appears for two users", hsh)
		}
	}

	// Deleting the conversation removes it from search.
	if rec := h.doAsSession(sess, http.MethodDelete, "/api/v1/chat/conversations/"+conv.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if res := chatSearchAs(t, h, sess, "orchid"); len(res) != 0 {
		t.Fatalf("deleted chat still found: %+v", res)
	}
	// Bearer tokens cannot search.
	if rec := h.do(http.MethodGet, "/api/v1/chat/search?q=orchid", nil); rec.Code != http.StatusForbidden {
		t.Errorf("bearer search = %d", rec.Code)
	}
}

type chatTermRow struct{ user, hash, message string }

func chatSearchTermRows(t *testing.T, h *harness) []chatTermRow {
	t.Helper()
	rows, err := h.store.DB().QueryContext(context.Background(), `SELECT user_id, term_hash, message_id FROM chat_search_term`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []chatTermRow
	for rows.Next() {
		var r chatTermRow
		if err := rows.Scan(&r.user, &r.hash, &r.message); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestChatReindexRebuildsTheIndex: the admin reindex rebuilds every user's
// index from decrypted content (after a wipe, a key change or an upgrade).
func TestChatReindexRebuildsTheIndex(t *testing.T) {
	h, sess := chatHarness(t)
	conv := chatNewConversation(t, h, sess)
	chatEvents(t, h, sess, chatSend(t, h, sess, conv.ID, map[string]any{"content": "kayak paddling tips"})["run_id"].(string), "")
	if _, err := h.store.DB().ExecContext(context.Background(), `DELETE FROM chat_search_term`); err != nil {
		t.Fatal(err)
	}
	if res := chatSearchAs(t, h, sess, "kayak"); len(res) != 0 {
		t.Fatalf("wiped index still finds: %+v", res)
	}
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/chat/reindex", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("reindex: %d %s", rec.Code, rec.Body.String())
	}
	var counts map[string]float64
	decodeInto(t, rec, &counts)
	if counts["messages_indexed"] != 2 || counts["titles_indexed"] != 1 || counts["unreadable"] != 0 {
		t.Fatalf("counts = %v", counts)
	}
	res := chatSearchAs(t, h, sess, "kayaks paddle")
	if len(res) != 1 || !strings.Contains(res[0].Snippet, "kayak") {
		t.Fatalf("after reindex = %+v", res)
	}
	// Non-admins cannot trigger it.
	member, _, err := h.store.UpsertUserFromIdentity(context.Background(), "sub-member", "member@example.com", "Member", false, false)
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := h.server.Sessions.Create(context.Background(), member.ID)
	if rec := h.doAsSession(ms, http.MethodPost, "/api/v1/admin/chat/reindex", map[string]any{}); rec.Code != http.StatusForbidden {
		t.Errorf("member reindex = %d", rec.Code)
	}
}

func waitForChatTitle(t *testing.T, h *harness, sess *auth.Session, id string, want func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var title string
	for time.Now().Before(deadline) {
		var got struct {
			Conversation store.ChatConversation `json:"conversation"`
		}
		decodeInto(t, h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+id, nil), &got)
		title = got.Conversation.Title
		if want(title) {
			return title
		}
		time.Sleep(25 * time.Millisecond)
	}
	return title
}

// TestChatTrimsHistoryToTheContextWindow: with a small window the oldest
// turns are left out, the question is kept, and the count is recorded.
func TestChatTrimsHistoryToTheContextWindow(t *testing.T) {
	h, sess := chatHarness(t)
	conv := chatNewConversation(t, h, sess)
	for _, word := range []string{"alpha", "bravo"} {
		body := word + " " + strings.Repeat("x", 1000)
		chatEvents(t, h, sess, chatSend(t, h, sess, conv.ID, map[string]any{"content": body})["run_id"].(string), "")
	}
	if err := h.store.SetModelContextWindow(context.Background(), h.model.ID, 2000); err != nil {
		t.Fatal(err)
	}
	_, done := chatEvents(t, h, sess, chatSend(t, h, sess, conv.ID, map[string]any{"content": "charlie final question"})["run_id"].(string), "")
	last := string(h.upstreamRequests[len(h.upstreamRequests)-1].Body)
	if strings.Contains(last, "alpha") || !strings.Contains(last, "charlie final question") {
		t.Fatalf("trimmed request = %.300s", last)
	}
	dropped := done["context_dropped"].(float64)
	if dropped < 1 {
		t.Fatalf("context_dropped = %v", dropped)
	}
	var got struct {
		Messages []chatMessageView `json:"messages"`
	}
	decodeInto(t, h.doAsSession(sess, http.MethodGet, "/api/v1/chat/conversations/"+conv.ID, nil), &got)
	if lastMsg := got.Messages[len(got.Messages)-1]; lastMsg.ContextDropped != int(dropped) {
		t.Fatalf("stored context_dropped = %d, want %v", lastMsg.ContextDropped, dropped)
	}
	// Unknown window (0): nothing is dropped.
	if err := h.store.SetModelContextWindow(context.Background(), h.model.ID, 0); err != nil {
		t.Fatal(err)
	}
	_, done = chatEvents(t, h, sess, chatSend(t, h, sess, conv.ID, map[string]any{"content": "delta"})["run_id"].(string), "")
	if done["context_dropped"].(float64) != 0 || !strings.Contains(string(h.upstreamRequests[len(h.upstreamRequests)-1].Body), "alpha") {
		t.Fatalf("unknown window trimmed: %v", done["context_dropped"])
	}
}

func TestTrimChatHistory(t *testing.T) {
	msg := func(role string, chars int, tag string) chatWireMessage {
		return chatWireMessage{Role: role, Content: tag + strings.Repeat("x", chars)}
	}
	// withParts is a user message carrying files: tag text plus the parts.
	withParts := func(tag string, parts ...chatPart) chatWireMessage {
		return chatWireMessage{Role: "user", Content: append([]chatPart{{Type: "text", Text: tag}}, parts...)}
	}
	doc := func(estTokens int64) chatPart {
		return chatPart{Type: "text", Text: "<file name=\"a.txt\">\nshort\n</file>", est: estTokens}
	}
	img := chatPart{Type: "image_url", img: &store.ChatAttachment{ID: "i1"}}
	tags := func(h []chatWireMessage) []string {
		out := []string{}
		for _, m := range h {
			switch c := m.Content.(type) {
			case string:
				out = append(out, c[:2])
			case []chatPart:
				out = append(out, c[0].Text[:2])
			}
		}
		return out
	}
	// window 2000 → budget 2000-1024-512 = 464 tokens.
	cases := []struct {
		name    string
		history []chatWireMessage
		window  int64
		want    []string
		dropped int
	}{
		{"unknown window", []chatWireMessage{msg("user", 10000, "u1"), msg("user", 10, "u2")}, 0, []string{"u1", "u2"}, 0},
		{"fits", []chatWireMessage{msg("user", 400, "u1"), msg("assistant", 400, "a1"), msg("user", 400, "u2")}, 2000, []string{"u1", "a1", "u2"}, 0},
		{"drops oldest", []chatWireMessage{msg("user", 1000, "u1"), msg("assistant", 400, "a1"), msg("user", 1000, "u2")}, 2000, []string{"a1", "u2"}, 1},
		{"keeps system", []chatWireMessage{msg("system", 200, "s1"), msg("user", 1000, "u1"), msg("assistant", 1000, "a1"), msg("user", 200, "u2")}, 2000, []string{"s1", "a1", "u2"}, 1},
		{"latest alone too big", []chatWireMessage{msg("user", 100, "u1"), msg("user", 9000, "u2")}, 2000, []string{"u2"}, 1},
		{"single message", []chatWireMessage{msg("user", 9000, "u1")}, 2000, []string{"u1"}, 0},
		{"big window keeps 1/8 for reply", []chatWireMessage{msg("user", 4*120000, "u1"), msg("user", 10, "u2")}, 128000, []string{"u2"}, 1},
		// A document counts by its recorded estimate, not its (short) wire text.
		{"document counts its estimate", []chatWireMessage{withParts("u1", doc(600)), msg("assistant", 40, "a1"), msg("user", 40, "u2")}, 2000, []string{"a1", "u2"}, 1},
		{"small document fits", []chatWireMessage{withParts("u1", doc(100)), msg("user", 40, "u2")}, 2000, []string{"u1", "u2"}, 0},
		// An image is a flat 1000 tokens: in a 2000 window it pushes out the turn.
		{"image is a flat 1000", []chatWireMessage{withParts("u1", img), msg("user", 40, "u2")}, 2000, []string{"u2"}, 1},
		{"image fits a big window", []chatWireMessage{withParts("u1", img), msg("user", 40, "u2")}, 8000, []string{"u1", "u2"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := trimChatHistory(tc.history, tc.window)
			if !reflect.DeepEqual(tags(got), tc.want) || dropped != tc.dropped {
				t.Fatalf("got %v dropped %d, want %v dropped %d", tags(got), dropped, tc.want, tc.dropped)
			}
		})
	}
	if got := chatEstimateTokens([]chatWireMessage{withParts("", doc(250), img), msg("user", 400, "")}); got != (4+0+250+4+1000)+(4+100) {
		t.Fatalf("estimate = %d", got)
	}
}

func TestCleanChatTitle(t *testing.T) {
	cases := map[string]string{
		`"Spring Tomato Planting"`:                      "Spring Tomato Planting",
		"Title: Repotting Orchids.":                     "Repotting Orchids",
		"\n\n  **Kayak Tips**  \nextra line":            "Kayak Tips",
		"one two three four five six seven eight nine":  "one two three four five six",
		"“Curly quotes!”":                               "Curly quotes",
		"   ":                                           "",
		"<think>user wants a title</think>\nKyoto Trip": "Kyoto Trip",
		"<think>still thinking about":                   "",
	}
	for in, want := range cases {
		if got := cleanChatTitle(in); got != want {
			t.Errorf("cleanChatTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
