package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

// headerReasoningAdjusted tells the caller Janus changed the requested
// reasoning effort to fit the model, e.g. "medium->omitted" or "medium->high".
const headerReasoningAdjusted = "X-Janus-Reasoning-Adjusted"

// maxRefusalBody bounds how much of a vendor 400 is read to learn from it.
const maxRefusalBody = 64 << 10

// connReasoning decodes a connection's per-model reasoning facts.
func connReasoning(conn *store.SubscriptionConnection) map[string]subscription.Reasoning {
	out := map[string]subscription.Reasoning{}
	if conn != nil && len(conn.Reasoning) > 0 {
		_ = json.Unmarshal(conn.Reasoning, &out)
	}
	return out
}

// reasoningFit is what happened to one request's reasoning effort.
type reasoningFit struct {
	requested string // what the client asked for ("" = nothing)
	sent      string // what goes to the vendor ("" = omitted)
}

func (f reasoningFit) changed() bool { return f.requested != f.sent }

// label is the short form used in the header and the usage record.
func (f reasoningFit) label() string {
	if !f.changed() {
		return ""
	}
	to := f.sent
	if to == "" {
		to = "omitted"
	}
	return f.requested + "->" + to
}

// fitReasoningBody fits a chat/responses body's reasoning effort to what
// the model accepts. The body is returned unchanged when there is nothing
// to fit or it is not a JSON object.
func fitReasoningBody(body []byte, facts *subscription.Reasoning) ([]byte, reasoningFit) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, reasoningFit{}
	}
	requested := strings.ToLower(strings.TrimSpace(subscription.RequestedEffort(m)))
	fit := reasoningFit{requested: requested, sent: requested}
	if requested == "" || facts == nil {
		return body, fit
	}
	sent, changed := facts.Fit(requested)
	if !changed {
		return body, fit
	}
	subscription.SetEffort(m, sent)
	out, err := json.Marshal(m)
	if err != nil {
		return body, reasoningFit{requested: requested, sent: requested}
	}
	fit.sent = sent
	return out, fit
}

// learnReasoningRefusal reads a vendor 400 (up to maxRefusalBody) and, when
// it is a refusal of the reasoning value Janus sent, stores what it teaches
// and returns the updated facts. The body read is returned either way so
// the caller can still relay it.
func (s *Server) learnReasoningRefusal(ctx context.Context, route *personalRoute, sent string, r io.Reader) (*subscription.Reasoning, []byte) {
	raw, _ := io.ReadAll(io.LimitReader(r, maxRefusalBody))
	all := connReasoning(route.conn)
	native := route.model.Name
	var prev *subscription.Reasoning
	if p, ok := all[native]; ok {
		prev = &p
	}
	next, ok := subscription.LearnFromRefusal(prev, sent, raw)
	if !ok {
		return nil, raw
	}
	all[native] = next
	if encoded, err := json.Marshal(all); err == nil {
		if err := s.Store.SetSubscriptionReasoning(ctx, route.conn.ID, encoded); err != nil {
			s.Logger.WarnContext(ctx, "store learned reasoning", "subscription_id", route.conn.ID, "error", err.Error())
		} else {
			route.conn.Reasoning = encoded
		}
	}
	s.Logger.InfoContext(ctx, "personal subscription: learned reasoning limits", "provider", route.provider.ID(),
		"model", native, "refused", sent, "levels", strings.Join(next.Levels, ","), "rejected", strings.Join(next.Rejected, ","),
		"supported", fmt.Sprint(next.Supported != nil && *next.Supported))
	return &next, raw
}

// refreshReasoningFacts stores a fresh catalog's reasoning facts, keeping
// what was learned where the catalog is silent.
func (s *Server) refreshReasoningFacts(ctx context.Context, conn *store.SubscriptionConnection, models []string, catalog map[string]subscription.Reasoning) {
	merged := subscription.MergeReasoning(models, catalog, connReasoning(conn))
	encoded, err := json.Marshal(merged)
	if err != nil {
		return
	}
	if err := s.Store.SetSubscriptionReasoning(ctx, conn.ID, encoded); err != nil {
		s.Logger.WarnContext(ctx, "store reasoning facts", "subscription_id", conn.ID, "error", err.Error())
	}
}
