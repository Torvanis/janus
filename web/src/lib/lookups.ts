/**
 * Lookup sources for LookupInput: one per thing an admin might filter by.
 * Each takes the typed text and returns matches with a human label; each
 * also resolves ids back to labels so a saved filter shows names, not ids.
 *
 * Kept out of the components so the same "type 'samp', get Casey Sample"
 * behaviour is identical wherever a user/model/upstream picker appears.
 */
import { api } from './api';
import type { AdminUserRow, Group, Model, ServiceTokenRow, Upstream } from './types';
import type { LookupOption } from '../components/LookupInput';

const LIMIT = 12;

/** Search the server directory, not a preloaded first page. Resolve chosen IDs independently. */
export const userLookup = {
  search: async (query: string): Promise<LookupOption[]> => {
    const params = new URLSearchParams({ limit: String(LIMIT), sort: 'name', search: query.trim() });
    const data = await api.get<{ users: AdminUserRow[] }>(`/api/v1/admin/users?${params}`);
    return data.users.map(userOption);
  },
  resolve: async (ids: string[]): Promise<LookupOption[]> =>
    Promise.all(
      ids.map(async (id) => {
        const data = await api.get<{ user: AdminUserRow } | AdminUserRow>(`/api/v1/admin/users/${encodeURIComponent(id)}`);
        return userOption('user' in data ? data.user : data);
      }),
    ),
};
function userOption(user: AdminUserRow): LookupOption {
  return { id: user.id, label: user.name || user.email, detail: user.name ? user.email : undefined };
}

const lower = (s: string) => s.toLowerCase();

/** Client-side filter for small lists we fetch whole (upstreams, codes). */
function matchAll(options: LookupOption[], query: string): LookupOption[] {
  const q = lower(query.trim());
  if (!q) return options.slice(0, LIMIT);
  return options
    .filter((o) => lower(o.label).includes(q) || lower(o.detail ?? '').includes(q) || lower(o.id).includes(q))
    .slice(0, LIMIT);
}

/**
 * People OR service tokens: the troubleshooting `user_ids` filter matches
 * either on the backend, so one picker searches both. Tokens are listed
 * with a "service token" detail line so they are never mistaken for a person.
 */
export const principalLookup = {
  search: async (query: string): Promise<LookupOption[]> => {
    const q = query.trim();
    const uq = new URLSearchParams({ limit: String(LIMIT), sort: 'name' });
    const tq = new URLSearchParams({ limit: String(LIMIT), status: 'active' });
    if (q) {
      uq.set('search', q);
      tq.set('search', q);
    }
    const [users, tokens] = await Promise.all([
      api.get<{ users: AdminUserRow[] }>(`/api/v1/admin/users?${uq}`),
      api.get<{ service_tokens: ServiceTokenRow[] }>(`/api/v1/admin/service-tokens?${tq}`),
    ]);
    const out: LookupOption[] = [
      ...(users.users ?? []).map((u) => ({ id: u.id, label: u.name || u.email, detail: u.name ? u.email : undefined })),
      ...(tokens.service_tokens ?? []).map((s) => ({ id: s.id, label: s.name, detail: 'service token' })),
    ];
    // Interleave kinds so a full user page cannot hide every service token.
    const people = out.filter((o) => o.detail !== 'service token');
    const services = out.filter((o) => o.detail === 'service token');
    return Array.from({ length: LIMIT }, (_, i) => [people[i], services[i]])
      .flat()
      .filter((o): o is LookupOption => !!o)
      .slice(0, LIMIT);
  },
  resolve: async (ids: string[]): Promise<LookupOption[]> => {
    const tokens = await api
      .get<{ service_tokens: ServiceTokenRow[] }>('/api/v1/admin/service-tokens')
      .then((r) => new Map((r.service_tokens ?? []).map((s) => [s.id, s])))
      .catch(() => new Map<string, ServiceTokenRow>());
    return Promise.all(
      ids.map(async (id) => {
        const tok = tokens.get(id);
        if (tok) return { id, label: tok.name, detail: 'service token' };
        return api
          .get<{ user: AdminUserRow } | AdminUserRow>(`/api/v1/admin/users/${encodeURIComponent(id)}`)
          .then((r) => ('user' in r ? r.user : r))
          .then((u) => ({ id: u.id, label: u.name || u.email, detail: u.name ? u.email : undefined }))
          .catch(() => ({ id, label: id }));
      }),
    );
  },
};

/** Models are matched by NAME on the wire (usage_event.model_name). */
export const modelNameLookup = {
  search: async (query: string): Promise<LookupOption[]> => {
    const qs = new URLSearchParams({ limit: '50' });
    if (query.trim()) qs.set('search', query.trim());
    const res = await api.get<{ models: Model[] }>(`/api/v1/admin/models?${qs}`);
    const seen = new Set<string>();
    const out: LookupOption[] = [];
    for (const m of res.models ?? []) {
      if (m.classifier_role || seen.has(m.name)) continue;
      seen.add(m.name);
      out.push({
        id: m.name,
        label: m.display_name || m.name,
        detail: m.display_name ? `${m.name} · ${m.upstream_name}` : m.upstream_name,
      });
    }
    return matchAll(out, query);
  },
  resolve: async (names: string[]): Promise<LookupOption[]> => {
    const res = await api.get<{ models: Model[] }>('/api/v1/admin/models?limit=500');
    const byName = new Map((res.models ?? []).map((m) => [m.name, m]));
    return names.map((n) => {
      const m = byName.get(n);
      return m ? { id: n, label: m.display_name || n, detail: m.upstream_name } : { id: n, label: n };
    });
  },
};

export const upstreamLookup = {
  search: async (query: string): Promise<LookupOption[]> => {
    const res = await api.get<{ upstreams: Upstream[] }>('/api/v1/admin/upstreams');
    return matchAll(
      (res.upstreams ?? []).map((u) => ({ id: u.id, label: u.name, detail: u.adapter_type })),
      query,
    );
  },
  resolve: async (ids: string[]): Promise<LookupOption[]> => {
    const res = await api.get<{ upstreams: Upstream[] }>('/api/v1/admin/upstreams');
    const byId = new Map((res.upstreams ?? []).map((u) => [u.id, u]));
    return ids.map((id) => {
      const u = byId.get(id);
      return u ? { id, label: u.name, detail: u.adapter_type } : { id, label: id };
    });
  },
};

/**
 * Error codes the gateway emits. Static because the set is the code's, not
 * the database's; free text stays allowed for codes added later.
 */
const ERROR_CODES: LookupOption[] = [
  { id: 'policy.quota_exceeded', label: 'policy.quota_exceeded', detail: 'Spend or token quota hit' },
  { id: 'policy.rate_limit', label: 'policy.rate_limit', detail: 'Requests-per-minute cap' },
  { id: 'policy.user_disabled', label: 'policy.user_disabled', detail: 'Account deactivated' },
  { id: 'policy.model_not_granted', label: 'policy.model_not_granted', detail: 'No grant for this model' },
  { id: 'policy.endpoint_blocked', label: 'policy.endpoint_blocked', detail: 'Blocking rule matched' },
  { id: 'policy.token_invalid', label: 'policy.token_invalid', detail: 'Bad or revoked API token' },
  { id: 'policy.security_blocked', label: 'policy.security_blocked', detail: 'Security Gateway blocked' },
  { id: 'policy.response_too_large', label: 'policy.response_too_large', detail: 'Response exceeded cap' },
  { id: 'policy.service_token_scope', label: 'policy.service_token_scope', detail: 'Service token outside its scope' },
  { id: 'policy.service_token_expired', label: 'policy.service_token_expired', detail: 'Service token past expiry' },
  { id: 'policy.managed_model_unavailable', label: 'policy.managed_model_unavailable', detail: 'Alias has no live target' },
  {
    id: 'policy.managed_model_fallback_exhausted',
    label: 'policy.managed_model_fallback_exhausted',
    detail: 'Every fallback failed',
  },
  { id: 'upstream.unavailable', label: 'upstream.unavailable', detail: 'Provider down or timed out' },
  { id: 'upstream.rate_limit', label: 'upstream.rate_limit', detail: 'Provider returned 429' },
  { id: 'invalid_request_error', label: 'invalid_request_error', detail: 'Malformed request' },
  { id: 'authentication_error', label: 'authentication_error', detail: 'Not signed in' },
  { id: 'permission_error', label: 'permission_error', detail: 'Forbidden' },
  { id: 'not_found_error', label: 'not_found_error', detail: 'Unknown route or model' },
  { id: 'server_error', label: 'server_error', detail: 'Gateway fault' },
];

export const errorCodeLookup = {
  search: async (query: string): Promise<LookupOption[]> => matchAll(ERROR_CODES, query),
  resolve: async (ids: string[]): Promise<LookupOption[]> =>
    ids.map((id) => ERROR_CODES.find((c) => c.id === id) ?? { id, label: id }),
};

const HTTP_STATUSES: LookupOption[] = [
  { id: '400', label: '400', detail: 'Bad request' },
  { id: '401', label: '401', detail: 'Unauthorized' },
  { id: '403', label: '403', detail: 'Forbidden' },
  { id: '404', label: '404', detail: 'Not found' },
  { id: '408', label: '408', detail: 'Request timeout' },
  { id: '413', label: '413', detail: 'Payload too large' },
  { id: '422', label: '422', detail: 'Unprocessable' },
  { id: '429', label: '429', detail: 'Too many requests' },
  { id: '500', label: '500', detail: 'Server error' },
  { id: '502', label: '502', detail: 'Bad gateway' },
  { id: '503', label: '503', detail: 'Unavailable' },
  { id: '504', label: '504', detail: 'Gateway timeout' },
];

export const httpStatusLookup = {
  search: async (query: string): Promise<LookupOption[]> => matchAll(HTTP_STATUSES, query),
  resolve: async (ids: string[]): Promise<LookupOption[]> =>
    ids.map((id) => HTTP_STATUSES.find((c) => c.id === id) ?? { id, label: id }),
};

/** Groups, by name. Small list: fetched once per search and filtered here. */
export const groupLookup = {
  search: async (query: string): Promise<LookupOption[]> => {
    const res = await api.get<{ groups: Group[] }>('/api/v1/admin/groups');
    const q = query.trim().toLowerCase();
    return res.groups
      .filter((g) => !q || g.name.toLowerCase().includes(q))
      .slice(0, 20)
      .map((g) => ({
        id: g.id,
        label: g.name,
        detail: g.from_idp ? `${g.member_count} members · from identity provider` : `${g.member_count} members`,
      }));
  },
  resolve: async (ids: string[]): Promise<LookupOption[]> => {
    if (ids.length === 0) return [];
    const res = await api.get<{ groups: Group[] }>('/api/v1/admin/groups');
    const want = new Set(ids);
    return res.groups.filter((g) => want.has(g.id)).map((g) => ({ id: g.id, label: g.name }));
  },
};
