import { useCollectionState, useRepairCollectionPage } from '../../lib/collection-state';
import { LookupInput } from '../../components/LookupInput';
import { userLookup } from '../../lib/lookups';
import { useMemo, useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api, qs } from '../../lib/api';
import { listAdminModels } from '../../api/client';
import { publicModelName, type RequestSecurity, type UsageEvent } from '../../lib/types';
import { formatBytes, formatDateTime, formatDuration, formatNumber, formatUSD, statusTone, titleCase } from '../../lib/format';
import { AsyncSection, Badge, Drawer, EmptyState, Pagination } from '../../components/ui';
import { ChargedTeam } from '../../components/ChargedTeam';
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

const MODALITIES = [
  'chat',
  'embedding',
  'image',
  'audio',
  'video',
  'tts',
  'stt',
  'function',
  'response',
  'assistant',
  'thread',
  'file',
  'moderation',
  'fine_tune',
];

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
  | 'security';

/** Every column the viewer can show. sortKey marks the server-sortable ones. */
const COLUMNS: Array<{ key: ColumnKey; label: () => string; sortKey?: string }> = [
  { key: 'time', label: () => t('tables.time'), sortKey: 'time' },
  { key: 'user', label: () => t('adminRequests.user') },
  { key: 'chargedTo', label: () => 'Charged to' },
  { key: 'model', label: () => t('tables.model') },
  { key: 'modality', label: () => t('tables.modality') },
  { key: 'status', label: () => t('tables.status') },
  { key: 'latency', label: () => t('tables.latency'), sortKey: 'latency' },
  { key: 'tokensIn', label: () => t('tables.tokensIn'), sortKey: 'tokens_in' },
  { key: 'tokensOut', label: () => t('tables.tokensOut'), sortKey: 'tokens_out' },
  { key: 'cost', label: () => t('tables.cost'), sortKey: 'cost' },
  { key: 'endpoint', label: () => t('requests.endpoint') },
  { key: 'requestId', label: () => t('requests.requestId') },
  { key: 'streaming', label: () => t('requests.streaming') },
  { key: 'ttfb', label: () => t('requests.ttfb') },
  { key: 'requestSize', label: () => t('requests.requestSize') },
  { key: 'responseSize', label: () => t('requests.responseSize') },
  { key: 'errorCode', label: () => t('requests.errorCode') },
  { key: 'clientApp', label: () => t('adminRequests.clientApp') },
  { key: 'security', label: () => t('adminRequests.securityColumn') },
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

  // Model options come from the full admin catalog (usage records the public
  // name at request time), unioned with names seen on the page and the active
  // filter value so a since-removed model stays selectable.
  const adminModels = useQuery({
    queryKey: ['admin', 'models', 'filter-options'],
    queryFn: () => listAdminModels({}),
    staleTime: 60_000,
  });
  const modelOptions = [
    ...new Set(
      [
        ...(adminModels.data?.models ?? []).map((m) => publicModelName(m)),
        ...(requests.data?.requests ?? []).map((event) => event.model),
        model,
      ].filter(Boolean),
    ),
  ].sort();

  const exportUrl = `/api/v1/requests.csv${qs(filterParams)}`;
  const hasFilters = Boolean(userFilter || principal || modality || status || model || accounting || tokensIn || tokensOut);
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
          label={t('tables.modality')}
          value={modality}
          onChange={setModality}
          options={[
            { value: '', label: t('requests.allModalities') },
            ...MODALITIES.map((value) => ({ value, label: titleCase(value) })),
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
        <FilterSelect
          label={t('tables.model')}
          value={model}
          onChange={setModel}
          options={[{ value: '', label: t('requests.allModels') }, ...modelOptions.map((name) => ({ value: name, label: name }))]}
        />
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
                            <EventCell key={column.key} column={column.key} event={event} />
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
              <section className="card">
                <h3 style={{ marginTop: 0 }}>{t('adminRequests.securityTitle')}</h3>
                <RequestSecuritySection event={selected} />
              </section>
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
function EventCell({ column, event }: { column: ColumnKey; event: UsageEvent }): ReactNode {
  switch (column) {
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
      return <td className="truncate">{event.model || '—'}</td>;
    case 'chargedTo':
      return (
        <td className="small">
          <ChargedTeam event={event} />
        </td>
      );
    case 'modality':
      return <td className="small">{titleCase(event.modality)}</td>;
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
