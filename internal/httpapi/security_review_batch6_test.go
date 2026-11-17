package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/headerpolicy"
	"github.com/torvanis/janus/internal/store"
)

// Regression tests for the 2026-11 security review, batch 6 (rulings of
// 2026-11-11). The preview's network policy (WEB-03) is tested in
// web/src/routes/chat/chat-files.test.tsx; the console header that stops a
// preview navigating away is asserted in TestSecurityHeadersOnEveryResponse.

// headerLogFor is the header log of the request that sent the named header.
func (h *harness) headerLogFor(t *testing.T, n int, sent string) *store.RequestHeaderLog {
	t.Helper()
	events := h.waitForUsageEvents(n)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range events {
			l, _ := h.store.RequestHeaderLogByUsageID(context.Background(), ev.ID)
			if l == nil {
				continue
			}
			for _, e := range l.Inbound {
				if e.Name == sent {
					return l
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no header log for the request that sent %s", sent)
	return nil
}

// LEAK-03: header values were stored unless an administrator turned that
// off, masked by a list of known credential header names. No list covers
// what a client or a proxy in front of the gateway may send. A deployment
// that has chosen nothing now keeps names only.
func TestHeaderValuesAreStoredOnlyWhenChosen(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	sent := map[string]string{
		"Ocp-Apim-Subscription-Key": "azure-key-0123456789",
		"X-Amzn-Oidc-Data":          "eyJhbGciOiJFUzI1NiJ9.signed-identity",
		"X-Forwarded-For":           "203.0.113.7",
	}
	if got := store.DefaultHeaderLogSettings().Mode; got != headerpolicy.LogNames {
		t.Fatalf("default mode = %q", got)
	}
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/admin/header-policies", nil)
	var page struct {
		Settings struct {
			Mode string `json:"mode"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || page.Settings.Mode != "names" {
		t.Fatalf("never-saved setting shown as %q (%v)", page.Settings.Mode, err)
	}

	h.chatWithHeaders(t, sent)
	l := h.headerLogFor(t, 1, "X-Amzn-Oidc-Data")
	names := map[string]bool{}
	for _, list := range [][]headerpolicy.Logged{l.Inbound, l.Outbound} {
		for _, e := range list {
			names[e.Name] = true
			if len(e.Values) != 0 {
				t.Errorf("value stored for %s by default: %v", e.Name, e.Values)
			}
		}
	}
	for name := range sent {
		if !names[name] {
			t.Errorf("%s: the name is still recorded, got %v", name, names)
		}
	}
	raw, _ := json.Marshal(l)
	for _, v := range sent {
		if strings.Contains(string(raw), v) {
			t.Fatalf("header value %q stored by default", v)
		}
	}

	// Chosen: values are kept, and a header a rule marks secret is masked
	// when the caller sends it too, not only on the way out.
	rec = h.doAsSession(sess, http.MethodPost, "/api/v1/admin/header-policies", map[string]any{
		"name":  "partner",
		"rules": []map[string]any{{"action": "set", "name": "X-Partner-Key", "value": "set-by-policy", "secret": true}},
	})
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("create policy: %d %s", rec.Code, rec.Body.String())
	}
	policies, _ := h.store.ListHeaderPolicies(context.Background())
	pid := ""
	for _, p := range policies {
		if p.Name == "partner" {
			pid = p.ID
		}
	}
	if rec = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/header-policies/assignments", map[string]any{"target_type": "upstream", "target_id": h.model.UpstreamID, "policy_id": pid}); rec.Code != http.StatusOK {
		t.Fatalf("assign: %d %s", rec.Code, rec.Body.String())
	}
	if rec = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/header-policies/settings", map[string]any{"mode": "values"}); rec.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()
	h.chatWithHeaders(t, map[string]string{"X-Partner-Key": "sent-by-caller", "X-Forwarded-For": "203.0.113.7"})
	l = h.headerLogFor(t, 2, "X-Partner-Key")
	var sawSecret, sawPlain bool
	for _, e := range l.Inbound {
		switch e.Name {
		case "X-Partner-Key":
			sawSecret = e.Masked && !strings.Contains(strings.Join(e.Values, ""), "sent-by-caller")
		case "X-Forwarded-For":
			sawPlain = strings.Join(e.Values, "") == "203.0.113.7"
		}
	}
	if !sawSecret {
		t.Errorf("a secret-rule header sent by the caller is not masked inbound: %+v", l.Inbound)
	}
	if !sawPlain {
		t.Errorf("values mode did not keep an ordinary value: %+v", l.Inbound)
	}
}
