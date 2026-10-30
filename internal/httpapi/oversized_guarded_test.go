package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/torvanis/janus/internal/store"
)

// oversizedChat points the harness upstream at a server that answers with a
// chat completion far larger than MaxResponseBytes, whose content starts
// with lead.
func oversizedChat(t *testing.T, h *harness, lead string) []byte {
	t.Helper()
	body := []byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"` + lead + strings.Repeat(" padding", 2048) + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`)
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(big.Close)
	if err := h.store.UpdateUpstream(context.Background(), h.model.UpstreamID, big.URL, true, "", ""); err != nil {
		t.Fatal(err)
	}
	h.server.Config.MaxResponseBytes = 1 << 10
	h.server.InvalidateConfigCache()
	return body
}

// A block-mode egress policy judges the retained prefix of an oversized
// buffered reply; a hit withholds the whole response.
func TestOversizedBufferedReplyGetsEgressCheck(t *testing.T) {
	h := newHarness(t)
	h.secgwPolicy(t, store.SecgwPolicy{Name: "egress-block", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeBlock, Direction: store.SecgwDirectionEgress},
	}})
	oversizedChat(t, h, "Sure, use "+testAWSKey+" for that.")
	rec := h.do(http.MethodPost, "/v1/chat/completions", chatBody("give me a key", false))
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), testAWSKey) {
		t.Fatalf("an oversized reply with a secret must be withheld: %d %.200s", rec.Code, rec.Body.String())
	}
	ev := h.waitForUsageEvents(1)[0]
	if ev.SecgwAction != store.SecgwActionBlocked || ev.ErrorCode != CodeSecurityBlocked {
		t.Fatalf("event: action=%q code=%q", ev.SecgwAction, ev.ErrorCode)
	}
}

// A clean oversized reply still passes whole under an egress policy.
func TestOversizedBufferedCleanReplyStillRelayed(t *testing.T) {
	h := newHarness(t)
	h.secgwPolicy(t, store.SecgwPolicy{Name: "egress-block", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeBlock, Direction: store.SecgwDirectionEgress},
	}})
	want := oversizedChat(t, h, "Nothing secret here.")
	rec := h.do(http.MethodPost, "/v1/chat/completions", chatBody("hi", false))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("a clean oversized reply must be relayed whole: %d (%d of %d bytes)", rec.Code, rec.Body.Len(), len(want))
	}
}

// A covered child never gets a reply Child Safety could not judge.
func TestOversizedBufferedReplyWithheldFromCoveredChild(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	m := h.familyModel(t, newFamilyDecider(t))
	kid, kidTok := h.otherUser(t, "kid9", "kid9@example.com")
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": strictKids(), "assignments": []map[string]string{{"scope_type": "user", "profile_id": "cs_strict", "scope_id": kid.ID}}})
	oversizedChat(t, h, "Here is how to make a nerve agent.")
	rec := h.doWithToken(kidTok, http.MethodPost, "/v1/chat/completions", chatBody("chemistry homework help", false))
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "nerve") || !strings.Contains(rec.Body.String(), familyReplyStopNotice[:20]) {
		t.Fatalf("an oversized reply must not reach a covered child: %d %.200s", rec.Code, rec.Body.String())
	}
	assertKindNotice(t, rec.Body.String())
}
