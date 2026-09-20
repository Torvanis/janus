import { sortCollection } from '../lib/collections';
import { useMemo, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import type { TimePoint } from '../lib/types';
import { seriesColor, type AreaSeries } from '../components/charts';
import { t } from '../lib/i18n';
import type { UsageEvent } from '../lib/types';
import { formatBytes, formatDateTime, formatDuration, formatNumber, formatUSD, statusTone, titleCase } from '../lib/format';
import { Badge } from '../components/ui';
import { ChargedTeam } from '../components/ChargedTeam';
import { useLocalOnly } from '../app/session';
import { useUrlState } from '../lib/hooks';

export type RangeKey = 'day' | 'week' | 'month' | 'quarter';

/** Which token quantity a token time-series chart plots. */
export type TokenDirection = 'in' | 'out' | 'total';

const TOKEN_DIRECTIONS: Array<{ key: TokenDirection; label: () => string }> = [
  { key: 'in', label: () => t('shared.tokenDirectionIn') },
  { key: 'out', label: () => t('shared.tokenDirectionOut') },
  { key: 'total', label: () => t('shared.tokenDirectionTotal') },
];

/**
 * Builds the AreaChart input for a token time-series chart in the given
 * direction mode. 'total' splits the series into two stacked layers —
 * tokens-in at the baseline, tokens-out stacked on top, one value per bucket —
 * so the stack's upper boundary (and the chart's peak/total footer) equals the
 * combined in + out totals, matching what the single-series tokens chart
 * rendered before the split. 'in' and 'out' plot only that direction, with a
 * footer that reflects the selected quantity alone.
 */
export function tokenDirectionSeries(series: TimePoint[], direction: TokenDirection = 'total'): AreaSeries[] {
  const tokensIn: AreaSeries = {
    key: 'tokens_in',
    label: t('explore.tokensIn'),
    color: seriesColor(0),
    values: series.map((point) => point.totals.tokens_in),
  };
  const tokensOut: AreaSeries = {
    key: 'tokens_out',
    label: t('explore.tokensOut'),
    color: seriesColor(1),
    values: series.map((point) => point.totals.tokens_out),
  };
  switch (direction) {
    case 'in':
      return [tokensIn];
    case 'out':
      return [tokensOut];
    case 'total':
      return [tokensIn, tokensOut];
  }
}

/**
 * Segmented In / Out / Total control for token time-series charts. Rendered
 * only while the Tokens metric is active; its value lives in the URL (like the
 * metric segmented control) so the view survives a reload and is shareable.
 */
export function TokenDirectionPicker({
  value,
  onChange,
}: {
  value: TokenDirection;
  onChange: (next: TokenDirection) => void;
}): ReactNode {
  return (
    <div className="segmented" role="group" aria-label={t('shared.tokenDirection')}>
      {TOKEN_DIRECTIONS.map((direction) => (
        <button key={direction.key} type="button" aria-pressed={value === direction.key} onClick={() => onChange(direction.key)}>
          {direction.label()}
        </button>
      ))}
    </div>
  );
}

const RANGES: Array<{ key: RangeKey; label: () => string }> = [
  { key: 'day', label: () => t('shared.rangeDay') },
  { key: 'week', label: () => t('shared.rangeWeek') },
  { key: 'month', label: () => t('shared.rangeMonth') },
  { key: 'quarter', label: () => t('shared.rangeQuarter') },
];

/** Range selector whose value lives in the URL so a view is shareable. */
export function RangePicker({ value, onChange }: { value: RangeKey; onChange: (next: RangeKey) => void }): ReactNode {
  return (
    <div className="segmented" role="group" aria-label={t('shared.timeRange')}>
      {RANGES.map((range) => (
        <button key={range.key} type="button" aria-pressed={value === range.key} onClick={() => onChange(range.key)}>
          {range.label()}
        </button>
      ))}
    </div>
  );
}

/** Search input with a consistent label and clear affordance. */
export function SearchInput({
  value,
  onChange,
  placeholder,
  label = t('common.search'),
}: {
  value: string;
  onChange: (next: string) => void;
  placeholder: string;
  label?: string;
}): ReactNode {
  return (
    <div className="row" style={{ gap: 4 }}>
      <input
        className="input"
        type="search"
        value={value}
        placeholder={placeholder}
        aria-label={label}
        onChange={(event) => onChange(event.target.value)}
        style={{ minWidth: 180 }}
      />
      {value ? (
        <button type="button" className="btn btn-ghost btn-sm" onClick={() => onChange('')} aria-label={t('shared.clearSearch')}>
          ✕
        </button>
      ) : null}
    </div>
  );
}

/** Labelled select used for every list filter. */
export function FilterSelect({
  label,
  value,
  onChange,
  options,
}: {
  label: string;
  value: string;
  onChange: (next: string) => void;
  options: Array<{ value: string; label: string }>;
}): ReactNode {
  return (
    <label className="row" style={{ gap: 6 }}>
      <span className="sr-only">{label}</span>
      <select className="select" value={value} onChange={(event) => onChange(event.target.value)} aria-label={label}>
        {options.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
    </label>
  );
}

/** Token-count thresholds offered by the request-log size filters. */
export const TOKEN_SIZE_THRESHOLDS = [1_000, 10_000, 100_000, 500_000] as const;

/**
 * Translates one token-size filter value into the API's strict-comparison
 * query parameters (`<prefix>_gt` / `<prefix>_lt`). Returns an object with
 * both keys so callers can spread it into `qs()` unconditionally — empty
 * values are dropped by `qs`.
 */
export function tokenSizeParams(prefix: 'tokens_in' | 'tokens_out', value: string): Record<string, string> {
  const match = /^(gt|lt):(\d+)$/.exec(value);
  const op = match?.[1] ?? '';
  const threshold = match?.[2] ?? '';
  return {
    [`${prefix}_gt`]: op === 'gt' ? threshold : '',
    [`${prefix}_lt`]: op === 'lt' ? threshold : '',
  };
}

/** Options for a token-size filter, in ascending threshold order. */
export function tokenSizeOptions(anyLabel: string): Array<{ value: string; label: string }> {
  const options: Array<{ value: string; label: string }> = [{ value: '', label: anyLabel }];
  for (const threshold of TOKEN_SIZE_THRESHOLDS) {
    const count = formatNumber(threshold);
    options.push({ value: `lt:${threshold}`, label: t('shared.tokensSmallerThan', { count }) });
    options.push({ value: `gt:${threshold}`, label: t('shared.tokensGreaterThan', { count }) });
  }
  return options;
}

/**
 * Labelled select that narrows a request list to rows whose token count in
 * one direction is greater or smaller than a fixed threshold. Both request
 * pages and the admin user drawer render one per direction so "show me the
 * big prompts" and "show me the long completions" are each a single pick.
 */
export function TokenSizeFilter({
  direction,
  value,
  onChange,
}: {
  direction: 'in' | 'out';
  value: string;
  onChange: (next: string) => void;
}): ReactNode {
  const label = direction === 'in' ? t('tables.tokensIn') : t('tables.tokensOut');
  const anyLabel = direction === 'in' ? t('shared.anyTokensIn') : t('shared.anyTokensOut');
  return <FilterSelect label={label} value={value} onChange={onChange} options={tokenSizeOptions(anyLabel)} />;
}

/**
 * Ascending variant of each sort key. "time" keeps the backend's historical
 * "oldest" name; everything else follows the `<key>_asc` convention that both
 * the requests API and the client-side token sorter understand.
 */
export function ascSortKey(sortKey: string): string {
  return sortKey === 'time' ? 'oldest' : `${sortKey}_asc`;
}

/**
 * Sortable column header used across every table. Clicking an inactive column
 * sorts by it descending; clicking the active column toggles the direction,
 * and aria-sort reflects the real state.
 */
export function SortHeader({
  label,
  sortKey,
  active,
  onSort,
}: {
  label: string;
  sortKey: string;
  active: string;
  onSort: (key: string) => void;
}): ReactNode {
  const asc = ascSortKey(sortKey);
  const isDesc = active === sortKey;
  const isAsc = active === asc;
  return (
    <th scope="col" aria-sort={isDesc ? 'descending' : isAsc ? 'ascending' : 'none'}>
      <button type="button" onClick={() => onSort(isDesc ? asc : sortKey)}>
        {label}
        <span aria-hidden="true" style={{ opacity: isDesc || isAsc ? 1 : 0.3 }}>
          {isAsc ? '↑' : '↓'}
        </span>
      </button>
    </th>
  );
}

/**
 * Client-side sorting for a table whose rows are already fully loaded.
 *
 * Sort state lives in the URL (`?sort=<key>` / `?sort=<key>_asc`, the same
 * convention SortHeader speaks), so a sorted view is a shareable, refresh-safe
 * link rather than component state that evaporates on navigation.
 *
 * Pass a `columns` map from sort key to a value accessor; strings compare
 * with localeCompare, everything else numerically. Use this for lists the API
 * returns whole (upstreams, quotas, rules, alerts). Tables that paginate
 * server-side must sort server-side instead, or they would only ever reorder
 * the page you happen to be looking at.
 */
export function useTableSort<T>(
  rows: readonly T[],
  columns: Record<string, (row: T) => string | number | null | undefined>,
  defaultKey: string,
  paramName = 'sort',
): { sorted: T[]; sort: string; setSort: (key: string) => void } {
  const [sort, setSort] = useUrlState(paramName, defaultKey);
  const sorted = useMemo(() => {
    const ascending = sort.endsWith('_asc');
    const key = sort.replace(/_asc$/, '');
    const accessor = columns[key] ?? columns[defaultKey.replace(/_asc$/, '')];
    if (!accessor) return [...rows];
    return sortCollection(rows, accessor, ascending);
  }, [rows, columns, sort, defaultKey]);
  return { sorted, sort, setSort };
}

/** Small labelled value used inside detail drawers. */
export function DetailRow({ label, children }: { label: string; children: ReactNode }): ReactNode {
  return (
    <div className="row-between" style={{ alignItems: 'flex-start', gap: 'var(--janus-space-4)' }}>
      <span className="small muted" style={{ flex: '0 0 40%' }}>
        {label}
      </span>
      <span className="small" style={{ flex: 1, textAlign: 'right', wordBreak: 'break-word' }}>
        {children}
      </span>
    </div>
  );
}

/**
 * Prominent, permanent caveat for the policy-rule surfaces. Blocking rules read
 * client-supplied signals; every one except the bearer token can be forged.
 */
export function PolicyCaveat(): ReactNode {
  return (
    <div className="banner banner-warning" role="note">
      <div>
        <strong>{t('shared.policyCaveatTitle')}</strong>
        <div className="small" style={{ marginTop: 4 }}>
          {t('shared.policyCaveatBody')}
        </div>
      </div>
    </div>
  );
}

/**
 * Full request-detail drawer body shared by the user-scoped Requests page and
 * the admin instance-wide log viewer. `owner` lets the admin variant prepend
 * the owning user's identity row; the user page omits it.
 */
/**
 * AccountingBadge names where a request's metering figures came from. The
 * distinction matters most for the two zero-ish cases: an unmetered media
 * response (a configuration gap the admin should fix) must never read like a
 * free request or like an error response.
 */
function AccountingBadge({ method }: { method: string }): ReactNode {
  switch (method) {
    case 'byte_count_fallback':
      return <Badge tone="warning">{t('requests.estimatedFromBytes')}</Badge>;
    case 'upstream_reported_cost':
      return <Badge tone="success">{t('requests.costReportedByUpstream')}</Badge>;
    case 'unmetered_modality':
      return (
        <span title={t('requests.unmeteredModalityHint')}>
          <Badge tone="danger">{t('requests.unmeteredModality')}</Badge>
        </span>
      );
    case 'not_billable':
      return <Badge tone="neutral">{t('requests.notBillable')}</Badge>;
    default:
      return <Badge tone="success">{t('requests.reportedByUpstream')}</Badge>;
  }
}

export function RequestDetail({
  event,
  owner,
  actions,
  extra,
}: {
  event: UsageEvent;
  owner?: ReactNode;
  /** Optional controls rendered above the detail rows (e.g. a capture download). */
  actions?: ReactNode;
  /**
   * Optional sections appended after the detail rows. The admin log uses it
   * for the security-gateway breakdown; the user-facing log, which has no
   * business seeing classifier internals, simply omits it.
   */
  extra?: ReactNode;
}): ReactNode {
  const localOnly = useLocalOnly();
  return (
    <>
      {actions ? <div className="row wrap">{actions}</div> : null}
      <div className="stack">
        {owner}
        <DetailRow label={t('tables.time')}>{formatDateTime(event.created_at)}</DetailRow>
        <DetailRow label={t('tables.model')}>{event.model || '—'}</DetailRow>
        <DetailRow label="Charged to">
          <ChargedTeam event={event} />
        </DetailRow>
        <DetailRow label={t('requests.endpoint')}>
          <span className="mono">{event.endpoint_path}</span>
        </DetailRow>
        <DetailRow label={t('requests.method')}>{event.http_method}</DetailRow>
        <DetailRow label={t('tables.modality')}>{titleCase(event.modality)}</DetailRow>
        <DetailRow label={t('tables.status')}>
          <Badge tone={statusTone(event.http_status)}>{event.http_status}</Badge>
        </DetailRow>
        {event.error_code ? (
          <DetailRow label={t('requests.errorCode')}>
            <Link to={`/docs/api/errors#${event.error_code}`} className="mono">
              {event.error_code}
            </Link>
          </DetailRow>
        ) : null}
        <DetailRow label={t('requests.streaming')}>{event.streaming ? t('tables.yes') : t('tables.no')}</DetailRow>
      </div>

      <section>
        <h3 style={{ marginBottom: 'var(--janus-space-3)' }}>{t('requests.metering')}</h3>
        <div className="stack">
          <DetailRow label={t('requests.inputTokens')}>{formatNumber(event.tokens_in)}</DetailRow>
          <DetailRow label={t('requests.outputTokens')}>{formatNumber(event.tokens_out)}</DetailRow>
          <DetailRow label={t('requests.cachedInputTokens')}>{formatNumber(event.tokens_cached)}</DetailRow>
          <DetailRow label={t('requests.cacheWrite5mTokens')}>{formatNumber(event.tokens_cache_write_5m)}</DetailRow>
          <DetailRow label={t('requests.cacheWrite1hTokens')}>{formatNumber(event.tokens_cache_write_1h)}</DetailRow>
          {/* Local-only mode (JANUS_LOCAL_ONLY) has no billing relationship,
              so the cost row is hidden here rather than at each call site —
              this component backs both the user Requests drawer and the admin
              log viewer, and both must honour the mode. */}
          {localOnly ? null : <DetailRow label={t('tables.cost')}>{formatUSD(event.cost_nanousd)}</DetailRow>}
          <DetailRow label={t('requests.countingMethod')}>
            <AccountingBadge method={event.token_accounting_method} />
          </DetailRow>
          {event.throughput_source ? (
            <DetailRow label={t('requests.throughput')}>
              <span className="row wrap" style={{ gap: 'var(--janus-space-2)' }}>
                <span>
                  {t('requests.tokensInPerSecond')}: {formatNumber(Math.round(event.tokens_in_per_second ?? 0))}
                </span>
                <span>
                  {t('requests.tokensOutPerSecond')}: {formatNumber(Math.round(event.tokens_out_per_second ?? 0))}
                </span>
                <Badge tone={event.throughput_source === 'upstream' ? 'success' : 'neutral'}>
                  {event.throughput_source === 'upstream' ? t('requests.throughputReported') : t('requests.throughputCalculated')}
                </Badge>
              </span>
            </DetailRow>
          ) : null}
          {event.fallback_reason ? (
            <DetailRow label={t('requests.servedByFallback')}>
              <Badge tone="warning">{event.fallback_reason}</Badge>
            </DetailRow>
          ) : null}
          <DetailRow label={t('requests.finishReason')}>{event.finish_reason || '—'}</DetailRow>
        </div>
      </section>

      <section>
        <h3 style={{ marginBottom: 'var(--janus-space-3)' }}>{t('requests.timingAndSize')}</h3>
        <div className="stack">
          <DetailRow label={t('requests.totalLatency')}>{formatDuration(event.latency_ms)}</DetailRow>
          <DetailRow label={t('requests.ttfb')}>{formatDuration(event.ttfb_ms)}</DetailRow>
          <DetailRow label={t('requests.requestSize')}>{formatBytes(event.request_bytes)}</DetailRow>
          <DetailRow label={t('requests.responseSize')}>{formatBytes(event.response_bytes)}</DetailRow>
        </div>
      </section>

      <section>
        <h3 style={{ marginBottom: 'var(--janus-space-3)' }}>{t('requests.client')}</h3>
        <div className="stack">
          <DetailRow label={t('requests.userAgent')}>{event.client_user_agent || '—'}</DetailRow>
          <DetailRow label={t('requests.sourceIp')}>{event.client_ip || '—'}</DetailRow>
          <DetailRow label={t('requests.xForwardedFor')}>{event.x_forwarded_for || '—'}</DetailRow>
          <DetailRow label={t('requests.requestId')}>
            <span className="mono">{event.request_id || '—'}</span>
          </DetailRow>
        </div>
      </section>
      {extra}
    </>
  );
}
