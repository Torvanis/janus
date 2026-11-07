package httpapi

import (
	"context"
	"net/http"

	"github.com/torvanis/janus/internal/headerpolicy"
	"github.com/torvanis/janus/internal/store"
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
	mode := headerpolicy.LogValues
	var secret map[string]bool
	if snap != nil {
		mode = snap.LogMode
	}
	if mode == headerpolicy.LogOff {
		return
	}
	entry := &store.RequestHeaderLog{
		Inbound: headerpolicy.ForLog(rw.inbound, mode, nil),
		Changes: rw.changes,
		Target:  rw.target,
	}
	if rw.policy != nil {
		entry.PolicyID, entry.PolicyName = rw.policy.ID, rw.policy.Name
		if snap != nil {
			secret = snap.Secret[rw.policy.ID]
		}
	}
	entry.Outbound = headerpolicy.ForLog(outbound, mode, secret)
	event.AfterInsertHeaders = func(ctx context.Context) {
		entry.UsageID, entry.CreatedAt = event.ID, event.CreatedAt
		if err := s.Store.InsertRequestHeaderLog(ctx, entry); err != nil {
			s.Logger.WarnContext(ctx, "write request header log", "error", err, "usage_id", event.ID)
		}
	}
}
