/** Shapes returned by the Janus application API. */

export interface Totals {
  tokens_in: number;
  tokens_out: number;
  tokens_cached: number;
  /**
   * Summed prompt-cache write tokens (5-minute / 1-hour TTL entries). Together
   * with tokens_in and tokens_cached these feed the canonical cache-hit rate —
   * see cacheHitRate() in lib/cache.ts, the only place the formula lives.
   */
  tokens_cache_write_5m: number;
  tokens_cache_write_1h: number;
  cost_nanousd: number;
  request_count: number;
  error_count: number;
}

export interface Breakdown {
  key: string;
  label: string;
  totals: Totals;
}

export interface TimePoint {
  bucket: string;
  totals: Totals;
}

export interface Team {
  listed?: boolean;
  archived_at?: string;
  my_role?: 'member' | 'moderator' | 'leader';
  id: string;
  name: string;
  lead_user_id: string;
  lead_name: string;
  lead_can_edit_quotas: boolean;
  member_count: number;
}

export interface Me {
  id: string;
  email: string;
  name: string;
  /**
   * The EFFECTIVE role. A user promoted through an admin group has admin
   * capability while their stored role stays "user", so gating UI on the
   * stored column hides the admin surface from a real administrator.
   */
  /** The sticky role column, before group promotion is applied. */
  stored_role?: string;
  /** True when admin capability comes from IdP group membership. */
  admin_via_group?: boolean;
  role: 'user' | 'team_lead' | 'admin';
  is_active: boolean;
  timezone: string;
  locale: string;
  groups: string[];
  teams: Team[];
  leads_teams: string[];
  active_team_id?: string;
  active_sessions: number;
  /**
   * Instance-wide feature flags (keyed by store.DefaultFeatureFlags names,
   * e.g. spend_emphasis) surfaced to every signed-in viewer so the UI can
   * adapt — session.tsx derives the default dashboard metric from this map.
   */
  feature_flags: Record<string, boolean>;
  endpoint: string;
  /**
   * True when the deployment runs in local-only mode (JANUS_LOCAL_ONLY):
   * cost tracking is disabled instance-wide, so no cost/spend/pricing
   * surface may render. Optional for wire compatibility with older servers.
   */
  local_only?: boolean;
  /**
   * License banner state. edition community|business|enterprise; status
   * valid|expiring|grace|expired|invalid; restricted true when creating
   * new items is blocked. Optional for wire compatibility with older servers.
   */
  license?: LicenseSummary;
}

export interface RenewalNotice {
  suppress_expiring: boolean;
  reason: string;
  fresh_until?: string;
}

export interface LicenseSync {
  enabled: boolean;
  has_token: boolean;
  enabled_managed?: boolean;
  token_managed?: boolean;
  mode: 'manual' | 'online' | 'offline' | 'file';
  health: 'never' | 'healthy' | 'stale' | 'error' | 'disabled';
  last_attempt_at?: string;
  last_success_at?: string;
  fresh_until?: string;
  error_code?: string;
  subscription?: {
    schema_version: number;
    status: string;
    auto_renew: boolean;
    cancel_at_period_end: boolean;
    paid_through?: string | null;
    fresh_until?: string;
  };
}

export interface LicenseSyncPut {
  enabled?: boolean;
  token?: string;
  clear_token?: boolean;
}

export interface LicenseSummary {
  renewal_notice?: RenewalNotice;
  edition: 'community' | 'business' | 'enterprise';
  status: 'valid' | 'expiring' | 'grace' | 'expired' | 'invalid';
  restricted: boolean;
  features: string[];
  expires_at?: string;
  grace_until?: string;
}

export interface LicenseClaims {
  key_id: string;
  license_id: string;
  org: string;
  issued_to: string;
  edition: string;
  seats: number;
  nodes: number;
  site?: string;
  features: string[];
  issued: string;
  exp?: string;
  grace_days: number;
  term: 'subscription' | 'trial' | 'perpetual';
  maintenance_until?: string;
  offline?: boolean;
  notes?: string;
}

export interface LicenseState {
  installed: boolean;
  source?: 'file' | 'database';
  edition: 'community' | 'business' | 'enterprise';
  status: 'valid' | 'expiring' | 'grace' | 'expired' | 'invalid';
  error?: string;
  claims?: LicenseClaims;
  seats: number;
  nodes: number;
  features: string[];
  clock_skew?: boolean;
  checked_at: string;
  expires_at?: string;
  grace_until?: string;
}

export interface LicenseDocument {
  renewal_notice?: RenewalNotice;
  license_sync?: LicenseSync;
  license: LicenseState;
  seats_used: number;
  nodes_live: number;
  seat_window: string;
  instance_id: string;
  file?: string;
  version: string;
  build_date?: string;
  portal_url: string;
  /** Cached result of the opt-in daily version check (JANUS_UPDATE_CHECK). */
  update: UpdateCheck;
}

export interface UpdateCheck {
  enabled: boolean;
  offline: boolean;
  checked_at?: string;
  error?: string;
  current: string;
  latest?: string;
  published?: string;
  notes_url?: string;
  advisory?: string;
  min_supported?: string;
  update_available: boolean;
  unsupported: boolean;
}

export interface ApiToken {
  team_id?: string;
  /**
   * 30-day usage, returned by GET /api/v1/tokens. Optional so a cached or
   * older payload still renders — the UI shows 0 rather than breaking.
   */
  requests_30d?: number;
  tokens_in_30d?: number;
  tokens_out_30d?: number;
  errors_30d?: number;
  spend_30d_usd?: number;
  top_model_30d?: string;
  model_count_30d?: number;
  id: string;
  user_id: string;
  prefix: string;
  description: string;
  created_at: string;
  /** RFC 3339 timestamp of last use; empty string when never used. */
  last_used_at: string;
  /**
   * RFC 3339 revocation timestamp; empty string while the token is active.
   * Always test via {@link isTokenRevoked} rather than raw truthiness.
   */
  revoked_at: string; // '' while active — never render badges from raw truthiness
}

/**
 * True when a token has been revoked. The gateway serializes `revoked_at` as
 * an empty string for active tokens, but older builds leaked Go's zero time
 * ("0001-01-01T00:00:00Z") — a truthy string that made every active token
 * render with a "revoked" badge. Guarding the prefix here keeps the UI
 * correct against any gateway that still emits the raw zero value.
 */
export function isTokenRevoked(token: Pick<ApiToken, 'revoked_at'>): boolean {
  return Boolean(token.revoked_at) && !token.revoked_at.startsWith('0001-01-01');
}

export type MetadataSource = 'admin' | 'upstream' | 'reference' | 'unknown';
export interface ModelMetadataField {
  value: number | null;
  source: MetadataSource;
  override: number | null;
  automatic_value: number | null;
  automatic_source: MetadataSource;
}
export interface Model {
  metadata?: Record<string, ModelMetadataField>;
  metadata_warnings?: string[];
  id: string;
  upstream_id: string;
  upstream_name: string;
  adapter_type: string;
  /** Native upstream model name — the stable identifier the provider knows. */
  name: string;
  /** Admin-set alias shown to users. Empty string when never renamed. */
  display_name: string;
  status: 'enabled' | 'disabled' | 'pending_approval' | 'stale';
  modalities: string[];
  rate_in_nanousd: number;
  rate_out_nanousd: number;
  rate_cached_nanousd: number;
  /** Prompt-cache write rates (5-minute / 1-hour TTL entries), nano-USD per MTok. */
  rate_cache_write_5m_nanousd: number;
  rate_cache_write_1h_nanousd: number;
  /** Context window size in tokens. 0 means unknown — render a placeholder, not a number. */
  context_window: number;
  /** Zero time (0001-01-01…) means the model has never been priced. */
  rate_effective_from: string;
  discovered_at: string;
  grant_count: number;
  grant_source?: string;
  /**
   * Live health, resolved by the gateway at read time (GET /api/v1/models
   * only; the admin list and the OpenAI-compatible /v1/models omit it, hence
   * optional). request_count_10m counts requests that reached or tried to
   * reach the upstream in the trailing 10 minutes; error_count_10m the
   * upstream-attributable failures (5xx / upstream.* codes) among them.
   */
  request_count_10m?: number;
  error_count_10m?: number;
  /** error_count_10m / request_count_10m × 100, one decimal; 0 with no traffic. */
  error_rate_percent?: number;
  /** Mirrors the admin Upstreams page: did the owning upstream's last probe succeed? */
  upstream_reachable?: boolean;
  /** Go zero time (0001-01-01…) when the upstream has never been probed. */
  upstream_last_check_at?: string;
  upstream_last_error?: string;
  upstream_last_latency_ms?: number;
  /** Gateway-derived verdict; see modelHealth() for the client-side fallback. */
  health?: ModelHealth;
  /** Security gateway: 'text_classification' when the model is reserved as a classifier. Absent/empty otherwise. */
  classifier_role?: string;
}

/** Health verdict the gateway attaches to catalog models. */
export type ModelHealth = 'healthy' | 'degraded' | 'down' | 'unknown';

/**
 * The health verdict for a model. Prefers the gateway's own `health` field and
 * derives the same verdict from the raw fields for older gateways that send
 * the rollup without it; a response with no health data at all is "unknown".
 */
export function modelHealth(model: Model): ModelHealth {
  if (model.health) return model.health;
  if (model.upstream_reachable === undefined && model.request_count_10m === undefined) return 'unknown';
  const probed = Boolean(model.upstream_last_check_at) && !model.upstream_last_check_at!.startsWith('0001-01-01');
  const requests = model.request_count_10m ?? 0;
  const errors = model.error_count_10m ?? 0;
  const rate = model.error_rate_percent ?? (requests > 0 ? (errors / requests) * 100 : 0);
  if (probed && !model.upstream_reachable) return 'down';
  if (requests >= 3 && errors >= requests) return 'down';
  if (requests > 0 && rate >= 10) return 'degraded';
  if (!probed && requests === 0) return 'unknown';
  return 'healthy';
}

/** True when the model cannot be expected to serve a request right now. */
export function isModelDown(model: Model): boolean {
  return modelHealth(model) === 'down';
}

/**
 * The name users see for a model: the admin-set display name when one is set,
 * otherwise the native upstream name. Mirrors the gateway's Model.PublicName().
 */
export function publicModelName(model: Pick<Model, 'name' | 'display_name'>): string {
  return model.display_name || model.name;
}

/** True when an admin alias is in force and differs from the upstream name. */
export function isRenamedModel(model: Pick<Model, 'name' | 'display_name'>): boolean {
  return Boolean(model.display_name) && model.display_name !== model.name;
}

/** Mirrors maxModelDisplayNameLength in the gateway's admin API. */
export const MAX_MODEL_DISPLAY_NAME = 200;

export interface UsageEvent {
  /** Recorded attribution, independent of current token affinity. */
  team_ids?: string;
  team_names?: string[];
  id: string;
  created_at: string;
  /**
   * Set (admin request log only) when troubleshooting mode captured this
   * request's payloads, so a download can be offered for the row.
   */
  has_capture?: boolean;
  secgw_action?: string;
  secgw_violations?: number;
  user_id: string;
  token_id: string;
  /**
   * Set when the request came from a service token rather than a person. Such
   * an event deliberately carries an EMPTY user_id — that emptiness is what
   * keeps integration traffic out of people-oriented reports — so any UI that
   * resolves only user fields will render it as "unknown". Check these first.
   */
  service_token_id?: string;
  /** Resolved at read time for the admin request log; never persisted. */
  service_token_name?: string;
  /**
   * The calling application (agent harness, IDE plugin, app) when it
   * identifies itself via an attribution header. Empty when it does not:
   * client_user_agent only ever names the SDK.
   */
  client_app?: string;
  model: string;
  endpoint_path: string;
  http_method: string;
  modality: string;
  streaming: boolean;
  request_bytes: number;
  response_bytes: number;
  tokens_in: number;
  tokens_out: number;
  tokens_cached: number;
  /** Prompt-cache write token counts (5-minute / 1-hour TTL entries). */
  tokens_cache_write_5m: number;
  tokens_cache_write_1h: number;
  /**
   * Where the metering figures came from: 'upstream_reported' (token counts
   * priced by the rate card), 'upstream_reported_cost' (provider gave the
   * exact cost, e.g. X.ai cost_in_usd_ticks), 'byte_count_fallback' (text
   * response without usage, ~bytes/4), 'unmetered_modality' (media response
   * without usage — a configuration gap, recorded at zero), 'not_billable'
   * (error response, never costed).
   */
  token_accounting_method: string;
  cost_nanousd: number;
  finish_reason: string;
  http_status: number;
  latency_ms: number;
  ttfb_ms: number;
  /** Tokens per second as returned in the X-Janus-*Per-Second headers. */
  tokens_in_per_second?: number;
  tokens_out_per_second?: number;
  /** 'upstream' when the provider measured it, 'calculated' when the gateway derived it, '' when unknown. */
  throughput_source?: string;
  /** Failure mode that sent a managed-model request to its fallback (model is then the fallback); absent when the target served. */
  fallback_reason?: string;
  client_user_agent: string;
  client_ip: string;
  x_forwarded_for: string;
  error_code: string;
  quota_violated: boolean;
  request_id: string;
  /**
   * Owning user's identity, present only on admin cross-user queries
   * (scope=all / user_id overrides on /api/v1/requests). Resolved at read
   * time from the account table; absent on user-scoped responses.
   */
  user_email?: string;
  user_name?: string;
}

export interface RateLimitRule {
  id: string;
  subject_type: string;
  subject_id: string;
  subject_name: string;
  endpoint: string;
  requests_per_minute: number;
  created_at: string;
}

export interface QuotaStatus {
  id: string;
  metric: string;
  metric_label: string;
  window: string;
  window_label: string;
  subject_type: string;
  subject_name: string;
  model_name: string;
  breach_behavior: string;
  limit_value: number;
  current_value: number;
  limit_display: number;
  current_display: number;
  unit: 'usd' | 'count';
  percent: number;
  at_risk: boolean;
  breached: boolean;
  reset_at: string;
  alert_thresholds: number[];
  alert_thresholds_custom: boolean;
}

export interface Upstream {
  id: string;
  name: string;
  adapter_type: string;
  base_url: string;
  api_key_mask: string;
  has_api_key: boolean;
  enabled: boolean;
  last_check_at: string;
  last_error: string;
  last_latency_ms: number;
  model_count: number;
  created_at: string;
}

export interface Grant {
  id: string;
  model_id: string;
  /** Whether model_id names a real catalog model or a managed alias. */
  model_kind: 'model' | 'managed';
  model_name: string;
  grantee_type: 'user' | 'group' | 'all_users' | 'service_token' | 'all_service_tokens' | 'team' | 'all_teams';
  grantee_id: string;
  grantee_name: string;
  created_at: string;
}

/** Lifecycle state of a service credential, computed server-side. */
type ServiceTokenStatus = 'active' | 'revoked' | 'expired';

/**
 * A credential that authenticates a non-human integration.
 *
 * Service tokens belong to no user. Their usage is attributed to `name` the way
 * a person's usage is attributed to their identity, and it counts toward
 * org-wide reporting — but never appears in user-oriented reports such as top
 * users. They reach the /v1 inference API only.
 *
 * The optional timestamps are empty strings (not the RFC 3339 zero time) when
 * unset, so they can be treated as falsy.
 */
interface ServiceToken {
  id: string;
  name: string;
  description: string;
  prefix: string;
  created_by_user_id: string;
  created_by_label?: string;
  created_at: string;
  last_used_at: string;
  /** Empty when the credential never expires. */
  expires_at: string;
  /** Empty while the credential is active. */
  revoked_at: string;
  status: ServiceTokenStatus;
  grant_count: number;
}

/**
 * Failure modes that can send a managed model's traffic to its fallback.
 * Mirrors store.FallbackTrigger* on the server; ALL_FALLBACK_TRIGGERS is the
 * display order.
 */
export type FallbackTrigger = 'target_unavailable' | 'upstream_unreachable' | 'model_down' | 'model_degraded';
export const ALL_FALLBACK_TRIGGERS: FallbackTrigger[] = [
  'target_unavailable',
  'upstream_unreachable',
  'model_down',
  'model_degraded',
];
/** What the server applies when a fallback is set and no triggers are chosen. */
export const DEFAULT_FALLBACK_TRIGGERS: FallbackTrigger[] = ['target_unavailable', 'upstream_unreachable', 'model_down'];

/**
 * An admin-defined stable alias for a real catalog model.
 *
 * The indirection is deliberately transparent: the resolved target travels with
 * every response so the UI can tell a user exactly what they are talking to.
 * Reporting always reflects `target_*`, never the alias.
 */
export interface ManagedModel {
  id: string;
  name: string;
  description: string;
  status: 'enabled' | 'disabled';
  target_model_id: string;
  target_model_name: string;
  target_display_name?: string;
  /** What the alias resolves to right now, as a user would see it named. */
  target_public_name: string;
  target_status: string;
  target_upstream_id: string;
  target_upstream_name: string;
  modalities: string[];
  context_window: number;
  /** True when the alias is enabled AND its target can actually serve. */
  servable: boolean;
  /** True when the target is missing or not enabled — needs admin repair. */
  broken: boolean;
  broken_reason?: string;
  /**
   * Optional automatic fallback: a real catalog model (never another alias)
   * served when the target is unavailable for one of `fallback_triggers`.
   * Empty when none is configured.
   */
  fallback_model_id: string;
  fallback_triggers: FallbackTrigger[];
  fallback_model_name?: string;
  fallback_public_name?: string;
  fallback_status?: string;
  fallback_upstream_id?: string;
  fallback_upstream_name?: string;
  /** The configured fallback is itself missing or disabled. */
  fallback_broken: boolean;
  fallback_broken_reason?: string;
  grant_count: number;
  created_by_user_id: string;
  created_at: string;
  updated_at: string;
  grant_source?: string;
}

/**
 * A row of the admin service-tokens table.
 *
 * Extends the bare credential with the same 30-day usage aggregate the people
 * table carries, because the operational questions are the same and a service
 * token has no human owner to ask.
 */
export interface ServiceTokenRow extends ServiceToken {
  spend_30d_usd: number;
  requests_30d: number;
  tokens_in_30d: number;
  tokens_out_30d: number;
  errors_30d: number;
  /** Most-used underlying model over the window; empty when unused. */
  top_model_30d: string;
  model_count_30d: number;
}

/**
 * A row of the admin managed-models table.
 *
 * Usage here is keyed on the alias the caller actually requested, not on the
 * underlying model — the only place that distinction is reported, so an admin
 * can judge alias adoption before repointing or deleting one.
 */
export interface ManagedModelRow extends ManagedModel {
  spend_30d_usd: number;
  requests_30d: number;
  tokens_in_30d: number;
  tokens_out_30d: number;
  errors_30d: number;
  /** Distinct people + service tokens that called the alias in the window. */
  distinct_principals_30d: number;
}

export interface AdminUserRow {
  id: string;
  email: string;
  name: string;
  role: string;
  is_active: boolean;
  created_at: string;
  last_login_at: string;
  groups: string[];
  spend_30d_usd: number;
  requests_30d: number;
  /** Output tokens produced by this user in the trailing 30 days. */
  tokens_out_30d: number;
}

/** Payload of GET /api/v1/admin/users/{id} — the People detail drawer. */
export interface AdminUserDetail {
  user: AdminUserRow;
  groups: string[];
  teams: Team[];
  effective_grants: Array<{ model: string; upstream: string; source: string; status: string }>;
  tokens: Array<{ id: string; description: string; prefix: string; revoked_at: string }>;
  quotas: Array<{ id: string; metric_label: string; window_label: string; percent: number }>;
  recent_requests: Array<{
    team_ids?: string;
    team_names?: string[];
    id: string;
    model: string;
    http_status: number;
    created_at: string;
    cost_nanousd: number;
    latency_ms: number;
    tokens_in: number;
    tokens_out: number;
  }>;
  /** Resolved ?range= label the usage series covers (defaults to month). */
  usage_range: string;
  /** Per-user usage buckets for the drawer graph, scoped to this user only. */
  usage_series: TimePoint[];
  /** Window totals matching usage_series. */
  usage_totals: Totals;
}

export interface Group {
  id: string;
  name: string;
  from_idp: boolean;
  member_count: number;
}

export interface RuleClause {
  type: string;
  pattern: string;
  header?: string;
  negate?: boolean;
}

export interface BlockingRule {
  id: string;
  name: string;
  combinator: string;
  reason: string;
  enabled: boolean;
  clauses: RuleClause[];
  hit_count: number;
  last_hit_at: string;
  created_at: string;
}

export interface AlertRule {
  id: string;
  trigger: string;
  severity: string;
  channels: string[];
  webhook_url: string;
  enabled: boolean;
  created_at: string;
}

export interface AuditEntry {
  id: string;
  actor_user_id: string;
  actor_label: string;
  action: string;
  resource_type: string;
  resource_id: string;
  old_value: string;
  new_value: string;
  created_at: string;
}

export interface Notification {
  id: string;
  severity: string;
  title: string;
  body: string;
  read_at: string;
  created_at: string;
}

export interface PersonalDashboard {
  range: string;
  start: string;
  end: string;
  totals: Totals;
  per_model: Breakdown[];
  per_modality: Breakdown[];
  per_token: Breakdown[];
  series: TimePoint[];
  recent_requests: UsageEvent[];
}

/**
 * One recent completed request, as returned for the ambient layer. Carries no
 * model, user, or token identity — only what the animation needs.
 */
interface RecentJob {
  at: string;
  tokens_out: number;
  error: boolean;
}

export interface GlobalDashboard {
  top_models: Breakdown[];
  /** Newest-first recent requests; each becomes one shooting star. */
  recent_jobs?: RecentJob[];
  totals_30d: Totals;
  active_users_15m: number;
  series: TimePoint[];
  new_models: Model[];
  leaderboards_enabled: boolean;
  /** Leaderboard entries, server-ranked by output-token volume (tokens_out DESC). */
  top_users?: Breakdown[];
  /** Leaderboard entries, server-ranked by output-token volume (tokens_out DESC). */
  top_teams?: Breakdown[];
}

export interface SystemStatus {
  database: {
    ok: boolean;
    engine: string;
    migrations: string[];
    /** Which engine the gateway is running on. */
    backend: 'sqlite' | 'postgres';
    /** Credential-free location: a file path (sqlite) or host:port/dbname (postgres). Never a DSN. */
    location: string;
    /** True when JANUS_DATABASE_URL was unset and the built-in sqlite default is in force. */
    defaulted: boolean;
  };
  identity_provider: { ok: boolean; provider: string; dev_auth: boolean };
  quota_ledger: { ok: boolean; mode: string };
  email: { ok: boolean; detail: string };
  telemetry: { prometheus: string };
  upstreams: Array<{
    id: string;
    name: string;
    adapter_type: string;
    enabled: boolean;
    reachable: boolean;
    last_check_at: string;
    last_error: string;
    latency_ms: number;
    model_count: number;
  }>;
  discovery: { last_run_at: string; interval_minutes: number; interval: DiscoveryInterval };
  /**
   * How the last window_days of successful requests were metered. ok is false
   * when any media response was recorded unmetered — the upstream returned no
   * usage the adapter could read — which is a configuration gap listed per
   * model in summary.gaps.
   */
  metering: MeteringHealth;
  retention: { usage_days: number; audit_days: number; purge_at_utc: string };
  build: { version: string; sha: string; started_at: string; uptime_seconds: number };
  /**
   * Every registered flag with its current value (incl. spend_emphasis) —
   * the /admin/system panel renders one toggle per entry.
   */
  feature_flags: Record<string, boolean>;
  docs_feedback: Array<{ id: string; page: string; helpful: boolean; note: string; created_at: string }>;
  adapters: string[];
  /** True when the gateway runs in local-only mode (JANUS_LOCAL_ONLY): cost tracking disabled. */
  local_only?: boolean;
  /**
   * Per-hop upstream timeouts in force right now, with provenance. Same
   * document as GET /api/v1/admin/system/upstream-timeouts; the System page
   * edits it through PATCH/DELETE on that endpoint.
   */
  upstream_timeouts: UpstreamTimeouts;
}

/** One set of upstream timeout values, in whole seconds. In `overrides`, 0 means "not overridden". */
export interface UpstreamTimeoutValues {
  connect_seconds: number;
  ttfb_seconds: number;
  total_seconds: number;
}

/**
 * Response of GET/PATCH/DELETE /api/v1/admin/system/discovery-interval — how
 * often the gateway polls every upstream for its model catalog (which doubles
 * as the reachability probe). Loaded from JANUS_DISCOVERY_INTERVAL_MINUTES at
 * boot; an administrator override is persisted and applied without a restart.
 */
export interface DiscoveryInterval {
  /** Cadence the scheduler honours right now. */
  effective_minutes: number;
  /** Environment value (JANUS_DISCOVERY_INTERVAL_MINUTES). */
  default_minutes: number;
  /** Stored override; 0 when none is set. */
  override_minutes: number;
  source: 'default' | 'override';
  min_minutes: number;
  max_minutes: number;
  updated_at?: string;
  last_run_at: string;
  next_run_at?: string;
}

/** Body of PATCH /api/v1/admin/system/discovery-interval; 0 reverts to the environment default. */
export interface DiscoveryIntervalPatch {
  minutes: number;
}

interface AccountingGap {
  model_name: string;
  upstream_id: string;
  upstream_name: string;
  modality: string;
  requests: number;
  last_seen_at: string;
}

export interface MeteringHealth {
  ok: boolean;
  window_days: number;
  summary: {
    upstream_reported: number;
    upstream_reported_cost: number;
    byte_estimated: number;
    unmetered: number;
    gaps: AccountingGap[];
  };
}

/** Response of GET/PATCH/DELETE /api/v1/admin/system/upstream-timeouts. */
export interface UpstreamTimeouts {
  /** What the proxy enforces right now (defaults with overrides layered on). */
  effective: UpstreamTimeoutValues;
  /** Environment-derived values from JANUS_UPSTREAM_*_TIMEOUT_SECONDS. */
  defaults: UpstreamTimeoutValues;
  /** Administrator overrides persisted in the database; 0 per hop = inherit the default. */
  overrides: UpstreamTimeoutValues;
  /** Per hop, whether the effective value comes from the environment or an admin override. */
  source: { connect: 'env' | 'admin'; ttfb: 'env' | 'admin'; total: 'env' | 'admin' };
  /** When overrides were last saved; absent when nothing is overridden. */
  updated_at?: string;
  /** Largest value any hop may be set to (24h), for client-side validation. */
  max_seconds: number;
}

/** PATCH body: any subset; positive overrides, 0 reverts that hop to its env default. */
export type UpstreamTimeoutsPatch = Partial<UpstreamTimeoutValues>;

// --- Troubleshooting mode ---------------------------------------------------

/** One set of capture criteria. Lists are any-of; empty/absent = not applied. */
export interface TroubleshootingCriteria {
  models?: string[];
  user_ids?: string[];
  group_ids?: string[];
  upstream_ids?: string[];
  error_codes?: string[];
  http_statuses?: number[];
  outcome?: '' | 'success' | 'failure';
  tokens_in_gt?: number;
  tokens_in_lt?: number;
  tokens_out_gt?: number;
  tokens_out_lt?: number;
}

/**
 * Which proxied requests a troubleshooting session captures — three gates
 * evaluated together, rules-builder style:
 * - the top-level criteria are the *include* gate, combined by `match`
 *   (any = OR, all = AND); empty admits everything;
 * - `require` is the AND gate: every populated criterion must match;
 * - `exclude` is the NOT gate: matching any populated criterion vetoes.
 * Sessions saved before the optional gates existed simply omit them.
 */
interface TroubleshootingFilter extends TroubleshootingCriteria {
  /** How the top-level (include) criteria combine: all = AND, any = OR. */
  match: 'all' | 'any';
  require?: TroubleshootingCriteria;
  exclude?: TroubleshootingCriteria;
}

/** Bounds on captured data; 0 disables a bound, at least one must be set. */
interface TroubleshootingRetention {
  max_age_hours: number;
  max_count: number;
  max_bytes: number;
}

export interface TroubleshootingConfig {
  filter: TroubleshootingFilter;
  retention: TroubleshootingRetention;
  storage: 'database' | 'disk';
  capture_request_body: boolean;
  capture_response_body: boolean;
  encrypt: boolean;
  max_body_bytes: number;
}

interface TroubleshootingSession {
  id: string;
  enabled: boolean;
  config: TroubleshootingConfig;
  created_by: string;
  created_at: string;
  updated_at: string;
  /** Zero time (0001-01-01…) when the session has no expiry. */
  expires_at: string;
  disabled_at: string;
}

interface TroubleshootingStats {
  count: number;
  total_bytes: number;
  oldest_at: string;
  newest_at: string;
}

/** Response of GET/PUT/DELETE /api/v1/admin/troubleshooting. */
export interface TroubleshootingStatus {
  active: boolean;
  session: TroubleshootingSession | null;
  stats: TroubleshootingStats;
  backends: Array<'database' | 'disk'>;
  encryption_available: boolean;
  max_body_bytes_ceiling: number;
  max_session_hours: number;
  retention_last_run_at: string;
  warnings: string[];
}

/** Body of PUT /api/v1/admin/troubleshooting. */
export interface TroubleshootingPut {
  enabled?: boolean;
  config: TroubleshootingConfig;
  /** Hours from now; 0/omitted = default (24), negative = no expiry. */
  expires_in_hours?: number;
}

/* --- Security gateway (admin) ------------------------------------------- */

export type SecgwCheckKind = 'shape' | 'secrets' | 'pii' | 'terms' | 'prompt_injection' | 'content_safety';
export type ClassifierRole = 'text_classification' | 'generative_guard';

/** One MLCommons hazard category as Llama Guard emits it. Served by the API; never hardcoded here. */
export interface SecgwCategory {
  code: string;
  name: string;
  tier: 'critical' | 'standard' | 'contextual';
}
export type SecgwMode = 'observe' | 'redact' | 'block';
export type SecgwDirection = 'ingress' | 'egress' | 'both';

export interface SecgwCheck {
  kind: SecgwCheckKind;
  enabled: boolean;
  mode: SecgwMode;
  fail?: 'closed' | 'open';
  direction: SecgwDirection;
  options?: Record<string, unknown>;
  classifier_model_id?: string;
  hold_bytes?: number;
}

export interface SecgwPolicy {
  id: string;
  name: string;
  description: string;
  enabled: boolean;
  mandatory: boolean;
  checks: SecgwCheck[];
  capture: { prompt_injection_bodies?: boolean };
  synthetic_refusal: boolean;
  refusal_text?: string;
  created_by_user_id: string;
  created_at: string;
  updated_at: string;
  binding_count: number;
}

export type SecgwScopeType = 'org' | 'group' | 'upstream' | 'service_token' | 'model' | 'managed_model';

export interface SecgwBinding {
  id: string;
  policy_id: string;
  scope_type: SecgwScopeType;
  scope_id: string;
  created_at: string;
  policy_name?: string;
  scope_name?: string;
}

export type SecgwMatchMode = 'exact' | 'substring' | 'regex' | 'fuzzy';
export type SecgwSeverity = 'low' | 'medium' | 'high' | 'critical';

export interface SecgwTermList {
  id: string;
  name: string;
  match_mode: SecgwMatchMode;
  severity: SecgwSeverity;
  /** Only present on the single-item read (audited). */
  terms?: string[];
  allow?: string[];
  term_count: number;
  created_at: string;
  updated_at: string;
}

type SecgwViolationAction = 'observed' | 'redacted' | 'blocked' | 'stream_cut';

export interface SecgwViolation {
  id: string;
  request_id: string;
  usage_event_id: string;
  user_id?: string;
  service_token_id?: string;
  model_name: string;
  policy_id: string;
  binding_id: string;
  kind: string;
  rule_id: string;
  severity: string;
  direction: string;
  action: SecgwViolationAction;
  match_offset: number;
  match_length: number;
  /** A hash of the match — never the matched content itself. */
  match_hash: string;
  /** Only present on the single-item read (audited), and only for kinds that capture text. */
  match_text?: string;
  has_match_text: boolean;
  classifier_score?: number;
  classifier_model?: string;
  created_at: string;
  /** Display names resolved at read time; absent when the referent was deleted. */
  user_label?: string;
  service_token_name?: string;
  policy_name?: string;
  term_list_name?: string;
  capture_disabled?: boolean;
}

type SecgwResolvedCheck = SecgwCheck & {
  policy_id: string;
  policy_name: string;
  binding_id: string;
  scope_type: SecgwScopeType;
  floor: boolean;
};

interface SecgwTraceEntry {
  binding_id: string;
  policy_id: string;
  policy_name: string;
  scope_type: SecgwScopeType;
  scope_id?: string;
  outcome: 'applied' | 'skipped_disabled' | 'skipped_missing_policy' | 'overridden_by_mandatory';
  detail?: string;
}

export interface SecgwOverview {
  enabled: boolean;
  policies: number;
  policies_enabled: number;
  bindings: number;
  classifiers: number;
  violations_24h: Record<string, Record<string, number>>;
  protocols: string[];
  check_kinds: string[];
}

export interface SecgwEffective {
  subject: Record<string, unknown>;
  checks: SecgwResolvedCheck[];
  trace: SecgwTraceEntry[];
}

interface SecgwDryRunViolation {
  kind: string;
  rule_id: string;
  severity: string;
  action: string;
  message_index: number;
  offset: number;
  length: number;
  classifier_score?: number;
}

export interface SecgwDryRunResult {
  action: '' | 'observed' | 'redacted' | 'blocked';
  violations: SecgwDryRunViolation[] | null;
  redactions: number;
  block_kind: string;
  classifier_failed: boolean;
  redacted_body?: unknown;
}

export interface SecgwRulesCatalog {
  secret_rules: Array<{ ID: string; Description: string; Severity: string; Enabled: boolean }>;
  pii_classes: string[];
}

/** One classifier call the security gateway made while inspecting a request. */
export interface SecgwClassifierRun {
  kind?: string;
  model_name: string;
  direction?: string;
  latency_ms: number;
  http_status: number;
  findings: SecgwViolation[];
}

/** GET /admin/requests/{id}/security */
export interface RequestSecurity {
  request_id: string;
  classifier_runs: SecgwClassifierRun[];
  other_violations: SecgwViolation[];
  /** Completeness of the newest request-violation sample, BEFORE excluding classifier findings. */
  violations_has_more: boolean;
  violations_limit: number;
  violations_scanned: number;
  violations_scope_available: boolean;
  action?: string;
}
