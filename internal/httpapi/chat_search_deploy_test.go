package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A SearXNG set up with the deployment (JANUS_CHAT_SEARCH_SEARXNG_URL)
// turns Chat web search on at startup — once. An administrator's later
// choice survives restarts, and removing the bundled SearXNG turns off a
// search that still points at it.
func TestDeploymentSearXNGTurnsSearchOn(t *testing.T) {
	h, sess := chatHarness(t)
	ctx := context.Background()
	const bundled = "http://janus-searxng:8080"
	h.server.Config.ChatSearchSearXNGURL = bundled
	h.server.ApplyDeploymentChatSearch(ctx)
	cfg := h.server.chatAdmin(ctx).Search
	if !cfg.Enabled || cfg.Provider != "searxng" || cfg.URL != bundled || !cfg.Ready() {
		t.Fatalf("after install: %+v", cfg)
	}
	if audit := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/audit?action=chat_settings_updated", nil).Body.String(); !strings.Contains(audit, `"actor_label":"deployment"`) {
		t.Fatalf("no deployment audit entry: %s", audit)
	}

	// The administrator turns it off (sending a document without the
	// deployment record); a restart keeps it off.
	var doc map[string]any
	decodeInto(t, h.doAsSession(sess, http.MethodGet, "/api/v1/admin/chat", nil), &doc)
	doc["search"] = map[string]any{"enabled": false, "provider": "searxng", "url": bundled}
	if rec := h.doAsSession(sess, http.MethodPut, "/api/v1/admin/chat", doc); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	h.server.ApplyDeploymentChatSearch(ctx)
	if cfg := h.server.chatAdmin(ctx).Search; cfg.Enabled {
		t.Fatalf("restart overrode the administrator: %+v", cfg)
	}

	// The administrator turns it back on, then the chart drops SearXNG:
	// search goes off instead of calling a service that is gone.
	doc["search"] = map[string]any{"enabled": true, "provider": "searxng", "url": bundled}
	h.doAsSession(sess, http.MethodPut, "/api/v1/admin/chat", doc)
	h.server.Config.ChatSearchSearXNGURL = ""
	h.server.ApplyDeploymentChatSearch(ctx)
	if cfg := h.server.chatAdmin(ctx).Search; cfg.Enabled || cfg.URL != "" || cfg.Deployed != "" {
		t.Fatalf("after uninstall: %+v", cfg)
	}

	// An administrator's own service is left alone when no SearXNG is
	// bundled.
	doc["search"] = map[string]any{"enabled": true, "provider": "searxng", "url": "https://search.example.com"}
	h.doAsSession(sess, http.MethodPut, "/api/v1/admin/chat", doc)
	h.server.ApplyDeploymentChatSearch(ctx)
	if cfg := h.server.chatAdmin(ctx).Search; !cfg.Enabled || cfg.URL != "https://search.example.com" {
		t.Fatalf("own service changed: %+v", cfg)
	}

	// Installing SearXNG while an administrator's own service is in use
	// keeps theirs (it is only recorded and offered).
	h.server.Config.ChatSearchSearXNGURL = bundled
	h.server.ApplyDeploymentChatSearch(ctx)
	if cfg := h.server.chatAdmin(ctx).Search; !cfg.Enabled || cfg.URL != "https://search.example.com" || cfg.Deployed != bundled {
		t.Fatalf("own service replaced: %+v", cfg)
	}
	if get := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/chat", nil).Body.String(); !strings.Contains(get, `"deployed":"`+bundled+`"`) {
		t.Fatalf("console not told SearXNG is installed: %s", get)
	}
}

// A model that keeps searching gets chatMaxSearchRounds rounds, and the last
// round's results tell it to answer: it is never left with the tool gone
// and no explanation (a real Qwen-on-vLLM reply came back empty).
func TestChatJanusSearchLimitTellsModelToAnswer(t *testing.T) {
	h, sess := chatHarness(t)
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"K8s","url":"https://kubernetes.io/releases/","content":"1.37.1"}]}`))
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
		if strings.Contains(string(b), `"description":"Search the web for current information`) {
			// Always wants another search while the tool is offered.
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"web_search\",\"arguments\":\"{\\\"query\\\":\\\"k8s\\\"}\"}}]}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Kubernetes 1.37.1.\"}}]}\n\ndata: [DONE]\n\n"))
	})
	h.server.Config.ChatSearchSearXNGURL = searx.URL
	h.server.ApplyDeploymentChatSearch(context.Background())
	conv := chatConversationFor(t, h, sess, "test-model")
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "latest k8s?"})
	_, done := chatEvents(t, h, sess, sent["run_id"].(string), "")
	if done["content"] != "Kubernetes 1.37.1." {
		t.Fatalf("reply = %v", done["content"])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != chatMaxSearchRounds+1 {
		t.Fatalf("%d upstream requests, want %d", len(bodies), chatMaxSearchRounds+1)
	}
	last := bodies[len(bodies)-1]
	if !strings.Contains(last, "last web search for this reply") || strings.Contains(last, `"description":"Search the web for current information`) {
		t.Fatalf("final request does not tell the model to answer: %s", last)
	}
	if strings.Contains(bodies[len(bodies)-2], "last web search for this reply") {
		t.Fatal("limit note sent before the last round")
	}
}
