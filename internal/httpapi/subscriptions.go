package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
)

// Personal provider subscriptions.
//
// A user connects their OWN vendor plan (SuperGrok, …) through the vendor's
// device sign-in, then calls it through Janus as my/<provider>/<model> with
// their normal Janus token. The connection is bound to that one user: the
// proxy only ever looks it up by the calling user's id, a service token can
// never use one, and there is no fallback to anyone else's connection.
//
// Organization controls still apply (policy rules, rate limits, Security
// Gateway, token and request quotas). The organization pays nothing, so the
// usage event records zero cost with cost_status known_free and carries
// subscription_id, which is what reports filter on.

// refreshSkew refreshes an access token this long before it expires, so a
// request never starts with a credential that dies mid-stream.
const refreshSkew = 2 * time.Minute

// personalRoute is a resolved my/<provider>/<model> request.
type personalRoute struct {
	provider subscription.Provider
	conn     *store.SubscriptionConnection
	model    *store.Model
}

// subscriptionRefreshLocks serializes refreshes per connection inside one
// replica. Across replicas, RotateSubscriptionTokens' compare-and-swap is
// what keeps a rotated refresh token from being lost.
var subscriptionRefreshLocks sync.Map

// subscriptionsAvailable reports whether personal subscriptions can be used
// at all: never in offline mode (the feature exists to reach vendor clouds,
// which an air-gapped install cannot), otherwise the administrator's master
// switch decides.
func (s *Server) subscriptionsAvailable(ctx context.Context) (bool, error) {
	if s.Config.Offline {
		return false, nil
	}
	return s.Store.PersonalSubscriptionsEnabled(ctx)
}

// enabledSubscriptionProviders returns the providers a user may use right
// now: empty whenever the feature itself is unavailable.
func (s *Server) enabledSubscriptionProviders(ctx context.Context) (map[string]bool, error) {
	on, err := s.subscriptionsAvailable(ctx)
	if err != nil || !on {
		return map[string]bool{}, err
	}
	settings, err := s.Store.SubscriptionProviderSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, p := range subscription.All() {
		if settings[p.ID()] {
			out[p.ID()] = true
		}
	}
	return out, nil
}

// resolvePersonalRoute authorizes and resolves a my/<provider>/<model> name
// for the calling user. It never consults grants: the grant is "this is your
// own connected plan and your administrator allows the provider".
func (s *Server) resolvePersonalRoute(ctx context.Context, user *store.User, serviceToken *store.ServiceToken, modelName string) (*personalRoute, *APIError) {
	providerID, native, ok := subscription.ParseModel(modelName)
	if !ok {
		return nil, ErrInvalidRequest("Personal subscription models are addressed as my/<provider>/<model>, for example my/xai/grok-4.3. Call /v1/models to list yours.").WithParam("model")
	}
	if serviceToken != nil || user == nil {
		return nil, ErrForbidden("Personal subscriptions belong to one person and cannot be used with a service token. Use the organization's models instead.")
	}
	provider, ok := subscription.Get(providerID)
	if !ok {
		return nil, ErrModelNotGranted(modelName)
	}
	available, err := s.subscriptionsAvailable(ctx)
	if err != nil {
		s.Logger.ErrorContext(ctx, "read personal subscription setting", "error", err.Error())
		return nil, ErrInternal()
	}
	if !available {
		return nil, ErrForbidden("Personal subscriptions are turned off for this organization. Use the organization's models instead.")
	}
	enabled, err := s.enabledSubscriptionProviders(ctx)
	if err != nil {
		s.Logger.ErrorContext(ctx, "read subscription provider settings", "error", err.Error())
		return nil, ErrInternal()
	}
	if !enabled[providerID] {
		return nil, ErrForbidden("Personal " + provider.DisplayName() + " subscriptions are turned off for this organization. Ask a Janus administrator to enable them.")
	}
	conn, err := s.Store.UserSubscription(ctx, user.ID, providerID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrForbidden("You have not connected a " + provider.DisplayName() + " subscription. Connect one on the Subscriptions page in Janus, then retry.")
	}
	if err != nil {
		s.Logger.ErrorContext(ctx, "load subscription", "error", err.Error())
		return nil, ErrInternal()
	}
	if conn.Status != store.SubscriptionActive {
		return nil, subscriptionReauthError(provider, conn.LastError)
	}
	if !slices.Contains(conn.SelectedModels, native) {
		// Same code as any model the caller cannot use, with the one step
		// that fixes it: the model exists on their plan but is switched off.
		return nil, newError(http.StatusForbidden, CodeModelNotGranted, "permission_error",
			"The model "+modelName+" is not turned on for your "+provider.DisplayName()+" subscription. Select it on the Subscriptions page in Janus, then retry.").WithParam("model")
	}
	return &personalRoute{
		provider: provider, conn: conn,
		model: &store.Model{
			ID:           "subscription:" + providerID + ":" + native,
			UpstreamID:   "subscription:" + providerID,
			UpstreamName: provider.DisplayName(),
			AdapterType:  provider.AdapterType(),
			Name:         native,
			DisplayName:  modelName,
			Status:       store.ModelEnabled,
		},
	}, nil
}

// baseURL is where this connection's requests go: the provider's fixed
// endpoint, or the account's own host for providers that route per account.
func (r *personalRoute) baseURL() string {
	if cr, ok := r.provider.(subscription.ConnectionRouter); ok {
		return cr.ConnectionBaseURL(r.conn.Meta)
	}
	return r.provider.InferenceBaseURL()
}

// accessExpiring reports whether the stored access credential must be
// renewed before use. A zero expiry means it does not expire on a timer.
func accessExpiring(c *store.SubscriptionConnection) bool {
	return !c.AccessExpiresAt.IsZero() && time.Until(c.AccessExpiresAt) <= refreshSkew
}

func subscriptionReauthError(p subscription.Provider, reason string) *APIError {
	msg := "Your " + p.DisplayName() + " subscription needs to be reconnected. Open the Subscriptions page in Janus and connect it again."
	if reason != "" {
		msg += " Reason: " + reason
	}
	return newError(http.StatusForbidden, CodeSubscriptionReauth, "permission_error", msg)
}

// subscriptionAccessToken returns a usable access token for the connection,
// refreshing it first when it is expired or about to expire.
func (s *Server) subscriptionAccessToken(ctx context.Context, route *personalRoute) (string, *APIError) {
	conn := route.conn
	if !accessExpiring(conn) {
		token, err := s.Cipher.Decrypt(conn.EncryptedAccessToken())
		if err == nil {
			return token, nil
		}
		s.Logger.ErrorContext(ctx, "decrypt subscription access token", "subscription_id", conn.ID, "error", err.Error())
		return "", ErrUpstreamUnavailable(route.provider.DisplayName())
	}
	lock, _ := subscriptionRefreshLocks.LoadOrStore(conn.ID, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	// Another request on this replica (or another replica) may have
	// refreshed while we waited: re-read before spending the refresh token.
	fresh, err := s.Store.SubscriptionByID(ctx, conn.ID)
	if err != nil {
		return "", ErrUpstreamUnavailable(route.provider.DisplayName())
	}
	if fresh.Status != store.SubscriptionActive {
		return "", subscriptionReauthError(route.provider, fresh.LastError)
	}
	if !accessExpiring(fresh) {
		token, err := s.Cipher.Decrypt(fresh.EncryptedAccessToken())
		if err != nil {
			return "", ErrUpstreamUnavailable(route.provider.DisplayName())
		}
		return token, nil
	}
	refresh, err := s.Cipher.Decrypt(fresh.EncryptedRefreshToken())
	if err != nil || refresh == "" {
		reason := "Janus could not read the stored sign-in."
		if err == nil {
			reason = "The sign-in expired and this provider cannot renew it."
		}
		_ = s.Store.MarkSubscriptionReauth(ctx, fresh.ID, reason)
		return "", subscriptionReauthError(route.provider, reason)
	}
	tokens, err := route.provider.Refresh(ctx, s.upstreamHTTPClient(ctx), refresh)
	var reauth *subscription.ReauthError
	if errors.As(err, &reauth) {
		_ = s.Store.MarkSubscriptionReauth(ctx, fresh.ID, reauth.Reason)
		s.Logger.WarnContext(ctx, "subscription needs reconnect", "subscription_id", fresh.ID, "provider", route.provider.ID(), "reason", reauth.Reason)
		return "", subscriptionReauthError(route.provider, reauth.Reason)
	}
	if err != nil {
		s.Logger.WarnContext(ctx, "subscription refresh failed", "subscription_id", fresh.ID, "provider", route.provider.ID(), "error", err.Error())
		return "", ErrUpstreamUnavailable(route.provider.DisplayName())
	}
	encAccess, err1 := s.Cipher.Encrypt(tokens.AccessToken)
	encRefresh, err2 := s.Cipher.Encrypt(tokens.RefreshToken)
	if err1 != nil || err2 != nil {
		return "", ErrInternal()
	}
	won, err := s.Store.RotateSubscriptionTokens(ctx, fresh.ID, fresh.EncryptedRefreshToken(), encAccess, encRefresh, tokens.ExpiresAt)
	if err != nil {
		s.Logger.ErrorContext(ctx, "store refreshed subscription tokens", "subscription_id", fresh.ID, "error", err.Error())
		return "", ErrInternal()
	}
	if !won {
		// Another replica refreshed with the same refresh token first; its
		// credentials are now the live ones.
		latest, err := s.Store.SubscriptionByID(ctx, fresh.ID)
		if err != nil {
			return "", ErrUpstreamUnavailable(route.provider.DisplayName())
		}
		token, err := s.Cipher.Decrypt(latest.EncryptedAccessToken())
		if err != nil {
			return "", ErrUpstreamUnavailable(route.provider.DisplayName())
		}
		return token, nil
	}
	return tokens.AccessToken, nil
}

// personalCatalog lists the caller's my/<provider>/<model> names for
// /v1/models and the SPA. Only active connections to enabled providers.
func (s *Server) personalCatalog(ctx context.Context, user *store.User) ([]map[string]any, error) {
	if user == nil {
		return nil, nil
	}
	enabled, err := s.enabledSubscriptionProviders(ctx)
	if err != nil || len(enabled) == 0 {
		return nil, err
	}
	conns, err := s.Store.ListUserSubscriptions(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, c := range conns {
		p, ok := subscription.Get(c.Provider)
		if !ok || !enabled[c.Provider] || c.Status != store.SubscriptionActive {
			continue
		}
		facts := connReasoning(c)
		for _, m := range c.Models {
			if !slices.Contains(c.SelectedModels, m) {
				continue
			}
			var efforts []string
			if f, ok := facts[m]; ok {
				efforts = f.Efforts()
			}
			entry := map[string]any{
				"id": subscription.ModelName(c.Provider, m), "object": "model", "created": c.CreatedAt.Unix(), "owned_by": p.DisplayName(),
				"janus": map[string]any{
					"upstream": p.DisplayName(), "adapter_type": p.AdapterType(), "native_name": m,
					"grant_source": "personal_subscription", "managed": false,
					"personal_subscription": map[string]any{"provider": c.Provider, "subscription_id": c.ID},
				},
			}
			// The accepted reasoning_effort values, when known: an empty
			// list means the model takes none. Unknown models omit it.
			// Anything else a client sends is fitted, never refused.
			if efforts != nil {
				entry["janus"].(map[string]any)["reasoning_efforts"] = efforts
			}
			out = append(out, entry)
		}
	}
	return out, nil
}

// notePersonalResponse reacts to the provider's verdict on the credential.
// A 401 means the access token died early: force a refresh next time. A 403
// from xAI means the plan is not entitled to API use: the user must act.
func (s *Server) notePersonalResponse(ctx context.Context, route *personalRoute, status int) {
	switch status {
	case http.StatusUnauthorized:
		if err := s.Store.ExpireSubscriptionAccess(ctx, route.conn.ID); err != nil {
			s.Logger.WarnContext(ctx, "expire subscription access", "subscription_id", route.conn.ID, "error", err.Error())
		}
	case http.StatusForbidden:
		s.Logger.WarnContext(ctx, "provider refused subscription request", "subscription_id", route.conn.ID, "provider", route.provider.ID())
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	go func() {
		defer cancel()
		_ = s.Store.TouchSubscription(bg, route.conn.ID)
	}()
}

// --- user API ------------------------------------------------------------------

func (s *Server) mountSubscriptionRoutes(r chi.Router) {
	r.Get("/me/subscriptions", s.handleListMySubscriptions)
	r.Post("/me/subscriptions/{provider}/connect", s.handleStartSubscriptionConnect)
	r.Post("/me/subscriptions/connect/{pendingID}/poll", s.handlePollSubscriptionConnect)
	r.Post("/me/subscriptions/{provider}/key", s.handleConnectSubscriptionKey)
	r.Post("/me/subscriptions/{id}/refresh-models", s.handleRefreshSubscriptionModels)
	r.Post("/me/subscriptions/{id}/check", s.handleCheckMySubscription)
	r.Put("/me/subscriptions/{id}/models", s.handleSelectSubscriptionModels)
	r.Delete("/me/subscriptions/{id}", s.handleDisconnectMySubscription)
}

func (s *Server) mountSubscriptionAdminRoutes(r chi.Router) {
	r.Get("/admin/subscriptions", s.handleAdminListSubscriptions)
	r.Put("/admin/subscriptions/feature", s.handleAdminSetSubscriptionFeature)
	r.Put("/admin/subscriptions/providers", s.handleAdminSetSubscriptionProviders)
	r.Delete("/admin/subscriptions/{id}", s.handleAdminDisconnectSubscription)
}

// errSubscriptionsUnavailable is returned by every user-facing write while
// the feature is off (by the administrator, or by offline mode).
func errSubscriptionsUnavailable(offline bool) *APIError {
	if offline {
		return ErrForbidden("Personal subscriptions are not available on an offline Janus installation.")
	}
	return ErrForbidden("Personal subscriptions are turned off for this organization.")
}

// requireSubscriptions writes the unavailable error and returns false when
// the feature is off.
func (s *Server) requireSubscriptions(w http.ResponseWriter, r *http.Request) bool {
	on, err := s.subscriptionsAvailable(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return false
	}
	if !on {
		WriteError(w, r, errSubscriptionsUnavailable(s.Config.Offline))
		return false
	}
	return true
}

type subscriptionProviderView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	// Auth is "device" (sign in at the vendor with a code) or "key" (paste
	// a key created at the vendor).
	Auth       string                      `json:"auth"`
	KeyHelp    string                      `json:"key_help,omitempty"`
	KeyURL     string                      `json:"key_url,omitempty"`
	Connection *subscriptionConnectionView `json:"connection"`
}

// subscriptionConnectionView is a connection plus Janus's own count of the
// traffic it carried, which every provider has even when the vendor
// publishes no plan usage.
type subscriptionConnectionView struct {
	*store.SubscriptionConnection
	Activity map[string]store.SubscriptionActivity `json:"activity"`
	// ReasoningEfforts lists, per model, the reasoning_effort values it
	// accepts ([] = none); models Janus knows nothing about are absent.
	ReasoningEfforts map[string][]string `json:"reasoning_efforts"`
}

func providerAuth(p subscription.Provider) (kind, help, link string) {
	if k, ok := p.(subscription.KeyAuth); ok {
		help, link = k.KeyHelp()
		return "key", help, link
	}
	return "device", "", ""
}

func (s *Server) connectionView(ctx context.Context, c *store.SubscriptionConnection) *subscriptionConnectionView {
	if c == nil {
		return nil
	}
	v := &subscriptionConnectionView{SubscriptionConnection: c, Activity: map[string]store.SubscriptionActivity{}, ReasoningEfforts: map[string][]string{}}
	for model, f := range connReasoning(c) {
		if e := f.Efforts(); e != nil {
			v.ReasoningEfforts[model] = e
		}
	}
	now := time.Now().UTC()
	for label, since := range map[string]time.Time{"day": now.Add(-24 * time.Hour), "week": now.Add(-7 * 24 * time.Hour)} {
		a, err := s.Store.SubscriptionActivitySince(ctx, c.ID, since)
		if err != nil {
			s.Logger.WarnContext(ctx, "subscription activity", "subscription_id", c.ID, "error", err.Error())
			continue
		}
		v.Activity[label] = a
	}
	return v
}

func (s *Server) handleListMySubscriptions(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	available, err := s.subscriptionsAvailable(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if !available {
		WriteJSON(w, http.StatusOK, map[string]any{"available": false, "providers": []subscriptionProviderView{}, "model_prefix": subscription.ModelPrefix})
		return
	}
	enabled, err := s.enabledSubscriptionProviders(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	conns, err := s.Store.ListUserSubscriptions(r.Context(), user.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	byProvider := map[string]*store.SubscriptionConnection{}
	for _, c := range conns {
		byProvider[c.Provider] = c
	}
	out := []subscriptionProviderView{}
	for _, p := range subscription.All() {
		// Disabled providers are hidden unless the user still holds a
		// connection they may want to remove.
		if !enabled[p.ID()] && byProvider[p.ID()] == nil {
			continue
		}
		kind, help, link := providerAuth(p)
		out = append(out, subscriptionProviderView{
			ID: p.ID(), Name: p.DisplayName(), Description: p.Description(),
			Enabled: enabled[p.ID()], Auth: kind, KeyHelp: help, KeyURL: link,
			Connection: s.connectionView(r.Context(), byProvider[p.ID()]),
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"available": true, "providers": out, "model_prefix": subscription.ModelPrefix})
}

func (s *Server) handleStartSubscriptionConnect(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	providerID := chi.URLParam(r, "provider")
	provider, ok := subscription.Get(providerID)
	if !ok {
		WriteError(w, r, ErrNotFoundf("That subscription provider"))
		return
	}
	enabled, err := s.enabledSubscriptionProviders(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if !enabled[providerID] {
		WriteError(w, r, ErrForbidden("Personal "+provider.DisplayName()+" subscriptions are turned off for this organization."))
		return
	}
	if _, isKey := provider.(subscription.KeyAuth); isKey {
		WriteError(w, r, ErrInvalidRequest(provider.DisplayName()+" connects with a key: paste it on the Subscriptions page."))
		return
	}
	auth, err := provider.StartDeviceAuthorization(r.Context(), s.upstreamHTTPClient(r.Context()))
	if err != nil {
		s.Logger.WarnContext(r.Context(), "start subscription device authorization", "provider", providerID, "error", err.Error())
		WriteError(w, r, ErrUpstreamUnavailable(provider.DisplayName()).WithReason(err.Error()))
		return
	}
	encDevice, err := s.Cipher.Encrypt(auth.DeviceCode)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	now := time.Now().UTC()
	pending := &store.SubscriptionPending{
		UserID: user.ID, Provider: providerID, UserCode: auth.UserCode,
		VerificationURI: auth.VerificationURI, VerificationURIComplete: auth.VerificationURIComplete,
		Interval: auth.Interval, NextPollAt: now.Add(auth.Interval), ExpiresAt: now.Add(auth.ExpiresIn),
	}
	if err := s.Store.CreateSubscriptionPending(r.Context(), pending, encDevice); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"pending_id": pending.ID, "provider": providerID,
		"user_code": auth.UserCode, "verification_uri": auth.VerificationURI,
		"verification_uri_complete": auth.VerificationURIComplete,
		"interval_seconds":          int(auth.Interval / time.Second),
		"expires_at":                pending.ExpiresAt,
	})
}

// handlePollSubscriptionConnect advances a device sign-in by at most one
// vendor call. The SPA calls it on the interval the start response gave.
func (s *Server) handlePollSubscriptionConnect(w http.ResponseWriter, r *http.Request) {
	if !s.requireSubscriptions(w, r) {
		return
	}
	user := UserFrom(r.Context())
	ctx := r.Context()
	pending, err := s.Store.SubscriptionPendingByID(ctx, chi.URLParam(r, "pendingID"), user.ID)
	if errors.Is(err, store.ErrNotFound) {
		WriteJSON(w, http.StatusOK, map[string]any{"status": "expired", "message": "This sign-in is no longer active. Start again."})
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	provider, ok := subscription.Get(pending.Provider)
	if !ok {
		_ = s.Store.DeleteSubscriptionPending(ctx, pending.ID)
		WriteError(w, r, ErrNotFoundf("That subscription provider"))
		return
	}
	if time.Now().After(pending.ExpiresAt) {
		_ = s.Store.DeleteSubscriptionPending(ctx, pending.ID)
		WriteJSON(w, http.StatusOK, map[string]any{"status": "expired", "message": "The code expired before it was approved. Start again."})
		return
	}
	claimed, err := s.Store.ClaimSubscriptionPoll(ctx, pending.ID, pending.Interval)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if !claimed {
		WriteJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	deviceCode, err := s.Cipher.Decrypt(pending.EncryptedDeviceCode())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	client := s.upstreamHTTPClient(ctx)
	tokens, err := provider.PollDeviceAuthorization(ctx, client, deviceCode)
	switch {
	case errors.Is(err, subscription.ErrAuthorizationPending):
		WriteJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	case errors.Is(err, subscription.ErrSlowDown):
		_, _ = s.Store.ClaimSubscriptionPoll(ctx, pending.ID, pending.Interval+5*time.Second)
		WriteJSON(w, http.StatusOK, map[string]any{"status": "pending", "interval_seconds": int((pending.Interval + 5*time.Second) / time.Second)})
		return
	case errors.Is(err, subscription.ErrExpired):
		_ = s.Store.DeleteSubscriptionPending(ctx, pending.ID)
		WriteJSON(w, http.StatusOK, map[string]any{"status": "expired", "message": "The code expired before it was approved. Start again."})
		return
	case errors.Is(err, subscription.ErrDenied):
		_ = s.Store.DeleteSubscriptionPending(ctx, pending.ID)
		WriteJSON(w, http.StatusOK, map[string]any{"status": "denied", "message": "The sign-in was declined at " + provider.DisplayName() + "."})
		return
	case err != nil:
		s.Logger.WarnContext(ctx, "poll subscription device authorization", "provider", provider.ID(), "error", err.Error())
		WriteJSON(w, http.StatusOK, map[string]any{"status": "pending", "message": "Still waiting on " + provider.DisplayName() + "; retrying."})
		return
	}
	// Approved. The device code is spent either way, so the pending row goes.
	_ = s.Store.DeleteSubscriptionPending(ctx, pending.ID)

	account, err := provider.Account(ctx, client, tokens.AccessToken)
	var reauth *subscription.ReauthError
	if errors.As(err, &reauth) {
		// e.g. a GitHub account with no Copilot: the user must act.
		WriteJSON(w, http.StatusOK, map[string]any{"status": "failed", "message": reauth.Reason})
		return
	}
	if err != nil {
		s.Logger.WarnContext(ctx, "read subscription account", "provider", provider.ID(), "error", err.Error())
		WriteJSON(w, http.StatusOK, map[string]any{"status": "failed", "message": "Signed in, but Janus could not read which " + provider.DisplayName() + " account it is. Try again."})
		return
	}
	s.completeSubscriptionConnect(w, r, provider, account, tokens)
}

// handleConnectSubscriptionKey connects a key-based provider (Mistral Vibe):
// the user pastes a key created at the vendor, Janus verifies it live and
// stores it encrypted like a sign-in token.
func (s *Server) handleConnectSubscriptionKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireSubscriptions(w, r) {
		return
	}
	ctx := r.Context()
	providerID := chi.URLParam(r, "provider")
	provider, ok := subscription.Get(providerID)
	if !ok {
		WriteError(w, r, ErrNotFoundf("That subscription provider"))
		return
	}
	keyed, ok := provider.(subscription.KeyAuth)
	if !ok {
		WriteError(w, r, ErrInvalidRequest(provider.DisplayName()+" connects by signing in, not with a key."))
		return
	}
	enabled, err := s.enabledSubscriptionProviders(ctx)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if !enabled[providerID] {
		WriteError(w, r, ErrForbidden("Personal "+provider.DisplayName()+" subscriptions are turned off for this organization."))
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	key := strings.TrimSpace(body.Key)
	if key == "" {
		WriteError(w, r, ErrInvalidRequest("Paste the key you created at "+provider.DisplayName()+".").WithParam("key"))
		return
	}
	account, err := keyed.VerifyKey(ctx, s.upstreamHTTPClient(ctx), key)
	if err != nil {
		var reauth *subscription.ReauthError
		msg := err.Error()
		if errors.As(err, &reauth) {
			msg = reauth.Reason
		}
		WriteJSON(w, http.StatusOK, map[string]any{"status": "failed", "message": msg})
		return
	}
	s.completeSubscriptionConnect(w, r, provider, account, &subscription.Tokens{AccessToken: key})
}

// completeSubscriptionConnect stores a verified credential as the user's
// connection for the provider and answers the connect call.
func (s *Server) completeSubscriptionConnect(w http.ResponseWriter, r *http.Request, provider subscription.Provider, account *subscription.Account, tokens *subscription.Tokens) {
	ctx := r.Context()
	user := UserFrom(ctx)
	client := s.upstreamHTTPClient(ctx)
	// One vendor account belongs to one Janus user. Connecting an account
	// that someone else already connected would make it shared.
	if existing, err := s.Store.SubscriptionByAccount(ctx, provider.ID(), account.Subject); err == nil && existing.UserID != user.ID {
		_ = provider.Revoke(ctx, client, tokens.RefreshToken)
		WriteJSON(w, http.StatusOK, map[string]any{"status": "failed", "message": "That " + provider.DisplayName() + " account is already connected to another Janus user. A subscription can only be connected by the person who owns it."})
		return
	}
	models, catalog, err := subscription.FetchModels(ctx, provider, client, tokens.AccessToken)
	var reauth *subscription.ReauthError
	if errors.As(err, &reauth) {
		_ = provider.Revoke(ctx, client, tokens.RefreshToken)
		WriteJSON(w, http.StatusOK, map[string]any{"status": "failed", "message": reauth.Reason})
		return
	}
	if err != nil {
		// Not fatal: the connection works; the list refreshes later.
		s.Logger.WarnContext(ctx, "list subscription models", "provider", provider.ID(), "error", err.Error())
		models = nil
	}
	encAccess, err1 := s.Cipher.Encrypt(tokens.AccessToken)
	encRefresh, err2 := s.Cipher.Encrypt(tokens.RefreshToken)
	if err1 != nil || err2 != nil {
		WriteError(w, r, ErrInternal())
		return
	}
	previous, _ := s.Store.UserSubscription(ctx, user.ID, provider.ID())
	// A first connection starts with nothing selected: the user picks the
	// few models they want rather than having a whole plan catalog appear
	// in every tool. A reconnect keeps the choices that still exist.
	var selected []string
	if previous != nil {
		selected = keepAvailable(previous.SelectedModels, models)
	}
	// Reasoning facts: the catalog's, plus anything learned on this
	// connection before a reconnect.
	reasoningFacts, _ := json.Marshal(subscription.MergeReasoning(models, catalog, connReasoning(previous)))
	conn, err := s.Store.UpsertSubscription(ctx, store.SubscriptionUpsert{
		UserID: user.ID, Provider: provider.ID(),
		AccountSubject: account.Subject, AccountEmail: account.Email, Name: account.Name,
		EncryptedAccess: encAccess, EncryptedRefresh: encRefresh, AccessExpiresAt: tokens.ExpiresAt,
		Meta: account.Meta, Models: models, SelectedModels: selected, Reasoning: reasoningFacts,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	action := "subscription_connected"
	if previous != nil {
		action = "subscription_reconnected"
	}
	s.audit(r, action, "subscription", conn.ID, nil, map[string]any{
		"provider": provider.ID(), "account_email": account.Email, "models": len(models),
	})
	// Read the plan usage now so the card is complete on first view.
	s.checkSubscription(context.WithoutCancel(ctx), conn, false)
	if fresh, err := s.Store.SubscriptionByID(ctx, conn.ID); err == nil {
		conn = fresh
	}
	WriteJSON(w, http.StatusOK, map[string]any{"status": "connected", "connection": s.connectionView(ctx, conn)})
}

func (s *Server) ownedSubscription(w http.ResponseWriter, r *http.Request) (*store.SubscriptionConnection, bool) {
	user := UserFrom(r.Context())
	conn, err := s.Store.SubscriptionByID(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && conn.UserID != user.ID) {
		// Another user's connection is reported as absent, not forbidden:
		// its existence is not this caller's business.
		WriteError(w, r, ErrNotFoundf("That subscription"))
		return nil, false
	}
	if err != nil {
		WriteError(w, r, err)
		return nil, false
	}
	return conn, true
}

func (s *Server) handleRefreshSubscriptionModels(w http.ResponseWriter, r *http.Request) {
	if !s.requireSubscriptions(w, r) {
		return
	}
	conn, ok := s.ownedSubscription(w, r)
	if !ok {
		return
	}
	provider, found := subscription.Get(conn.Provider)
	if !found {
		WriteError(w, r, ErrNotFoundf("That subscription provider"))
		return
	}
	route := &personalRoute{provider: provider, conn: conn}
	if conn.Status != store.SubscriptionActive {
		WriteError(w, r, subscriptionReauthError(provider, conn.LastError))
		return
	}
	token, apiErr := s.subscriptionAccessToken(r.Context(), route)
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	models, catalog, err := subscription.FetchModels(r.Context(), provider, s.upstreamHTTPClient(r.Context()), token)
	var reauth *subscription.ReauthError
	if errors.As(err, &reauth) {
		_ = s.Store.MarkSubscriptionReauth(r.Context(), conn.ID, reauth.Reason)
		WriteError(w, r, subscriptionReauthError(provider, reauth.Reason))
		return
	}
	if err != nil {
		WriteError(w, r, ErrUpstreamUnavailable(provider.DisplayName()).WithReason(err.Error()))
		return
	}
	if err := s.Store.SetSubscriptionModels(r.Context(), conn.ID, models, keepAvailable(conn.SelectedModels, models)); err != nil {
		WriteError(w, r, err)
		return
	}
	s.refreshReasoningFacts(r.Context(), conn, models, catalog)
	updated, err := s.Store.SubscriptionByID(r.Context(), conn.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, s.connectionView(r.Context(), updated))
}

// keepAvailable returns the members of selected that are still in available,
// in selected's order.
func keepAvailable(selected, available []string) []string {
	out := []string{}
	for _, m := range selected {
		if slices.Contains(available, m) {
			out = append(out, m)
		}
	}
	return out
}

// handleSelectSubscriptionModels stores which of the plan's models the owner
// wants Janus to offer. Only selected models appear in /v1/models and are
// accepted by the proxy.
func (s *Server) handleSelectSubscriptionModels(w http.ResponseWriter, r *http.Request) {
	if !s.requireSubscriptions(w, r) {
		return
	}
	conn, ok := s.ownedSubscription(w, r)
	if !ok {
		return
	}
	var body struct {
		Selected []string `json:"selected"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	selected := []string{}
	for _, m := range body.Selected {
		m = strings.TrimSpace(m)
		if m == "" || slices.Contains(selected, m) {
			continue
		}
		if !slices.Contains(conn.Models, m) {
			WriteError(w, r, ErrInvalidRequest("The model "+m+" is not available on this subscription. Refresh the model list and try again.").WithParam("selected"))
			return
		}
		selected = append(selected, m)
	}
	if err := s.Store.SetSubscriptionSelectedModels(r.Context(), conn.ID, selected); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "subscription_models_selected", "subscription", conn.ID,
		map[string]any{"selected": conn.SelectedModels}, map[string]any{"selected": selected})
	updated, err := s.Store.SubscriptionByID(r.Context(), conn.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, s.connectionView(r.Context(), updated))
}

func (s *Server) handleDisconnectMySubscription(w http.ResponseWriter, r *http.Request) {
	conn, ok := s.ownedSubscription(w, r)
	if !ok {
		return
	}
	s.disconnectSubscription(w, r, conn, "subscription_disconnected")
}

func (s *Server) disconnectSubscription(w http.ResponseWriter, r *http.Request, conn *store.SubscriptionConnection, action string) {
	ctx := r.Context()
	// Revoke at the vendor first (best effort, bounded) so the credential
	// stops working even if a copy survived somewhere; the local delete
	// happens regardless.
	if provider, ok := subscription.Get(conn.Provider); ok {
		if refresh, err := s.Cipher.Decrypt(conn.EncryptedRefreshToken()); err == nil && refresh != "" {
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := provider.Revoke(rctx, s.upstreamHTTPClient(ctx), refresh); err != nil {
				s.Logger.WarnContext(ctx, "revoke subscription at vendor", "subscription_id", conn.ID, "provider", conn.Provider, "error", err.Error())
			}
			cancel()
		}
	}
	if err := s.Store.DeleteSubscription(ctx, conn.ID); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, action, "subscription", conn.ID, map[string]any{
		"provider": conn.Provider, "user_id": conn.UserID, "account_email": conn.AccountEmail,
	}, nil)
	WriteJSON(w, http.StatusOK, map[string]any{"id": conn.ID, "disconnected": true})
}

// --- admin API -----------------------------------------------------------------

func (s *Server) handleAdminListSubscriptions(w http.ResponseWriter, r *http.Request) {
	if s.Config.Offline {
		// Not offered at all offline: the SPA hides the controls on this.
		WriteJSON(w, http.StatusOK, map[string]any{"offline": true, "enabled": false, "providers": []any{}, "connections": []any{}})
		return
	}
	featureOn, err := s.Store.PersonalSubscriptionsEnabled(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	settings, err := s.Store.SubscriptionProviderSettings(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	conns, err := s.Store.ListAllSubscriptions(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	providers := []map[string]any{}
	for _, p := range subscription.All() {
		count := 0
		for _, c := range conns {
			if c.Provider == p.ID() {
				count++
			}
		}
		providers = append(providers, map[string]any{
			"id": p.ID(), "name": p.DisplayName(), "description": p.Description(), "admin_note": p.AdminNote(),
			"enabled": settings[p.ID()], "connections": count,
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"offline": false, "enabled": featureOn, "providers": providers, "connections": conns})
}

// handleAdminSetSubscriptionFeature flips the organization-wide switch.
// Turning it off keeps every connection (and its selection) so turning it
// back on restores exactly what users had.
func (s *Server) handleAdminSetSubscriptionFeature(w http.ResponseWriter, r *http.Request) {
	if s.Config.Offline {
		WriteError(w, r, errSubscriptionsUnavailable(true))
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	if body.Enabled == nil {
		WriteError(w, r, ErrInvalidRequest("Provide enabled: true or false.").WithParam("enabled"))
		return
	}
	old, err := s.Store.PersonalSubscriptionsEnabled(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.Store.SetPersonalSubscriptionsEnabled(r.Context(), *body.Enabled); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "personal_subscriptions_updated", "system_setting", store.SettingPersonalSubscriptions,
		map[string]any{"enabled": old}, map[string]any{"enabled": *body.Enabled})
	WriteJSON(w, http.StatusOK, map[string]any{"enabled": *body.Enabled})
}

func (s *Server) handleAdminSetSubscriptionProviders(w http.ResponseWriter, r *http.Request) {
	if s.Config.Offline {
		WriteError(w, r, errSubscriptionsUnavailable(true))
		return
	}
	var body struct {
		Enabled map[string]bool `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	old, err := s.Store.SubscriptionProviderSettings(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	next := map[string]bool{}
	known := []string{}
	for _, p := range subscription.All() {
		known = append(known, p.ID())
		next[p.ID()] = old[p.ID()]
	}
	for id, on := range body.Enabled {
		if !slices.Contains(known, id) {
			WriteError(w, r, ErrInvalidRequest("Unknown subscription provider "+id+". Known providers: "+strings.Join(known, ", ")+".").WithParam("enabled"))
			return
		}
		next[id] = on
	}
	if err := s.Store.SetSubscriptionProviderSettings(r.Context(), next); err != nil {
		WriteError(w, r, err)
		return
	}
	s.audit(r, "subscription_providers_updated", "system_setting", store.SettingSubscriptionProviders, old, next)
	WriteJSON(w, http.StatusOK, map[string]any{"enabled": next})
}

func (s *Server) handleAdminDisconnectSubscription(w http.ResponseWriter, r *http.Request) {
	conn, err := s.Store.SubscriptionByID(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, r, ErrNotFoundf("That subscription"))
		return
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.disconnectSubscription(w, r, conn, "subscription_disconnected_by_admin")
}
