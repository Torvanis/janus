import { useCollectionState, useRepairCollectionPage } from '../../lib/collection-state';
import { LookupInput } from '../../components/LookupInput';
import { userLookup } from '../../lib/lookups';
import { useMemo, useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api, qs } from '../../lib/api';
import {
  type ClassificationCategory,
  type ClassificationTag,
  type ClassifierRunTrace,
  type RequestSecurity,
  type UsageEvent,
} from '../../lib/types';
import {
  formatBytes,
  formatDateTime,
  formatDuration,
  formatNumber,
  formatUSD,
  modalityLabel,
  statusTone,
  titleCase,
} from '../../lib/format';
import { AsyncSection, Badge, Drawer, EmptyState, Pagination } from '../../components/ui';
import { ChargedTeam } from '../../components/ChargedTeam';
import { reasonLabel } from '../../lib/routing';
import { RouteDecisionLine } from './Routing';
import { useLocalPreference, useUrlState, useUrlStateBatch } from '../../lib/hooks';
import { t } from '../../lib/i18n';
import {
  DetailRow,
  FilterSelect,
  RangePicker,
  RequestDetail,
  SortHeader,
  TokenSizeFilter,
  tokenSizeParams,
  type RangeKey,
} from '../shared';

const PAGE_SIZE = 50;

/** One (kind, model) pair in the current scope, with its request count. */
interface RequestFacet {
  modality: string;
  model: string;
  count: number;
}

type ColumnKey =
  | 'time'
  | 'user'
  | 'chargedTo'
  | 'model'
  | 'modality'
  | 'status'
  | 'latency'
  | 'tokensIn'
  | 'tokensOut'
  | 'cost'
  | 'endpoint'
  | 'requestId'
  | 'streaming'
  | 'ttfb'
  | 'requestSize'
  | 'responseSize'
  | 'errorCode'
  | 'clientApp'
  | 'security'
  | 'classified'
  | 'classifierTime';

/** Every column the viewer can show. sortKey marks the server-sortable ones. */
const COLUMNS: Array<{ key: ColumnKey; label: () => string; sortKey?: string }> = [
  { key: 'time', label: () => t('tables.time'), sortKey: 'time' },
  { key: 'user', label: () => t('adminRequests.user') },
  { key: 'chargedTo', label: () => 'Charged to' },
  { key: 'model', label: () => t('tables.model'), sortKey: 'model' },
  { key: 'modality', label: () => 'Kind', sortKey: 'modality' },
  { key: 'status', label: () => t('tables.status'), sortKey: 'status' },
  { key: 'latency', label: () => t('tables.latency'), sortKey: 'latency' },
  { key: 'tokensIn', label: () => t('tables.tokensIn'), sortKey: 'tokens_in' },
  { key: 'tokensOut', label: () => t('tables.tokensOut'), sortKey: 'tokens_out' },
  { key: 'cost', label: () => t('tables.cost'), sortKey: 'cost' },
  { key: 'endpoint', label: () => t('requests.endpoint') },
  { key: 'requestId', label: () => t('requests.requestId') },
  { key: 'streaming', label: () => t('requests.streaming') },
  { key: 'ttfb', label: () => t('requests.ttfb'), sortKey: 'ttfb' },
  { key: 'requestSize', label: () => t('requests.requestSize') },
  { key: 'responseSize', label: () => t('requests.responseSize') },
  { key: 'errorCode', label: () => t('requests.errorCode') },
  { key: 'clientApp', label: () => t('adminRequests.clientApp') },
  { key: 'security', label: () => t('adminRequests.securityColumn') },
  { key: 'classified', label: () => 'Classified as' },
  { key: 'classifierTime', label: () => 'Classifier wait', sortKey: 'classifier' },
];

const DEFAULT_COLUMNS: ColumnKey[] = [
  'time',
  'user',
  'chargedTo',
  'model',
  'modality',
  'status',
  'latency',
  'tokensIn',
  'tokensOut',
  'cost',
  'security',
  'classified',
];

/**
 * Column keys as persisted by earlier releases. The combined "tokens" column
 * was split into tokensIn / tokensOut; a stored preference naming the old key
 * maps onto both so nobody loses their token columns on upgrade.
 */
const LEGACY_COLUMN_KEYS: Record<string, ColumnKey[]> = { tokens: ['tokensIn', 'tokensOut'] };

/** Column choices belong to the person, not the link: persisted locally. */
const COLUMNS_PREF_KEY = 'janus.adminRequests.columns';

/**
 * Who made the request. A service-token event deliberately carries no user id
 * — that emptiness is what keeps integration traffic out of people-oriented
 * reports — so resolving only user fields renders every integration call as
 * "Unknown user". Check the service token first.
 */
function ownerLabel(event: UsageEvent): string {
  if (event.service_token_name) return event.service_token_name;
  if (event.service_token_id) return t('adminRequests.deletedServiceToken');
  return event.user_name || event.user_email || event.user_id || t('adminRequests.unknownUser');
}

/** True when the event was made by a service token rather than a person. */
function isServiceEvent(event: UsageEvent): boolean {
  return Boolean(event.service_token_id || event.service_token_name);
}

/**
 * Instance-wide request log for administrators: every user's proxied calls
 * with filtering, sorting, adjustable columns, CSV export, and a drill-down
 * drawer identifying the owning user. The cross-user scope is enforced
 * server-side (`scope=all` is honoured only for admins).
 */
export function AdminRequestsPage(): ReactNode {
  const [range] = useUrlState<RangeKey>('range', 'week');
  const [userFilter] = useUrlState('user', '');
  const [modality] = useUrlState('modality', '');
  const [status] = useUrlState('status', '');
  const [model] = useUrlState('model', '');
  // Counting method: the System page's Metering tile links here with
  // accounting=unmetered_modality to list the requests behind a gap.
  const [accounting] = useUrlState('accounting', '');
  // The gateway's own classifier calls are hidden by default: they share the
  // caller's request_id so cost stays attributable, but they are platform
  // overhead, not requests a person made, and listing them flat makes one
  // chat look like three rows.
  const [includeInternal] = useUrlState('include_internal', '');
  // Token-size filters live in the URL as `gt:<n>` / `lt:<n>` per direction
  // and are translated to the API's tokens_in_gt / tokens_out_lt parameters.
  const [tokensIn] = useUrlState('tokens_in', '');
  const [tokensOut] = useUrlState('tokens_out', '');
  // The backend already accepts principal=users|service_tokens; without a
  // control for it an admin cannot separate human traffic from integrations.
  const [principal] = useUrlState('principal', '');
  // Usage insights label: a category, category/subcategory, * (any) or -
  // (not classified).
  const [classified] = useUrlState('classified', '');
  const [sort, setSort] = useCollectionState('sort', 'time');
  const [page, setPage] = useUrlState('page', '0');
  // Filter/range changes must atomically reset the page offset, otherwise an
  // admin standing on page ≥2 who narrows the list lands on an out-of-range
  // offset and gets an empty page.
  const batchParams = useUrlStateBatch();
  const setRange = (next: RangeKey) => batchParams({ range: next === 'week' ? null : next, page: null });
  const setUserFilter = (next: string) => batchParams({ user: next, page: null });
  const setPrincipal = (next: string) => batchParams({ principal: next, page: null });
  const setModality = (next: string) => batchParams({ modality: next, page: null });
  const setStatus = (next: string) => batchParams({ status: next, page: null });
  const setModel = (next: string) => batchParams({ model: next, page: null });
  const setAccounting = (next: string) => batchParams({ accounting: next, page: null });
  const toggleInternal = () => batchParams({ include_internal: includeInternal === 'true' ? null : 'true', page: null });
  const setTokensIn = (next: string) => batchParams({ tokens_in: next, page: null });
  const setTokensOut = (next: string) => batchParams({ tokens_out: next, page: null });
  const setClassified = (next: string) => batchParams({ classified: next, page: null });
  const clearFilters = () =>
    batchParams({
      user: null,
      principal: null,
      modality: null,
      status: null,
      model: null,
      accounting: null,
      include_internal: null,
      tokens_in: null,
      tokens_out: null,
      classified: null,
      page: null,
    });
  const [selected, setSelected] = useState<UsageEvent | null>(null);
  const [pickerOpen, setPickerOpen] = useState(false);

  // Visible columns persist across visits (localStorage), defaulting to the
  // sensible core set. Stored as a comma-joined list of column keys.
  const [columnsPref, setColumnsPref] = useLocalPreference(COLUMNS_PREF_KEY, DEFAULT_COLUMNS.join(','));
  const visibleColumns = useMemo(() => {
    const chosen = new Set<ColumnKey>();
    for (const key of columnsPref.split(',').filter(Boolean)) {
      for (const mapped of LEGACY_COLUMN_KEYS[key] ?? [key as ColumnKey]) chosen.add(mapped);
    }
    return chosen.size > 0 ? chosen : new Set(DEFAULT_COLUMNS);
  }, [columnsPref]);
  const toggleColumn = (key: ColumnKey) => {
    const next = new Set(visibleColumns);
    if (next.has(key)) {
      // At least one column must remain visible.
      if (next.size === 1) return;
      next.delete(key);
    } else {
      next.add(key);
    }
    setColumnsPref(
      COLUMNS.map((column) => column.key)
        .filter((candidate) => next.has(candidate))
        .join(','),
    );
  };

  const offset = Number.parseInt(page, 10) || 0;
  const filterParams = {
    scope: 'all',
    range,
    user_id: userFilter,
    principal,
    modality,
    status,
    model,
    accounting,
    include_internal: includeInternal,
    classified,
    sort,
    ...tokenSizeParams('tokens_in', tokensIn),
    ...tokenSizeParams('tokens_out', tokensOut),
  };

  const requests = useQuery({
    queryKey: [
      'admin',
      'requests',
      range,
      userFilter,
      principal,
      modality,
      status,
      model,
      accounting,
      includeInternal,
      tokensIn,
      tokensOut,
      classified,
      sort,
      offset,
    ],
    queryFn: () =>
      api.get<{ requests: UsageEvent[]; total_count: number }>(
        `/api/v1/requests${qs({ ...filterParams, limit: PAGE_SIZE, offset })}`,
      ),
    refetchInterval: 15_000,
  });
  useRepairCollectionPage(requests.isFetching ? undefined : requests.data?.total_count, PAGE_SIZE);
  // Troubleshooting mode marks captured rows; the download column only
  // appears when this page has at least one, so the log looks unchanged for
  // instances that never enable capture.
  const hasCaptures = requests.data?.requests.some((event) => event.has_capture) ?? false;

  // Kind and model options come from what actually happened in this scope
  // (range, who, outcome...), each with its request count, ignoring the kind
  // and model filters themselves so the other choices stay visible. A chosen
  // value with no requests stays selectable so the filter can be read back.
  const facetParams = { ...filterParams, sort: undefined };
  const facets = useQuery({
    queryKey: [
      'admin',
      'requests',
      'facets',
      range,
      userFilter,
      principal,
      status,
      accounting,
      includeInternal,
      tokensIn,
      tokensOut,
      classified,
    ],
    queryFn: () => api.get<{ facets: RequestFacet[] }>(`/api/v1/requests/facets${qs(facetParams)}`),
    staleTime: 30_000,
  });
  const { kindOptions, modelGroups } = useMemo(() => {
    const rows = facets.data?.facets ?? [];
    const kinds = new Map<string, number>();
    for (const f of rows) if (!model || f.model === model) kinds.set(f.modality, (kinds.get(f.modality) ?? 0) + f.count);
    if (modality && !kinds.has(modality)) kinds.set(modality, 0);
    const byKind = new Map<string, Map<string, number>>();
    for (const f of rows) {
      if (modality && f.modality !== modality) continue;
      const m = byKind.get(f.modality) ?? new Map<string, number>();
      m.set(f.model, (m.get(f.model) ?? 0) + f.count);
      byKind.set(f.modality, m);
    }
    const groups = [...byKind.entries()]
      .map(([kind, names]) => ({
        kind,
        total: [...names.values()].reduce((x, y) => x + y, 0),
        models: [...names.entries()].sort((x, y) => y[1] - x[1] || x[0].localeCompare(y[0])),
      }))
      .sort((x, y) => y.total - x.total);
    if (model && !groups.some((g) => g.models.some(([name]) => name === model))) {
      groups.push({ kind: '', total: 0, models: [[model, 0]] });
    }
    return {
      kindOptions: [...kinds.entries()].sort((x, y) => y[1] - x[1]),
      modelGroups: groups,
    };
  }, [facets.data, modality, model]);
  const withCount = (label: string, n: number) => `${label} (${n.toLocaleString()})`;

  // Categories for the classification filter; empty (and the filter
  // hidden) when Usage insights has never been set up.
  const categories = useQuery({
    queryKey: ['classification', 'categories'],
    queryFn: () => api.get<{ categories: ClassificationCategory[] }>('/api/v1/admin/classification/categories'),
    staleTime: 60_000,
    retry: false,
  });
  const categoryName = useMemo(() => {
    const names = new Map(
      (categories.data?.categories ?? []).map((c) => [c.parent_slug ? `${c.parent_slug}/${c.slug}` : c.slug, c.name]),
    );
    return (slug: string) => names.get(slug) ?? slug.replace(/_/g, ' ');
  }, [categories.data]);
  const classifiedOptions = useMemo(() => {
    const cats = (categories.data?.categories ?? []).filter((c) => c.enabled);
    const roots = cats.filter((c) => !c.parent_slug);
    const out = [
      { value: '', label: 'Any classification' },
      { value: '*', label: 'Classified' },
      { value: '-', label: 'Not classified' },
    ];
    for (const r of roots) {
      out.push({ value: r.slug, label: r.name });
      for (const c of cats.filter((x) => x.parent_slug === r.slug))
        out.push({ value: `${r.slug}/${c.slug}`, label: `${r.name} › ${c.name}` });
    }
    return out;
  }, [categories.data]);
  const insightsConfigured = (categories.data?.categories?.length ?? 0) > 0;

  const exportUrl = `/api/v1/requests.csv${qs(filterParams)}`;
  const hasFilters = Boolean(
    userFilter || principal || modality || status || model || accounting || tokensIn || tokensOut || classified,
  );
  const shownColumns = COLUMNS.filter((column) => visibleColumns.has(column.key));

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminRequests.title')}</h1>
          <p className="page-subtitle">{t('adminRequests.subtitle')}</p>
        </div>
        <div className="row wrap">
          <RangePicker value={range} onChange={setRange} />
          <div style={{ position: 'relative' }}>
            <button
              type="button"
              className="btn btn-sm"
              aria-haspopup="true"
              aria-expanded={pickerOpen}
              onClick={() => setPickerOpen((open) => !open)}
            >
              {t('adminRequests.columns')}
            </button>
            {pickerOpen ? (
              <div
                className="menu"
                role="group"
                aria-label={t('adminRequests.columnPicker')}
                style={{ position: 'absolute', right: 0, zIndex: 30, minWidth: 220, padding: 'var(--janus-space-3)' }}
              >
                {COLUMNS.map((column) => (
                  <label key={column.key} className="row" style={{ gap: 8, padding: '2px 4px' }}>
                    <input type="checkbox" checked={visibleColumns.has(column.key)} onChange={() => toggleColumn(column.key)} />
                    <span className="small">{column.label()}</span>
                  </label>
                ))}
              </div>
            ) : null}
          </div>
          <a className="btn btn-sm" href={exportUrl} download>
            {t('requests.exportCsv')}
          </a>
        </div>
      </header>

      <div className="row wrap">
        <FilterSelect
          label={t('adminRequests.principal')}
          value={principal}
          onChange={setPrincipal}
          options={[
            { value: '', label: t('adminRequests.allPrincipals') },
            { value: 'users', label: t('adminRequests.peopleOnly') },
            { value: 'service_tokens', label: t('adminRequests.servicesOnly') },
          ]}
        />
        <LookupInput
          label={t('adminRequests.user')}
          value={userFilter ? [userFilter] : []}
          onChange={(ids) => setUserFilter(ids.at(-1) ?? '')}
          {...userLookup}
        />
        <FilterSelect
          label="Kind"
          value={modality}
          onChange={setModality}
          options={[
            { value: '', label: 'All kinds' },
            ...kindOptions.map(([value, n]) => ({ value, label: withCount(modalityLabel(value), n) })),
          ]}
        />
        <FilterSelect
          label={t('tables.status')}
          value={status}
          onChange={setStatus}
          options={[
            { value: '', label: t('requests.allOutcomes') },
            { value: 'success', label: t('requests.succeeded') },
            { value: 'error', label: t('requests.failed') },
          ]}
        />
        <label className="row" style={{ gap: 6 }}>
          <span className="sr-only">{t('tables.model')}</span>
          <select className="select" aria-label={t('tables.model')} value={model} onChange={(e) => setModel(e.target.value)}>
            <option value="">{t('requests.allModels')}</option>
            {modelGroups.map((g) => (
              <optgroup
                key={g.kind || 'other'}
                label={g.kind ? withCount(modalityLabel(g.kind), g.total) : 'No requests in this view'}
              >
                {g.models.map(([name, n]) => (
                  <option key={`${g.kind}/${name}`} value={name}>
                    {withCount(name || '(no model)', n)}
                  </option>
                ))}
              </optgroup>
            ))}
          </select>
        </label>
        <FilterSelect
          label={t('requests.countingMethod')}
          value={accounting}
          onChange={setAccounting}
          options={[
            { value: '', label: t('requests.allCountingMethods') },
            { value: 'upstream_reported', label: t('requests.reportedByUpstream') },
            { value: 'upstream_reported_cost', label: t('requests.costReportedByUpstream') },
            { value: 'byte_count_fallback', label: t('requests.estimatedFromBytes') },
            { value: 'unmetered_modality', label: t('requests.unmeteredModality') },
            { value: 'not_billable', label: t('requests.notBillable') },
          ]}
        />
        {insightsConfigured || classified ? (
          <FilterSelect label="Classified as" value={classified} onChange={setClassified} options={classifiedOptions} />
        ) : null}
        <TokenSizeFilter direction="in" value={tokensIn} onChange={setTokensIn} />
        <TokenSizeFilter direction="out" value={tokensOut} onChange={setTokensOut} />
        <button
          type="button"
          className="btn btn-ghost btn-sm"
          aria-pressed={includeInternal === 'true'}
          title={t('adminRequests.showInternalHint')}
          onClick={toggleInternal}
        >
          {includeInternal === 'true' ? t('adminRequests.hideInternal') : t('adminRequests.showInternal')}
        </button>
        {hasFilters ? (
          <button type="button" className="btn btn-ghost btn-sm" onClick={clearFilters}>
            {t('tables.clearFilters')}
          </button>
        ) : null}
      </div>

      <section className="card card-flush">
        <AsyncSection
          query={requests}
          empty={{
            when: (data) => data.total_count === 0 && !hasFilters,
            title: t('adminRequests.emptyTitle'),
            body: t('adminRequests.emptyBody'),
          }}
        >
          {(data) =>
            data.requests.length === 0 ? (
              <EmptyState
                title={t('adminRequests.noMatchTitle')}
                body={t('adminRequests.noMatchBody')}
                action={
                  <button type="button" className="btn" onClick={clearFilters}>
                    {t('tables.clearFilters')}
                  </button>
                }
              />
            ) : (
              <>
                <div className="table-wrap" style={{ maxHeight: '64dvh', overflowY: 'auto' }}>
                  <table className="data">
                    <thead>
                      <tr>
                        {shownColumns.map((column) =>
                          column.sortKey ? (
                            <SortHeader
                              key={column.key}
                              label={column.label()}
                              sortKey={column.sortKey}
                              active={sort}
                              onSort={setSort}
                            />
                          ) : (
                            <th key={column.key} scope="col">
                              {column.label()}
                            </th>
                          ),
                        )}
                        {hasCaptures ? <th scope="col">{t('adminRequests.captureColumn')}</th> : null}
                      </tr>
                    </thead>
                    <tbody>
                      {data.requests.map((event) => (
                        <tr
                          key={event.id}
                          data-clickable="true"
                          onClick={() => setSelected(event)}
                          tabIndex={0}
                          onKeyDown={(keyEvent) => {
                            if (keyEvent.key === 'Enter') setSelected(event);
                          }}
                        >
                          {shownColumns.map((column) => (
                            <EventCell key={column.key} column={column.key} event={event} categoryName={categoryName} />
                          ))}
                          {hasCaptures ? (
                            <td className="small">
                              {event.has_capture ? (
                                <a
                                  className="btn btn-ghost btn-sm"
                                  href={captureDownloadUrl(event)}
                                  download
                                  onClick={(clickEvent) => clickEvent.stopPropagation()}
                                  aria-label={t('adminRequests.downloadCaptureFor', { id: event.request_id || event.id })}
                                >
                                  {t('adminRequests.downloadCapture')}
                                </a>
                              ) : (
                                <span className="muted">—</span>
                              )}
                            </td>
                          ) : null}
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                <Pagination
                  offset={offset}
                  limit={PAGE_SIZE}
                  total={data.total_count}
                  onChange={(next) => setPage(String(next))}
                />
              </>
            )
          }
        </AsyncSection>
      </section>

      <Drawer open={Boolean(selected)} onClose={() => setSelected(null)} title={t('requests.detailTitle')}>
        {selected ? (
          <RequestDetail
            event={selected}
            actions={
              selected.has_capture ? (
                <a className="btn btn-primary btn-sm" href={captureDownloadUrl(selected)} download>
                  {t('adminRequests.downloadForAnalysis')}
                </a>
              ) : null
            }
            extra={
              <>
                <section className="card">
                  <h3 style={{ marginTop: 0 }}>{t('adminRequests.securityTitle')}</h3>
                  <RequestSecuritySection event={selected} />
                </section>
                <section className="card">
                  <h3 style={{ marginTop: 0 }}>Classification</h3>
                  <RequestClassificationSection event={selected} categoryName={categoryName} />
                </section>
                {selected.has_capture ? (
                  <section className="card">
                    <h3 style={{ marginTop: 0 }}>Classifier calls (troubleshooting capture)</h3>
                    <CapturedClassifierRuns event={selected} />
                  </section>
                ) : null}
              </>
            }
            owner={
              <DetailRow label={t('adminRequests.user')}>
                <span>{ownerLabel(selected)}</span>
                {selected.user_email && selected.user_email !== ownerLabel(selected) ? (
                  <span className="small muted"> · {selected.user_email}</span>
                ) : null}
                {selected.user_id ? (
                  <>
                    {' '}
                    <Link to={`/admin/users/${selected.user_id}`}>{t('adminRequests.viewInPeople')}</Link>
                  </>
                ) : null}
              </DetailRow>
            }
          />
        ) : null}
      </Drawer>
    </div>
  );
}

/** tar.gz of one captured request (troubleshooting mode). */
function captureDownloadUrl(event: UsageEvent): string {
  return `/api/v1/admin/requests/${encodeURIComponent(event.id)}/download`;
}

/**
 * Whether the security gateway inspected this request, in one glance.
 *
 * "checked" is the load-bearing state: a request a policy covered and found
 * nothing has no violation row anywhere, so without it a clean request and
 * an uninspected one look identical. The detail drawer breaks down which
 * classifiers ran and what they said.
 */
function SecurityCell({ event }: { event: UsageEvent }): ReactNode {
  const action = event.secgw_action ?? '';
  const count = event.secgw_violations ?? 0;
  if (!action) return <span className="muted">—</span>;
  if (action === 'blocked') return <Badge tone="danger">{t('adminRequests.securityBlocked')}</Badge>;
  if (action === 'stream_cut') return <Badge tone="danger">{t('adminRequests.securityStreamCut')}</Badge>;
  if (action === 'redacted') return <Badge tone="warning">{t('adminRequests.securityRedacted')}</Badge>;
  if (count > 0) return <Badge tone="warning">{t('adminRequests.securityFindings', { count })}</Badge>;
  return <Badge tone="success">{t('adminRequests.securityChecked')}</Badge>;
}

/**
 * The checks that ran on ONE request. Classifier calls are usage events of
 * their own (so their cost stays attributable) which made a single chat look
 * like several rows in the log; they belong here, attached to the request
 * that caused them, with what they concluded.
 */
function RequestSecuritySection({ event }: { event: UsageEvent }): ReactNode {
  const security = useQuery({
    queryKey: ['request-security', event.id],
    queryFn: () => api.get<RequestSecurity>(`/api/v1/admin/requests/${event.id}/security`),
    enabled: Boolean(event.secgw_action),
  });
  if (!event.secgw_action) {
    return (
      <p className="small muted" style={{ margin: 0 }}>
        {t('adminRequests.securityNotChecked')}
      </p>
    );
  }
  return (
    <AsyncSection query={security}>
      {(data) => {
        const runs = data.classifier_runs ?? [];
        const other = data.other_violations ?? [];
        if (runs.length === 0 && other.length === 0 && data.violations_scope_available && !data.violations_has_more) {
          return (
            <p className="small muted" style={{ margin: 0 }}>
              {t('adminRequests.securityNoClassifiers')}
            </p>
          );
        }
        return (
          <div className="stack" style={{ gap: 8 }}>
            {runs.map((run, i) => (
              <div key={`${run.model_name}-${i}`} className="small">
                <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                  <strong>{run.model_name}</strong>
                  {run.http_status >= 400 ? (
                    <Badge tone="danger">{t('adminRequests.securityRunFailed', { error: run.http_status })}</Badge>
                  ) : run.findings.length === 0 && data.violations_has_more ? (
                    <Badge tone="warning">No findings in bounded preview</Badge>
                  ) : run.findings.length === 0 ? (
                    <Badge tone="success">{t('adminRequests.securityRunClean')}</Badge>
                  ) : (
                    <Badge tone="warning">{t('adminRequests.securityFindings', { count: run.findings.length })}</Badge>
                  )}
                  <span className="muted">{formatDuration(run.latency_ms)}</span>
                </div>
                {run.findings.map((f) => (
                  <div key={f.id} className="muted" style={{ marginLeft: 12 }}>
                    {titleCase(f.kind.replace(/_/g, ' '))} · {f.rule_id}
                    {f.classifier_score ? ` · ${f.classifier_score.toFixed(3)}` : ''} · {f.action}
                  </div>
                ))}
              </div>
            ))}
            <p className="small muted" role="status">
              {data.violations_scope_available === false
                ? 'Request violation scope unavailable: this event has no request ID.'
                : data.violations_has_more
                  ? `${other.length} non-classifier findings among the newest ${data.violations_scanned} request violations (limit ${data.violations_limit}). Additional request violations are omitted; this is not a complete findings list.`
                  : data.violations_scope_available
                    ? `${other.length} non-classifier findings from all ${data.violations_scanned} request violations.`
                    : 'Request violation preview; completeness unavailable.'}
            </p>
            {other.map((v) => (
              <div key={v.id} className="small">
                <strong>{titleCase(v.kind.replace(/_/g, ' '))}</strong>{' '}
                <span className="muted">
                  {v.rule_id} · {v.action}
                </span>
              </div>
            ))}
            <Link className="small" to="/admin/security/violations">
              {t('adminRequests.securityViewAll')}
            </Link>
          </div>
        );
      }}
    </AsyncSection>
  );
}
/** "Coding › Rust" for a classification tag. */
function tagLabel(tag: ClassificationTag, categoryName: (slug: string) => string): string {
  const cat = categoryName(tag.category);
  return tag.subcategory ? `${cat} › ${categoryName(`${tag.category}/${tag.subcategory}`)}` : cat;
}

const FEATURE_LABELS: Record<string, string> = {
  prompt_injection: 'Prompt injection',
  content_safety: 'Content safety',
  usage_insights: 'Usage insights',
};

function featureLabel(feature: string): string {
  return feature
    .split('+')
    .map((f) => FEATURE_LABELS[f] ?? titleCase(f.replace(/_/g, ' ')))
    .join(' + ');
}

/**
 * How Usage insights labelled the request, and what classification cost
 * it: the wait before the request was forwarded (guards and an inline
 * label) versus work done in the background afterwards.
 */
function RequestClassificationSection({
  event,
  categoryName,
}: {
  event: UsageEvent;
  categoryName: (slug: string) => string;
}): ReactNode {
  const tag = event.classification_tag;
  const waited = event.classifier_ms ?? 0;
  const calls = event.classifier_calls ?? 0;
  return (
    <div className="stack" style={{ gap: 8 }}>
      {event.route_reason ? (
        <DetailRow label="Auto-routing">
          <span className="stack" style={{ gap: 2 }}>
            <span>
              {event.requested_model_name ?? ''} → {event.model} · {reasonLabel(event.route_reason)}
            </span>
            {event.request_id ? <RouteDecisionLine requestId={event.request_id} /> : null}
          </span>
        </DetailRow>
      ) : null}
      <DetailRow label="Classified as">
        {tag && tag.status === 'ok' ? (
          <span className="row wrap" style={{ gap: 'var(--janus-space-2)' }}>
            <span>{tagLabel(tag, categoryName)}</span>
            <span className="small muted">{Math.round(tag.confidence * 100)}% sure</span>
            <Badge tone={tag.mode === 'inline' ? 'info' : 'neutral'}>{tag.mode === 'inline' ? 'Inline' : 'Background'}</Badge>
          </span>
        ) : tag ? (
          <Badge tone="warning">{tag.status === 'dropped' ? 'Skipped (queue full)' : `Failed (${tag.status})`}</Badge>
        ) : (
          <span className="muted">Not classified</span>
        )}
      </DetailRow>
      <DetailRow label="Waited on classifiers">
        {waited > 0 || calls > 0 ? (
          <span>
            {formatDuration(waited)}
            <span className="small muted">
              {' '}
              · {calls} classifier {calls === 1 ? 'call' : 'calls'} before forwarding
            </span>
          </span>
        ) : (
          <span className="muted">None</span>
        )}
      </DetailRow>
      {tag && tag.mode !== 'inline' ? (
        <DetailRow label="Background labelling">
          <span>
            {formatDuration(tag.latency_ms)}
            <span className="small muted">
              {' '}
              after {formatDuration(tag.queue_ms)} in the queue · {tag.calls} {tag.calls === 1 ? 'call' : 'calls'} · did not delay
              the request
            </span>
          </span>
        </DetailRow>
      ) : null}
      {tag ? (
        <DetailRow label="Classifier">
          <span className="small">{tag.classifier}</span>
        </DetailRow>
      ) : null}
    </div>
  );
}

/** Every classifier call troubleshooting mode captured for the request. */
function CapturedClassifierRuns({ event }: { event: UsageEvent }): ReactNode {
  const runs = useQuery({
    queryKey: ['capture-classifiers', event.id],
    queryFn: () =>
      api.get<{ classifier_runs: ClassifierRunTrace[] }>(
        `/api/v1/admin/troubleshooting/captures/${encodeURIComponent(event.id)}/classifiers`,
      ),
  });
  return (
    <AsyncSection query={runs}>
      {(data) =>
        (data.classifier_runs ?? []).length === 0 ? (
          <p className="small muted" style={{ margin: 0 }}>
            No classifier ran for this request.
          </p>
        ) : (
          <div className="stack" style={{ gap: 'var(--janus-space-3)' }}>
            {(data.classifier_runs ?? []).map((run, i) => (
              <details key={`${run.started_at}-${i}`} className="small">
                <summary className="row wrap" style={{ gap: 8, cursor: 'pointer' }}>
                  <strong>{featureLabel(run.feature)}</strong>
                  <span className="muted">{run.classifier}</span>
                  <Badge tone={run.status === 'ok' ? 'success' : 'danger'}>{run.status}</Badge>
                  <span className="muted">
                    {formatDuration(run.latency_ms)} · {run.mode === 'inline' ? 'inline' : 'background'}
                  </span>
                </summary>
                <div className="stack" style={{ gap: 6, marginTop: 6 }}>
                  {run.error ? <div className="banner banner-danger">{run.error}</div> : null}
                  {run.questions?.length ? <div className="muted">Questions: {run.questions.join(', ')}</div> : null}
                  {run.request ? (
                    <div className="code-block">
                      <pre style={{ maxHeight: 220, overflow: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
                        {JSON.stringify(run.request, null, 2)}
                      </pre>
                    </div>
                  ) : (
                    <div className="muted">
                      The session does not capture request bodies, so questions and answers were not kept.
                    </div>
                  )}
                  {run.response ? (
                    <div className="code-block">
                      <pre style={{ maxHeight: 220, overflow: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
                        {JSON.stringify(run.response, null, 2)}
                      </pre>
                    </div>
                  ) : null}
                </div>
              </details>
            ))}
          </div>
        )
      }
    </AsyncSection>
  );
}

function EventCell({
  column,
  event,
  categoryName,
}: {
  column: ColumnKey;
  event: UsageEvent;
  categoryName: (slug: string) => string;
}): ReactNode {
  switch (column) {
    case 'classified': {
      const tag = event.classification_tag;
      if (!tag) return <td className="small muted">—</td>;
      if (tag.status !== 'ok') return <td className="small muted">{tag.status === 'dropped' ? 'Skipped' : 'Failed'}</td>;
      return (
        <td
          className="small truncate"
          title={`${Math.round(tag.confidence * 100)}% · ${tag.mode === 'inline' ? 'inline' : 'background'}`}
        >
          {tagLabel(tag, categoryName)}
        </td>
      );
    }
    case 'classifierTime':
      return (
        <td className="num small">
          {event.classifier_calls ? formatDuration(event.classifier_ms ?? 0) : <span className="muted">—</span>}
        </td>
      );
    case 'time':
      return <td className="small muted">{formatDateTime(event.created_at)}</td>;
    case 'user':
      return (
        <td className="truncate" title={event.user_email || event.service_token_name || undefined}>
          {ownerLabel(event)}
          {isServiceEvent(event) ? (
            <>
              {' '}
              <Badge tone="neutral">{t('adminRequests.serviceBadge')}</Badge>
            </>
          ) : null}
        </td>
      );
    case 'clientApp':
      // Empty is the common case: most SDKs send no attribution header at
      // all, so an em dash is the honest answer rather than a guess.
      return (
        <td className="truncate small muted" title={event.client_user_agent || undefined}>
          {event.client_app || '—'}
        </td>
      );
    case 'model':
      return (
        <td
          className="truncate"
          title={event.route_reason ? `${event.requested_model_name ?? ''} · ${reasonLabel(event.route_reason)}` : undefined}
        >
          {event.model || '—'}
          {event.route_reason ? <span className="small muted"> ⇶</span> : null}
        </td>
      );
    case 'chargedTo':
      return (
        <td className="small">
          <ChargedTeam event={event} />
        </td>
      );
    case 'modality':
      return <td className="small">{modalityLabel(event.modality)}</td>;
    case 'security':
      return (
        <td className="small">
          <SecurityCell event={event} />
        </td>
      );
    case 'status':
      return (
        <td>
          <Badge tone={statusTone(event.http_status)}>
            {event.http_status}
            {event.streaming ? ` · ${t('requests.stream')}` : ''}
          </Badge>
        </td>
      );
    case 'latency':
      return <td className="num small">{formatDuration(event.latency_ms)}</td>;
    case 'tokensIn':
      return <td className="num small">{formatNumber(event.tokens_in)}</td>;
    case 'tokensOut':
      return <td className="num small">{formatNumber(event.tokens_out)}</td>;
    case 'cost':
      // A zero here can mean "free" or "not metered"; the two must not look
      // alike, so estimated and unmetered rows carry a marker inline.
      return (
        <td className="num small">
          {formatUSD(event.cost_nanousd)}
          {event.token_accounting_method === 'unmetered_modality' ? (
            <span title={t('requests.unmeteredModalityHint')} style={{ marginLeft: 'var(--janus-space-2)' }}>
              <Badge tone="danger">{t('requests.notMeteredShort')}</Badge>
            </span>
          ) : event.token_accounting_method === 'byte_count_fallback' ? (
            <span title={t('requests.estimatedFromBytes')} style={{ marginLeft: 'var(--janus-space-2)' }}>
              <Badge tone="warning">{t('requests.estimatedShort')}</Badge>
            </span>
          ) : null}
        </td>
      );
    case 'endpoint':
      return (
        <td className="small truncate">
          <span className="mono">{event.endpoint_path || '—'}</span>
        </td>
      );
    case 'requestId':
      return (
        <td className="small truncate">
          <span className="mono">{event.request_id || '—'}</span>
        </td>
      );
    case 'streaming':
      return <td className="small">{event.streaming ? t('tables.yes') : t('tables.no')}</td>;
    case 'ttfb':
      return <td className="num small">{formatDuration(event.ttfb_ms)}</td>;
    case 'requestSize':
      return <td className="num small">{formatBytes(event.request_bytes)}</td>;
    case 'responseSize':
      return <td className="num small">{formatBytes(event.response_bytes)}</td>;
    case 'errorCode':
      return <td className="small">{event.error_code ? <span className="mono">{event.error_code}</span> : '—'}</td>;
    default:
      return <td>—</td>;
  }
}
