package httpapi

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
)

// A personal plan provider behind an access proxy: every call Janus makes to
// the vendor on its own (sign-in, token exchange, account, model list, the
// connection check, token renewal, revocation) carries the header policy an
// administrator assigned to that provider, as users' requests already did.
func TestHeaderPolicyOnPersonalPlanCalls(t *testing.T) {
	h := newHarness(t)
	fake := newFakeXAI(t)
	ctx := context.Background()
	sess := adminSession(t, h)

	// Put the access proxy in front of the vendor.
	var mu sync.Mutex
	passed, refused := map[string]int{}, map[string]int{}
	inner := fake.srv.Config.Handler
	fake.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := r.Header.Get(gateHeader) == gateSecret && r.Header.Get("Cf-Access-Client-Id") == "client-id.access"
		mu.Lock()
		if ok {
			passed[r.URL.Path]++
		} else {
			refused[r.URL.Path]++
		}
		mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html><title>Error ・ Cloudflare Access</title></html>"))
			return
		}
		inner.ServeHTTP(w, r)
	})
	seen := func(m map[string]int) []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, 0, len(m))
		for p := range m {
			out = append(out, p)
		}
		sort.Strings(out)
		return out
	}
	count := func(path string) int { mu.Lock(); defer mu.Unlock(); return passed[path] }

	h.enableXAISubscriptions(t, sess, true)

	// Without a policy the proxy turns the sign-in away.
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/xai/connect", nil); rec.Code == http.StatusOK {
		t.Fatalf("connect without the policy must fail at the access proxy: %d %s", rec.Code, rec.Body.String())
	}
	if got := seen(refused); len(got) != 1 || got[0] != "/oauth2/device/code" || len(seen(passed)) != 0 {
		t.Fatalf("before the policy: refused %v, passed %v", got, seen(passed))
	}

	// One policy for the provider, for everyone.
	h.accessPolicyFor(t, "personal_subscription", "xai")
	mu.Lock()
	refused = map[string]int{}
	mu.Unlock()

	// Sign-in: device code, token exchange, account, model list.
	connID, out := h.connectXAI(t, sess)
	if connID == "" {
		t.Fatalf("connect: %v", out)
	}
	for _, p := range []string{"/oauth2/device/code", "/oauth2/token", "/oauth2/userinfo", "/v1/models"} {
		if count(p) == 0 {
			t.Errorf("connect: no request to %s passed the access proxy", p)
		}
	}

	// "Refresh models".
	n := count("/v1/models")
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/"+connID+"/refresh-models", nil); rec.Code != http.StatusOK {
		t.Errorf("refresh models: %d %s", rec.Code, rec.Body.String())
	}
	if count("/v1/models") <= n {
		t.Error("refresh models did not reach the vendor")
	}

	// The connection check.
	n = count("/v1/models") + count("/oauth2/userinfo")
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/"+connID+"/check", nil); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "reauth") {
		t.Errorf("check: %d %s", rec.Code, rec.Body.String())
	}
	if count("/v1/models")+count("/oauth2/userinfo") <= n {
		t.Error("the connection check did not reach the vendor")
	}

	// Token renewal: the access token is expired, the next use renews it.
	if err := h.store.ExpireSubscriptionAccess(ctx, connID); err != nil {
		t.Fatal(err)
	}
	n = count("/oauth2/token")
	if rec := h.doAsSession(sess, http.MethodPost, "/api/v1/me/subscriptions/"+connID+"/refresh-models", nil); rec.Code != http.StatusOK {
		t.Errorf("refresh models after expiry: %d %s", rec.Code, rec.Body.String())
	}
	if count("/oauth2/token") <= n {
		t.Error("the token renewal did not reach the vendor")
	}

	// Disconnect revokes the token at the vendor.
	if rec := h.doAsSession(sess, http.MethodDelete, "/api/v1/me/subscriptions/"+connID, nil); rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Errorf("disconnect: %d %s", rec.Code, rec.Body.String())
	}
	if count("/oauth2/revoke") == 0 {
		t.Error("the revocation did not pass the access proxy")
	}

	if got := seen(refused); len(got) != 0 {
		t.Errorf("with the policy assigned, the access proxy still refused: %v", got)
	}
}
