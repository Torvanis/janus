package httpapi

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// An X-Forwarded-For rule matches only a chain that came through a trusted
// proxy (JANUS_TRUSTED_PROXIES). A client talking to Janus directly can put
// any address in the header, so its header is ignored: it can neither
// trigger a block nor (with Negate) slip past an "only from this network"
// rule by claiming an allowed address.
func TestXFFRuleTrustsOnlyTrustedProxies(t *testing.T) {
	h := newHarness(t)
	_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
	h.server.Config.TrustedProxies = []*net.IPNet{trusted}
	// Block callers forwarded from 203.0.113.0/24.
	if rec := h.do(http.MethodPost, "/api/v1/admin/rules/blocking", map[string]any{
		"name": "block-net", "combinator": "and", "reason": "xff test",
		"clauses": []map[string]any{{"type": "x_forwarded_for_cidr", "pattern": "203.0.113.0/24"}},
	}); rec.Code != http.StatusCreated {
		t.Fatalf("create rule: %d %s", rec.Code, rec.Body.String())
	}
	send := func(peer, xff string) int {
		body, _ := json.Marshal(map[string]any{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+h.token)
		req.Header.Set("X-Forwarded-For", xff)
		req.RemoteAddr = peer
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := send("10.1.2.3:5000", "203.0.113.9"); got != http.StatusForbidden {
		t.Fatalf("forwarded by a trusted proxy: %d, want 403", got)
	}
	if got := send("198.51.100.7:5000", "203.0.113.9"); got != http.StatusOK {
		t.Fatalf("a direct client wrote the header itself: %d, want 200 (header ignored)", got)
	}
	if got := send("10.1.2.3:5000", "198.51.100.1"); got != http.StatusOK {
		t.Fatalf("trusted proxy, other network: %d, want 200", got)
	}
}
