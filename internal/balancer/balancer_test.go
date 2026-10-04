package balancer

import (
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"
	"time"
)

func members(ids ...string) []Member {
	out := make([]Member, len(ids))
	for i, id := range ids {
		out[i] = Member{ID: id, Weight: 1, Healthy: true}
	}
	return out
}

func rnd() *rand.Rand { return rand.New(rand.NewPCG(7, 11)) }

// Every replica must place a conversation on the same member: HRW depends
// only on (key, member ids, weights), never on order or process state.
func TestAffinityIsDeterministicAcrossOrderAndReplicas(t *testing.T) {
	cfg := Config{Policy: PolicyLeastLoaded, Affinity: AffinityStrict}
	a := members("m1", "m2", "m3", "m4")
	b := []Member{a[2], a[0], a[3], a[1]}
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("conv-%d", i)
		p1 := Choose(cfg, a, key, uint64(i), rnd())
		p2 := Choose(cfg, b, key, uint64(i*7), rand.New(rand.NewPCG(uint64(i), 3)))
		if p1.Order[0].ID != p2.Order[0].ID {
			t.Fatalf("key %s: replica A chose %s, replica B chose %s", key, p1.Order[0].ID, p2.Order[0].ID)
		}
		if p1.Reason != ReasonAffinity {
			t.Fatalf("reason = %s", p1.Reason)
		}
	}
}

// Removing one member moves only that member's conversations.
func TestAffinityMinimalMovementOnMemberLoss(t *testing.T) {
	cfg := Config{Policy: PolicyRoundRobin, Affinity: AffinityStrict}
	all := members("m1", "m2", "m3", "m4")
	moved, onLost := 0, 0
	for i := 0; i < 2000; i++ {
		key := fmt.Sprintf("k%d", i)
		before := Choose(cfg, all, key, 0, rnd()).Order[0].ID
		down := append([]Member{}, all...)
		down[1].Healthy = false // m2 out
		after := Choose(cfg, down, key, 0, rnd())
		if before == "m2" {
			onLost++
			if after.Reason != ReasonMoved {
				t.Fatalf("session on lost member should be reported moved, got %s", after.Reason)
			}
			continue
		}
		if after.Order[0].ID != before {
			moved++
		}
	}
	if moved != 0 {
		t.Fatalf("%d sessions not on the lost member moved", moved)
	}
	if onLost < 350 || onLost > 650 {
		t.Fatalf("expected ~1/4 of sessions on m2, got %d/2000", onLost)
	}
}

// Weights skew HRW placement proportionally.
func TestAffinityRespectsWeights(t *testing.T) {
	ms := members("big", "small")
	ms[0].Weight = 3
	counts := map[string]int{}
	for i := 0; i < 8000; i++ {
		counts[Choose(Config{Policy: PolicyRoundRobin, Affinity: AffinityStrict}, ms, fmt.Sprint(i), 0, rnd()).Order[0].ID]++
	}
	ratio := float64(counts["big"]) / float64(counts["small"])
	if math.Abs(ratio-3) > 0.35 {
		t.Fatalf("weight 3:1 gave %v", counts)
	}
}

// Bounded affinity spills an overloaded member's sessions to their second
// choice, deterministically, and keeps them there while the overload lasts.
func TestBoundedAffinitySpillsDeterministically(t *testing.T) {
	cfg := Config{Policy: PolicyLeastLoaded, Affinity: AffinityBounded, SpillPct: 25}
	ms := members("m1", "m2", "m3")
	var key string
	for i := 0; ; i++ {
		key = fmt.Sprint("s", i)
		if Choose(cfg, ms, key, 0, rnd()).Order[0].ID == "m1" {
			break
		}
	}
	busy := append([]Member{}, ms...)
	busy[0].Load = Load{Live: true, Running: 8, Waiting: 4}
	busy[1].Load = Load{Live: true, Running: 2}
	busy[2].Load = Load{Live: true, Running: 2}
	p := Choose(cfg, busy, key, 0, rnd())
	if p.Order[0].ID == "m1" || p.Reason != ReasonSpill || p.FirstChoice != "m1" {
		t.Fatalf("expected spill off m1, got %s (%s, first %s)", p.Order[0].ID, p.Reason, p.FirstChoice)
	}
	second := p.Order[0].ID
	for i := 0; i < 20; i++ {
		if got := Choose(cfg, busy, key, uint64(i), rand.New(rand.NewPCG(uint64(i), 9))).Order[0].ID; got != second {
			t.Fatalf("spill target not stable: %s then %s", second, got)
		}
	}
	// Strict mode never spills.
	cfg.Affinity = AffinityStrict
	if got := Choose(cfg, busy, key, 0, rnd()).Order[0].ID; got != "m1" {
		t.Fatalf("strict affinity moved to %s", got)
	}
	// An idle pool never spills.
	cfg.Affinity = AffinityBounded
	if got := Choose(cfg, ms, key, 0, rnd()); got.Order[0].ID != "m1" || got.Reason != ReasonAffinity {
		t.Fatalf("idle pool spilled: %+v", got)
	}
	// Nor a nearly idle one: a single running request is far above the
	// mean in relative terms but not worth throwing the cache away for
	// (found on the two-replica rig: 13 of 20 idle conversations moved).
	noise := append([]Member{}, ms...)
	noise[0].Load = Load{Live: true, Running: 1}
	if got := Choose(cfg, noise, key, 0, rnd()); got.Order[0].ID != "m1" || got.Reason != ReasonAffinity {
		t.Fatalf("one running request caused a spill: %+v", got)
	}
	ctxCfg := Config{Policy: PolicyContext, Affinity: AffinityBounded, SpillPct: 25}
	cnoise := append([]Member{}, ms...)
	cnoise[0].Load = Load{Live: true, ContextUsed: 10_000, ContextCapacity: 131_072}
	if got := Choose(ctxCfg, cnoise, key, 0, rnd()); got.Order[0].ID != "m1" {
		t.Fatalf("8%% context use caused a spill: %+v", got)
	}
	cnoise[0].Load.ContextUsed = 100_000
	if got := Choose(ctxCfg, cnoise, key, 0, rnd()); got.Order[0].ID == "m1" || got.Reason != ReasonSpill {
		t.Fatalf("76%% context use vs idle peers did not spill: %+v", got)
	}
}

// Least loaded sends traffic away from the busy member (P2C: with 2 members
// the lower score always wins).
func TestLeastLoadedPrefersShortQueue(t *testing.T) {
	ms := members("a", "b")
	ms[0].Load = Load{Live: true, Running: 4, Waiting: 3}
	ms[1].Load = Load{Live: true, Running: 5}
	for i := 0; i < 50; i++ {
		p := Choose(Config{Policy: PolicyLeastLoaded, Affinity: AffinityOff}, ms, "", 0, rand.New(rand.NewPCG(uint64(i), 1)))
		if p.Order[0].ID != "b" {
			t.Fatalf("waiting should count double: chose %s", p.Order[0].ID)
		}
		if len(p.Order) != 2 {
			t.Fatalf("retry candidate missing")
		}
	}
}

// Context-aware balancing compares the SHARE of context capacity in use,
// so a big-context member with more requests can still be the lighter one.
func TestContextPolicyBalancesOnContextShare(t *testing.T) {
	ms := members("small", "large")
	// small: 2 requests but 90k of its 100k context held.
	ms[0].Load = Load{Live: true, Running: 2, ContextUsed: 90_000, ContextCapacity: 100_000}
	// large: 6 short chats, 60k of 400k.
	ms[1].Load = Load{Live: true, Running: 6, ContextUsed: 60_000, ContextCapacity: 400_000}
	cfg := Config{Policy: PolicyContext, Affinity: AffinityOff}
	for i := 0; i < 20; i++ {
		if got := Choose(cfg, ms, "", 0, rand.New(rand.NewPCG(uint64(i), 5))).Order[0].ID; got != "large" {
			t.Fatalf("context policy chose %s", got)
		}
	}
	// Least-loaded would have picked the other one: that is the difference.
	if got := Choose(Config{Policy: PolicyLeastLoaded, Affinity: AffinityOff}, ms, "", 0, rnd()).Order[0].ID; got != "small" {
		t.Fatalf("least loaded chose %s", got)
	}
	// Pending context this replica sent but the server hasn't reported yet
	// counts too.
	ms[1].Load.PendingContext = 330_000
	if got := Choose(cfg, ms, "", 0, rnd()).Order[0].ID; got != "small" {
		t.Fatalf("pending context ignored: chose %s", got)
	}
}

func TestRoundRobinFollowsWeights(t *testing.T) {
	ms := members("a", "b")
	ms[0].Weight = 3
	counts := map[string]int{}
	for i := uint64(0); i < 400; i++ {
		counts[Choose(Config{Policy: PolicyRoundRobin, Affinity: AffinityOff}, ms, "", i, nil).Order[0].ID]++
	}
	if counts["a"] != 300 || counts["b"] != 100 {
		t.Fatalf("round robin 3:1 gave %v", counts)
	}
}

func TestFailoverHonoursPriorityTiers(t *testing.T) {
	ms := members("primary", "standby")
	ms[1].Priority = 10
	p := Choose(Config{Policy: PolicyFailover}, ms, "", 0, nil)
	if p.Order[0].ID != "primary" || p.Order[1].ID != "standby" {
		t.Fatalf("order %v", ids(p.Order))
	}
	ms[0].Healthy = false
	if p := Choose(Config{Policy: PolicyFailover}, ms, "", 0, nil); p.Order[0].ID != "standby" {
		t.Fatalf("did not fail over: %v", ids(p.Order))
	}
	ms[1].Healthy = false
	if p := Choose(Config{Policy: PolicyFailover}, ms, "", 0, nil); len(p.Order) != 0 {
		t.Fatalf("no healthy members must return an empty pick")
	}
}

func ids(ms []Member) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func TestBreakerOpensHalfOpensAndBacksOff(t *testing.T) {
	b := NewBreaker()
	now := time.Unix(1000, 0)
	for i := 0; i < 2; i++ {
		if !b.Failure(now).IsZero() {
			t.Fatalf("opened after %d failures, before the threshold", i+1)
		}
	}
	until := b.Failure(now)
	if until != now.Add(5*time.Second) {
		t.Fatalf("first cooldown %v", until.Sub(now))
	}
	if b.Allow(now.Add(time.Second)) {
		t.Fatal("allowed while open")
	}
	if !b.Allow(now.Add(6 * time.Second)) {
		t.Fatal("no half-open trial")
	}
	if b.Allow(now.Add(6 * time.Second)) {
		t.Fatal("second concurrent trial allowed")
	}
	// Trial fails: re-open with doubled cooldown.
	until = b.Failure(now.Add(6 * time.Second))
	if until.Sub(now.Add(6*time.Second)) != 10*time.Second {
		t.Fatalf("backoff %v", until.Sub(now.Add(6*time.Second)))
	}
	if !b.Allow(until) {
		t.Fatal("no trial after backoff")
	}
	b.Success()
	for i := 0; i < 2; i++ {
		if !b.Allow(until) {
			t.Fatalf("closed breaker refused request %d", i+1)
		}
	}
}

const vllmMetrics = `# HELP vllm:num_requests_running Number of requests in model execution batches.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0",model_name="qwen3.8-27b"} 3.0
vllm:num_requests_waiting{engine="0",model_name="qwen3.8-27b"} 2.0
vllm:kv_cache_usage_perc{engine="0",model_name="qwen3.8-27b"} 0.25
vllm:prefix_cache_queries_total{engine="0",model_name="qwen3.8-27b"} 1000.0
vllm:prefix_cache_hits_total{engine="0",model_name="qwen3.8-27b"} 750.0
vllm:cache_config_info{block_size="16",cache_dtype="fp8_e4m3",enable_prefix_caching="True",engine="0",num_gpu_blocks="8192"} 1.0
`

func TestParseVLLMMetrics(t *testing.T) {
	m := ParseMetrics(strings.NewReader(vllmMetrics))
	if !m.Found || m.Engine != "vllm" || m.Running != 3 || m.Waiting != 2 || m.KVUsage != 0.25 {
		t.Fatalf("%+v", m)
	}
	if m.KVCapacityTokens != 8192*16 || m.PrefixHits != 750 || m.PrefixQueries != 1000 {
		t.Fatalf("capacity/prefix %+v", m)
	}
}

// Exact lines from vLLM 0.28 serving Qwen3.8-27B (hybrid attention+mamba)
// on the homelab: capacity must come from kv_cache_size_tokens, not
// blocks x block_size (which overstates it).
func TestParseVLLMHybridCapacity(t *testing.T) {
	m := ParseMetrics(strings.NewReader(`vllm:num_requests_running{engine="0",model_name="qwen3.8-27b"} 0.0
vllm:num_requests_waiting{engine="0",model_name="qwen3.8-27b"} 0.0
vllm:num_requests_waiting_by_reason{engine="0",model_name="qwen3.8-27b",reason="capacity"} 0.0
vllm:kv_cache_usage_perc{engine="0",model_name="qwen3.8-27b"} 0.0
vllm:prefix_cache_queries_total{engine="0",model_name="qwen3.8-27b"} 16681.0
vllm:prefix_cache_hits_total{engine="0",model_name="qwen3.8-27b"} 7840.0
vllm:cache_config_info{_block_size_resolved="True",block_size="784",cache_dtype="auto",enable_prefix_caching="True",engine="0",gpu_memory_utilization="0.45",kv_cache_max_concurrency="4.333333333333333",kv_cache_memory_bytes="10737418240",kv_cache_size_tokens="141994",num_gpu_blocks="208",num_gpu_blocks_override="None",sliding_window="None"} 1.0
`))
	if m.KVCapacityTokens != 141994 || m.PrefixHits != 7840 || m.Waiting != 0 {
		t.Fatalf("%+v", m)
	}
}

func TestParseLlamaCppMetrics(t *testing.T) {
	m := ParseMetrics(strings.NewReader(`# HELP llamacpp:requests_processing Number of requests processing.
# TYPE llamacpp:requests_processing gauge
llamacpp:requests_processing 2
llamacpp:requests_deferred 1
llamacpp:prompt_tokens_total 1234
llamacpp:n_tokens_max 8164
`))
	if !m.Found || m.Engine != "llamacpp" || m.Running != 2 || m.Waiting != 1 || m.KVCapacityTokens != 0 {
		t.Fatalf("%+v", m)
	}
}

func TestParseSlots(t *testing.T) {
	capacity, used, n := parseSlots([]byte(`[
	 {"id":0,"n_ctx":32768,"is_processing":true,"n_past":12000},
	 {"id":1,"n_ctx":32768,"is_processing":false,"n_past":30000},
	 {"id":2,"n_ctx":32768,"is_processing":true,"n_prompt_tokens":800,"next_token":[{"n_decoded":200}]}
	]`))
	if capacity != 3*32768 || used != 13000 || n != 3 {
		t.Fatalf("capacity=%v used=%v n=%v", capacity, used, n)
	}
	// Current llama-server shape (no n_past; next_token is an array).
	capacity, used, n = parseSlots([]byte(`[{"id":0,"n_ctx":32768,"speculative":false,"is_processing":true,"id_task":52,"n_prompt_tokens":8164,"n_prompt_tokens_processed":0,"n_prompt_tokens_cache":0,"next_token":[{"has_next_token":true,"n_remain":-1,"n_decoded":36}]},
	 {"id":1,"n_ctx":32768,"is_processing":false,"n_prompt_tokens":30,"next_token":[{"n_decoded":0}]}]`))
	if capacity != 65536 || used != 8200 || n != 2 {
		t.Fatalf("llama-server slots: capacity=%v used=%v n=%v", capacity, used, n)
	}
}

func TestSessionKeyStableAcrossTurnsAndScopedToCaller(t *testing.T) {
	turn1 := []byte(`{"model":"p","messages":[{"role":"system","content":"You are X"},{"role":"user","content":"hi"}]}`)
	turn2 := []byte(`{"messages":[{"role":"system","content":"You are X"},{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"more"}],"model":"p","stream":true}`)
	k1, src := SessionKey(http.Header{}, turn1, "user-a")
	k2, _ := SessionKey(http.Header{}, turn2, "user-a")
	if k1 == "" || k1 != k2 || src != "derived" {
		t.Fatalf("derived key not stable: %q %q %s", k1, k2, src)
	}
	other, _ := SessionKey(http.Header{}, turn1, "user-b")
	if other == k1 {
		t.Fatal("key not scoped to caller")
	}
	diff, _ := SessionKey(http.Header{}, []byte(`{"messages":[{"role":"system","content":"You are X"},{"role":"user","content":"something else"}]}`), "user-a")
	if diff == k1 {
		t.Fatal("different conversations share a key")
	}
	h := http.Header{}
	h.Set("X-Session-Id", "abc")
	hk, src := SessionKey(h, turn1, "user-a")
	if src != "header" || hk == k1 {
		t.Fatalf("header key: %s %s", hk, src)
	}
	pk, src := SessionKey(http.Header{}, []byte(`{"prompt_cache_key":"c1","messages":[{"role":"user","content":"x"}]}`), "user-a")
	if src != "prompt_cache_key" || pk == "" {
		t.Fatalf("prompt_cache_key: %s", src)
	}
	if k, _ := SessionKey(http.Header{}, []byte(`{"input":"embed me"}`), "u"); k == "" {
		t.Fatal("responses-style input should still key")
	}
	if k, _ := SessionKey(http.Header{}, []byte(`not json`), "u"); k != "" {
		t.Fatal("non-JSON body keyed")
	}
}

func TestServerRoot(t *testing.T) {
	for in, want := range map[string]string{
		"http://x:8000/v1":  "http://x:8000",
		"http://x:8000/v1/": "http://x:8000",
		"http://x:8080":     "http://x:8080",
	} {
		if got := ServerRoot(in); got != want {
			t.Fatalf("%s -> %s", in, got)
		}
	}
}
