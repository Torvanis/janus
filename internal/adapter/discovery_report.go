package adapter

import (
	"context"
	"sync"
)

// DiscoveryReport collects what a Discover call could not list. Some
// providers list a class of models in a second, best-effort request
// (OpenRouter's decision models); when that request fails, Discover still
// returns the rest, and the report tells discovery not to read the missing
// rows as "the models are gone".
type DiscoveryReport struct {
	mu       sync.Mutex
	unlisted map[string]string // classifier role -> why the listing failed
}

type discoveryReportKey struct{}

// WithDiscoveryReport returns ctx carrying r for Discover to fill in.
func WithDiscoveryReport(ctx context.Context, r *DiscoveryReport) context.Context {
	return context.WithValue(ctx, discoveryReportKey{}, r)
}

// Unlisted returns the classifier roles whose listing failed, with the
// reason for each.
func (r *DiscoveryReport) Unlisted() map[string]string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.unlisted))
	for k, v := range r.unlisted {
		out[k] = v
	}
	return out
}

// reportUnlisted records that the models with classifier role role could
// not be listed this run.
func reportUnlisted(ctx context.Context, role string, err error) {
	r, _ := ctx.Value(discoveryReportKey{}).(*DiscoveryReport)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unlisted == nil {
		r.unlisted = map[string]string{}
	}
	r.unlisted[role] = err.Error()
}
