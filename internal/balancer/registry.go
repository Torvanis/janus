package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Target is one upstream server the prober watches.
type Target struct {
	UpstreamID  string
	BaseURL     string // the upstream's configured base URL (may end in /v1)
	APIKey      string
	AdapterType string
}

// Probe is the latest observation of one upstream.
type Probe struct {
	At      time.Time `json:"at"`
	Healthy bool      `json:"healthy"`
	Error   string    `json:"error,omitempty"`
	Metrics ServerMetrics
	// SlotsCapacity / SlotsUsed come from llama.cpp's /slots when metrics
	// don't carry context figures.
	SlotsCapacity float64
	SlotsUsed     float64
	Slots         int
}

// Registry is a gateway replica's view of every pooled upstream: probe
// results, this replica's in-flight work, passive failure breakers and the
// round-robin counters. It is safe for concurrent use.
type Registry struct {
	Interval time.Duration
	// Stale is how old a probe may be before its load figures are ignored.
	Stale  time.Duration
	Client *http.Client
	Logger *slog.Logger

	mu       sync.RWMutex
	probes   map[string]Probe // by upstream id
	targets  map[string]Target
	inflight map[string]*memberLoad // by model id
	breakers map[string]*Breaker    // by pool id + "/" + model id
	rr       map[string]*atomic.Uint64
	rnd      *rand.Rand
	rndMu    sync.Mutex
}

type memberLoad struct {
	requests atomic.Int64
	context  atomic.Int64 // estimated prompt tokens in flight
}

// NewRegistry returns a registry with the default 2s probe interval.
func NewRegistry(client *http.Client, logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{
		Interval: 2 * time.Second, Stale: 10 * time.Second, Client: client, Logger: logger,
		probes: map[string]Probe{}, targets: map[string]Target{},
		inflight: map[string]*memberLoad{}, breakers: map[string]*Breaker{},
		rr:  map[string]*atomic.Uint64{},
		rnd: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x6a616e7573)),
	}
}

// SetTargets replaces the set of upstreams to watch (called when pool
// configuration changes; cheap to call with an unchanged set).
func (r *Registry) SetTargets(targets []Target) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := map[string]Target{}
	for _, t := range targets {
		next[t.UpstreamID] = t
	}
	for id := range r.probes {
		if _, ok := next[id]; !ok {
			delete(r.probes, id)
		}
	}
	r.targets = next
}

// Run probes every target each Interval until ctx ends.
func (r *Registry) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		r.probeAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Registry) probeAll(ctx context.Context) {
	r.mu.RLock()
	targets := make([]Target, 0, len(r.targets))
	for _, t := range r.targets {
		targets = append(targets, t)
	}
	r.mu.RUnlock()
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			p := r.probe(ctx, t)
			r.mu.Lock()
			if _, still := r.targets[t.UpstreamID]; still {
				prev, had := r.probes[t.UpstreamID]
				r.probes[t.UpstreamID] = p
				if had && prev.Healthy != p.Healthy {
					r.Logger.InfoContext(ctx, "pool member health changed", "upstream_id", t.UpstreamID, "healthy", p.Healthy, "error", p.Error)
				}
			}
			r.mu.Unlock()
		}(t)
	}
	wg.Wait()
}

// ServerRoot strips a trailing /v1 (and slashes) from an upstream base URL:
// vLLM and llama.cpp serve /metrics and /health at the root.
func ServerRoot(base string) string {
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/v1")
	return strings.TrimRight(base, "/")
}

func (r *Registry) probe(ctx context.Context, t Target) Probe {
	ctx, cancel := context.WithTimeout(ctx, max(r.Interval, time.Second))
	defer cancel()
	root := ServerRoot(t.BaseURL)
	p := Probe{At: time.Now()}
	status, body, err := r.get(ctx, root+"/health", t.APIKey, 4096)
	if err != nil {
		p.Error = "health: " + err.Error()
		return p
	}
	if status >= 300 {
		// llama.cpp answers 503 while loading the model: not servable yet.
		p.Error = fmt.Sprintf("health: HTTP %d %s", status, strings.TrimSpace(string(body)))
		return p
	}
	p.Healthy = true
	if status, body, err := r.get(ctx, root+"/metrics", t.APIKey, 2<<20); err == nil && status < 300 {
		p.Metrics = ParseMetrics(strings.NewReader(string(body)))
	}
	if p.Metrics.Engine != "vllm" {
		// llama.cpp: context figures come from /slots (per-slot n_ctx and
		// the tokens each busy slot holds). Absent unless --slots is on.
		if status, body, err := r.get(ctx, root+"/slots", t.APIKey, 1<<20); err == nil && status < 300 {
			p.SlotsCapacity, p.SlotsUsed, p.Slots = parseSlots(body)
		}
	}
	return p
}

func (r *Registry) get(ctx context.Context, url, key string, limit int64) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return resp.StatusCode, body, err
}

// parseSlots reads llama.cpp's /slots: total context capacity across slots
// and the context held by slots that are processing a request.
func parseSlots(body []byte) (capacity, used float64, n int) {
	var slots []struct {
		NCtx         float64 `json:"n_ctx"`
		IsProcessing bool    `json:"is_processing"`
		NPast        float64 `json:"n_past"`
		NPrompt      float64 `json:"n_prompt_tokens"`
		// next_token is an object in older llama.cpp and a one-element
		// array in newer releases.
		NextToken json.RawMessage `json:"next_token"`
	}
	if json.Unmarshal(body, &slots) != nil {
		return 0, 0, 0
	}
	for _, s := range slots {
		capacity += s.NCtx
		if !s.IsProcessing {
			continue
		}
		held := s.NPast
		if held == 0 {
			held = s.NPrompt + decodedTokens(s.NextToken)
		}
		used += held
	}
	return capacity, used, len(slots)
}

// ProbeOf returns the latest probe for an upstream.
func (r *Registry) ProbeOf(upstreamID string) (Probe, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.probes[upstreamID]
	return p, ok
}

func (r *Registry) load(modelID string) *memberLoad {
	r.mu.RLock()
	l := r.inflight[modelID]
	r.mu.RUnlock()
	if l != nil {
		return l
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if l = r.inflight[modelID]; l == nil {
		l = &memberLoad{}
		r.inflight[modelID] = l
	}
	return l
}

// Acquire records a request this replica is sending to a member; call the
// returned release when the response (stream included) is finished.
func (r *Registry) Acquire(modelID string, promptTokens float64) (release func()) {
	l := r.load(modelID)
	l.requests.Add(1)
	tokens := int64(promptTokens)
	l.context.Add(tokens)
	var once sync.Once
	return func() {
		once.Do(func() {
			l.requests.Add(-1)
			l.context.Add(-tokens)
		})
	}
}

// Breaker returns the passive-failure breaker for one pool member.
func (r *Registry) Breaker(poolID, modelID string) *Breaker {
	key := poolID + "/" + modelID
	r.mu.RLock()
	b := r.breakers[key]
	r.mu.RUnlock()
	if b != nil {
		return b
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if b = r.breakers[key]; b == nil {
		b = NewBreaker()
		r.breakers[key] = b
	}
	return b
}

// NextRR advances and returns a pool's round-robin counter.
func (r *Registry) NextRR(poolID string) uint64 {
	r.mu.RLock()
	c := r.rr[poolID]
	r.mu.RUnlock()
	if c == nil {
		r.mu.Lock()
		if c = r.rr[poolID]; c == nil {
			c = &atomic.Uint64{}
			r.rr[poolID] = c
		}
		r.mu.Unlock()
	}
	return c.Add(1) - 1
}

// Rand returns a locked random source draw function for Choose.
func (r *Registry) Rand() *rand.Rand {
	r.rndMu.Lock()
	seed := r.rnd.Uint64()
	r.rndMu.Unlock()
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}

// MemberInput is what the proxy knows about one pool member before live
// state is layered on.
type MemberInput struct {
	ModelID    string
	UpstreamID string
	Weight     int
	Priority   int
	// Servable: enabled, model and upstream enabled.
	Servable bool
	// Ejected: a published ejection from another replica is in force.
	Ejected bool
	// ContextCapacity: admin override; 0 = discover.
	ContextCapacity int64
	// ContextWindow: the model's context window, the last-resort capacity.
	ContextWindow int64
	// Watched: the upstream is probed (vLLM-style engine). Unwatched
	// members (cloud APIs, Ollama) rely on passive signals only.
	Watched bool
}

// Members builds balancer members with live load for one pool.
func (r *Registry) Members(poolID string, in []MemberInput, now time.Time) []Member {
	out := make([]Member, 0, len(in))
	for _, m := range in {
		l := r.load(m.ModelID)
		member := Member{ID: m.ModelID, Weight: max(m.Weight, 1), Priority: m.Priority}
		member.Load.InFlight = int(l.requests.Load())
		member.Load.PendingContext = float64(l.context.Load())
		healthy := m.Servable && !m.Ejected
		if healthy && !r.Breaker(poolID, m.ModelID).Allow(now) {
			healthy = false
		}
		if p, ok := r.ProbeOf(m.UpstreamID); ok && m.Watched && now.Sub(p.At) <= r.Stale {
			if !p.Healthy {
				healthy = false
			}
			mt := p.Metrics
			if mt.Found {
				member.Load.Live = true
				member.Load.Running, member.Load.Waiting, member.Load.KVUsage = mt.Running, mt.Waiting, mt.KVUsage
				// The server's own counts include our in-flight requests
				// once it has seen them; count ours on top only for the
				// part it cannot have seen (bounded by what we sent).
				member.Load.InFlight = max(0, member.Load.InFlight-int(mt.Running+mt.Waiting))
			}
			capTokens := mt.KVCapacityTokens
			if capTokens == 0 {
				capTokens = p.SlotsCapacity
			}
			if capTokens > 0 {
				member.Load.ContextCapacity = capTokens
				if p.SlotsCapacity > 0 && mt.Engine != "vllm" {
					member.Load.ContextUsed = p.SlotsUsed
				} else {
					member.Load.ContextUsed = mt.KVUsage * capTokens
				}
				// The server has seen at least part of what we sent; keep
				// only the share it can't have reported yet (a conservative
				// half, since probes are ~1 interval old on average).
				member.Load.PendingContext /= 2
			}
		}
		if m.ContextCapacity > 0 {
			member.Load.ContextCapacity = float64(m.ContextCapacity)
		} else if member.Load.ContextCapacity == 0 && m.ContextWindow > 0 {
			member.Load.ContextCapacity = float64(m.ContextWindow)
		}
		member.Healthy = healthy
		out = append(out, member)
	}
	return out
}

func decodedTokens(raw json.RawMessage) float64 {
	type nt struct {
		NDecoded float64 `json:"n_decoded"`
	}
	var one nt
	if json.Unmarshal(raw, &one) == nil {
		return one.NDecoded
	}
	var many []nt
	if json.Unmarshal(raw, &many) == nil {
		var sum float64
		for _, x := range many {
			sum += x.NDecoded
		}
		return sum
	}
	return 0
}
