// Package balancer picks which member of a model pool serves a request.
//
// It is deliberately free of storage and HTTP-server concerns: the proxy
// hands it the pool's members with their live load (Snapshot) and a session
// key, and it returns an ordered list of members to try. Everything here is
// deterministic given its inputs except the tie-breaking random draw of the
// load-aware policies, which is injected.
//
// Design constraints (Janus runs as N replicas with no shared hot-path state):
//
//   - Affinity uses weighted rendezvous (HRW) hashing, so every replica maps
//     a conversation to the same member without coordination, and adding or
//     removing a member moves only that member's share of conversations.
//   - Load comes from the member server itself (vLLM / llama.cpp /metrics),
//     which already counts traffic from every replica, plus this replica's own
//     in-flight requests between probes.
//   - Load-aware choices use power-of-two-choices rather than argmax, so
//     replicas that share the same (slightly stale) view don't all stampede
//     the same "least loaded" member.
package balancer

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"sort"
)

// Policies (mirror store.PoolPolicy*; duplicated to keep this package free
// of internal imports).
const (
	PolicyFailover    = "failover"
	PolicyRoundRobin  = "round_robin"
	PolicyLeastLoaded = "least_loaded"
	PolicyContext     = "context"
)

// Affinity modes (mirror store.PoolAffinity*).
const (
	AffinityBounded = "bounded"
	AffinityStrict  = "strict"
	AffinityOff     = "off"
)

// Reasons recorded on the request log for why a member served.
const (
	ReasonPolicy   = "policy"   // the policy chose it with no session key
	ReasonAffinity = "affinity" // the session's first choice
	ReasonSpill    = "spill"    // the session's first choice was overloaded
	ReasonMoved    = "moved"    // the session's first choice is out of service
	ReasonRetry    = "retry"    // an earlier member failed before any byte
	ReasonOnly     = "only"     // a pool with one servable member
)

// Member is one pool member with its live state, as the proxy sees it.
type Member struct {
	ID       string // model id; the HRW identity, so it must be stable
	Weight   int
	Priority int
	// Healthy is false when the member is ejected (breaker open, probe
	// failing, published ejection) and not due a half-open trial.
	Healthy bool
	Load    Load
}

// Load is a member's current work, combining the server's own report with
// this replica's in-flight count.
type Load struct {
	// Live reports that Running/Waiting/ContextUsed came from the server's
	// metrics endpoint recently; otherwise only InFlight is known.
	Live    bool
	Running float64
	Waiting float64
	// InFlight is requests this replica has open to the member right now.
	InFlight int
	// ContextUsed is the context tokens the member is holding for active
	// requests; ContextCapacity is how many it can hold at once. Either may
	// be zero when unknown.
	ContextUsed     float64
	ContextCapacity float64
	// PendingContext is the prompt tokens this replica has sent to the
	// member that the server may not have reported yet (between probes).
	PendingContext float64
	// KVUsage is the server's KV-cache fill fraction (0..1), a tiebreak.
	KVUsage float64
}

// queueScore is the least-loaded cost: waiting requests count double
// (they are queued behind everything running), plus our own in-flight
// requests the server may not have counted yet. Divided by weight so a
// weight-2 member is "as loaded" as a weight-1 member at twice the queue.
func (l Load) queueScore(weight int) float64 {
	q := 2*l.Waiting + l.Running + float64(l.InFlight)
	if !l.Live {
		q = float64(l.InFlight)
	}
	return q / float64(max(weight, 1))
}

// contextScore is the context-aware cost: the share of the member's
// context capacity that is spoken for. Members with no known capacity fall
// back to the queue score so the comparison still means something.
func (l Load) contextScore(weight int) float64 {
	if l.ContextCapacity <= 0 {
		return l.queueScore(weight)
	}
	return (l.ContextUsed + l.PendingContext) / l.ContextCapacity / float64(max(weight, 1))
}

// Config is a pool's balancing settings.
type Config struct {
	Policy   string
	Affinity string
	// SpillPct: a member more than this percent above the pool mean (on the
	// policy's own score) is considered overloaded by bounded affinity.
	SpillPct int
}

// Pick is the outcome: the members to try in order (first is the choice,
// the rest are pre-byte retry candidates), and why the first was chosen.
type Pick struct {
	Order  []Member
	Reason string
	// FirstChoice is the session's HRW first choice (even if it was not
	// used), so the proxy can measure the affinity hit rate.
	FirstChoice string
}

// Choose orders the pool's members for one request. key is the session key
// ("" = no affinity), rr is a round-robin counter the caller advances per
// request, and rnd supplies randomness for power-of-two-choices.
func Choose(cfg Config, members []Member, key string, rr uint64, rnd *rand.Rand) Pick {
	healthy := make([]Member, 0, len(members))
	for _, m := range members {
		if m.Healthy {
			healthy = append(healthy, m)
		}
	}
	if len(healthy) == 0 {
		return Pick{}
	}
	// Failover: only the best priority tier is eligible; lower tiers are
	// retry candidates. Other policies also honour priority tiers, balancing
	// within the best tier and failing over tier by tier.
	tiers := priorityTiers(healthy)
	best := tiers[0]
	rest := flatten(tiers[1:])
	if len(healthy) == 1 {
		return Pick{Order: healthy, Reason: ReasonOnly, FirstChoice: healthy[0].ID}
	}

	var ordered []Member
	reason := ReasonPolicy
	first := ""
	switch {
	case cfg.Policy == PolicyFailover || cfg.Policy == "":
		// Within a tier, failover still keeps a conversation on one member
		// (HRW) so equal-priority standbys don't split sessions.
		ordered = hrwOrder(best, keyOr(key, "failover"))
		if key != "" {
			reason, first = ReasonAffinity, ordered[0].ID
		}
	case key != "" && cfg.Affinity != AffinityOff:
		ordered = hrwOrder(best, key)
		first = ordered[0].ID
		reason = ReasonAffinity
		if cfg.Affinity == AffinityBounded && len(ordered) > 1 {
			ordered, reason = boundedSpill(cfg, ordered)
		}
	case cfg.Policy == PolicyRoundRobin:
		ordered = weightedRoundRobin(best, rr)
	case cfg.Policy == PolicyLeastLoaded:
		ordered = powerOfTwo(best, rnd, func(m Member) float64 { return m.Load.queueScore(m.Weight) })
	case cfg.Policy == PolicyContext:
		ordered = powerOfTwo(best, rnd, func(m Member) float64 { return m.Load.contextScore(m.Weight) })
	default:
		ordered = weightedRoundRobin(best, rr)
	}
	// The unhealthy session member is reported as "moved" so the log shows
	// why a conversation changed server.
	if key != "" && cfg.Affinity != AffinityOff && reason == ReasonAffinity {
		if all := hrwOrder(eligibleTier(members), key); len(all) > 0 && !all[0].Healthy {
			reason = ReasonMoved
		}
	}
	return Pick{Order: append(ordered, rest...), Reason: reason, FirstChoice: first}
}

// Absolute spill floors: how far above the pool mean a member must be,
// beyond the relative SpillPct, before bounded affinity moves a conversation.
const (
	spillFloorQueue   = 4.0  // weighted queue units (a waiting request counts 2)
	spillFloorContext = 0.15 // share of context capacity
)

// boundedSpill implements consistent hashing with bounded loads: walk the
// session's HRW order and take the first member whose score is within
// (1+spill) of the tier mean. The walk is deterministic, so a spilled
// session consistently lands on its second choice rather than scattering.
func boundedSpill(cfg Config, ordered []Member) ([]Member, string) {
	score := func(m Member) float64 { return m.Load.queueScore(m.Weight) }
	floor := spillFloorQueue
	if cfg.Policy == PolicyContext {
		score = func(m Member) float64 { return m.Load.contextScore(m.Weight) }
		floor = spillFloorContext
	}
	var sum float64
	for _, m := range ordered {
		sum += score(m)
	}
	mean := sum / float64(len(ordered))
	// The relative threshold alone spills on noise when the pool is nearly
	// idle (one request running vs none is "infinitely" above the mean), so
	// a member must ALSO be an absolute amount above the mean: moving a
	// conversation throws away its cache, which only pays off under real
	// imbalance.
	limit := max(mean*(1+float64(cfg.SpillPct)/100), mean+floor)
	for i, m := range ordered {
		if score(m) <= limit {
			if i == 0 {
				return ordered, ReasonAffinity
			}
			out := append([]Member{m}, ordered[:i]...)
			out = append(out, ordered[i+1:]...)
			return out, ReasonSpill
		}
	}
	return ordered, ReasonAffinity
}

// hrwOrder sorts members by weighted rendezvous score for key, highest
// first. Weighted HRW (Schindelhauer & Schomaker): score = -w / ln(u), with
// u a uniform hash of (key, member) in (0,1).
func hrwOrder(members []Member, key string) []Member {
	type scored struct {
		m Member
		s float64
	}
	list := make([]scored, len(members))
	for i, m := range members {
		u := unitHash(key, m.ID)
		list[i] = scored{m, -float64(max(m.Weight, 1)) / math.Log(u)}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].s != list[j].s {
			return list[i].s > list[j].s
		}
		return list[i].m.ID < list[j].m.ID
	})
	out := make([]Member, len(list))
	for i, x := range list {
		out[i] = x.m
	}
	return out
}

// unitHash maps (key, id) to a uniform value strictly inside (0,1).
func unitHash(key, id string) float64 {
	h := sha256.New()
	h.Write([]byte(key))
	h.Write([]byte{0})
	h.Write([]byte(id))
	v := binary.BigEndian.Uint64(h.Sum(nil)[:8])
	return (float64(v>>11) + 0.5) / float64(uint64(1)<<53)
}

// weightedRoundRobin expands members by weight and rotates by rr, then
// appends the remaining distinct members as retry candidates.
func weightedRoundRobin(members []Member, rr uint64) []Member {
	sorted := append([]Member{}, members...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var ring []int
	for i, m := range sorted {
		for w := 0; w < max(m.Weight, 1); w++ {
			ring = append(ring, i)
		}
	}
	// Interleave so weight 3,1 yields A B A A rather than A A A B.
	ring = interleave(sorted, ring)
	start := ring[rr%uint64(len(ring))]
	out := []Member{sorted[start]}
	for k := 1; k < len(sorted); k++ {
		out = append(out, sorted[(start+k)%len(sorted)])
	}
	return out
}

// interleave produces a smooth weighted sequence (nginx smooth WRR) of the
// member indexes, one full cycle long.
func interleave(members []Member, ring []int) []int {
	total := len(ring)
	current := make([]int, len(members))
	out := make([]int, 0, total)
	for n := 0; n < total; n++ {
		best := -1
		for i, m := range members {
			current[i] += max(m.Weight, 1)
			if best < 0 || current[i] > current[best] {
				best = i
			}
		}
		current[best] -= total
		out = append(out, best)
	}
	return out
}

// powerOfTwo samples two members (weighted by Weight), serves the one with
// the lower score, and orders the rest by score as retry candidates.
func powerOfTwo(members []Member, rnd *rand.Rand, score func(Member) float64) []Member {
	if rnd == nil {
		rnd = rand.New(rand.NewPCG(1, 2))
	}
	a := weightedDraw(members, rnd, -1)
	b := weightedDraw(members, rnd, a)
	pick := a
	sa, sb := score(members[a]), score(members[b])
	if sb < sa || (sb == sa && members[b].Load.KVUsage < members[a].Load.KVUsage) {
		pick = b
	}
	rest := make([]Member, 0, len(members)-1)
	for i, m := range members {
		if i != pick {
			rest = append(rest, m)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return score(rest[i]) < score(rest[j]) })
	return append([]Member{members[pick]}, rest...)
}

func weightedDraw(members []Member, rnd *rand.Rand, exclude int) int {
	total := 0
	for i, m := range members {
		if i != exclude {
			total += max(m.Weight, 1)
		}
	}
	x := rnd.IntN(total)
	for i, m := range members {
		if i == exclude {
			continue
		}
		x -= max(m.Weight, 1)
		if x < 0 {
			return i
		}
	}
	return 0
}

func priorityTiers(members []Member) [][]Member {
	sorted := append([]Member{}, members...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })
	var tiers [][]Member
	for _, m := range sorted {
		if len(tiers) == 0 || tiers[len(tiers)-1][0].Priority != m.Priority {
			tiers = append(tiers, []Member{m})
			continue
		}
		tiers[len(tiers)-1] = append(tiers[len(tiers)-1], m)
	}
	return tiers
}

// eligibleTier is the best-priority tier of ALL members, healthy or not:
// the tier a session would live in if nothing were broken.
func eligibleTier(members []Member) []Member {
	if len(members) == 0 {
		return nil
	}
	return priorityTiers(members)[0]
}

func flatten(tiers [][]Member) []Member {
	var out []Member
	for _, t := range tiers {
		out = append(out, t...)
	}
	return out
}

func keyOr(key, def string) string {
	if key == "" {
		return def
	}
	return key
}
