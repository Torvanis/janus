package secgw

import (
	"testing"

	"github.com/torvanis/janus/internal/store"
)

func snapWith(policies []*store.SecgwPolicy, bindings []*store.SecgwBinding) *store.SecgwSnapshot {
	snap := &store.SecgwSnapshot{Policies: map[string]*store.SecgwPolicy{}, Bindings: bindings}
	for _, p := range policies {
		snap.Policies[p.ID] = p
	}
	return snap
}

func check(kind store.SecgwCheckKind, mode string) store.SecgwCheck {
	return store.SecgwCheck{Kind: kind, Enabled: true, Mode: mode, Fail: store.SecgwFailClosed, Direction: store.SecgwDirectionIngress}
}

func TestResolveEmptySnapshotIsNoop(t *testing.T) {
	eff := Resolve(&store.SecgwSnapshot{}, Subject{UserID: "u"})
	if !eff.Empty() {
		t.Fatalf("expected no checks, got %+v", eff.Checks)
	}
	if _, ok := eff.Has(store.SecgwCheckSecrets, store.SecgwDirectionIngress); ok {
		t.Fatal("Has on empty effective")
	}
}

func TestResolveMostSpecificWins(t *testing.T) {
	org := &store.SecgwPolicy{ID: "p-org", Name: "org", Enabled: true, Checks: []store.SecgwCheck{check(store.SecgwCheckSecrets, store.SecgwModeBlock)}}
	grp := &store.SecgwPolicy{ID: "p-grp", Name: "grp", Enabled: true, Checks: []store.SecgwCheck{check(store.SecgwCheckSecrets, store.SecgwModeObserve)}}
	snap := snapWith([]*store.SecgwPolicy{org, grp}, []*store.SecgwBinding{
		{ID: "b-grp", PolicyID: "p-grp", ScopeType: store.SecgwScopeGroup, ScopeID: "g1"},
		{ID: "b-org", PolicyID: "p-org", ScopeType: store.SecgwScopeOrg},
	})
	// Non-mandatory org policy: the group relaxes it.
	eff := Resolve(snap, Subject{UserID: "u", GroupIDs: []string{"g1"}})
	c, ok := eff.Has(store.SecgwCheckSecrets, store.SecgwDirectionIngress)
	if !ok || c.Mode != store.SecgwModeObserve || c.BindingID != "b-grp" || c.Floor {
		t.Fatalf("group should override non-mandatory org: %+v", c)
	}
	// A user outside the group gets the org policy.
	eff = Resolve(snap, Subject{UserID: "u2"})
	c, _ = eff.Has(store.SecgwCheckSecrets, store.SecgwDirectionIngress)
	if c.Mode != store.SecgwModeBlock || c.BindingID != "b-org" {
		t.Fatalf("org policy expected: %+v", c)
	}
	if eff.Policy == nil || eff.Policy.ID != "p-org" {
		t.Fatalf("effective policy: %+v", eff.Policy)
	}
}

func TestResolveMandatoryFloorTightensNeverRelaxes(t *testing.T) {
	org := &store.SecgwPolicy{ID: "p-org", Name: "org", Enabled: true, Mandatory: true, Checks: []store.SecgwCheck{
		check(store.SecgwCheckSecrets, store.SecgwModeRedact),
		check(store.SecgwCheckPII, store.SecgwModeBlock),
	}}
	tokenPol := &store.SecgwPolicy{ID: "p-tok", Name: "tok", Enabled: true, Checks: []store.SecgwCheck{
		check(store.SecgwCheckSecrets, store.SecgwModeBlock), // tighten: allowed
		{Kind: store.SecgwCheckPII, Enabled: true, Mode: store.SecgwModeObserve, Fail: store.SecgwFailOpen, Direction: store.SecgwDirectionIngress}, // relax: refused
		check(store.SecgwCheckShape, store.SecgwModeBlock), // add: allowed
	}}
	snap := snapWith([]*store.SecgwPolicy{org, tokenPol}, []*store.SecgwBinding{
		{ID: "b-org", PolicyID: "p-org", ScopeType: store.SecgwScopeOrg},
		{ID: "b-tok", PolicyID: "p-tok", ScopeType: store.SecgwScopeServiceToken, ScopeID: "svc1"},
	})
	eff := Resolve(snap, Subject{ServiceTokenID: "svc1"})

	sec, _ := eff.Has(store.SecgwCheckSecrets, store.SecgwDirectionIngress)
	if sec.Mode != store.SecgwModeBlock || !sec.Floor || sec.BindingID != "b-tok" {
		t.Fatalf("tightening should apply and keep floor: %+v", sec)
	}
	pii, _ := eff.Has(store.SecgwCheckPII, store.SecgwDirectionIngress)
	if pii.Mode != store.SecgwModeBlock || pii.Fail != store.SecgwFailClosed || pii.BindingID != "b-org" {
		t.Fatalf("relaxation must be refused: %+v", pii)
	}
	if _, ok := eff.Has(store.SecgwCheckShape, store.SecgwDirectionIngress); !ok {
		t.Fatal("added check missing")
	}
	var refused *TraceEntry
	for i := range eff.Trace {
		if eff.Trace[i].Outcome == "overridden_by_mandatory" {
			refused = &eff.Trace[i]
		}
	}
	if refused == nil || refused.BindingID != "b-tok" || refused.Detail == "" {
		t.Fatalf("trace must name the refused relaxation: %+v", eff.Trace)
	}
	if eff.Policy.ID != "p-tok" {
		t.Fatalf("most specific policy owns request-level settings: %s", eff.Policy.ID)
	}
}

func TestResolveFloorProtectsAgainstDisableAndNarrowing(t *testing.T) {
	org := &store.SecgwPolicy{ID: "p-org", Name: "org", Enabled: true, Mandatory: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: true, Mode: store.SecgwModeBlock, Fail: store.SecgwFailClosed, Direction: store.SecgwDirectionBoth, HoldBytes: 512},
	}}
	model := &store.SecgwPolicy{ID: "p-m", Name: "m", Enabled: true, Checks: []store.SecgwCheck{
		{Kind: store.SecgwCheckSecrets, Enabled: false, Mode: store.SecgwModeBlock, Fail: store.SecgwFailClosed, Direction: store.SecgwDirectionIngress, HoldBytes: 64},
	}}
	snap := snapWith([]*store.SecgwPolicy{org, model}, []*store.SecgwBinding{
		{ID: "b-org", PolicyID: "p-org", ScopeType: store.SecgwScopeOrg},
		{ID: "b-m", PolicyID: "p-m", ScopeType: store.SecgwScopeModel, ScopeID: "m1"},
	})
	eff := Resolve(snap, Subject{UserID: "u", ModelID: "m1"})
	c, ok := eff.Has(store.SecgwCheckSecrets, store.SecgwDirectionEgress)
	if !ok || !c.Enabled || c.Direction != store.SecgwDirectionBoth || c.HoldBytes != 512 {
		t.Fatalf("floor breached: %+v", c)
	}
}

func TestResolveDisabledPolicySkippedAndManagedScope(t *testing.T) {
	off := &store.SecgwPolicy{ID: "p-off", Name: "off", Enabled: false, Checks: []store.SecgwCheck{check(store.SecgwCheckSecrets, store.SecgwModeBlock)}}
	mm := &store.SecgwPolicy{ID: "p-mm", Name: "mm", Enabled: true, Checks: []store.SecgwCheck{check(store.SecgwCheckTerms, store.SecgwModeBlock)}}
	snap := snapWith([]*store.SecgwPolicy{off, mm}, []*store.SecgwBinding{
		{ID: "b-off", PolicyID: "p-off", ScopeType: store.SecgwScopeOrg},
		{ID: "b-mm", PolicyID: "p-mm", ScopeType: store.SecgwScopeManagedModel, ScopeID: "alias1"},
	})
	eff := Resolve(snap, Subject{UserID: "u", ManagedModelID: "alias1"})
	if _, ok := eff.Has(store.SecgwCheckSecrets, store.SecgwDirectionIngress); ok {
		t.Fatal("disabled policy applied")
	}
	if _, ok := eff.Has(store.SecgwCheckTerms, store.SecgwDirectionIngress); !ok {
		t.Fatal("managed-model binding not applied")
	}
	if eff.Trace[0].Outcome != "skipped_disabled" {
		t.Fatalf("trace: %+v", eff.Trace)
	}
	// Egress-only check is not reported for ingress.
	eg := &store.SecgwPolicy{ID: "p-eg", Name: "eg", Enabled: true, Checks: []store.SecgwCheck{{Kind: store.SecgwCheckPII, Enabled: true, Mode: store.SecgwModeRedact, Direction: store.SecgwDirectionEgress}}}
	snap = snapWith([]*store.SecgwPolicy{eg}, []*store.SecgwBinding{{ID: "b", PolicyID: "p-eg", ScopeType: store.SecgwScopeOrg}})
	eff = Resolve(snap, Subject{UserID: "u"})
	if _, ok := eff.Has(store.SecgwCheckPII, store.SecgwDirectionIngress); ok {
		t.Fatal("egress check reported on ingress")
	}
	if _, ok := eff.Has(store.SecgwCheckPII, store.SecgwDirectionEgress); !ok {
		t.Fatal("egress check missing on egress")
	}
}

// An upstream binding covers every model from that provider and sits
// between group and model in specificity: broader than one model, so a
// model binding still wins; narrower than a group, so it overrides one.
func TestResolveUpstreamScopeCoversEveryModelFromProvider(t *testing.T) {
	observe := &store.SecgwPolicy{ID: "obs", Enabled: true, Checks: []store.SecgwCheck{check(store.SecgwCheckSecrets, store.SecgwModeObserve)}}
	block := &store.SecgwPolicy{ID: "blk", Enabled: true, Checks: []store.SecgwCheck{check(store.SecgwCheckSecrets, store.SecgwModeBlock)}}
	redact := &store.SecgwPolicy{ID: "red", Enabled: true, Checks: []store.SecgwCheck{check(store.SecgwCheckSecrets, store.SecgwModeRedact)}}
	snap := snapWith([]*store.SecgwPolicy{observe, block, redact}, []*store.SecgwBinding{
		{ID: "b-grp", PolicyID: "obs", ScopeType: store.SecgwScopeGroup, ScopeID: "g1"},
		{ID: "b-up", PolicyID: "blk", ScopeType: store.SecgwScopeUpstream, ScopeID: "up-openai"},
		{ID: "b-model", PolicyID: "red", ScopeType: store.SecgwScopeModel, ScopeID: "m-gpt4"},
	})

	// Any model from up-openai gets the upstream policy over the group's.
	eff := Resolve(snap, Subject{UserID: "u", GroupIDs: []string{"g1"}, ModelID: "m-gpt35", UpstreamID: "up-openai"})
	if c, ok := eff.Has(store.SecgwCheckSecrets, store.SecgwDirectionIngress); !ok || c.Mode != store.SecgwModeBlock || c.BindingID != "b-up" {
		t.Fatalf("upstream binding must beat group: %+v", c)
	}
	// A model binding is more specific than its upstream.
	eff = Resolve(snap, Subject{UserID: "u", GroupIDs: []string{"g1"}, ModelID: "m-gpt4", UpstreamID: "up-openai"})
	if c, ok := eff.Has(store.SecgwCheckSecrets, store.SecgwDirectionIngress); !ok || c.Mode != store.SecgwModeRedact || c.BindingID != "b-model" {
		t.Fatalf("model binding must beat upstream: %+v", c)
	}
	// A model from a different provider is untouched by it.
	eff = Resolve(snap, Subject{UserID: "u", GroupIDs: []string{"g1"}, ModelID: "m-claude", UpstreamID: "up-anthropic"})
	if c, ok := eff.Has(store.SecgwCheckSecrets, store.SecgwDirectionIngress); !ok || c.Mode != store.SecgwModeObserve || c.BindingID != "b-grp" {
		t.Fatalf("other provider must fall through to group: %+v", c)
	}
}
