package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/store"
)

// fakeSearchProvider is an xAI-like upstream: chat completions refuses
// hosted tools, the Responses API runs them and streams search activity,
// citations and text.
type fakeSearchProvider struct {
	mu           sync.Mutex
	paths        []string
	tools        []string
	instructions string
}

const fakeSearchSSE = `data: {"type":"response.created","response":{"id":"resp_1","model":"test-model"}}

data: {"type":"response.output_item.added","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}}

data: {"type":"response.output_item.done","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"news today","sources":[{"type":"url","url":"https://apnews.com/a"}]}}}

data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message"}}

data: {"type":"response.output_text.delta","output_index":1,"delta":"Talks resume today."}

data: {"type":"response.output_text.annotation.added","output_index":1,"annotation":{"type":"url_citation","url":"https://apnews.com/a","title":"Talks resume | AP","start_index":0,"end_index":19}}

data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":120,"output_tokens":9,"cost_in_usd_ticks":120000000}}}

`

func (f *fakeSearchProvider) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Tools []struct {
				Type string `json:"type"`
			} `json:"tools"`
			Instructions string `json:"instructions"`
		}
		_ = json.Unmarshal(body, &in)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.instructions = in.Instructions
		f.tools = nil
		hosted := 0
		for _, tl := range in.Tools {
			// Provider-run tools only: Janus's own function tools
			// (create_file, memory) ride along on every request.
			if tl.Type != "function" {
				f.tools = append(f.tools, tl.Type)
				hosted++
			}
		}
		f.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			if hosted > 0 {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`tools[0].type: unknown variant web_search`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"No tools here.\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(fakeSearchSSE))
	}
}

// searchHarness points the test model's upstream at the fake provider and
// registers its host as one that runs web and X search.
func searchHarness(t *testing.T) (*harness, *fakeSearchProvider) {
	h, _ := chatHarness(t)
	f := &fakeSearchProvider{}
	up := repointUpstream(t, h, f.handler(t))
	u, _ := url.Parse(up.URL)
	adapter.HostedToolHosts[u.Hostname()] = map[string]bool{"web_search": true, "x_search": true}
	t.Cleanup(func() { delete(adapter.HostedToolHosts, u.Hostname()) })
	return h, f
}

// A chat whose model's provider runs search: Chat offers web and X search,
// the proxy serves it through the provider's Responses API, and the reply
// keeps what was searched and the sources it cited. The exact provider
// cost (search fees included) is what is metered.
func TestChatUsesProviderSearch(t *testing.T) {
	h, f := searchHarness(t)
	sess := adminSession(t, h)
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "test-model"})
	var conv store.ChatConversation
	decodeInto(t, rec, &conv)

	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": "What's in the news today?"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["content"] != "Talks resume today." {
		t.Fatalf("done = %v", done)
	}
	f.mu.Lock()
	gotPath, gotTools := f.paths[len(f.paths)-1], strings.Join(f.tools, ",")
	f.mu.Unlock()
	if !strings.HasSuffix(gotPath, "/responses") || gotTools != "web_search,x_search" {
		t.Fatalf("upstream got %s tools %q, want /responses web_search,x_search", gotPath, gotTools)
	}
	srcs, _ := done["sources"].([]any)
	if len(srcs) != 1 || srcs[0].(map[string]any)["url"] != "https://apnews.com/a" || srcs[0].(map[string]any)["title"] != "Talks resume | AP" {
		t.Fatalf("sources = %v", done["sources"])
	}
	acts, _ := done["activity"].([]any)
	if len(acts) != 1 || acts[0].(map[string]any)["status"] != "done" || acts[0].(map[string]any)["query"] != "news today" {
		t.Fatalf("activity = %v", done["activity"])
	}
	// xAI's exact cost (ticks) is metered: 120,000,000 ticks = $0.012.
	if c := done["cost_usd"].(float64); c < 0.0119 || c > 0.0121 {
		t.Fatalf("cost_usd = %v, want 0.012 (provider-reported)", c)
	}
	// The model was told it can search.
	f.mu.Lock()
	sys := f.instructions
	f.mu.Unlock()
	if !strings.Contains(sys, "You can use web search and X (Twitter) search") {
		t.Errorf("system note missing: %s", sys)
	}
	// Stored encrypted with the message.
	m, _ := h.store.ChatMessage(context.Background(), done["id"].(string))
	if m.ExtrasEnc == "" || strings.Contains(m.ExtrasEnc, "apnews") {
		t.Fatalf("extras stored as %q", m.ExtrasEnc)
	}
}

// Turning a tool off — for this chat, or for everyone — stops it being sent;
// with every tool off the chat goes to chat completions as before.
func TestChatToolsCanBeTurnedOff(t *testing.T) {
	h, f := searchHarness(t)
	sess := adminSession(t, h)
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/chat/conversations", map[string]any{"model": "test-model"})
	var conv store.ChatConversation
	decodeInto(t, rec, &conv)
	if rec := h.doAsSession(sess, http.MethodPatch, "/api/v1/chat/conversations/"+conv.ID, map[string]any{"tools_off": []string{"x_search"}}); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	sent := chatSend(t, h, sess, conv.ID, map[string]any{"content": "news?"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	f.mu.Lock()
	tools := strings.Join(f.tools, ",")
	f.mu.Unlock()
	if tools != "web_search" {
		t.Fatalf("tools with x_search off for the chat = %q", tools)
	}

	// Admin turns web search off for everyone: nothing left, plain chat.
	var doc map[string]any
	decodeInto(t, h.doAsSession(sess, http.MethodGet, "/api/v1/admin/chat", nil), &doc)
	doc["tools_off"] = []string{"web_search"}
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/chat", doc); rec.Code != http.StatusOK {
		t.Fatalf("admin put: %d %s", rec.Code, rec.Body.String())
	}
	h.server.chatAdminMu.Lock()
	h.server.chatAdminCache = nil
	h.server.chatAdminMu.Unlock()
	sent = chatSend(t, h, sess, conv.ID, map[string]any{"content": "news again?"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	f.mu.Lock()
	path, tools := f.paths[len(f.paths)-1], strings.Join(f.tools, ",")
	f.mu.Unlock()
	if !strings.HasSuffix(path, "/chat/completions") || tools != "" || done["content"] != "No tools here." {
		t.Fatalf("with every tool off: path %s tools %q content %v", path, tools, done["content"])
	}
	if rec := h.doAsSession(sess, http.MethodPatch, "/api/v1/chat/conversations/"+conv.ID, map[string]any{"tools_off": []string{"teleport"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown tool accepted: %d", rec.Code)
	}
}

// The picker learns which tools each model has; a provider that does not
// run search gets none (and the API refuses a hosted tool it cannot run).
func TestChatCatalogListsToolsOnlyWhereTheProviderRunsThem(t *testing.T) {
	h, _ := searchHarness(t)
	sess := adminSession(t, h)
	var cat struct {
		Models []struct {
			Name  string   `json:"name"`
			Tools []string `json:"tools"`
		} `json:"models"`
		Tools []struct {
			ID string `json:"id"`
		} `json:"tools"`
	}
	decodeInto(t, h.doAsSession(sess, http.MethodGet, "/api/v1/chat/catalog", nil), &cat)
	found := false
	for _, m := range cat.Models {
		if m.Name == "test-model" {
			found = strings.Join(m.Tools, ",") == "web_search,x_search"
		}
	}
	if !found || len(cat.Tools) != len(store.ChatTools) {
		t.Fatalf("catalog = %+v", cat)
	}

	// A provider host that runs no search (every fake shares 127.0.0.1).
	for host := range adapter.HostedToolHosts {
		if host == "127.0.0.1" {
			delete(adapter.HostedToolHosts, host)
		}
	}
	plain, _ := chatHarness(t)
	var plainCat struct {
		Models []struct {
			Name  string   `json:"name"`
			Tools []string `json:"tools"`
		} `json:"models"`
	}
	decodeInto(t, plain.doAsSession(adminSession(t, plain), http.MethodGet, "/api/v1/chat/catalog", nil), &plainCat)
	for _, m := range plainCat.Models {
		if len(m.Tools) > 0 {
			t.Fatalf("a provider that runs no search got tools: %+v", m)
		}
	}
	rec := plain.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "test-model",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "tools": []any{map[string]any{"type": "web_search"}}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "does not run the web_search tool") {
		t.Fatalf("hosted tool on a provider that cannot run it: %d %s", rec.Code, rec.Body.String())
	}
}

// A chat model asked for a picture calls create_image: Janus makes it with
// the chat's image model (a separate metered image request), shows it on
// the same reply, and the model then finishes its answer knowing it exists.
func TestChatModelMakesImagesWithTheImageTool(t *testing.T) {
	h, sess := chatHarness(t)
	addImageModel(t, h, "test-image")
	f := newFakeImageUpstream(t, h)
	f.callImageTool = true

	conv := chatConversationFor(t, h, sess, "test-model")
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "Make me a picture of a lion on a cliff"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["status"] != store.ChatStatusComplete {
		t.Fatalf("done = %v", done)
	}
	if len(f.prompts) != 1 || f.prompts[0] != "a lion on a cliff" {
		t.Fatalf("image prompts = %q", f.prompts)
	}
	if atts, _ := done["attachments"].([]any); len(atts) != 1 {
		t.Fatalf("reply attachments = %v", done["attachments"])
	}
	if c, _ := done["content"].(string); !strings.Contains(c, "Here you go.") || !strings.Contains(c, "Nice image.") {
		t.Fatalf("content = %q", done["content"])
	}
	acts, _ := done["activity"].([]any)
	if len(acts) != 1 || acts[0].(map[string]any)["type"] != "create_image" || acts[0].(map[string]any)["status"] != "done" {
		t.Fatalf("activity = %v", done["activity"])
	}
	// The follow-up carried the call and its result.
	last := string(f.chats[len(f.chats)-1])
	if !strings.Contains(last, `"role":"tool"`) || !strings.Contains(last, `"tool_call_id":"call_1"`) || !strings.Contains(last, "Created 1 image(s) with test-image") {
		t.Fatalf("follow-up = %s", last)
	}
	// Turned off for the chat: not offered.
	if rec := h.doAsSession(sess, http.MethodPatch, "/api/v1/chat/conversations/"+conv, map[string]any{"tools_off": []string{"create_image"}}); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d", rec.Code)
	}
	sent = chatSend(t, h, sess, conv, map[string]any{"content": "Another one"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	if strings.Contains(string(f.chats[len(f.chats)-1]), "create_image") {
		t.Fatal("create_image offered after it was turned off for the chat")
	}
}

// A model whose provider runs no search (a local model) gets Janus-run web
// search once the administrator sets up a search service: the model calls
// web_search, Janus searches (here a fake SearXNG), the model answers with
// the results, and the reply shows the search and its sources. Off until
// set up; the API key is never sent back to the console or written to the
// audit log.
func TestChatJanusRunSearchForLocalModels(t *testing.T) {
	h, sess := chatHarness(t)
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Talks resume","url":"https://apnews.com/a","content":"Peace talks resume today."}]}`))
	}))
	t.Cleanup(searx.Close)
	var mu sync.Mutex
	var bodies []string
	repointUpstream(t, h, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(b), `"name":"web_search"`) && !strings.Contains(string(b), `"role":"tool"`) {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_s\",\"type\":\"function\",\"function\":{\"name\":\"web_search\",\"arguments\":\"{\\\"query\\\":\\\"news today\\\"}\"}}]}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Talks resume today [apnews.com](https://apnews.com/a).\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":9}}\n\ndata: [DONE]\n\n"))
	})
	conv := chatConversationFor(t, h, sess, "test-model")

	// Off by default: no search tool offered.
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "news?"})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	if strings.Contains(bodies[len(bodies)-1], `"web_search"`) {
		t.Fatal("web_search offered before an administrator set up search")
	}

	// Set up Brave with a key: stored encrypted, never echoed.
	var doc map[string]any
	decodeInto(t, h.doAsSession(sess, http.MethodGet, "/api/v1/admin/chat", nil), &doc)
	doc["search"] = map[string]any{"enabled": true, "provider": "brave"}
	doc["search_key"] = "BSA-secret-key-123"
	rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/chat", doc)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "BSA-secret") || !strings.Contains(rec.Body.String(), `"brave":true`) {
		t.Fatalf("put brave: %d %s", rec.Code, rec.Body.String())
	}
	if get := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/chat", nil).Body.String(); strings.Contains(get, "BSA-secret") || strings.Contains(get, `"keys"`) {
		t.Fatalf("key leaked on GET: %s", get)
	}
	saved, _ := h.store.ChatAdminSettings(context.Background())
	enc := saved.Search.Keys["brave"]
	if enc == "" || strings.Contains(enc, "BSA-secret") || h.server.chatDecrypt(enc) != "BSA-secret-key-123" {
		t.Fatalf("key not stored encrypted: %q", enc)
	}
	var leaked int
	if err := h.store.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'chat_settings_updated' AND (old_value LIKE ? OR new_value LIKE ? OR new_value LIKE ?)`,
		"%"+enc+"%", "%"+enc+"%", "%BSA-secret%").Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatal("search key (or its ciphertext) written to the audit log")
	}
	// Turning it on without a key is refused with a clear message.
	doc2 := map[string]any{"search": map[string]any{"enabled": true, "provider": "exa"}}
	for k, v := range doc {
		if k != "search" && k != "search_key" {
			doc2[k] = v
		}
	}
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/chat", doc2); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "API key") {
		t.Fatalf("exa without key: %d %s", rec.Code, rec.Body.String())
	}

	// Switch to SearXNG (keyless) at the fake.
	doc["search"] = map[string]any{"enabled": true, "provider": "searxng", "url": searx.URL}
	delete(doc, "search_key")
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/chat", doc); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"brave":true`) {
		t.Fatalf("put searxng (brave key kept): %d %s", rec.Code, rec.Body.String())
	}
	h.server.chatAdminMu.Lock()
	h.server.chatAdminCache = nil
	h.server.chatAdminMu.Unlock()

	// The admin test button.
	var test map[string]any
	decodeInto(t, h.doAsSession(sess, http.MethodPost, "/api/v1/admin/chat/search-test", map[string]any{"provider": "searxng", "url": searx.URL}), &test)
	if test["ok"] != true {
		t.Fatalf("search test = %v", test)
	}

	sent = chatSend(t, h, sess, conv, map[string]any{"content": "What's in the news?"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if c, _ := done["content"].(string); !strings.Contains(c, "Talks resume today") {
		t.Fatalf("content = %v", done["content"])
	}
	last := bodies[len(bodies)-1]
	if !strings.Contains(last, `"role":"tool"`) || !strings.Contains(last, "Peace talks resume today.") || !strings.Contains(last, "https://apnews.com/a") {
		t.Fatalf("model did not get the results: %s", last)
	}
	srcs, _ := done["sources"].([]any)
	acts, _ := done["activity"].([]any)
	if len(srcs) != 1 || len(acts) != 1 || acts[0].(map[string]any)["query"] != "news today" {
		t.Fatalf("sources %v activity %v", srcs, acts)
	}
}
