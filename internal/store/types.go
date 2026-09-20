package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// nowUTC is the single clock source for the store, kept swappable for tests.
var nowUTC = func() time.Time { return time.Now().UTC() }

// NewID returns a RFC 4122-shaped random identifier.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is unrecoverable and must not be papered over.
		panic(fmt.Sprintf("generate id: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Role values, ordered least to most privileged.
const (
	RoleUser     = "user"
	RoleTeamLead = "team_lead"
	RoleAdmin    = "admin"
)

// User is an authenticated principal, auto-created on first successful sign-in.
type User struct {
	ID     string `json:"id"`
	AuthID string `json:"-"`
	Email  string `json:"email"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	// AdminViaGroup records that the user's most recent sign-in carried
	// membership in a configured administrator group at the identity
	// provider. Unlike Role it is transient: it is re-evaluated on every
	// sign-in, so leaving the group revokes the capability at the next
	// sign-in. It never modifies Role.
	AdminViaGroup bool      `json:"admin_via_group"`
	IsActive      bool      `json:"is_active"`
	Timezone      string    `json:"timezone"`
	Locale        string    `json:"locale"`
	CreatedAt     time.Time `json:"created_at"`
	LastLoginAt   time.Time `json:"last_login_at"`
}

// IsAdmin reports whether the user may reach the /admin/v1 surface.
// Administrator capability composes from three sources with OR: an explicit
// role grant (sticky), the bootstrap email list (promotes Role, sticky), and
// IdP admin-group membership (AdminViaGroup, re-evaluated per sign-in).
func (u *User) IsAdmin() bool { return u != nil && (u.Role == RoleAdmin || u.AdminViaGroup) }

// Group is either mirrored from IdP token claims (read-only) or created in-app.
type Group struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	FromIDP     bool      `json:"from_idp"`
	MemberCount int       `json:"member_count"`
	CreatedAt   time.Time `json:"created_at"`
}

type TeamMembershipSource struct {
	SourceType string `json:"source_type"`
	SourceID   string `json:"source_id"`
}

type TeamMember struct {
	ID      string                 `json:"id"`
	UserID  string                 `json:"user_id"`
	Email   string                 `json:"email"`
	Name    string                 `json:"name"`
	Role    string                 `json:"role"`
	Sources []TeamMembershipSource `json:"sources"`
}

// Team owns quotas and gives its members a shared dashboard.
type Team struct {
	Listed            bool      `json:"listed"`
	ArchivedAt        time.Time `json:"archived_at"`
	Role              string    `json:"my_role,omitempty"`
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	LeadUserID        string    `json:"lead_user_id"`
	LeadName          string    `json:"lead_name"`
	LeadCanEditQuotas bool      `json:"lead_can_edit_quotas"`
	MemberCount       int       `json:"member_count"`
	CreatedAt         time.Time `json:"created_at"`
}

// Token is a downstream API credential. Only the SHA-256 digest is stored.
type Token struct {
	TeamID      string    `json:"team_id"`
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Prefix      string    `json:"prefix"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsedAt  time.Time `json:"last_used_at"`
	RevokedAt   time.Time `json:"revoked_at"` // zero value = active; MarshalJSON emits "" (never the RFC 3339 zero time)
}

// Revoked reports whether the token has been withdrawn.
func (t *Token) Revoked() bool { return !t.RevokedAt.IsZero() }

// MarshalJSON emits revoked_at as an empty string while the token is active.
// time.Time's default encoding renders the zero value as
// "0001-01-01T00:00:00Z" — a non-empty (truthy) string that API consumers
// misread as a real revocation timestamp, badging active tokens as revoked.
// Serialising the zero value as "" mirrors the schema intent
// (revoked_at TEXT NOT NULL DEFAULT ”: empty until revoked) so clients can
// treat the field as falsy for active tokens and as an RFC 3339 timestamp
// for revoked ones.
func (t Token) MarshalJSON() ([]byte, error) {
	// alias drops Token's methods so the nested json.Marshal cannot recurse.
	type alias Token
	revokedAt := ""
	if t.Revoked() {
		revokedAt = t.RevokedAt.UTC().Format(time.RFC3339Nano)
	}
	return json.Marshal(struct {
		alias
		RevokedAt string `json:"revoked_at"`
	}{alias: alias(t), RevokedAt: revokedAt})
}

// Upstream is a configured provider endpoint.
type Upstream struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	AdapterType   string    `json:"adapter_type"`
	BaseURL       string    `json:"base_url"`
	APIKeyMask    string    `json:"api_key_mask"`
	HasAPIKey     bool      `json:"has_api_key"`
	Enabled       bool      `json:"enabled"`
	LastCheckAt   time.Time `json:"last_check_at"`
	LastError     string    `json:"last_error"`
	LastLatencyMs int       `json:"last_latency_ms"`
	ModelCount    int       `json:"model_count"`
	CreatedAt     time.Time `json:"created_at"`

	encryptedKey string
}

// EncryptedKey exposes the stored ciphertext to the crypto layer only.
func (u *Upstream) EncryptedKey() string { return u.encryptedKey }

// Model status values.
const (
	ModelPending  = "pending_approval"
	ModelEnabled  = "enabled"
	ModelDisabled = "disabled"
	ModelStale    = "stale"
)

// Model is a model discovered on an upstream and curated by an admin.
type Model struct {
	Metadata         map[string]MetadataField `json:"metadata"`
	MetadataWarnings []string                 `json:"metadata_warnings"`
	metadataState    metadataState
	ID               string   `json:"id"`
	UpstreamID       string   `json:"upstream_id"`
	UpstreamName     string   `json:"upstream_name"`
	AdapterType      string   `json:"adapter_type"`
	Name             string   `json:"name"`
	DisplayName      string   `json:"display_name"`
	Status           string   `json:"status"`
	Modalities       []string `json:"modalities"`
	RateInNano       int64    `json:"rate_in_nanousd"`
	RateOutNano      int64    `json:"rate_out_nanousd"`
	RateCachedNano   int64    `json:"rate_cached_nanousd"`
	// RateCacheWrite5mNano and RateCacheWrite1hNano price prompt-cache writes
	// (5-minute and 1-hour TTL entries) the way some upstreams bill them.
	// Models priced before migration 0013 carry 0, which is what was billed.
	RateCacheWrite5mNano int64 `json:"rate_cache_write_5m_nanousd"`
	RateCacheWrite1hNano int64 `json:"rate_cache_write_1h_nanousd"`
	// ContextWindow is the model's context window size in tokens. 0 means
	// "unknown" — rows created before migration 0014 and models the bundled
	// seed does not cover carry 0, and the UI shows a neutral placeholder.
	ContextWindow int64 `json:"context_window"`
	// ClassifierRole marks this model as a Security Gateway guard
	// classifier (ClassifierRole* constants). Non-empty removes the model
	// from the servable catalog, /v1/models and grant targeting; it is
	// reachable only from the gateway's internal filter path.
	ClassifierRole    string    `json:"classifier_role,omitempty"`
	RateEffectiveFrom time.Time `json:"rate_effective_from"`
	DiscoveredAt      time.Time `json:"discovered_at"`
	GrantCount        int       `json:"grant_count"`
	GrantSource       string    `json:"grant_source,omitempty"`

	// Health telemetry. Every field below is resolved at read time for
	// catalog responses (see Store.ModelHealthStats and Model.SetHealth) and
	// is never persisted on the model row.
	//
	// RequestCount10m / ErrorCount10m count the proxied requests addressed
	// to this model over the trailing ModelHealthWindow, and the
	// upstream-attributable failures among them. ErrorRatePercent is
	// ErrorCount10m / RequestCount10m × 100, rounded to one decimal; 0 when
	// there was no traffic.
	RequestCount10m  int64   `json:"request_count_10m"`
	ErrorCount10m    int64   `json:"error_count_10m"`
	ErrorRatePercent float64 `json:"error_rate_percent"`
	// UpstreamReachable mirrors the admin Upstreams page: true when the last
	// health probe of the owning upstream succeeded. UpstreamLastCheckAt is
	// the zero time when the upstream has never been probed.
	UpstreamReachable     bool      `json:"upstream_reachable"`
	UpstreamLastCheckAt   time.Time `json:"upstream_last_check_at"`
	UpstreamLastError     string    `json:"upstream_last_error"`
	UpstreamLastLatencyMs int       `json:"upstream_last_latency_ms"`
	// Health is the derived verdict (ModelHealth* constants) so every
	// consumer agrees on what "down" means. Empty until SetHealth runs.
	Health string `json:"health,omitempty"`
}

// Model health verdicts, from best to worst. "unknown" means the upstream has
// never been probed and the model saw no traffic in the window — there is
// simply nothing to judge yet.
const (
	ModelHealthHealthy  = "healthy"
	ModelHealthDegraded = "degraded"
	ModelHealthDown     = "down"
	ModelHealthUnknown  = "unknown"
)

// ModelHealthWindow is the trailing window the per-model error rollup covers.
const ModelHealthWindow = 10 * time.Minute

// Thresholds for deriving a Model's health verdict from its error rollup.
const (
	// modelHealthDegradedPercent is the error rate at or above which a model
	// with traffic is badged degraded.
	modelHealthDegradedPercent = 10.0
	// modelHealthDownMinRequests is how many requests must have failed in the
	// window before a 100% error rate alone marks a model down. Below this a
	// couple of unlucky calls only count as degraded.
	modelHealthDownMinRequests = 3
)

// ModelHealthStats is the per-model rollup of usage events over the health
// window: how many requests reached (or tried to reach) the upstream and how
// many of those failed for an upstream-attributable reason.
type ModelHealthStats struct {
	Requests int64
	Errors   int64
}

// ErrorRatePercent is Errors / Requests as a percentage rounded to one
// decimal; 0 when there were no requests.
func (h ModelHealthStats) ErrorRatePercent() float64 {
	if h.Requests <= 0 {
		return 0
	}
	return math.Round(float64(h.Errors)/float64(h.Requests)*1000) / 10
}

// TrafficDown is the traffic-only half of the "down" verdict: at least
// modelHealthDownMinRequests requests in the window and every one of them
// failed. Exposed so routing decisions (managed-model fallback) apply exactly
// the rule the health badge shows.
func (h ModelHealthStats) TrafficDown() bool {
	return h.Requests >= modelHealthDownMinRequests && h.Errors >= h.Requests
}

// Degraded reports an error rate at or above modelHealthDegradedPercent on
// any traffic — the same rule as the "degraded" health verdict.
func (h ModelHealthStats) Degraded() bool {
	return h.Requests > 0 && h.ErrorRatePercent() >= modelHealthDegradedPercent
}

// Reachable reports whether the upstream's most recent probe succeeded. An
// upstream that has never been probed is neither reachable nor unreachable;
// callers that need a verdict should also consult LastCheckAt.
func (u *Upstream) Reachable() bool {
	return u != nil && !u.LastCheckAt.IsZero() && u.LastError == ""
}

// ProbedUnreachable reports a probe that has run and failed — the signal a
// managed-model fallback treats as "the whole provider is down".
func (u *Upstream) ProbedUnreachable() bool {
	return u != nil && !u.LastCheckAt.IsZero() && u.LastError != ""
}

// SetHealth attaches an upstream's probe state and the model's error rollup
// to the model and derives the Health verdict:
//
//   - down: the owning upstream's last probe failed, OR at least
//     modelHealthDownMinRequests requests in the window and every one of
//     them failed.
//   - degraded: any traffic with an error rate ≥ modelHealthDegradedPercent
//     (including a 100% rate on fewer than modelHealthDownMinRequests calls).
//   - unknown: no probe has ever run and no traffic in the window.
//   - healthy: everything else.
//
// A nil upstream (deleted underneath the model) is treated as never probed.
func (m *Model) SetHealth(up *Upstream, stats ModelHealthStats) {
	m.RequestCount10m = stats.Requests
	m.ErrorCount10m = stats.Errors
	m.ErrorRatePercent = stats.ErrorRatePercent()
	probed := false
	if up != nil {
		probed = !up.LastCheckAt.IsZero()
		m.UpstreamReachable = probed && up.LastError == ""
		m.UpstreamLastCheckAt = up.LastCheckAt
		m.UpstreamLastError = up.LastError
		m.UpstreamLastLatencyMs = up.LastLatencyMs
	} else {
		m.UpstreamReachable = false
		m.UpstreamLastCheckAt = time.Time{}
		m.UpstreamLastError = ""
		m.UpstreamLastLatencyMs = 0
	}
	switch {
	case probed && !m.UpstreamReachable:
		m.Health = ModelHealthDown
	case stats.Requests >= modelHealthDownMinRequests && stats.Errors >= stats.Requests:
		m.Health = ModelHealthDown
	case stats.Requests > 0 && m.ErrorRatePercent >= modelHealthDegradedPercent:
		m.Health = ModelHealthDegraded
	case !probed && stats.Requests == 0:
		m.Health = ModelHealthUnknown
	default:
		m.Health = ModelHealthHealthy
	}
}

// HasRates reports whether the model may be enabled (a rate card is mandatory
// so a model can never be served without cost attribution).
func (m *Model) HasRates() bool { return m.RateInNano > 0 || m.RateOutNano > 0 }

// PublicName is the name presented to downstream callers: the admin-set
// display name when one exists, otherwise the native upstream name. It is
// never empty for a persisted model.
func (m *Model) PublicName() string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return m.Name
}

// ValidationError reports a rejected write together with the offending field,
// so the API layer can map it to a 400 with a param instead of a generic 500.
type ValidationError struct {
	Field   string
	Message string
}

// Error renders "field: message".
func (e *ValidationError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Message) }

// RateCard is one versioned pricing entry for a model: all five billing
// dimensions in nano-USD per million tokens (1 USD/MTok = 1e9 nano-USD).
// Versions are immutable; changing rates appends a new version and historical
// usage keeps the price that was in force when it ran.
type RateCard struct {
	EffectiveFrom        time.Time `json:"effective_from"`
	RateInNano           int64     `json:"rate_in_nanousd"`
	RateOutNano          int64     `json:"rate_out_nanousd"`
	RateCachedNano       int64     `json:"rate_cached_nanousd"`
	RateCacheWrite5mNano int64     `json:"rate_cache_write_5m_nanousd"`
	RateCacheWrite1hNano int64     `json:"rate_cache_write_1h_nanousd"`
}

// ErrNegativeRate is returned by SetModelRates when any dimension is below
// zero. Zero itself is a valid, explicitly saved price (self-hosted models).
var ErrNegativeRate = errors.New("rates must be zero or positive")

// Grantee types for model access.
//
// GranteeAllUsers covers every authenticated *human* principal and
// deliberately does NOT reach service tokens: granting "everyone" a model
// must never silently hand that model to every unattended integration in the
// organisation. Service tokens are reached only by the two explicit grantee
// types below.
const (
	GranteeUser     = "user"
	GranteeGroup    = "group"
	GranteeAllUsers = "all_users"
	// GranteeServiceToken grants one named service token.
	GranteeServiceToken = "service_token"
	// GranteeAllServiceTokens grants every service token, present and future.
	GranteeAllServiceTokens = "all_service_tokens"
)

// Model kinds a grant can target. A grant on ModelKindManaged references a
// managed_model row (an alias); a grant on ModelKindModel references a real
// catalog model.
const (
	ModelKindModel   = "model"
	ModelKindManaged = "managed"
)

// Grant links a model — real or managed — to a user, a group, everyone, a
// service token, or every service token.
type Grant struct {
	ID          string    `json:"id"`
	ModelID     string    `json:"model_id"`
	ModelKind   string    `json:"model_kind"`
	ModelName   string    `json:"model_name"`
	GranteeType string    `json:"grantee_type"`
	GranteeID   string    `json:"grantee_id"`
	GranteeName string    `json:"grantee_name"`
	CreatedAt   time.Time `json:"created_at"`
}

// ServiceTokenPrefix marks a credential as belonging to a service token rather
// than a user, so an operator can tell the two apart at a glance in logs and
// configuration files.
const ServiceTokenPrefix = "janus_svc_"

// ServiceToken is a credential that authenticates a non-human integration.
//
// It belongs to no user. Usage is attributed to Name the way a user's usage is
// attributed to their identity, but service-token traffic is excluded from
// people-oriented reporting by construction (usage_event.user_id is empty and
// service_token_id is set) while still counting toward org-wide totals.
//
// Service tokens reach the OpenAI-compatible proxy surface (/v1/*) and nothing
// else: an unattended credential embedded in a website or agent must not be
// able to read dashboards or enumerate the organisation.
type ServiceToken struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Prefix      string `json:"prefix"`
	CreatedBy   string `json:"created_by_user_id"`
	// CreatedByLabel is resolved at read time for the admin list; it is
	// never persisted on the service_token row.
	CreatedByLabel string    `json:"created_by_label,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	LastUsedAt     time.Time `json:"last_used_at"`
	// ExpiresAt is optional. A zero value means the credential never
	// expires; MarshalJSON emits "" for it so clients can treat the field
	// as falsy rather than parsing Go's RFC 3339 zero time.
	ExpiresAt time.Time `json:"expires_at"`
	RevokedAt time.Time `json:"revoked_at"`
	// GrantCount is resolved at read time for the admin list.
	GrantCount int `json:"grant_count"`
}

// Revoked reports whether the credential has been withdrawn by an admin.
func (t *ServiceToken) Revoked() bool { return t != nil && !t.RevokedAt.IsZero() }

// Expired reports whether the credential's optional lifetime has elapsed.
func (t *ServiceToken) Expired(at time.Time) bool {
	return t != nil && !t.ExpiresAt.IsZero() && !at.Before(t.ExpiresAt)
}

// Usable reports whether the credential may authenticate a request right now.
func (t *ServiceToken) Usable(at time.Time) bool {
	return t != nil && !t.Revoked() && !t.Expired(at)
}

// Status renders the credential's lifecycle state for the admin UI.
func (t *ServiceToken) Status(at time.Time) string {
	switch {
	case t == nil:
		return "unknown"
	case t.Revoked():
		return "revoked"
	case t.Expired(at):
		return "expired"
	default:
		return "active"
	}
}

// MarshalJSON emits the optional timestamps as empty strings when unset, for
// the same reason Token.MarshalJSON does: time.Time's zero value encodes as
// "0001-01-01T00:00:00Z", a truthy string that clients misread as a real
// revocation or expiry. It also publishes the derived lifecycle state so every
// consumer agrees on what "active" means.
func (t ServiceToken) MarshalJSON() ([]byte, error) {
	// alias drops ServiceToken's methods so the nested Marshal cannot recurse.
	type alias ServiceToken
	format := func(v time.Time) string {
		if v.IsZero() {
			return ""
		}
		return v.UTC().Format(time.RFC3339Nano)
	}
	return json.Marshal(struct {
		alias
		LastUsedAt string `json:"last_used_at"`
		ExpiresAt  string `json:"expires_at"`
		RevokedAt  string `json:"revoked_at"`
		Status     string `json:"status"`
	}{
		alias:      alias(t),
		LastUsedAt: format(t.LastUsedAt),
		ExpiresAt:  format(t.ExpiresAt),
		RevokedAt:  format(t.RevokedAt),
		Status:     t.Status(nowUTC()),
	})
}

// Managed model statuses. A managed model is enabled (servable and offered in
// catalogs) or disabled (retained, granted, but not servable).
const (
	ManagedModelEnabled  = "enabled"
	ManagedModelDisabled = "disabled"
)

// Failure modes that can send a managed model's traffic to its fallback. The
// hard part of "fall back when unavailable" is deciding what unavailable
// means: one caller's 503 must not reroute everyone, while a dead upstream
// should. Each trigger is a DIFFERENT, independently observable signal, so an
// admin picks the sensitivity per alias:
//
//   - FallbackTriggerTargetUnavailable: configuration — the target model is
//     gone or disabled, or its upstream is disabled. Deterministic; every
//     request would fail.
//   - FallbackTriggerUpstreamUnreachable: the target's upstream failed its
//     most recent reachability probe (discovery records last_error). A
//     whole-provider outage seen from outside any single request.
//   - FallbackTriggerModelDown: the target model's recent traffic (last 10
//     minutes, at least 3 requests) has ALL failed with upstream-side errors.
//     Genuine outage, never a single unlucky caller.
//   - FallbackTriggerModelDegraded: the target's recent error rate is at or
//     above the degraded threshold (10%). The most sensitive option; off by
//     default because a noisy tenant can trip it.
const (
	FallbackTriggerTargetUnavailable   = "target_unavailable"
	FallbackTriggerUpstreamUnreachable = "upstream_unreachable"
	FallbackTriggerModelDown           = "model_down"
	FallbackTriggerModelDegraded       = "model_degraded"
)

// AllFallbackTriggers is every recognised trigger, in display order.
var AllFallbackTriggers = []string{
	FallbackTriggerTargetUnavailable,
	FallbackTriggerUpstreamUnreachable,
	FallbackTriggerModelDown,
	FallbackTriggerModelDegraded,
}

// DefaultFallbackTriggers are the failure modes a fallback reacts to when the
// admin sets one without choosing: the three that indicate a genuine outage,
// not the error-rate one that transient noise can trip.
var DefaultFallbackTriggers = []string{
	FallbackTriggerTargetUnavailable,
	FallbackTriggerUpstreamUnreachable,
	FallbackTriggerModelDown,
}

// HasFallbackTrigger reports whether the alias reacts to the given failure
// mode. An alias without a fallback reacts to nothing.
func (m *ManagedModel) HasFallbackTrigger(trigger string) bool {
	if m == nil || m.FallbackModelID == "" {
		return false
	}
	for _, t := range m.FallbackTriggers {
		if t == trigger {
			return true
		}
	}
	return false
}

// ManagedModel is an admin-defined stable alias for a real catalog model.
//
// Users select the alias ("current-best", "best-coder") and never have to track
// which underlying model is in force; an admin repoints it at any time without
// the caller changing a line of configuration. The indirection is deliberately
// NOT a secret: the catalog surfaces the alias's current target so a user can
// always see what they are actually talking to.
//
// Reporting always reflects the underlying model. A proxied request records the
// target's model_id and model_name on usage_event, and the alias name lands in
// the separate requested_model_name column.
type ManagedModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	// TargetModelID is the real catalog model this alias currently resolves
	// to. It is never another managed model: aliases do not chain.
	TargetModelID string `json:"target_model_id"`
	// FallbackModelID optionally names a real catalog model to serve when
	// the target is unavailable. Like the target it is never another
	// managed model, so a fallback can never chain into a second fallback.
	// Empty means no fallback: an unavailable target fails the request.
	FallbackModelID string `json:"fallback_model_id"`
	// FallbackTriggers lists the failure modes (FallbackTrigger*) that count
	// as "unavailable" for THIS alias. Defaults to DefaultFallbackTriggers
	// when a fallback is set and none are chosen. Empty when no fallback.
	FallbackTriggers []string `json:"fallback_triggers"`
	// The remaining fields are resolved from the target at read time and are
	// never persisted on the managed_model row. They are what the SPA needs
	// to render a transparent model card.
	TargetName         string   `json:"target_model_name"`
	TargetDisplayName  string   `json:"target_display_name,omitempty"`
	TargetPublicName   string   `json:"target_public_name"`
	TargetStatus       string   `json:"target_status"`
	TargetUpstreamID   string   `json:"target_upstream_id"`
	TargetUpstreamName string   `json:"target_upstream_name"`
	Modalities         []string `json:"modalities"`
	ContextWindow      int64    `json:"context_window"`
	// Servable reports whether a request to this alias can succeed right
	// now: the alias is enabled AND its target still exists and is enabled.
	Servable bool `json:"servable"`
	// Broken reports that the target has gone away or is no longer enabled,
	// so the admin UI can badge the alias for repair instead of letting it
	// fail silently at request time. Reason carries the human explanation.
	Broken       bool   `json:"broken"`
	BrokenReason string `json:"broken_reason,omitempty"`
	// Fallback presentation, resolved at read time like the target fields.
	// FallbackBroken reports a fallback that has itself gone away or been
	// disabled — the alias will still serve from its target, but the safety
	// net the admin configured is gone and the UI should say so.
	FallbackName         string    `json:"fallback_model_name,omitempty"`
	FallbackPublicName   string    `json:"fallback_public_name,omitempty"`
	FallbackStatus       string    `json:"fallback_status,omitempty"`
	FallbackUpstreamID   string    `json:"fallback_upstream_id,omitempty"`
	FallbackUpstreamName string    `json:"fallback_upstream_name,omitempty"`
	FallbackBroken       bool      `json:"fallback_broken"`
	FallbackBrokenReason string    `json:"fallback_broken_reason,omitempty"`
	GrantCount           int       `json:"grant_count"`
	CreatedBy            string    `json:"created_by_user_id"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
	// GrantSource labels how the caller obtained access ("direct", "all
	// service tokens", …), mirroring Model.GrantSource. It is set only on
	// caller-scoped catalog responses.
	GrantSource string `json:"grant_source,omitempty"`
}

// Quota metrics and windows.
const (
	MetricTokensIn  = "tokens_in"
	MetricTokensOut = "tokens_out"
	MetricCostUSD   = "cost_usd"
	MetricRequests  = "requests"

	WindowDaily     = "daily"
	WindowWeekly    = "weekly"
	WindowMonthly   = "monthly"
	WindowRolling24 = "rolling_24h"
	WindowRolling7  = "rolling_7d"
	WindowRolling30 = "rolling_30d"

	BreachLetFinish = "let_finish"
	BreachHardKill  = "hard_kill"
)

// Quota is an enforcement rule. Limit is expressed in the metric's own unit;
// cost_usd limits are stored in nano-USD like every other money value.
type Quota struct {
	ID             string `json:"id"`
	SubjectType    string `json:"subject_type"`
	SubjectID      string `json:"subject_id"`
	SubjectName    string `json:"subject_name"`
	ModelID        string `json:"model_id"`
	ModelName      string `json:"model_name"`
	Metric         string `json:"metric"`
	Limit          int64  `json:"limit_value"`
	Window         string `json:"window"`
	BreachBehavior string `json:"breach_behavior"`
	// AlertThresholds are the admin-configured warning percentages for this
	// rule, sorted ascending. Empty means the 80/95 defaults apply.
	// The 100% breach alert always fires regardless of this list.
	AlertThresholds []int     `json:"alert_thresholds"`
	CreatedAt       time.Time `json:"created_at"`

	CurrentValue int64     `json:"current_value"`
	ResetAt      time.Time `json:"reset_at"`
}

// UsageEvent is the immutable per-request record. Prompt and response bodies are
// never present here or anywhere else in the system.
type UsageEvent struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UserID    string    `json:"user_id"`
	// ServiceTokenID attributes the request to a service token instead of a
	// user. Exactly one of UserID / ServiceTokenID is non-empty on any event
	// recorded after migration 0015: that mutual exclusion is what keeps
	// service-token traffic out of people-oriented reporting by construction
	// while leaving it fully counted in org-wide totals.
	ServiceTokenID string   `json:"service_token_id,omitempty"`
	TokenID        string   `json:"token_id"`
	TeamIDs        string   `json:"team_ids"`
	TeamNames      []string `json:"team_names"`
	// GroupIDs is admission-time membership, persisted only in the reporting
	// snapshot. nil is unknown, while a non-nil empty slice is known empty.
	GroupIDs []string `json:"-"`
	// Optional reporting labels and classifications captured by the caller at
	// admission. The catalog currently has numeric metadata only, so missing
	// taxonomy stays unavailable rather than guessed from model names.
	Project       string `json:"project,omitempty"`
	CostCenter    string `json:"cost_center,omitempty"`
	ModelFamily   string `json:"model_family,omitempty"`
	ModelProvider string `json:"model_provider,omitempty"`
	ModelHosting  string `json:"model_hosting,omitempty"`
	// CostStatus is an explicit admission-time signal: known_free, priced,
	// unpriced, disabled, or unknown. Zero recorded cost alone proves nothing.
	CostStatus string `json:"cost_status,omitempty"`
	UpstreamID string `json:"upstream_id"`
	ModelID    string `json:"model_id"`
	ModelName  string `json:"model"`
	// RequestedModelName records the managed-model alias the caller actually
	// asked for, when the request was addressed to one. ModelName always
	// carries the UNDERLYING model, so model reporting keeps reflecting real
	// models; this field exists so admins can still see alias adoption.
	// Empty for requests addressed directly to a real model.
	RequestedModelName string `json:"requested_model_name,omitempty"`
	EndpointPath       string `json:"endpoint_path"`
	HTTPMethod         string `json:"http_method"`
	Modality           string `json:"modality"`
	Streaming          bool   `json:"streaming"`
	RequestBytes       int64  `json:"request_bytes"`
	ResponseBytes      int64  `json:"response_bytes"`
	AttachmentCount    int    `json:"attachment_count"`
	TokensIn           int64  `json:"tokens_in"`
	TokensOut          int64  `json:"tokens_out"`
	TokensCached       int64  `json:"tokens_cached"`
	// TokensCacheWrite5m and TokensCacheWrite1h count prompt-cache write
	// tokens (5-minute and 1-hour TTL entries) as reported by cache-billing
	// upstreams (Anthropic reports them separately from input_tokens, so
	// they are additive to TokensIn). Persisted per event so historical
	// usage keeps its own counts if cache-write rates are ever repriced.
	// Together with TokensIn and TokensCached these define the canonical
	// cache-hit rate: TokensCached / (TokensIn + TokensCacheWrite5m +
	// TokensCacheWrite1h) — see store.Totals for the aggregate form.
	TokensCacheWrite5m int64  `json:"tokens_cache_write_5m"`
	TokensCacheWrite1h int64  `json:"tokens_cache_write_1h"`
	AccountingMode     string `json:"token_accounting_method"`
	// CostNano is the request's cost in nano-USD, computed once at recording
	// time. In local-only mode (JANUS_LOCAL_ONLY) cost tracking is off and
	// new events record 0 here; the column and any historical values are kept
	// untouched, so disabling the mode simply resumes cost tracking.
	CostNano      int64  `json:"cost_nanousd"`
	FinishReason  string `json:"finish_reason"`
	HTTPStatus    int    `json:"http_status"`
	LatencyMs     int    `json:"latency_ms"`
	TTFBMs        int    `json:"ttfb_ms"`
	UpstreamMs    int    `json:"upstream_latency_ms"`
	UserAgent     string `json:"client_user_agent"`
	ClientIP      string `json:"client_ip"`
	XForwardedFor string `json:"x_forwarded_for"`
	Referer       string `json:"referer"`
	// ClientApp names the calling application — an agent harness, IDE
	// plugin, or app — when it identifies itself via an attribution header.
	// User-Agent only ever reveals the SDK, so this is the only way to tell
	// two different agents apart. Caller-supplied and untrusted: a reporting
	// label, never an authorisation input.
	ClientApp      string `json:"client_app"`
	ErrorCode      string `json:"error_code"`
	QuotaViolated  bool   `json:"quota_violated"`
	BlockingRuleID string `json:"blocking_rule_id"`
	RequestID      string `json:"request_id"`
	// Throughput as returned to the caller in the X-Janus-*Per-Second
	// headers. ThroughputSource is usage.ThroughputUpstream when the
	// provider measured it, usage.ThroughputCalculated when the gateway
	// derived it from token counts and its own clock, "" when neither was
	// possible (no tokens, or unmetered). Kept so historical analysis can
	// separate provider speed from network-inclusive gateway timing.
	TokensInPerSecond  float64 `json:"tokens_in_per_second"`
	TokensOutPerSecond float64 `json:"tokens_out_per_second"`
	ThroughputSource   string  `json:"throughput_source"`
	// FallbackReason is the FallbackTrigger* that sent this request to the
	// managed alias's fallback model (ModelName is then the fallback,
	// RequestedModelName the alias). Empty when the target served.
	FallbackReason string `json:"fallback_reason,omitempty"`
	// SecgwAction summarises what the Security Gateway did to this
	// request ('' = no policy applied / nothing matched; observed,
	// redacted, blocked, stream_cut). SecgwViolations counts the matched
	// checks. Both exist so the request log can show the outcome without a
	// join, and so the troubleshooting filter can key on it.
	SecgwAction     string `json:"secgw_action,omitempty"`
	SecgwViolations int    `json:"secgw_violations,omitempty"`

	// UserEmail and UserName identify the owning user on admin cross-user
	// request-log queries (scope=all / user_id overrides). They are resolved
	// by the API layer from app_user at read time — never persisted on the
	// usage_event row — and omitted entirely from user-scoped responses.
	UserEmail string `json:"user_email,omitempty"`
	UserName  string `json:"user_name,omitempty"`
	// ServiceTokenName is the resolved display name of the service token
	// that made the request, for the admin request log. Like UserEmail it is
	// resolved at read time and never persisted on the event row.
	ServiceTokenName string `json:"service_token_name,omitempty"`
	// HasCapture marks events whose request/response payloads were captured
	// by troubleshooting mode, so the admin request log can offer a
	// download. Resolved at read time by the API layer; never persisted.
	HasCapture bool `json:"has_capture,omitempty"`
	// AfterInsert, when set, runs on the goroutine that inserted the row,
	// after the insert, with the row's ID populated. Used to write
	// dependent rows (Security Gateway violations) without a second
	// goroutine reading the event concurrently. Never persisted.
	AfterInsert func(ctx context.Context) `json:"-"`
}

// RunAfterInsert invokes AfterInsert once, if set.
func (e *UsageEvent) RunAfterInsert(ctx context.Context) {
	if e.AfterInsert != nil {
		f := e.AfterInsert
		e.AfterInsert = nil
		f(ctx)
	}
}

// AuditEntry records one administrative change. The table is append-only.
type AuditEntry struct {
	ID           string    `json:"id"`
	ActorUserID  string    `json:"actor_user_id"`
	ActorLabel   string    `json:"actor_label"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	OldValue     string    `json:"old_value"`
	NewValue     string    `json:"new_value"`
	CreatedAt    time.Time `json:"created_at"`
}

// BlockingRule is a composable policy control. Every signal except the bearer
// token is client-supplied and therefore spoofable; the UI says so.
type BlockingRule struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Combinator string       `json:"combinator"`
	Reason     string       `json:"reason"`
	Enabled    bool         `json:"enabled"`
	Clauses    []RuleClause `json:"clauses"`
	HitCount   int64        `json:"hit_count"`
	LastHitAt  time.Time    `json:"last_hit_at"`
	CreatedAt  time.Time    `json:"created_at"`
}

// RuleClause is one signal test inside a blocking rule.
type RuleClause struct {
	Type    string `json:"type"`
	Pattern string `json:"pattern"`
	Header  string `json:"header,omitempty"`
	Negate  bool   `json:"negate,omitempty"`
}

// Blocking rule clause types (8 signals).
const (
	ClauseUserAgent  = "user_agent_pattern"
	ClauseSourceIP   = "source_ip_cidr"
	ClauseXFF        = "x_forwarded_for_cidr"
	ClauseEndpoint   = "endpoint_regex"
	ClauseMethod     = "http_method"
	ClauseHeader     = "header_match"
	ClauseTokenMatch = "downstream_token_pattern"
	ClauseModelName  = "model_name"
)

// AlertRule configures how a trigger is delivered.
type AlertRule struct {
	ID         string    `json:"id"`
	Trigger    string    `json:"trigger"`
	Severity   string    `json:"severity"`
	Channels   []string  `json:"channels"`
	WebhookURL string    `json:"webhook_url"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
}

// Notification is the always-on in-app delivery channel.
type Notification struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Severity  string    `json:"severity"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	ReadAt    time.Time `json:"read_at"`
	CreatedAt time.Time `json:"created_at"`
}

// DocsFeedback is anonymous page-level feedback from the documentation site.
type DocsFeedback struct {
	ID         string    `json:"id"`
	Page       string    `json:"page"`
	Helpful    bool      `json:"helpful"`
	Note       string    `json:"note"`
	ResolvedAt time.Time `json:"resolved_at"`
	CreatedAt  time.Time `json:"created_at"`
}
