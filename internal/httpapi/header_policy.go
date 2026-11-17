package httpapi

import (
	"context"
	"net/http"
	"net/url"

	"github.com/torvanis/janus/internal/headerpolicy"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

// headerPolicySnapshot returns the cached policies and assignments. A load
// error is logged and treated as "no policy" for that read: header rewriting
// is not a security boundary the proxy may fail closed on, and refusing all
// traffic because a policy row is unreadable would be worse.
func (s *Server) headerPolicySnapshot(ctx context.Context) *store.HeaderPolicySnapshot {
	snap, err := cachedRead(&s.configCache, "header_policy", func() (*store.HeaderPolicySnapshot, error) {
		return s.Store.LoadHeaderPolicySnapshot(ctx, s.headerCipher())
	})
	if err != nil {
		s.Logger.WarnContext(ctx, "header policy snapshot unavailable; forwarding caller headers unchanged", "error", err)
		return nil
	}
	return snap
}

// headerRewrite is the per-request outcome, logged after the event row.
type headerRewrite struct {
	policy  *headerpolicy.Policy
	target  string
	inbound http.Header
	changes []headerpolicy.Change
}

// applyHeaderPolicy rewrites h (a clone of the caller's headers) with the
// policy for the target and returns what to log. targetType/targetID name
// the upstream or the personal-subscription provider.
func (s *Server) applyHeaderPolicy(ctx context.Context, h http.Header, targetType, targetID string) *headerRewrite {
	snap := s.headerPolicySnapshot(ctx)
	out := &headerRewrite{inbound: h.Clone()}
	if snap == nil {
		return out
	}
	p, scope := snap.Resolve(targetType, targetID)
	out.policy = p
	out.target = scope
	out.changes = headerpolicy.Apply(h, p)
	return out
}

// recordHeaderLog stores the request's headers once the usage row exists.
// outbound is the header set the provider received (adapter credentials
// included, and masked).
func (s *Server) recordHeaderLog(event *store.UsageEvent, rw *headerRewrite, outbound http.Header) {
	if rw == nil || event == nil {
		return
	}
	snap := s.headerPolicySnapshot(context.Background())
	// When the setting cannot be read, keep the least: names only.
	mode := headerpolicy.LogNames
	var secret map[string]bool
	if snap != nil {
		mode = snap.LogMode
		if rw.policy != nil {
			secret = snap.Secret[rw.policy.ID]
		}
	}
	if mode == headerpolicy.LogOff {
		return
	}
	// A header a rule marks secret is masked in both lists: the caller may
	// send that same header, and it is no less secret on the way in.
	entry := &store.RequestHeaderLog{
		Inbound: headerpolicy.ForLog(rw.inbound, mode, secret),
		Changes: rw.changes,
		Target:  rw.target,
	}
	if rw.policy != nil {
		entry.PolicyID, entry.PolicyName = rw.policy.ID, rw.policy.Name
	}
	entry.Outbound = headerpolicy.ForLog(outbound, mode, secret)
	event.AfterInsertHeaders = func(ctx context.Context) {
		entry.UsageID, entry.CreatedAt = event.ID, event.CreatedAt
		if err := s.Store.InsertRequestHeaderLog(ctx, entry); err != nil {
			s.Logger.WarnContext(ctx, "write request header log", "error", err, "usage_id", event.ID)
		}
	}
}

// upstreamPolicy returns a lookup of the header policy in force for one
// upstream (its own, else the default), read from the cached snapshot each
// time so an edit applies without a restart.
func (s *Server) upstreamPolicy(upstreamID string) func(ctx context.Context) *headerpolicy.Policy {
	return func(ctx context.Context) *headerpolicy.Policy {
		p, _ := s.headerPolicySnapshot(ctx).Resolve(store.HeaderTargetUpstream, upstreamID)
		return p
	}
}

// ownUpstreamClient is the client for a call Janus itself makes to an upstream
// (discovery, probes, classifiers, decision models, routing evaluation,
// background job polls). It is upstreamHTTPClient plus the upstream's header
// policy: the headers the policy adds go out on every request to that
// upstream's origin, so a policy that carries the upstream's access
// credentials (a Cloudflare Access service token, a gateway key) works for
// these calls as it does for users' requests.
//
// Every call to an upstream that does not come from a caller's request must
// use this, never upstreamHTTPClient directly: TestOwnUpstreamCallsUseThePolicyClient
// lists the remaining direct uses and fails when one is added.
func (s *Server) ownUpstreamClient(ctx context.Context, upstreamID, baseURL string) *http.Client {
	return headerpolicy.Client(s.upstreamHTTPClient(ctx), baseURL, s.upstreamPolicy(upstreamID))
}

// ownPersonalClient is ownUpstreamClient for a personal plan provider: the
// client for the calls Janus itself makes to the vendor (sign-in, token
// renewal and revocation, account, model list, plan usage). The headers the
// provider's policy adds go out on requests to the provider's own endpoints
// (subscription.OwnsURL) and to no other host. The policy is the one an
// administrator assigned to the provider for everyone, else the default.
func (s *Server) ownPersonalClient(ctx context.Context, provider subscription.Provider) *http.Client {
	id := provider.ID()
	return headerpolicy.ClientFor(s.upstreamHTTPClient(ctx),
		func(u *url.URL) bool { return subscription.OwnsURL(provider, u) },
		func(ctx context.Context) *headerpolicy.Policy {
			p, _ := s.headerPolicySnapshot(ctx).Resolve(store.HeaderTargetPersonal, id)
			return p
		})
}
