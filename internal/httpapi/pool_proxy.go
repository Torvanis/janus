package httpapi

import (
	"context"
	"errors"

	"github.com/go-chi/chi/v5"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/balancer"
	"github.com/torvanis/janus/internal/store"
)

// Load-balanced model pools on the proxy path.
//
// A managed alias with more than one enabled member is a pool. The proxy
// resolves and authorises the ALIAS exactly as before (grants, quotas and
// the stable name live there); only the choice of which real model serves
// changes. The balancer orders the members for this request; the proxy tries
// them in that order, moving to the next only when the previous one failed
// before any byte reached the client, and falls back to the alias's fallback
// model only when every member is out.

// Response header naming the pool member that served and why.
const (
	headerPoolMember = "X-Janus-Pool-Member"
	headerPoolReason = "X-Janus-Pool-Reason"
)

// maxPoolAttempts bounds pre-byte retries across members for one request.
const maxPoolAttempts = 3

// poolAdapterWatch lists the upstream types whose servers expose /health
// and /metrics (vLLM, llama.cpp and other OpenAI-compatible self-hosted
// engines). Cloud APIs and Ollama are balanced on passive signals only.
var poolAdapterWatch = map[string]bool{"openai_compatible": true, "vlm": true, "llama_cpp": true}

// poolRoute is one request's walk through a pool.
type poolRoute struct {
	poolID  string
	order   []*store.Model
	reason  string
	next    int
	release func()
	tokens  float64
}

// balancer returns the replica's pool registry, creating it on first use so
// test wiring needs nothing extra.
func (s *Server) poolRegistry() *balancer.Registry {
	s.poolOnce.Do(func() {
		s.pools = balancer.NewRegistry(&http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
			TLSClientConfig: s.Config.TLSClientConfig(), MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second,
		}}, s.Logger)
	})
	return s.pools
}

// isPool reports whether a resolution goes through a multi-member pool.
func isPool(resolved store.ResolvedModel) bool {
	if resolved.Managed == nil {
		return false
	}
	enabled := 0
	for _, m := range resolved.Managed.Pool.Members {
		if m.Enabled {
			enabled++
		}
	}
	return enabled > 1
}

// cachedPoolEjections returns the live cross-replica ejections, keyed by
// pool id + "/" + model id, through the config cache.
func (s *Server) cachedPoolEjections(ctx context.Context) map[string]bool {
	list, err := cachedRead(&s.configCache, "pool_ejections", func() ([]store.PoolMemberEjection, error) {
		return s.Store.PoolMemberEjections(ctx, time.Now().UTC())
	})
	out := map[string]bool{}
	if err != nil {
		// Routing must not fail on a storage blip; local breakers still work.
		s.Logger.WarnContext(ctx, "pool ejections unavailable; using local health only", "error", err.Error())
		return out
	}
	now := time.Now()
	for _, e := range list {
		if e.Until.After(now) {
			out[e.ManagedModelID+"/"+e.ModelID] = true
		}
	}
	return out
}

// choosePoolMember orders the pool's members for this request. It returns
// nil when no member can serve.
func (s *Server) choosePoolMember(ctx context.Context, r *http.Request, body []byte, event *store.UsageEvent, managed *store.ManagedModel) *poolRoute {
	reg := s.poolRegistry()
	ejected := s.cachedPoolEjections(ctx)
	inputs := make([]balancer.MemberInput, 0, len(managed.Pool.Members))
	byID := map[string]*store.Model{}
	for _, pm := range managed.Pool.Members {
		if !pm.Enabled || pm.Missing {
			continue
		}
		model, err := s.cachedModelByID(ctx, pm.ModelID)
		if err != nil {
			continue
		}
		servable := model.Status == store.ModelEnabled
		watched := false
		if up, err := s.cachedUpstreamByID(ctx, model.UpstreamID); err != nil || !up.Enabled {
			servable = false
		} else {
			watched = poolAdapterWatch[up.AdapterType]
		}
		byID[model.ID] = model
		inputs = append(inputs, balancer.MemberInput{
			ModelID: model.ID, UpstreamID: model.UpstreamID, Weight: pm.Weight, Priority: pm.Priority,
			Servable: servable, Ejected: ejected[managed.ID+"/"+model.ID],
			ContextCapacity: pm.ContextCapacity, ContextWindow: model.ContextWindow, Watched: watched,
		})
	}
	now := time.Now()
	members := reg.Members(managed.ID, inputs, now)
	key := ""
	if managed.Pool.Affinity != store.PoolAffinityOff || managed.Pool.Policy == store.PoolPolicyFailover {
		caller := event.UserID
		if event.ServiceTokenID != "" {
			caller = "svc:" + event.ServiceTokenID
		}
		key, _ = balancer.SessionKey(r.Header, body, caller)
	}
	pick := balancer.Choose(balancer.Config{
		Policy: managed.Pool.Policy, Affinity: managed.Pool.Affinity, SpillPct: managed.Pool.SpillPct,
	}, members, key, reg.NextRR(managed.ID), reg.Rand())
	if len(pick.Order) == 0 {
		return nil
	}
	route := &poolRoute{poolID: managed.ID, reason: pick.Reason, tokens: balancer.EstimatePromptTokens(body)}
	for _, m := range pick.Order {
		if model := byID[m.ID]; model != nil {
			route.order = append(route.order, model)
		}
	}
	if len(route.order) == 0 {
		return nil
	}
	return route
}

// current is the member being tried.
func (p *poolRoute) current() *store.Model { return p.order[p.next] }

// begin marks the current member as carrying this request.
func (p *poolRoute) begin(reg *balancer.Registry) {
	p.end()
	p.release = reg.Acquire(p.current().ID, p.tokens)
}

// end releases the in-flight slot of the member last begun.
func (p *poolRoute) end() {
	if p.release != nil {
		p.release()
		p.release = nil
	}
}

// advance moves to the next member when a retry is allowed.
func (p *poolRoute) advance() bool {
	if p == nil || p.next+1 >= len(p.order) || p.next+1 >= maxPoolAttempts {
		return false
	}
	p.next++
	p.reason = balancer.ReasonRetry
	return true
}

// attempts is how many members have been tried.
func (p *poolRoute) attempts() int { return p.next + 1 }

// poolRetry switches the request to the route's next member and restamps
// the usage event, which records the member that finally served.
func (s *Server) poolRetry(ctx context.Context, p *poolRoute, event *store.UsageEvent) *store.Model {
	p.end()
	next := p.current()
	s.Logger.InfoContext(ctx, "pool retry on next member", "pool_id", p.poolID, "model", next.PublicName(), "upstream", next.UpstreamName, "attempt", p.attempts())
	s.stampServedModel(ctx, event, next)
	return next
}

// poolFailure records a member fault on this replica's breaker and, when it
// opens the breaker, publishes the ejection so every replica takes the
// member out together.
func (s *Server) poolFailure(ctx context.Context, p *poolRoute, why string) {
	model := p.current()
	until := s.poolRegistry().Breaker(p.poolID, model.ID).Failure(time.Now())
	if until.IsZero() {
		return
	}
	s.Logger.WarnContext(ctx, "pool member ejected", "pool_id", p.poolID, "model", model.PublicName(), "upstream", model.UpstreamName, "until", until, "reason", why)
	if s.Store == nil {
		return
	}
	host, _ := os.Hostname()
	e := store.PoolMemberEjection{ManagedModelID: p.poolID, ModelID: model.ID, Until: until.UTC(), Reason: why, InstanceID: host}
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.Store.EjectPoolMember(ctx, e); err != nil {
			s.Logger.WarnContext(ctx, "publish pool ejection", "error", err.Error())
		}
	}()
}

// poolSuccess closes the member's breaker (a half-open trial passed).
func (s *Server) poolSuccess(ctx context.Context, p *poolRoute) {
	model := p.current()
	b := s.poolRegistry().Breaker(p.poolID, model.ID)
	wasOpen := !b.OpenUntil().IsZero()
	b.Success()
	if wasOpen && s.Store != nil {
		// Lift the published ejection early so other replicas re-admit it
		// now rather than at the deadline.
		_ = s.Store.ClearPoolMemberEjection(ctx, p.poolID, model.ID)
		s.Logger.InfoContext(ctx, "pool member reinstated", "pool_id", p.poolID, "model", model.PublicName())
	}
}

// retryableUpstreamStatus reports statuses that mean "this member can't take
// the request right now" — safe to send to another member because nothing
// was generated.
func retryableUpstreamStatus(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout || code == http.StatusTooManyRequests
}

// memberFaultStatus reports statuses that count against a member's health
// (429 is load, not a fault).
func memberFaultStatus(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// clientGone reports an error caused by the caller going away, which must
// never count against a member or trigger a retry.
func clientGone(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled)
}

// cachedModelByID reads one catalog model through the config cache.
func (s *Server) cachedModelByID(ctx context.Context, id string) (*store.Model, error) {
	return cachedRead(&s.configCache, "model:"+id, func() (*store.Model, error) {
		return s.Store.ModelByID(ctx, id)
	})
}

// StartPoolProber keeps the registry's probe targets in step with pool
// configuration and probes them until ctx ends. Safe to call once per
// process; a gateway with no pools probes nothing.
func (s *Server) StartPoolProber(ctx context.Context, wg *sync.WaitGroup) {
	reg := s.poolRegistry()
	wg.Add(2)
	go func() {
		defer wg.Done()
		reg.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			s.refreshPoolTargets(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (s *Server) refreshPoolTargets(ctx context.Context) {
	ids, err := s.Store.PooledUpstreamIDs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.Logger.WarnContext(ctx, "list pooled upstreams", "error", err.Error())
		}
		return
	}
	targets := make([]balancer.Target, 0, len(ids))
	for _, id := range ids {
		up, err := s.Store.UpstreamByID(ctx, id)
		if err != nil || !up.Enabled || !poolAdapterWatch[up.AdapterType] {
			continue
		}
		key := ""
		if enc := up.EncryptedKey(); enc != "" {
			if k, err := s.Cipher.Decrypt(enc); err == nil {
				key = k
			}
		}
		targets = append(targets, balancer.Target{UpstreamID: up.ID, BaseURL: strings.TrimSpace(up.BaseURL), APIKey: key, AdapterType: up.AdapterType})
	}
	s.poolRegistry().SetTargets(targets)
}

// poolMemberHealth is one member's live state in the admin health panel.
type poolMemberHealth struct {
	ModelID      string `json:"model_id"`
	PublicName   string `json:"public_name"`
	UpstreamName string `json:"upstream_name"`
	Enabled      bool   `json:"enabled"`
	// State: serving | ejected | probe_failing | disabled | unwatched
	State string `json:"state"`
	// Load is "live" when read from the server's metrics, "estimated" when
	// only this gateway's own in-flight count is known.
	Load            string     `json:"load"`
	Engine          string     `json:"engine,omitempty"`
	Running         float64    `json:"running"`
	Waiting         float64    `json:"waiting"`
	InFlight        int        `json:"in_flight"`
	KVUsage         float64    `json:"kv_usage"`
	ContextUsed     float64    `json:"context_used"`
	ContextCapacity float64    `json:"context_capacity"`
	PrefixHitRate   *float64   `json:"prefix_hit_rate,omitempty"`
	ProbedAt        *time.Time `json:"probed_at,omitempty"`
	ProbeError      string     `json:"probe_error,omitempty"`
	EjectedUntil    *time.Time `json:"ejected_until,omitempty"`
	EjectedReason   string     `json:"ejected_reason,omitempty"`
}

// handlePoolHealth reports each pool member's live state as seen by the
// replica that answers. Probes read load from the servers themselves, so
// the figures are the same on every replica; in-flight counts and local
// breakers are per replica.
func (s *Server) handlePoolHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := s.Store.ManagedModelByID(ctx, chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That managed model"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	reg := s.poolRegistry()
	now := time.Now()
	ejections, _ := s.Store.PoolMemberEjections(ctx, now.UTC())
	ejected := map[string]store.PoolMemberEjection{}
	for _, e := range ejections {
		if e.ManagedModelID == m.ID {
			ejected[e.ModelID] = e
		}
	}
	inputs := make([]balancer.MemberInput, 0, len(m.Pool.Members))
	for _, pm := range m.Pool.Members {
		inputs = append(inputs, balancer.MemberInput{
			ModelID: pm.ModelID, UpstreamID: pm.UpstreamID, Weight: pm.Weight, Priority: pm.Priority,
			Servable: pm.Servable(), ContextCapacity: pm.ContextCapacity, ContextWindow: pm.ContextWindow,
			Watched: poolAdapterWatch[pm.AdapterType],
		})
	}
	live := reg.Members(m.ID, inputs, now)
	out := make([]poolMemberHealth, 0, len(m.Pool.Members))
	for i, pm := range m.Pool.Members {
		lm := live[i]
		h := poolMemberHealth{
			ModelID: pm.ModelID, PublicName: pm.PublicName, UpstreamName: pm.UpstreamName, Enabled: pm.Enabled,
			Load: "estimated", Running: lm.Load.Running, Waiting: lm.Load.Waiting, InFlight: lm.Load.InFlight,
			KVUsage: lm.Load.KVUsage, ContextUsed: lm.Load.ContextUsed, ContextCapacity: lm.Load.ContextCapacity,
		}
		if lm.Load.Live {
			h.Load = "live"
		}
		watched := poolAdapterWatch[pm.AdapterType]
		if p, ok := reg.ProbeOf(pm.UpstreamID); ok && watched {
			at := p.At
			h.ProbedAt, h.ProbeError, h.Engine = &at, p.Error, p.Metrics.Engine
			if p.Metrics.PrefixQueries > 0 {
				rate := p.Metrics.PrefixHits / p.Metrics.PrefixQueries
				h.PrefixHitRate = &rate
			}
		}
		switch e, isEjected := ejected[pm.ModelID]; {
		case !pm.Servable():
			h.State = "disabled"
		case isEjected:
			until := e.Until
			h.State, h.EjectedUntil, h.EjectedReason = "ejected", &until, e.Reason
		case !reg.Breaker(m.ID, pm.ModelID).Healthy(now):
			until := reg.Breaker(m.ID, pm.ModelID).OpenUntil()
			h.State, h.EjectedUntil = "ejected", &until
		case h.ProbeError != "":
			h.State = "probe_failing"
		case !watched:
			h.State = "unwatched"
		default:
			h.State = "serving"
		}
		out = append(out, h)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"pool": m.Pool, "members": out, "checked_at": now.UTC()})
}
