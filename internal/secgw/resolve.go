// Package secgw is the Security Gateway: policy-driven ingress and egress
// filtering on the proxy path. This file resolves which checks apply to a
// request and where each one came from.
//
// Resolution walks bindings from least to most specific scope
// (org → group → model → managed_model → service_token); a later scope
// overrides an earlier one, EXCEPT that a check contributed by a Mandatory
// org-scope policy is a floor: later bindings may add checks or tighten
// mode/fail/hold, never relax or remove. Every decision is recorded in a
// Trace so the admin UI can show why a check is on — precedence you cannot
// see is precedence you cannot audit.
package secgw

import (
	"encoding/json"
	"sort"

	"github.com/torvanis/janus/internal/store"
)

// Subject is everything about the caller and target the resolver may inspect.
type Subject struct {
	UserID         string
	ServiceTokenID string
	GroupIDs       []string
	ModelID        string
	ManagedModelID string
	// UpstreamID is the provider the resolved model belongs to, so one
	// binding can cover every model from that provider.
	UpstreamID string
}

// ResolvedCheck is a check plus its provenance.
type ResolvedCheck struct {
	store.SecgwCheck
	PolicyID   string `json:"policy_id"`
	PolicyName string `json:"policy_name"`
	BindingID  string `json:"binding_id"`
	ScopeType  string `json:"scope_type"`
	// Floor reports that this check is held by a mandatory org policy and
	// may only be tightened by more specific bindings.
	Floor bool `json:"floor"`
}

// TraceEntry records one binding's contribution.
type TraceEntry struct {
	BindingID  string `json:"binding_id"`
	PolicyID   string `json:"policy_id"`
	PolicyName string `json:"policy_name"`
	ScopeType  string `json:"scope_type"`
	ScopeID    string `json:"scope_id,omitempty"`
	// Outcome is one of: applied, skipped_disabled, overridden_by_mandatory
	// (with Detail naming the check and the relaxation that was refused).
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// Effective is the merged check set for a subject.
type Effective struct {
	Checks map[store.SecgwCheckKind]ResolvedCheck `json:"checks"`
	Trace  []TraceEntry                           `json:"trace"`
	// Policy is the most specific policy that contributed, used for
	// request-level settings (synthetic refusal shape, capture config).
	Policy *store.SecgwPolicy `json:"-"`
}

// Empty reports that no check applies.
func (e *Effective) Empty() bool { return e == nil || len(e.Checks) == 0 }

// Has reports whether a check kind is enabled for the given direction.
func (e *Effective) Has(kind store.SecgwCheckKind, direction string) (ResolvedCheck, bool) {
	if e == nil {
		return ResolvedCheck{}, false
	}
	c, ok := e.Checks[kind]
	if !ok || !c.Enabled {
		return ResolvedCheck{}, false
	}
	if c.Direction == store.SecgwDirectionBoth || c.Direction == direction {
		return c, true
	}
	return ResolvedCheck{}, false
}

// Resolve computes the effective checks for sub over snap. It is pure and
// allocation-light; the snapshot is refreshed by the caller on the config
// cache cadence.
func Resolve(snap *store.SecgwSnapshot, sub Subject) *Effective {
	eff := &Effective{Checks: map[store.SecgwCheckKind]ResolvedCheck{}}
	if snap.Empty() {
		return eff
	}
	applicable := make([]*store.SecgwBinding, 0, 4)
	for _, b := range snap.Bindings {
		if bindingApplies(b, sub) {
			applicable = append(applicable, b)
		}
	}
	sort.SliceStable(applicable, func(i, j int) bool {
		return store.SecgwScopeRank[applicable[i].ScopeType] < store.SecgwScopeRank[applicable[j].ScopeType]
	})
	for _, b := range applicable {
		p := snap.Policies[b.PolicyID]
		entry := TraceEntry{BindingID: b.ID, PolicyID: b.PolicyID, ScopeType: b.ScopeType, ScopeID: b.ScopeID}
		if p == nil {
			entry.Outcome = "skipped_missing_policy"
			eff.Trace = append(eff.Trace, entry)
			continue
		}
		entry.PolicyName = p.Name
		if !p.Enabled {
			entry.Outcome = "skipped_disabled"
			eff.Trace = append(eff.Trace, entry)
			continue
		}
		floor := p.Mandatory && b.ScopeType == store.SecgwScopeOrg
		entry.Outcome = "applied"
		for _, c := range p.Checks {
			cand := ResolvedCheck{SecgwCheck: c, PolicyID: p.ID, PolicyName: p.Name, BindingID: b.ID, ScopeType: b.ScopeType, Floor: floor}
			prev, exists := eff.Checks[c.Kind]
			if !exists || !prev.Floor {
				eff.Checks[c.Kind] = cand
				continue
			}
			// prev is a floor: only tightening is allowed.
			merged, relaxed := tighten(prev, cand)
			eff.Checks[c.Kind] = merged
			if relaxed != "" {
				eff.Trace = append(eff.Trace, TraceEntry{BindingID: b.ID, PolicyID: p.ID, PolicyName: p.Name, ScopeType: b.ScopeType, ScopeID: b.ScopeID,
					Outcome: "overridden_by_mandatory", Detail: string(c.Kind) + ": " + relaxed})
			}
		}
		// A floor also protects against REMOVAL: a later policy that omits
		// a floored kind leaves it in place, which the loop above already
		// guarantees by never deleting. A later policy that lists the kind
		// with Enabled=false is a relaxation and is refused in tighten.
		eff.Policy = p
		eff.Trace = append(eff.Trace, entry)
	}
	return eff
}

func bindingApplies(b *store.SecgwBinding, sub Subject) bool {
	switch b.ScopeType {
	case store.SecgwScopeOrg:
		return true
	case store.SecgwScopeGroup:
		for _, g := range sub.GroupIDs {
			if g == b.ScopeID {
				return true
			}
		}
		return false
	case store.SecgwScopeUpstream:
		return sub.UpstreamID != "" && sub.UpstreamID == b.ScopeID
	case store.SecgwScopeServiceToken:
		return sub.ServiceTokenID != "" && sub.ServiceTokenID == b.ScopeID
	case store.SecgwScopeModel:
		return sub.ModelID != "" && sub.ModelID == b.ScopeID
	case store.SecgwScopeManagedModel:
		return sub.ManagedModelID != "" && sub.ManagedModelID == b.ScopeID
	}
	return false
}

var modeRank = map[string]int{store.SecgwModeObserve: 0, store.SecgwModeRedact: 1, store.SecgwModeBlock: 2}

// tighten merges cand onto a floored prev. It returns the merged check and
// a description of the first relaxation that was refused ("" when none).
func tighten(prev, cand ResolvedCheck) (ResolvedCheck, string) {
	out := prev
	relaxed := ""
	if !cand.Enabled && prev.Enabled {
		relaxed = "cannot disable a mandatory check"
	}
	if modeRank[cand.Mode] > modeRank[prev.Mode] {
		out.Mode = cand.Mode
		out.PolicyID, out.PolicyName, out.BindingID, out.ScopeType = cand.PolicyID, cand.PolicyName, cand.BindingID, cand.ScopeType
	} else if modeRank[cand.Mode] < modeRank[prev.Mode] && relaxed == "" {
		relaxed = "cannot relax mode " + prev.Mode + " to " + cand.Mode
	}
	if cand.Fail == store.SecgwFailClosed && prev.Fail == store.SecgwFailOpen {
		out.Fail = store.SecgwFailClosed
	} else if cand.Fail == store.SecgwFailOpen && prev.Fail == store.SecgwFailClosed && relaxed == "" {
		relaxed = "cannot relax fail-closed to fail-open"
	}
	// Direction: "both" is a superset of either; widening is tightening.
	if cand.Direction == store.SecgwDirectionBoth && prev.Direction != store.SecgwDirectionBoth {
		out.Direction = store.SecgwDirectionBoth
	} else if prev.Direction == store.SecgwDirectionBoth && cand.Direction != store.SecgwDirectionBoth && relaxed == "" {
		relaxed = "cannot narrow direction from both to " + cand.Direction
	}
	if cand.HoldBytes > prev.HoldBytes {
		out.HoldBytes = cand.HoldBytes
	} else if cand.HoldBytes != 0 && cand.HoldBytes < prev.HoldBytes && relaxed == "" {
		relaxed = "cannot lower hold_bytes"
	}
	// Options and classifier follow whichever policy owns the check now; a
	// more specific policy may change WHICH classifier backs a floored
	// check but not whether the check runs.
	if len(cand.Options) > 0 {
		// content_safety categories are part of the floor: a team policy
		// may ADD hazard categories to a mandatory org set, never drop one.
		// Otherwise a group-scoped binding could quietly un-block S4.
		if prev.Kind == store.SecgwCheckContentSafety {
			merged, dropped := mergeCategories(prev.Options, cand.Options)
			if dropped != "" && relaxed == "" {
				relaxed = "cannot remove mandatory category " + dropped
			}
			out.Options = merged
		} else {
			out.Options = cand.Options
		}
	}
	if cand.ClassifierModelID != "" {
		out.ClassifierModelID = cand.ClassifierModelID
	}
	out.Floor = true
	return out, relaxed
}

// ContentSafetyOptions is the content_safety check's kind-specific config.
type ContentSafetyOptions struct {
	// Categories are the S-codes that ENFORCE (block/observe per mode). A
	// verdict naming only codes outside this set is still recorded as
	// observed so the admin can see what the guard flagged, but never
	// blocks. Empty means "every category" — the safe default when an
	// admin has not narrowed it.
	Categories []string `json:"categories"`
	TimeoutMs  int      `json:"timeout_ms,omitempty"`
}

// Enforces reports whether any of the verdict's codes is selected. An
// empty selection enforces everything.
func (o ContentSafetyOptions) Enforces(codes []string) bool {
	if len(o.Categories) == 0 {
		return len(codes) > 0
	}
	for _, c := range codes {
		for _, sel := range o.Categories {
			if c == sel {
				return true
			}
		}
	}
	return false
}

// mergeCategories unions the floor's categories into the candidate's and
// reports the first floor category the candidate tried to drop. Non-
// category fields (timeout) follow the candidate. An EMPTY floor means
// "everything", which a candidate can only tighten to — never narrow.
func mergeCategories(floor, cand json.RawMessage) (json.RawMessage, string) {
	var f, c ContentSafetyOptions
	_ = json.Unmarshal(floor, &f)
	_ = json.Unmarshal(cand, &c)
	if len(f.Categories) == 0 {
		// Floor enforces all categories; the candidate cannot narrow it.
		if len(c.Categories) > 0 {
			c.Categories = nil
			out, _ := json.Marshal(c)
			return out, "(all categories are mandatory)"
		}
		return cand, ""
	}
	have := map[string]bool{}
	for _, x := range c.Categories {
		have[x] = true
	}
	dropped := ""
	if len(c.Categories) == 0 {
		// Candidate says "all": that is a superset, nothing dropped.
		return cand, ""
	}
	for _, x := range f.Categories {
		if !have[x] {
			if dropped == "" {
				dropped = x
			}
			c.Categories = append(c.Categories, x)
		}
	}
	out, _ := json.Marshal(c)
	return out, dropped
}
