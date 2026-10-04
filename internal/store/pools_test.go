package store

import (
	"context"
	"testing"
	"time"
)

// A new alias is a pool of one; a pool edit replaces members atomically and
// keeps target_model_id on the primary so pre-pool readers keep working.
func TestManagedModelPoolLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a := newEnabledModel(t, s, "qwen-a")
	b := newEnabledModel(t, s, "qwen-b")
	c := newEnabledModel(t, s, "qwen-c")

	m, err := s.CreateManagedModel(ctx, "qwen-pool", "", a.ID, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(m.Pool.Members) != 1 || m.Pool.Members[0].ModelID != a.ID || m.Pool.Policy != PoolPolicyFailover || m.Pool.IsLoadBalanced() {
		t.Fatalf("new alias pool = %+v", m.Pool)
	}

	spill := 40
	pool, err := s.NormalizePool(ctx, PoolInput{Policy: "context", SpillPct: &spill, Members: []PoolMemberInput{
		{ModelID: b.ID, Priority: 5}, {ModelID: a.ID, Weight: 2}, {ModelID: c.ID, Enabled: ptrBool(false)},
	}}, "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if err := s.SetManagedModelPool(ctx, m.ID, pool); err != nil {
		t.Fatalf("set pool: %v", err)
	}
	got, err := s.ManagedModelByID(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pool.Policy != PoolPolicyContext || got.Pool.Affinity != PoolAffinityBounded || got.Pool.SpillPct != 40 || len(got.Pool.Members) != 3 {
		t.Fatalf("pool = %+v", got.Pool)
	}
	// Primary = best priority (0) enabled member = a, even though b is listed first.
	if got.TargetModelID != a.ID {
		t.Fatalf("target = %s, want primary %s", got.TargetModelID, a.ID)
	}
	if got.Pool.Members[0].ModelID != b.ID || got.Pool.Members[1].Weight != 2 || got.Pool.Members[2].Enabled {
		t.Fatalf("members = %+v", got.Pool.Members)
	}
	if got.Pool.Members[0].UpstreamName == "" || got.Pool.Members[0].PublicName != "qwen-b" {
		t.Fatalf("member presentation not resolved: %+v", got.Pool.Members[0])
	}
	if !got.Pool.IsLoadBalanced() {
		t.Fatal("two enabled members must count as load balanced")
	}

	// A classic repoint would silently drop members, so it is refused.
	if err := s.SetManagedModelTarget(ctx, m.ID, c.ID); err == nil {
		t.Fatal("repointing a multi-member pool must be refused")
	}
	// The fallback cannot be a member, from either direction.
	if err := s.SetManagedModelFallback(ctx, m.ID, b.ID, nil); err == nil {
		t.Fatal("a member was accepted as the fallback")
	}
	if _, err := s.NormalizePool(ctx, PoolInput{Members: []PoolMemberInput{{ModelID: a.ID}}}, a.ID); err == nil {
		t.Fatal("the fallback was accepted as a member")
	}

	ups, err := s.PooledUpstreamIDs(ctx)
	if err != nil || len(ups) != 2 {
		t.Fatalf("pooled upstreams = %v %v, want the two enabled members' upstreams", ups, err)
	}

	// Ejections: publish, extend only forward, list live ones, prune on edit.
	now := time.Now().UTC()
	if err := s.EjectPoolMember(ctx, PoolMemberEjection{ManagedModelID: m.ID, ModelID: b.ID, Until: now.Add(time.Minute), Reason: "3 failures", InstanceID: "i1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.EjectPoolMember(ctx, PoolMemberEjection{ManagedModelID: m.ID, ModelID: b.ID, Until: now.Add(time.Second), InstanceID: "i2"}); err != nil {
		t.Fatal(err)
	}
	ej, err := s.PoolMemberEjections(ctx, now)
	if err != nil || len(ej) != 1 || ej[0].InstanceID != "i1" || ej[0].Until.Before(now.Add(59*time.Second)) {
		t.Fatalf("ejections = %+v %v (a shorter ejection must not shorten a longer one)", ej, err)
	}
	if ej, _ := s.PoolMemberEjections(ctx, now.Add(2*time.Minute)); len(ej) != 0 {
		t.Fatal("expired ejection still listed")
	}
	one, _ := s.NormalizePool(ctx, PoolInput{Members: []PoolMemberInput{{ModelID: a.ID}}}, "")
	if err := s.SetManagedModelPool(ctx, m.ID, one); err != nil {
		t.Fatal(err)
	}
	if ej, _ := s.PoolMemberEjections(ctx, now); len(ej) != 0 {
		t.Fatal("ejection of a removed member survived the edit")
	}
	if err := s.SetManagedModelTarget(ctx, m.ID, c.ID); err != nil {
		t.Fatalf("pool of one must still repoint: %v", err)
	}
	got, _ = s.ManagedModelByID(ctx, m.ID)
	if len(got.Pool.Members) != 1 || got.Pool.Members[0].ModelID != c.ID {
		t.Fatalf("repoint did not move the member: %+v", got.Pool.Members)
	}

	if err := s.DeleteManagedModel(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM managed_model_target WHERE managed_model_id = ?`, m.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("members left after delete: %d %v", n, err)
	}
}

func TestNormalizePoolRejectsBadInput(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a := newEnabledModel(t, s, "m-a")
	alias, err := s.CreateManagedModel(ctx, "alias-x", "", a.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	bad := []PoolInput{
		{Policy: "random", Members: []PoolMemberInput{{ModelID: a.ID}}},
		{Affinity: "sometimes", Members: []PoolMemberInput{{ModelID: a.ID}}},
		{},
		{Members: []PoolMemberInput{{ModelID: a.ID}, {ModelID: a.ID}}},
		{Members: []PoolMemberInput{{ModelID: "nope"}}},
		{Members: []PoolMemberInput{{ModelID: alias.ID}}},
		{Members: []PoolMemberInput{{ModelID: a.ID, Weight: 101}}},
		{Members: []PoolMemberInput{{ModelID: a.ID, Enabled: ptrBool(false)}}},
	}
	for i, in := range bad {
		if _, err := s.NormalizePool(ctx, in, ""); err == nil {
			t.Fatalf("case %d accepted: %+v", i, in)
		}
	}
}

func ptrBool(b bool) *bool { return &b }
