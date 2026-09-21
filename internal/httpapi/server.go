package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/auth"
	"github.com/torvanis/janus/internal/config"
	"github.com/torvanis/janus/internal/crypto"
	"github.com/torvanis/janus/internal/discovery"
	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/quota"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/telemetry"
	"github.com/torvanis/janus/internal/troubleshoot"
	"github.com/torvanis/janus/internal/updates"
)

// Server wires every dependency behind one HTTP handler.
type Server struct {
	Config   *config.Config
	Store    *store.Store
	Sessions auth.SessionStore
	OIDC     *auth.OIDCProvider
	// Registry serves admin-configured OIDC providers (Business: multi_oidc).
	Registry    *auth.Registry
	Cipher      *crypto.Cipher
	Quota       *quota.Engine
	RateLimits  *quota.RateLimiter
	Metrics     *telemetry.Metrics
	Trace       *telemetry.OTLP
	Alerts      *alerting.Dispatcher
	Discovery   *discovery.Service
	License     *license.Manager
	LicenseSync *license.SyncEngine
	// Updates is the opt-in version checker; nil when disabled at build.
	Updates   *updates.Checker
	Logger    *slog.Logger
	WebAssets fs.FS
	StartedAt time.Time
	// Troubleshoot persists request/response captures while an
	// administrator has a troubleshooting session enabled (see
	// troubleshoot.go). Optional: when nil one is built lazily from Store,
	// Cipher and Config so test wiring needs nothing extra.
	Troubleshoot *troubleshoot.Recorder

	troubleshootOnce sync.Once

	// Security Gateway classifier resolver, built lazily on first use so
	// test wiring needs nothing extra; holds one circuit breaker per
	// classifier model.
	secgwOnce     sync.Once
	secgwResolver *classifierResolver

	tokenCache  sync.Map    // digest -> cachedToken
	configCache configCache // hot-path configuration reads (see cache.go)
	router      http.Handler

	// pending tracks post-response background writers (usage events, quota
	// ledger updates) so shutdown can drain them after the HTTP listener has
	// finished its own in-flight handlers. Without this a SIGTERM landing
	// between the response and the async insert silently loses the event.
	pending sync.WaitGroup

	// upstreamClient is the pooled client for every upstream call, tagged
	// with the timeout set it was built for. The proxy compares that tag with
	// the currently effective timeouts (environment defaults merged with the
	// administrator's stored overrides) on each request and rebuilds the
	// client when they differ, so a runtime timeout change applies to the
	// next request without a restart. upstreamClientMu serialises rebuilds;
	// the pointer itself is lock-free on the hot path.
	upstreamClientMu sync.Mutex
	upstreamClient   atomic.Pointer[upstreamClientState]
}

// upstreamClientState pairs a built client with the timeouts baked into its
// transport, so a change can be detected by simple value comparison.
type upstreamClientState struct {
	timeouts config.UpstreamTimeouts
	client   *http.Client
}

type cachedToken struct {
	token    *store.Token
	user     *store.User
	cachedAt time.Time
}

// cachedServiceToken is the service-credential counterpart of cachedToken.
// It carries no user because a service token has no owning principal.
type cachedServiceToken struct {
	token    *store.ServiceToken
	cachedAt time.Time
}

// tokenCacheTTL bounds how long a revoked or disabled credential can keep
// working on a replica other than the one that processed the revocation: the
// revoking replica clears its own cache immediately (InvalidateTokenCache),
// and every other replica re-reads the database within this TTL. Operators
// who need faster cross-replica revocation can lower it at the cost of more
// database reads on the proxy hot path.
const tokenCacheTTL = 60 * time.Second

// Handler builds (once) and returns the routed handler.
func (s *Server) Handler() http.Handler {
	if s.RateLimits == nil {
		s.RateLimits = quota.NewRateLimiter()
	}
	if s.router == nil {
		if s.Config != nil {
			// cross-replica config staleness is bounded by this
			// TTL (JANUS_CONFIG_CACHE_TTL_SECONDS, default 5s).
			s.configCache.ttl = s.Config.ConfigCacheTTL
		}
		s.router = s.buildRouter()
	}
	return s.router
}

func (s *Server) buildRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(RequestContext())
	r.Use(Recoverer())
	r.Use(AccessLog(s.Logger))
	r.Use(s.resolvePrincipal)

	// Operational endpoints, deliberately unauthenticated for probes and scrapes.
	// /healthz is liveness (no dependency checks); /readyz is readiness and
	// consults the database and the identity provider — see handleReadyz for
	// the dependency semantics and the documented outage/recovery timing.
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)
	r.Handle("/metrics", s.Metrics.Handler())
	s.mountSCIMRoutes(r)

	// Identity.
	// The sign-in *page* is a client route; /auth/start initiates the flow so the
	// two never shadow each other.
	r.Get("/auth/start", s.handleLogin)
	r.Get("/auth/callback", s.handleCallback)
	r.Get("/auth/providers", s.handleAuthProviders)
	r.Get("/auth/start/{slug}", s.handleProviderLogin)
	r.Get("/auth/callback/{slug}", s.handleProviderCallback)
	r.Get("/auth/health", s.handleAuthHealth)
	r.Post("/auth/logout", s.handleLogout)
	// Local accounts (email + password, optional TOTP). JSON endpoints; the
	// SPA sign-in page drives them. See local_auth.go.
	s.mountLocalAuthRoutes(r)

	// Application API (browser sessions and bearer tokens both work).
	r.Route("/api/v1", func(api chi.Router) {
		api.Use(CSRFGuard)
		api.Get("/config", s.handlePublicConfig)
		api.Post("/docs/feedback", s.handleDocsFeedback)

		api.Group(func(pr chi.Router) {
			pr.Use(RequireUser)
			pr.Get("/me", s.handleMe)
			pr.Put("/me/active-team", s.handleSetActiveTeam)
			s.mountTeamRoutes(pr)
			s.mountReportRoutes(pr)
			s.mountReportClassificationRoutes(pr)
			pr.Patch("/me/preferences", s.handleUpdatePreferences)
			pr.Post("/me/sessions/revoke", s.handleRevokeSessions)
			s.mountLocalAccountRoutes(pr)

			pr.Get("/tokens", s.handleListTokens)
			pr.Post("/tokens", s.gateCreate("token", s.handleCreateToken))
			pr.Get("/tokens/{id}", s.handleTokenDetail)
			pr.Put("/tokens/{id}/team", s.handleChangeTokenTeam)
			pr.Delete("/tokens/{id}", s.handleRevokeToken)

			pr.Get("/models", s.handleListModels)
			pr.Get("/requests", s.handleListRequests)
			pr.Get("/requests.csv", s.handleExportRequests)

			pr.Get("/dashboard/personal", s.handlePersonalDashboard)
			pr.Get("/dashboard/quota", s.handleQuotaDashboard)
			pr.Get("/dashboard/team", s.handleTeamDashboard)
			pr.Get("/dashboard/global", s.handleGlobalDashboard)
			pr.Get("/dashboard/tokens/{id}", s.handleTokenUsage)

			pr.Get("/notifications", s.handleListNotifications)
			pr.Post("/notifications/read", s.handleMarkNotificationsRead)

			pr.Get("/help/{topic}", s.handleHelpTopic)

			// delegated team-quota management for team leads. The
			// handlers authorize per team (lead + lead_can_edit_quotas), so no
			// admin role gate applies here.
			pr.Get("/lead/teams", s.handleLeadTeams)
			// Roster management is available to a lead regardless of the
			// quota delegation flag: deciding who is on the team is a
			// separate concern from controlling its budget.
			pr.Get("/lead/teams/{id}/members", s.handleLeadTeamMembers)
			pr.Put("/lead/teams/{id}/members", s.handleLeadSetTeamMembers)
			pr.Post("/lead/teams/{id}/quotas", s.handleLeadCreateQuota)
			pr.Put("/lead/teams/{id}/quotas/{quotaID}", s.handleLeadUpdateQuota)
			pr.Delete("/lead/teams/{id}/quotas/{quotaID}", s.handleLeadDeleteQuota)
		})

		api.Group(func(ar chi.Router) {
			ar.Use(RequireAdmin)
			s.mountAdminRoutes(ar)
			s.mountSCIMAdminRoutes(ar)
		})
	})

	// OpenAI-compatible proxy surface. /v1/* is the sole proxy namespace.
	r.Handle("/v1/*", http.HandlerFunc(s.handleProxy))

	// Single-page app and documentation site.
	r.NotFound(s.serveWebApp)
	return r
}

// resolvePrincipal attaches the caller identity from a session cookie, a user
// bearer token, or a service token, without rejecting anonymous requests
// (route guards do that).
//
// The three are mutually exclusive. A service token is identified by its
// prefix, so the two credential kinds are told apart before any database work
// and a user-token lookup is never attempted with a service credential.
func (s *Server) resolvePrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
			presented := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
			if strings.HasPrefix(presented, store.ServiceTokenPrefix) {
				svc, err := s.resolveServiceToken(ctx, presented)
				switch {
				case err != nil:
					// Surface credential failures immediately on the proxy
					// surface, exactly as user tokens do, so an unattended
					// integration gets a precise 401 it can log and alert on.
					if strings.HasPrefix(r.URL.Path, "/v1/") {
						WriteError(w, r, err)
						return
					}
				default:
					ctx = context.WithValue(ctx, ctxServiceToken, svc)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			} else {
				token, user, err := s.resolveToken(ctx, presented)
				switch {
				case err != nil:
					// Surface credential failures immediately on the proxy surface so
					// clients see a precise 401 rather than a generic sign-in prompt.
					if strings.HasPrefix(r.URL.Path, "/v1/") {
						WriteError(w, r, err)
						return
					}
				default:
					ctx = context.WithValue(ctx, ctxUser, user)
					ctx = context.WithValue(ctx, ctxToken, token)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
		}

		if cookie, err := r.Cookie(auth.SessionCookie); err == nil && cookie.Value != "" {
			if session, err := s.Sessions.Get(ctx, cookie.Value); err == nil {
				if user, err := s.Store.UserByID(ctx, session.UserID); err == nil {
					ctx = context.WithValue(ctx, ctxUser, user)
					ctx = context.WithValue(ctx, ctxSession, session)
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// serviceTokenCacheKey namespaces service credentials inside the shared token
// cache so a service digest can never collide with a user digest entry.
func serviceTokenCacheKey(digest string) string { return "svc|" + digest }

// resolveServiceToken validates a presented service credential, using the same
// short-lived cache the user-token path uses so the proxy hot path stays off
// the database.
//
// Expiry is re-checked on every call, including on cache hits: a cached entry
// must not keep an expired credential alive for the remainder of the TTL.
func (s *Server) resolveServiceToken(ctx context.Context, presented string) (*store.ServiceToken, error) {
	if presented == "" {
		return nil, ErrTokenInvalid("")
	}
	digest := store.HashToken(presented)
	key := serviceTokenCacheKey(digest)
	if cached, ok := s.tokenCache.Load(key); ok {
		entry := cached.(cachedServiceToken)
		if time.Since(entry.cachedAt) < tokenCacheTTL {
			s.Metrics.TokenCacheHits.Inc()
			// Revocation is handled by cache invalidation; expiry is a clock
			// event with no write to hang an invalidation off, so it must be
			// evaluated against the current time on every use.
			if entry.token.Expired(time.Now().UTC()) {
				return nil, ErrServiceTokenExpired(entry.token.ExpiresAt)
			}
			return entry.token, nil
		}
		s.tokenCache.Delete(key)
	}
	s.Metrics.TokenCacheMisses.Inc()

	svc, err := s.Store.ServiceTokenByDigest(ctx, digest)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrTokenInvalid("")
	}
	if err != nil {
		return nil, err
	}
	if svc.Revoked() {
		return nil, ErrTokenInvalid("This service token has been revoked. Ask a Janus administrator to issue a replacement.")
	}
	if svc.Expired(time.Now().UTC()) {
		return nil, ErrServiceTokenExpired(svc.ExpiresAt)
	}
	s.tokenCache.Store(key, cachedServiceToken{token: svc, cachedAt: time.Now()})
	go func() {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.Store.TouchServiceToken(bg, svc.ID); err != nil {
			s.Logger.Warn("record service token last use", "error", err.Error())
		}
	}()
	return svc, nil
}

// resolveToken validates a presented credential, using a short-lived cache to
// keep the proxy hot path off the database.
func (s *Server) resolveToken(ctx context.Context, presented string) (*store.Token, *store.User, error) {
	if presented == "" {
		return nil, nil, ErrTokenInvalid("")
	}
	digest := store.HashToken(presented)
	if cached, ok := s.tokenCache.Load(digest); ok {
		entry := cached.(cachedToken)
		if time.Since(entry.cachedAt) < tokenCacheTTL {
			s.Metrics.TokenCacheHits.Inc()
			// Revocation and SCIM deactivation must be immediate on every replica.
			currentToken, err := s.Store.TokenByID(ctx, entry.token.ID)
			if err != nil || currentToken.Revoked() {
				return nil, nil, ErrTokenInvalid("This API token is no longer active.")
			}
			currentUser, err := s.Store.UserByID(ctx, entry.user.ID)
			if err != nil {
				return nil, nil, ErrTokenInvalid("")
			}
			if !currentUser.IsActive {
				return nil, nil, ErrUserDisabled()
			}
			return currentToken, currentUser, nil
		}
		s.tokenCache.Delete(digest)
	}
	s.Metrics.TokenCacheMisses.Inc()

	token, err := s.Store.TokenByDigest(ctx, digest)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, ErrTokenInvalid("")
	}
	if err != nil {
		return nil, nil, err
	}
	if token.Revoked() {
		return nil, nil, ErrTokenInvalid("This API token has been revoked. Generate a new one from the Janus tokens page.")
	}
	user, err := s.Store.UserByID(ctx, token.UserID)
	if err != nil {
		return nil, nil, ErrTokenInvalid("")
	}
	if !user.IsActive {
		return nil, nil, ErrUserDisabled()
	}
	s.tokenCache.Store(digest, cachedToken{token: token, user: user, cachedAt: time.Now()})
	go func() {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.Store.TouchToken(bg, token.ID); err != nil {
			s.Logger.Warn("record token last use", "error", err.Error())
		}
	}()
	return token, user, nil
}

// Drain blocks until every tracked post-response writer (usage events, quota
// ledger updates) has finished, or the context expires. main.go calls it after
// httpServer.Shutdown so the final requests before a SIGTERM are never lost
// from metering.
func (s *Server) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// InvalidateTokenCache drops cached credentials so a revocation takes effect at
// once rather than after the cache TTL.
func (s *Server) InvalidateTokenCache() {
	s.tokenCache.Range(func(k, _ any) bool {
		s.tokenCache.Delete(k)
		return true
	})
}

// --- Operational handlers ----------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleReadyz is the Kubernetes readiness probe. It answers 200 only
// when every dependency the replica needs to serve traffic is usable, and 503
// otherwise, with a per-dependency `checks` map explaining which one failed.
//
// Three checks run, each bounded by a shared 3 s deadline so a slow dependency
// cannot stall the kubelet:
//
//   - database: a single Store.Ping. Deliberately not retried or cached — the
//     database is local to the deployment and a failed ping is a real signal.
//   - identity_provider: in dev-auth mode always ok ("local evaluation
//     sign-in"); otherwise OIDC.Healthy, whose verdict is cached and tolerant
//     of transient faults. One failed discovery probe is retried immediately
//     and never flips the replica unready; only consecutive failed probe
//     rounds report "unreachable". A genuine outage therefore turns this
//     endpoint to 503 within the bounded window documented on
//     auth.HealthCheckOptions (≈40 s from the IdP going down with the
//     defaults, ≈8 s after the first probe that observes it), and a
//     recovered IdP clears the 503 within the shorter unhealthy re-check
//     interval (≈5 s) rather than the 30 s healthy cache TTL. Every failed
//     probe is logged at WARN with the discovery URL and error.
//
// /auth/health consumes the same OIDC.Healthy verdict, so the two never
// disagree about the identity provider.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := map[string]any{}
	ready := true

	// Database: single-shot, uncached (see the doc comment above).
	if err := s.Store.Ping(ctx); err != nil {
		checks["database"] = map[string]any{"ok": false, "detail": "unreachable"}
		ready = false
	} else {
		checks["database"] = map[string]any{"ok": true}
	}
	if s.Config.DevAuthEnabled {
		checks["identity_provider"] = map[string]any{"ok": true, "detail": "local evaluation sign-in"}
	} else if s.OIDC == nil {
		// No IdP is a supported shape since local accounts exist: people
		// sign in with email + password (or first-run setup). Not a
		// readiness failure — the database check above already covers
		// whether that path can work.
		checks["identity_provider"] = map[string]any{"ok": true, "detail": "local accounts"}
	} else if !s.OIDC.Healthy(ctx) {
		// a replica whose IdP is unreachable cannot sign users in and
		// must be rotated out rather than serve failing logins.
		checks["identity_provider"] = map[string]any{"ok": false, "detail": "unreachable"}
		ready = false
	} else {
		checks["identity_provider"] = map[string]any{"ok": true}
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	WriteJSON(w, status, map[string]any{"ready": ready, "checks": checks})
}

func (s *Server) handlePublicConfig(w http.ResponseWriter, r *http.Request) {
	flags, err := s.Store.FeatureFlags(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"public_url":     s.Config.PublicURL,
		"version":        s.Config.BuildVersion,
		"build":          s.Config.BuildSHA,
		"dev_auth":       s.Config.DevAuthEnabled,
		"feature_flags":  flags,
		"provider_label": s.providerLabel(),
	})
}

func (s *Server) providerLabel() string {
	if s.Config.DevAuthEnabled {
		return "Local evaluation sign-in"
	}
	if s.OIDC != nil {
		if issuer := s.OIDC.Metadata().Issuer; issuer != "" {
			return issuer
		}
	}
	return s.Config.OIDCProviderURL
}

// --- helpers ----------------------------------------------------------------

func decodeJSON(r *http.Request, dst any) error {
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20)) }()
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return ErrInvalidRequest("The request body is not valid JSON: " + err.Error())
	}
	return nil
}

func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// rangeBounds resolves the ?range= / ?start= / ?end= trio used by every
// dashboard endpoint into concrete UTC bounds.
func rangeBounds(r *http.Request) (start, end time.Time, label string) {
	now := time.Now().UTC()
	end = now
	label = r.URL.Query().Get("range")
	switch label {
	case "week":
		start = now.AddDate(0, 0, -7)
	case "month":
		start = now.AddDate(0, -1, 0)
	case "quarter":
		start = now.AddDate(0, -3, 0)
	case "custom":
		if v := r.URL.Query().Get("start"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				start = t.UTC()
			}
		}
		if v := r.URL.Query().Get("end"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				end = t.UTC()
			}
		}
		if start.IsZero() {
			start = now.AddDate(0, 0, -7)
		}
	default:
		label = "day"
		start = now.AddDate(0, 0, -1)
	}
	return start, end, label
}
