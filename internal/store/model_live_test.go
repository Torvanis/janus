package store

import (
	"context"
	"testing"
)

// Model.Live and ModelFilter.Live follow the upstream's enabled switch, read
// in the model join; the grant count comes from one grouped query.
func TestModelLiveFollowsUpstream(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a := newEnabledModel(t, s, "live-a")
	b := newEnabledModel(t, s, "live-b")
	if !a.Live() || !a.UpstreamEnabled {
		t.Fatalf("model on an enabled upstream = %+v, want live", a)
	}
	if _, err := s.CreateGrant(ctx, a.ID, ModelKindModel, GranteeAllUsers, ""); err != nil {
		t.Fatal(err)
	}
	up, err := s.UpstreamByID(ctx, a.UpstreamID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUpstream(ctx, up.ID, up.BaseURL, false, "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.ModelByID(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Live() || got.UpstreamEnabled || got.Status != ModelEnabled {
		t.Fatalf("model on a disabled upstream = live %v upstream %v status %s, want not live and still enabled", got.Live(), got.UpstreamEnabled, got.Status)
	}
	live, err := s.ListModels(ctx, ModelFilter{Status: ModelEnabled, Live: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != b.ID {
		t.Fatalf("live list = %v, want only %s", live, b.Name)
	}
	all, err := s.ListModels(ctx, ModelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, m := range all {
		counts[m.ID] = m.GrantCount
	}
	if len(all) != 2 || counts[a.ID] != 1 || counts[b.ID] != 0 {
		t.Fatalf("admin list = %d models, grant counts %v; want both with a=1 b=0", len(all), counts)
	}
}

// ServesLive: a pool counts any enabled live member, an alias counts its
// fallback only when it reacts to an unavailable target, a pool its
// fallback always.
func TestManagedModelServesLive(t *testing.T) {
	live := map[string]bool{"b": true, "fb": true}
	isLive := func(id string) bool { return live[id] }
	member := func(id string, enabled bool) PoolMember { return PoolMember{ModelID: id, Enabled: enabled} }
	cases := []struct {
		name string
		m    ManagedModel
		want bool
	}{
		{"alias on a live model", ManagedModel{TargetModelID: "b", Pool: ManagedModelPool{Members: []PoolMember{member("b", true)}}}, true},
		{"alias on a dead model", ManagedModel{TargetModelID: "a", Pool: ManagedModelPool{Members: []PoolMember{member("a", true)}}}, false},
		{"pre-pool alias row", ManagedModel{TargetModelID: "b"}, true},
		{"pool with one live member", ManagedModel{TargetModelID: "a", Pool: ManagedModelPool{Members: []PoolMember{member("a", true), member("b", true)}}}, true},
		{"live member switched off", ManagedModel{TargetModelID: "a", Pool: ManagedModelPool{Members: []PoolMember{member("a", true), member("b", false)}}}, false},
		{"missing live member", ManagedModel{TargetModelID: "a", Pool: ManagedModelPool{Members: []PoolMember{member("a", true), {ModelID: "b", Enabled: true, Missing: true}}}}, false},
		{"alias with fallback on target loss", ManagedModel{TargetModelID: "a", FallbackModelID: "fb", FallbackTriggers: []string{FallbackTriggerTargetUnavailable},
			Pool: ManagedModelPool{Members: []PoolMember{member("a", true)}}}, true},
		{"alias whose fallback ignores target loss", ManagedModel{TargetModelID: "a", FallbackModelID: "fb", FallbackTriggers: []string{FallbackTriggerModelDown},
			Pool: ManagedModelPool{Members: []PoolMember{member("a", true)}}}, false},
		{"alias with a dead fallback", ManagedModel{TargetModelID: "a", FallbackModelID: "c", FallbackTriggers: []string{FallbackTriggerTargetUnavailable},
			Pool: ManagedModelPool{Members: []PoolMember{member("a", true)}}}, false},
		{"dead pool with a live fallback", ManagedModel{TargetModelID: "a", FallbackModelID: "fb", FallbackTriggers: []string{FallbackTriggerModelDown},
			Pool: ManagedModelPool{Members: []PoolMember{member("a", true), member("c", true)}}}, true},
	}
	for _, c := range cases {
		if got := c.m.ServesLive(isLive); got != c.want {
			t.Errorf("%s: ServesLive = %v, want %v", c.name, got, c.want)
		}
	}
	r := &ManagedModel{TargetModelID: "snap"}
	if ids := RouterModelIDs(r, RouterConfig{Routes: []RouterRoute{{ModelID: "r1"}, {ModelID: ""}}}); len(ids) != 2 || ids[0] != "snap" || ids[1] != "r1" {
		t.Fatalf("router ids without a configured default = %v", ids)
	}
	if ids := RouterModelIDs(r, RouterConfig{Default: RouterDefault{ModelID: "d"}}); len(ids) != 1 || ids[0] != "d" {
		t.Fatalf("router ids = %v", ids)
	}
}
