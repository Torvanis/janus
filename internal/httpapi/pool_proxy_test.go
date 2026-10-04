package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torvanis/janus/internal/store"
)

// fakeEngine is a vLLM-like member: /health, /metrics with a settable
// queue, chat that answers with its own name, and a kill switch.
type fakeEngine struct {
	name    string
	srv     *httptest.Server
	down    atomic.Bool // connection-level failure is simulated by 503s
	running atomic.Int64
	waiting atomic.Int64
	mu      sync.Mutex
	chats   int
}

func newFakeEngine(t *testing.T, name string) *fakeEngine {
	t.Helper()
	f := &fakeEngine{name: name}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if f.down.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("ok"))
		case "/metrics":
			_, _ = fmt.Fprintf(w, "vllm:num_requests_running{model_name=\"q\"} %d\nvllm:num_requests_waiting{model_name=\"q\"} %d\nvllm:kv_cache_usage_perc{model_name=\"q\"} 0.1\n",
				f.running.Load(), f.waiting.Load())
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen"}]}`))
		default:
			if f.down.Load() {
				http.Error(w, "engine down", http.StatusServiceUnavailable)
				return
			}
			f.mu.Lock()
			f.chats++
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`, f.name)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEngine) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chats
}

// seedPool builds N fake engines as separate upstreams hosting the same
// model, and a granted alias "qwen-pool" over them with the given policy.
func seedPool(t *testing.T, h *harness, policy, affinity string, names ...string) (*store.ManagedModel, []*fakeEngine) {
	t.Helper()
	ctx := context.Background()
	var engines []*fakeEngine
	var members []store.PoolMemberInput
	for _, n := range names {
		f := newFakeEngine(t, n)
		up, err := h.store.CreateUpstream(ctx, "engine-"+n, "vlm", f.srv.URL+"/v1", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.UpsertDiscoveredModel(ctx, up.ID, "qwen", []string{"chat"}); err != nil {
			t.Fatal(err)
		}
		all, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: up.ID})
		if err := h.store.SetModelStatus(ctx, all[0].ID, store.ModelEnabled); err != nil {
			t.Fatal(err)
		}
		engines = append(engines, f)
		members = append(members, store.PoolMemberInput{ModelID: all[0].ID})
	}
	mm, err := h.store.CreateManagedModel(ctx, "qwen-pool", "", members[0].ModelID, h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := h.store.NormalizePool(ctx, store.PoolInput{Policy: policy, Affinity: affinity, Members: members}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetManagedModelPool(ctx, mm.ID, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateGrant(ctx, mm.ID, store.ModelKindManaged, store.GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	h.server.InvalidateConfigCache()
	h.server.InvalidateResolvedModelCache(mm.Name)
	mm, _ = h.store.ManagedModelByID(ctx, mm.ID)
	return mm, engines
}

func (h *harness) chatPool(t *testing.T, first string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":"qwen-pool","messages":[{"role":"system","content":"sys"},{"role":"user","content":%q}]}`, first)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func servedBy(rec *httptest.ResponseRecorder) string {
	member := rec.Header().Get(headerPoolMember)
	return strings.TrimPrefix(strings.SplitN(member, "@", 2)[1], "engine-")
}

// Round robin spreads, affinity sticks: the same conversation always lands
// on one member, different conversations spread across members.
func TestPoolRoutesByPolicyAndAffinity(t *testing.T) {
	h := newHarness(t)
	seedPool(t, h, "round_robin", "off", "a", "b", "c")
	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		rec := h.chatPool(t, fmt.Sprint("q", i), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		counts[servedBy(rec)]++
		if rec.Header().Get(headerPoolReason) != "policy" {
			t.Fatalf("reason %q", rec.Header().Get(headerPoolReason))
		}
	}
	if counts["a"] != 3 || counts["b"] != 3 || counts["c"] != 3 {
		t.Fatalf("round robin distribution %v", counts)
	}

	h2 := newHarness(t)
	seedPool(t, h2, "least_loaded", "bounded", "a", "b", "c")
	seen := map[string]bool{}
	for conv := 0; conv < 12; conv++ {
		first := ""
		for turn := 0; turn < 4; turn++ {
			rec := h2.chatPool(t, fmt.Sprint("conversation ", conv), nil)
			got := servedBy(rec)
			if first == "" {
				first = got
			} else if got != first {
				t.Fatalf("conversation %d moved from %s to %s", conv, first, got)
			}
			if rec.Header().Get(headerPoolReason) != "affinity" {
				t.Fatalf("reason %q", rec.Header().Get(headerPoolReason))
			}
		}
		seen[first] = true
	}
	if len(seen) < 2 {
		t.Fatalf("12 conversations all landed on %v", seen)
	}
	// An explicit session header wins over the derived key.
	a := servedBy(h2.chatPool(t, "x", map[string]string{"X-Session-Id": "s-1"}))
	for i := 0; i < 5; i++ {
		if b := servedBy(h2.chatPool(t, fmt.Sprint("different ", i), map[string]string{"X-Session-Id": "s-1"})); b != a {
			t.Fatalf("session header not sticky: %s then %s", a, b)
		}
	}
}

// A member that fails before any byte is skipped for the next member within
// the same request; after the breaker opens it is ejected, the ejection is
// published for other replicas, and the usage event records the retry.
func TestPoolRetriesAndEjectsFailingMember(t *testing.T) {
	h := newHarness(t)
	mm, engines := seedPool(t, h, "round_robin", "off", "a", "b")
	engines[0].down.Store(true)
	for i := 0; i < 6; i++ {
		rec := h.chatPool(t, fmt.Sprint("r", i), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body.String())
		}
		if servedBy(rec) != "b" {
			t.Fatalf("request %d served by %s", i, servedBy(rec))
		}
	}
	if engines[1].count() != 6 {
		t.Fatalf("b served %d", engines[1].count())
	}
	// 3 consecutive failures opened a's breaker; the rest skipped it without
	// trying (a received exactly 3 chat attempts, all refused).
	h.server.pending.Wait()
	ej, err := h.store.PoolMemberEjections(context.Background(), time.Now().UTC())
	if err != nil || len(ej) != 1 || ej[0].ModelID != mm.Pool.Members[0].ModelID {
		t.Fatalf("published ejections = %+v %v", ej, err)
	}
	events, _, err := h.store.ListRequests(context.Background(), store.RequestFilter{IncludeInternal: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	for _, e := range events {
		if e.PoolReason == "retry" && e.PoolAttempts == 2 {
			retries++
		}
	}
	if retries != 3 {
		t.Fatalf("expected 3 usage events recorded as retries, got %d", retries)
	}
	// Whole pool out, no fallback: a clear 503 naming the pool.
	engines[1].down.Store(true)
	rec := h.chatPool(t, "z", nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("all members down still returned 200")
	}
}

// Enabling balancing is a Business feature; a pool of one is not.
func TestPoolAdminGate(t *testing.T) {
	h := newHarness(t)
	promote(t, h)
	ctx := context.Background()
	up, _ := h.store.CreateUpstream(ctx, "second", "vlm", "http://127.0.0.1:1/v1", "", "")
	_, _ = h.store.UpsertDiscoveredModel(ctx, up.ID, "qwen", []string{"chat"})
	all, _ := h.store.ListModels(ctx, store.ModelFilter{UpstreamID: up.ID})
	_ = h.store.SetModelStatus(ctx, all[0].ID, store.ModelEnabled)

	two := map[string]any{"name": "lb", "pool": map[string]any{"policy": "context", "members": []map[string]any{
		{"model_id": h.model.ID}, {"model_id": all[0].ID},
	}}}
	if rec := h.do(http.MethodPost, "/api/v1/admin/managed-models", two); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("community create of a 2-member pool = %d %s", rec.Code, rec.Body.String())
	}
	one := map[string]any{"name": "single", "pool": map[string]any{"members": []map[string]any{{"model_id": h.model.ID}}}}
	if rec := h.do(http.MethodPost, "/api/v1/admin/managed-models", one); rec.Code != http.StatusCreated {
		t.Fatalf("community pool of one = %d %s", rec.Code, rec.Body.String())
	}
	withBusinessLicense(t, h)
	rec := h.do(http.MethodPost, "/api/v1/admin/managed-models", two)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"policy":"context"`) {
		t.Fatalf("business create = %d %s", rec.Code, rec.Body.String())
	}
}
