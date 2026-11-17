package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/balancer"
	"github.com/torvanis/janus/internal/store"
)

// gatedUpstream stands for an upstream behind an access proxy (a Cloudflare
// Access tunnel): every request without the service token header gets the
// proxy's 403 and never reaches the model server.
type gatedUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	passed  []string // paths that carried the token
	refused []string // paths that did not
}

const gateHeader, gateSecret = "Cf-Access-Client-Secret", "s3cret-service-token"

func newGatedUpstream(t *testing.T, inner http.Handler) *gatedUpstream {
	t.Helper()
	g := &gatedUpstream{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		ok := r.Header.Get(gateHeader) == gateSecret && r.Header.Get("Cf-Access-Client-Id") == "client-id.access"
		if ok {
			g.passed = append(g.passed, r.Method+" "+r.URL.Path)
		} else {
			g.refused = append(g.refused, r.Method+" "+r.URL.Path)
		}
		g.mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html><title>Error ・ Cloudflare Access</title></html>"))
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *gatedUpstream) seen() (passed, refused []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.passed...), append([]string{}, g.refused...)
}

// modelServer answers what Janus asks a model server on its own.
func modelServer() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gated-model","object":"model","max_model_len":8192}]}`))
		case r.URL.Path == "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`))
		case r.URL.Path == "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.URL.Path == "/metrics", r.URL.Path == "/slots":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// accessPolicy creates a policy that carries the access proxy's service
// token and assigns it to one upstream.
func (h *harness) accessPolicy(t *testing.T, upstreamID string) {
	t.Helper()
	h.accessPolicyFor(t, "upstream", upstreamID)
}

// accessPolicyFor creates the access-token policy and assigns it to a target.
func (h *harness) accessPolicyFor(t *testing.T, targetType, targetID string) {
	t.Helper()
	sess := adminSession(t, h)
	rec := h.doAsSession(sess, http.MethodPost, "/api/v1/admin/header-policies", map[string]any{
		"name": "Access " + targetID,
		"rules": []map[string]any{
			{"action": "remove", "name": "cf-*"},
			{"action": "set", "name": "CF-Access-Client-Id", "value": "client-id.access"},
			{"action": "set", "name": "CF-Access-Client-Secret", "value": gateSecret, "secret": true},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create policy: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Policy struct {
			ID string `json:"id"`
		} `json:"policy"`
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	pid := created.Policy.ID
	if pid == "" {
		pid = created.ID
	}
	if rec = h.doAsSession(sess, http.MethodPut, "/api/v1/admin/header-policies/assignments", map[string]any{
		"target_type": targetType, "target_id": targetID, "policy_id": pid,
	}); rec.Code != http.StatusOK {
		t.Fatalf("assign policy: %d %s", rec.Code, rec.Body.String())
	}
	h.server.InvalidateConfigCache()
}

// Every call Janus makes to an upstream carries the upstream's header
// policy, not only the calls that come from a user's request. Each step
// below is one kind of call; behind the access proxy it only succeeds when
// the policy's headers went out with it.
func TestHeaderPolicyOnEveryCallToAnUpstream(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	gate := newGatedUpstream(t, modelServer())
	key, _ := h.server.Cipher.Encrypt("up-key")
	up, err := h.store.CreateUpstream(ctx, "Gated", "llama_cpp", gate.URL+"/v1", key, "up-k…")
	if err != nil {
		t.Fatal(err)
	}
	passedSince := func(n int) []string {
		passed, _ := gate.seen()
		return passed[n:]
	}
	count := func() int { passed, _ := gate.seen(); return len(passed) }

	// Before there is a policy, the proxy turns discovery away: the failure this fixes.
	if res := h.server.Discovery.RunFor(ctx, up); res.Error == "" || !strings.Contains(res.Error, "403") {
		t.Fatalf("discovery without the policy must be refused by the access proxy: %+v", res)
	}
	h.accessPolicy(t, up.ID)

	// 1. Model discovery (the scheduled run and "Refresh models").
	n := count()
	if res := h.server.Discovery.RunFor(ctx, up); res.Error != "" || res.Found != 1 {
		t.Fatalf("discovery: %+v", res)
	}
	if got := passedSince(n); len(got) == 0 || got[0] != "GET /v1/models" {
		t.Errorf("discovery requests that passed: %v", got)
	}
	m, err := h.store.ModelByUpstreamAndName(ctx, up.ID, "gated-model")
	if err != nil {
		t.Fatal(err)
	}
	_ = h.store.SetModelRates(ctx, m.ID, store.RateCard{RateInNano: 1, EffectiveFrom: time.Now().UTC().Add(-time.Hour)})
	_ = h.store.SetModelStatus(ctx, m.ID, store.ModelEnabled)
	if _, err := h.store.CreateGrant(ctx, m.ID, store.ModelKindModel, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	m, _ = h.store.ModelByID(ctx, m.ID)
	provider, _ := adapter.Get(up.AdapterType)
	aup := adapter.Upstream{ID: up.ID, Name: up.Name, BaseURL: up.BaseURL, APIKey: "up-key", AdapterType: up.AdapterType}
	chat := []byte(`{"model":"gated-model","messages":[{"role":"user","content":"hi"}]}`)

	// 2. A user's request through the proxy (worked before; must still).
	n = count()
	if rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "gated-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}}); rec.Code != http.StatusOK {
		t.Errorf("proxy: %d %s", rec.Code, rec.Body.String())
	}
	if got := passedSince(n); len(got) != 1 {
		t.Errorf("proxy requests that passed: %v", got)
	}

	// 3. A capability probe request.
	if reply, err := h.server.capabilityProbeCall(ctx, m, aup, provider, map[string]any{"messages": []map[string]string{{"role": "user", "content": "hi"}}}, "req-probe"); err != nil || reply.Status != http.StatusOK {
		t.Errorf("capability probe: %+v %v", reply, err)
	}

	// 4. A chat classifier call.
	if _, _, refusal, err := h.server.classifierChatCall(provider, aup, "gated-model")(ctx, chat); err != nil || refusal != nil {
		t.Errorf("classifier call: %v %s", err, refusal)
	}

	// 5. Janus's own chat calls (routing evaluation, naming).
	resolved, err := h.store.ResolveModelForRequest(ctx, "gated-model")
	if err != nil {
		t.Fatal(err)
	}
	if text, _, err := h.server.internalChatTo(ctx, resolved, chat, "req-internal"); err != nil || text != "ok" {
		t.Errorf("internal chat: %q %v", text, err)
	}

	// 6. Pool health and load probes.
	reg := h.server.poolRegistry()
	reg.Interval = 20 * time.Millisecond
	reg.SetTargets([]balancer.Target{{UpstreamID: up.ID, BaseURL: up.BaseURL, APIKey: "up-key", AdapterType: up.AdapterType}})
	pctx, cancel := context.WithCancel(ctx)
	go reg.Run(pctx)
	deadline := time.Now().Add(3 * time.Second)
	healthy := false
	for time.Now().Before(deadline) && !healthy {
		if p, ok := reg.ProbeOf(up.ID); ok {
			healthy = p.Healthy
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if !healthy {
		p, _ := reg.ProbeOf(up.ID)
		t.Errorf("pool probe did not get through the access proxy: %+v", p)
	}

	// After the policy was assigned, nothing Janus sent was turned away.
	_, refused := gate.seen()
	if len(refused) != 1 { // the one discovery run before the policy existed
		t.Errorf("requests the access proxy refused: %v", refused)
	}

	// The secret is the upstream's: it is not sent to any other upstream.
	before := len(h.upstreamRequests)
	if rec := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}}); rec.Code != http.StatusOK {
		t.Fatalf("other upstream: %d", rec.Code)
	}
	if got := h.upstreamRequests[before].Header.Get(gateHeader); got != "" {
		t.Errorf("another upstream received the access secret")
	}
	if res := h.server.Discovery.RunFor(ctx, h.upstreamRow(t)); res.Error != "" {
		t.Errorf("discovery of the other upstream: %+v", res)
	}
	for _, r := range h.upstreamRequests[before:] {
		if r.Header.Get(gateHeader) != "" {
			t.Errorf("another upstream received the access secret on %s", r.Path)
		}
	}
}

func (h *harness) upstreamRow(t *testing.T) *store.Upstream {
	t.Helper()
	up, err := h.store.UpstreamByID(context.Background(), h.model.UpstreamID)
	if err != nil {
		t.Fatal(err)
	}
	return up
}

// Served decision requests (/v1/decisions, /v1/systemone) are built by
// Janus, not forwarded from the caller, so they too need the policy.
func TestHeaderPolicyOnServedDecisions(t *testing.T) {
	h := newHarness(t)
	oai := newDialectUpstream(t, "openai")
	gate := newGatedUpstream(t, oai.Config.Handler)
	luna := h.decisionModel(t, &dialectUpstream{Server: gate.Server}, "Gated decisions", "gpt-6-luna:decisions", 0.10, store.DecisionDialectOpenAI)

	if rec := h.doRaw(http.MethodPost, "/v1/decisions", "application/json", []byte(sprintf(oaiBody, luna.Name))); rec.Code == http.StatusOK {
		t.Fatalf("without the policy the access proxy must refuse: %d", rec.Code)
	}
	h.accessPolicy(t, luna.UpstreamID)
	rec := h.doRaw(http.MethodPost, "/v1/decisions", "application/json", []byte(sprintf(oaiBody, luna.Name)))
	if rec.Code != http.StatusOK {
		t.Fatalf("decisions with the policy: %d %s", rec.Code, rec.Body.String())
	}
	if passed, _ := gate.seen(); len(passed) != 1 || passed[0] != "POST /v1/decisions" {
		t.Errorf("passed: %v", passed)
	}
}

// The shared upstream client applies no header policy. A call Janus makes
// to an upstream on its own must use ownUpstreamClient; the direct uses left
// are listed here with why each is right, and a new one fails this test
// until it is looked at.
func TestOwnUpstreamCallsUseThePolicyClient(t *testing.T) {
	allowed := map[string]string{
		"header_policy.go":   "ownUpstreamClient itself wraps it",
		"proxy.go":           "the user-request path: the policy is applied to the caller's headers before the adapter runs; and upstreamHTTPClientFor's fallback",
		"provider_quirks.go": "resends of the user's request, with the already rewritten headers",
		"chat_assist.go":     "fetches a web page a user linked: not an upstream",
		"chat_images.go":     "fetches an image URL: not an upstream",
		"chat_tools.go":      "web search provider: not an upstream",
	}
	direct := regexp.MustCompile(`\bupstreamHTTPClient(For)?\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") || strings.HasPrefix(line, "func (s *Server) upstreamHTTPClient") {
				continue
			}
			if direct.MatchString(line) {
				found[f] = true
			}
		}
	}
	var unexpected, stale []string
	for f := range found {
		if allowed[f] == "" {
			unexpected = append(unexpected, f)
		}
	}
	for f := range allowed {
		if !found[f] {
			stale = append(stale, f)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(stale)
	if len(unexpected) > 0 {
		t.Errorf("these files call an upstream through upstreamHTTPClient, which applies no header policy: %v.\nUse s.ownUpstreamClient(ctx, upstream.ID, upstream.BaseURL) (s.ownPersonalClient(ctx, provider) for a personal plan provider), or add the file to the list with the reason it is not a call to an upstream.", unexpected)
	}
	if len(stale) > 0 {
		t.Errorf("listed but no longer using upstreamHTTPClient (remove from the list): %v", stale)
	}
}
