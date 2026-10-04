package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

// Background verification of personal subscriptions.
//
// Sign-ins die quietly: a vendor revokes a grant, a refresh token expires
// after a week of disuse, a plan lapses. Without a check the user finds out
// on their next request. Every SubscriptionCheckInterval each active
// connection is verified once (across all replicas, via a compare-and-swap
// claim on checked_at):
//
//   - an access token that is close to expiring is renewed, so refresh
//     tokens that expire when unused stay alive;
//   - the model list is read with the credential, which proves the vendor
//     still accepts it without spending a request from the plan;
//   - the vendor's plan usage is recorded when it publishes one.
//
// A vendor verdict that the credential is dead marks the connection
// "needs reconnect"; a network or vendor outage only records check_error so
// a flaky minute never forces a reconnect.

// SubscriptionCheckInterval is how often each connection is verified.
const SubscriptionCheckInterval = 3 * time.Hour

// subscriptionCheckTick is how often the job looks for due connections.
const subscriptionCheckTick = 5 * time.Minute

// subscriptionRenewAhead renews access tokens that expire within this long,
// so a token never lapses between two checks.
const subscriptionRenewAhead = SubscriptionCheckInterval + 30*time.Minute

// StartSubscriptionChecks runs the verification loop until ctx ends.
func (s *Server) StartSubscriptionChecks(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(subscriptionCheckTick)
		defer t.Stop()
		for {
			s.runSubscriptionChecks(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (s *Server) runSubscriptionChecks(ctx context.Context) {
	enabled, err := s.enabledSubscriptionProviders(ctx)
	if err != nil || len(enabled) == 0 {
		return
	}
	conns, err := s.Store.ListAllSubscriptions(ctx)
	if err != nil {
		s.Logger.WarnContext(ctx, "list subscriptions for check", "error", err.Error())
		return
	}
	for _, c := range conns {
		if ctx.Err() != nil {
			return
		}
		if !enabled[c.Provider] || c.Status != store.SubscriptionActive || time.Since(c.CheckedAt) < SubscriptionCheckInterval {
			continue
		}
		won, err := s.Store.ClaimSubscriptionCheck(ctx, c.ID, c.CheckedAt)
		if err != nil || !won {
			continue
		}
		full, err := s.Store.SubscriptionByID(ctx, c.ID)
		if err != nil {
			continue
		}
		s.checkSubscription(ctx, full, true)
	}
}

// checkSubscription verifies one connection and records the outcome. It
// returns the reason when the connection now needs reconnecting. renewAhead
// renews an access token that would expire before the next scheduled check
// (the background job); an on-demand check only renews what has expired.
func (s *Server) checkSubscription(ctx context.Context, conn *store.SubscriptionConnection, renewAhead bool) string {
	provider, ok := subscription.Get(conn.Provider)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	client := s.upstreamHTTPClient(ctx)
	route := &personalRoute{provider: provider, conn: conn}

	// Renew early so an unused refresh token does not age out.
	if renewAhead && !conn.AccessExpiresAt.IsZero() && time.Until(conn.AccessExpiresAt) < subscriptionRenewAhead && conn.AutoRenews {
		_ = s.Store.ExpireSubscriptionAccess(ctx, conn.ID)
		if fresh, err := s.Store.SubscriptionByID(ctx, conn.ID); err == nil {
			route.conn = fresh
		}
	}
	token, apiErr := s.subscriptionAccessToken(ctx, route)
	if apiErr != nil {
		if apiErr.Code == CodeSubscriptionReauth {
			return "reauth"
		}
		_ = s.Store.RecordSubscriptionCheck(ctx, conn.ID, "Could not renew the sign-in: "+apiErr.Message, nil)
		return ""
	}
	models, catalog, err := subscription.FetchModels(ctx, provider, client, token)
	if err != nil {
		var reauth *subscription.ReauthError
		if errors.As(err, &reauth) {
			_ = s.Store.MarkSubscriptionReauth(ctx, conn.ID, reauth.Reason)
			s.Logger.WarnContext(ctx, "subscription check: needs reconnect", "subscription_id", conn.ID, "provider", provider.ID(), "reason", reauth.Reason)
			return reauth.Reason
		}
		_ = s.Store.RecordSubscriptionCheck(ctx, conn.ID, provider.DisplayName()+" did not answer the check: "+err.Error(), nil)
		return ""
	}
	// The catalog is the cheap check; its reasoning facts keep the proxy's
	// fitting current as vendors add levels.
	s.refreshReasoningFacts(ctx, conn, models, catalog)
	var usage []byte
	if reporter, ok := provider.(subscription.UsageReporter); ok {
		u, err := reporter.Usage(ctx, client, token, conn.Meta)
		if err == nil && u != nil {
			usage, _ = json.Marshal(u)
		} else if err != nil {
			// Plan usage is informational; never fail the check on it.
			s.Logger.InfoContext(ctx, "subscription usage lookup", "provider", provider.ID(), "error", err.Error())
		}
	}
	_ = s.Store.RecordSubscriptionCheck(ctx, conn.ID, "", usage)
	return ""
}

// handleCheckMySubscription runs the verification now ("Check now" on the
// card) and returns the updated connection.
func (s *Server) handleCheckMySubscription(w http.ResponseWriter, r *http.Request) {
	if !s.requireSubscriptions(w, r) {
		return
	}
	conn, ok := s.ownedSubscription(w, r)
	if !ok {
		return
	}
	if conn.Status == store.SubscriptionActive {
		s.checkSubscription(r.Context(), conn, false)
	}
	updated, err := s.Store.SubscriptionByID(r.Context(), conn.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, s.connectionView(r.Context(), updated))
}
