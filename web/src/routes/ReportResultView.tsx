import { useEffect, useState, type ReactNode } from 'react';
import { pageOffset } from '../lib/collections';
import { useUrlState, useUrlStateBatch } from '../lib/hooks';
import type { ReportResult } from '../lib/reports';
import { useLocalOnly } from '../app/session';
import { isCostMetric } from '../lib/reports';
import { formatNumber } from '../lib/format';
import './report-result.css';

type Column = ReportResult['columns'][number];
function valueText(value: number | null | undefined, unit: string): string {
  if (value == null || !Number.isFinite(value)) return 'Unknown';
  if (/^(?:nano_?usd|USD)$/i.test(unit)) {
    const usd = /nano/i.test(unit) ? value / 1e9 : value;
    return usd !== 0 && Math.abs(usd) < 0.01
      ? `${usd < 0 ? '−' : ''}<$0.01`
      : new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD' }).format(usd);
  }
  if (unit === 'ratio' || unit === 'percent' || unit === '%') {
    const percent = unit === 'ratio' ? value * 100 : value;
    // Keep genuinely tiny measurements visible instead of rounding them to zero.
    return `${new Intl.NumberFormat(
      'en-US',
      Math.abs(percent) > 0 && Math.abs(percent) < 0.1 ? { maximumSignificantDigits: 6 } : { maximumFractionDigits: 1 },
    ).format(percent)}%`;
  }
  const number = Number.isInteger(value)
    ? formatNumber(value)
    : new Intl.NumberFormat(undefined, { maximumSignificantDigits: 15 }).format(value);
  return `${number}${unit === 'percent' || unit === '%' ? '%' : unit && !['count', 'ratio', 'number'].includes(unit) ? ` ${unit}` : ''}`;
}
function changeText(current: number | null | undefined, previous: number | null | undefined, unit: string): string {
  if (current == null || previous == null || !Number.isFinite(current) || !Number.isFinite(previous))
    return 'Prior comparison: Unknown';
  const difference = current - previous;
  const delta = `${difference > 0 ? '+' : ''}${valueText(difference, unit)}`;
  return previous === 0
    ? `Prior ${valueText(previous, unit)} · ${delta} · percentage change unavailable (prior is zero)`
    : `Prior ${valueText(previous, unit)} · ${delta} (${valueText((difference / Math.abs(previous)) * 100, 'percent')})`;
}
type Row = ReportResult['rows'][number];
function cell(row: Row, column: Column): string | number | null {
  if (Object.hasOwn(row.dimensions, column.key)) return row.dimensions[column.key] || null;
  const value = row.values[column.key];
  return value == null || !Number.isFinite(value) ? null : value;
}
// Escape separators as well as URL characters so run and section IDs cannot collide.
function panelName(...parts: string[]): string {
  return `report.${parts.map((part) => encodeURIComponent(part).replace(/\./g, '%2E')).join('.')}`;
}
function DataPanel({
  name,
  title,
  columns,
  rows,
  onDrillDown,
}: {
  name: string;
  title: string;
  columns: Column[];
  rows: Row[];
  onDrillDown?: (dimension: string, value: string) => void;
}): ReactNode {
  const [search] = useUrlState(`${name}.q`, '');
  const [pageValue] = useUrlState(`${name}.page`, '0');
  const [sortValue] = useUrlState(`${name}.sort`, '');
  const [size] = useUrlState(`${name}.size`, '25');
  const limit = [10, 25, 50, 100].includes(Number(size)) ? Number(size) : 25;
  const batch = useUrlStateBatch();
  const update = (key: string, value: string) => batch({ [`${name}.${key}`]: value, [`${name}.page`]: null });
  const setOffset = (next: number) => batch({ [`${name}.page`]: next ? String(next) : null });
  const sort = /:(asc|desc)$/.test(sortValue)
    ? { key: sortValue.replace(/:(asc|desc)$/, ''), descending: sortValue.endsWith(':desc') }
    : null;
  const term = search.trim().toLowerCase();
  // Only visible columns are searchable; hidden values in old snapshots stay hidden.
  const filtered = rows.filter(
    (row) =>
      columns.some((column) =>
        String(cell(row, column) ?? 'Unknown')
          .toLowerCase()
          .includes(term),
      ) || !term,
  );
  const dimension = columns.find((column) => rows.some((row) => Object.hasOwn(row.dimensions, column.key)));
  const sorted = [...filtered];
  const sortColumn = columns.find((column) => column.key === sort?.key);
  if (sort && sortColumn)
    sorted.sort((a, b) => {
      const av = cell(a, sortColumn),
        bv = cell(b, sortColumn);
      if (av === null) return bv === null ? 0 : 1;
      if (bv === null) return -1;
      const order = typeof av === 'number' && typeof bv === 'number' ? av - bv : String(av).localeCompare(String(bv));
      return sort.descending ? -order : order;
    });
  const offset = pageOffset(pageValue, limit, filtered.length);
  useEffect(() => {
    if (pageValue !== String(offset)) batch({ [`${name}.page`]: offset ? String(offset) : null });
  }, [pageValue, offset, name, batch]);
  const visible = sorted.slice(offset, offset + limit);
  return (
    <section className="report-result-panel" data-collection-mode="frozen-report">
      <h3>{title}</h3>
      <div className="report-result-controls">
        <input
          className="input"
          type="search"
          aria-label={`Search ${title}`}
          placeholder={`Search ${title}`}
          value={search}
          onChange={(e) => update('q', e.target.value)}
        />
        <label>
          Rows per page{' '}
          <select
            className="select"
            aria-label={`${title} rows per page`}
            value={limit}
            onChange={(e) => update('size', e.target.value)}
          >
            {[10, 25, 50, 100].map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
        <span>
          {filtered.length} of {rows.length} snapshot rows
        </span>
        {(search || sortValue) && (
          <button
            type="button"
            aria-label={`Clear ${title} filters and sort`}
            onClick={() => batch({ [`${name}.q`]: null, [`${name}.sort`]: null, [`${name}.page`]: null })}
          >
            Clear filters and sort
          </button>
        )}
      </div>
      <div className="report-result-table-scroll" role="region" aria-label={`${title} scrollable table`} tabIndex={0}>
        <table>
          <caption>{title}</caption>
          <thead>
            <tr>
              {columns.map((column) => (
                <th
                  key={column.key}
                  scope="col"
                  aria-sort={sort?.key === column.key ? (sort.descending ? 'descending' : 'ascending') : 'none'}
                >
                  <button
                    type="button"
                    aria-label={`Sort by ${column.label}`}
                    onClick={() => {
                      update('sort', `${column.key}:${sort?.key === column.key && !sort.descending ? 'desc' : 'asc'}`);
                    }}
                  >
                    {column.label}
                    {column.unit && <small> ({column.unit})</small>}
                    {sort?.key === column.key ? (sort.descending ? ' ↓' : ' ↑') : ' ↕'}
                  </button>
                </th>
              ))}
              {onDrillDown && dimension && <th scope="col">Explore</th>}
            </tr>
          </thead>
          <tbody>
            {visible.map((row, i) => (
              <tr key={offset + i}>
                {columns.map((column) => {
                  const value = cell(row, column);
                  return (
                    <td key={column.key} title={`${value ?? 'Unknown'} ${column.unit}`}>
                      {typeof value === 'string' ? (
                        value
                      ) : (
                        <>
                          {valueText(value, column.unit)}
                          {value !== null && /^(?:nano_?usd|USD|ratio|percent|%)$/i.test(column.unit) && (
                            <small className="report-result-exact">
                              {value} {column.unit}
                            </small>
                          )}
                        </>
                      )}
                    </td>
                  );
                })}
                {onDrillDown && dimension && (
                  <td>
                    <button type="button" onClick={() => onDrillDown(dimension.key, row.dimensions[dimension.key] ?? '')}>
                      Explore {row.dimensions[dimension.key] || 'Unknown'}
                    </button>
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {rows.length === 0 && (
        <p className="report-result-empty">
          {title === 'Report data' ? 'No rows matched this frozen report.' : 'No rows in this section.'}
        </p>
      )}
      {rows.length > 0 && filtered.length === 0 && (
        <p className="report-result-empty" role="status">
          No matching snapshot rows. Clear the search to see all frozen rows.
        </p>
      )}
      <div className="report-result-pagination">
        <p aria-live="polite">
          Showing {visible.length} of {filtered.length} · Rows {filtered.length ? offset + 1 : 0}–{offset + visible.length}
        </p>
        <div>
          <button
            type="button"
            aria-label={`Previous ${title} page`}
            disabled={offset === 0}
            onClick={() => setOffset(offset - limit)}
          >
            Previous
          </button>
          <button
            type="button"
            aria-label={`Next ${title} page`}
            disabled={offset + visible.length >= filtered.length}
            onClick={() => setOffset(offset + limit)}
          >
            Next
          </button>
        </div>
      </div>
    </section>
  );
}
// Calendar coordinates, not instants: source buckets are dates in the report zone.
function calendarTime(instant: string, timezone: string): number {
  const date = new Date(instant);
  if (!Number.isFinite(date.getTime())) return NaN;
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: timezone,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hourCycle: 'h23',
  }).formatToParts(date);
  const part = (type: Intl.DateTimeFormatPartTypes) => Number(parts.find((p) => p.type === type)?.value);
  return Date.UTC(
    part('year'),
    part('month') - 1,
    part('day'),
    part('hour'),
    part('minute'),
    part('second'),
    date.getUTCMilliseconds(),
  );
}
function SummaryChart({
  name,
  title,
  columns,
  rows,
  start,
  end,
  timezone,
  placement = '',
}: {
  name: string;
  title: string;
  columns: Column[];
  rows: Row[];
  start: string;
  end: string;
  timezone: string;
  placement?: string;
}) {
  const [metricKey, setMetricKey] = useState('');
  const [view, setView] = useState('chart');
  const metrics = columns.filter((c) => rows.some((r) => Object.hasOwn(r.values, c.key)));
  const dimensions = columns.filter((c) => !c.key.endsWith('_label') && rows.some((r) => Object.hasOwn(r.dimensions, c.key)));
  const dimension = dimensions.find((c) => /^(day|week|month)$/.test(c.key)) ?? dimensions[0];
  const metric = metrics.find((c) => c.key === metricKey) ?? metrics[0];
  if (!metric || !dimension || !rows.length) return null;
  // Never collapse grouped ratios or overlapping populations into an unlabeled series.
  if (dimensions.length > 1) {
    return (
      <section className={`report-result-panel ${placement}`}>
        <h3>{title}</h3>
        <p>Multiple grouping dimensions · complete-dimension table; measurements are not combined.</p>
        <DataPanel name={`${name}.summary`} title={`${title} summary`} columns={columns} rows={rows} />
      </section>
    );
  }
  const measured = rows
    .map((row, i) => {
      const raw = row.dimensions[dimension.key] || 'Unknown';
      const label = row.dimensions[`${dimension.key}_label`] || raw;
      return {
        label: /^[0-9a-f]{8}-[0-9a-f-]{27}$/i.test(label) ? `Unnamed ${dimension.label} ${i + 1}` : label,
        value: cell(row, metric) as number | null,
      };
    })
    .sort((a, b) => (a.value === null ? 1 : b.value === null ? -1 : b.value - a.value));
  const temporal = /^(day|week|month)$/.test(dimension.key);
  const bucketStart = new Date(calendarTime(start, timezone));
  bucketStart.setUTCHours(0, 0, 0, 0);
  if (dimension.key === 'week') bucketStart.setUTCDate(bucketStart.getUTCDate() - ((bucketStart.getUTCDay() + 6) % 7));
  if (dimension.key === 'month') bucketStart.setUTCDate(1);
  const axisStart = bucketStart.getTime();
  const axisEnd = calendarTime(end, timezone);
  const observations = rows
    .map((row) => ({ date: row.dimensions[dimension.key] ?? '', value: cell(row, metric) as number | null }))
    .sort((a, b) => Date.parse(a.date) - Date.parse(b.date));
  const unplaced = observations.filter(
    (r) => !Number.isFinite(Date.parse(r.date)) || Date.parse(r.date) < axisStart || Date.parse(r.date) >= axisEnd,
  ).length;
  const top = measured.slice(0, 5);
  const rawMinimum = measured.reduce((min, r) => Math.min(min, r.value ?? 0), 0);
  const rawMaximum = measured.reduce((max, r) => Math.max(max, r.value ?? 0), 0) || (rawMinimum === 0 ? 1 : 0);
  // Whole-count axes need an integral midpoint as well as integral bounds.
  const wholeCount = temporal && metric.unit === 'count';
  const minimum = wholeCount ? Math.floor(rawMinimum / 2) * 2 : rawMinimum;
  const maximum =
    wholeCount && measured.some((r) => r.value !== null && r.value !== 0) ? Math.ceil(rawMaximum / 2) * 2 : rawMaximum;
  const range = maximum - minimum;
  const yPosition = (value: number) => 15 + ((maximum - value) / range) * 170;
  const baseline = yPosition(0);
  const hasUnknown = measured.some((r) => r.value === null);
  return (
    <section className={`report-result-panel ${placement}`}>
      <h3>{title}</h3>
      <div className="report-result-controls">
        <label>
          Chart metric{' '}
          <select value={metric.key} onChange={(e) => setMetricKey(e.target.value)}>
            {metrics.map((c) => (
              <option key={c.key} value={c.key}>
                {c.label}
              </option>
            ))}
          </select>
        </label>
        <div aria-label={`${title} display`}>
          <button type="button" aria-pressed={view === 'chart'} onClick={() => setView('chart')}>
            Chart
          </button>
          <button type="button" aria-pressed={view === 'table'} onClick={() => setView('table')}>
            Table
          </button>
        </div>
      </div>
      {temporal ? (
        <p>{dimension.label} observations · entire reporting period</p>
      ) : (
        <p>
          Top {top.length} of {rows.length} rows by {metric.label} · all rows in Complete data.
        </p>
      )}
      {temporal && view === 'chart' && (
        <div className="report-result-trend">
          <div className="report-result-axis">
            <span>{valueText(maximum, metric.unit)}</span>
            <span>{wholeCount && range === 1 ? '' : valueText((maximum + minimum) / 2, metric.unit)}</span>
            <span>{valueText(minimum, metric.unit)}</span>
          </div>
          <svg
            role="img"
            aria-label={`${metric.label} by ${dimension.label} — entire period`}
            data-start={start}
            data-end={end}
            width="100%"
            height="200"
          >
            <line x1="0" x2="100%" y1={baseline} y2={baseline} stroke="currentColor" opacity=".3" />
            {observations.map((r, i) => {
              const date = Date.parse(r.date);
              if (!Number.isFinite(date) || axisEnd <= axisStart || date < axisStart || date >= axisEnd) return null;
              const x = `${2 + ((date - axisStart) / (axisEnd - axisStart)) * 96}%`;
              const y = yPosition(r.value ?? 0);
              return (
                <g key={i}>
                  <title>
                    {r.date}: {r.value ?? 'Unknown'} {metric.unit}
                  </title>
                  {r.value !== null && (
                    <>
                      <line x1={x} x2={x} y1={baseline} y2={y} stroke="currentColor" strokeWidth="3" />
                      <circle data-observation cx={x} cy={y} r="4" fill="currentColor" />
                    </>
                  )}
                </g>
              );
            })}
          </svg>
          {unplaced > 0 && (
            <p>
              {unplaced} {unplaced === 1 ? 'row cannot' : 'rows cannot'} be placed on the date axis; inspect Complete data.
            </p>
          )}
          <div className="report-result-date-axis">
            <span>
              {bucketStart.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric', timeZone: 'UTC' })}
            </span>
            <span>
              {new Date(end).toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric', timeZone: timezone })}
            </span>
          </div>
        </div>
      )}
      {!temporal && view === 'chart' ? (
        <div role="img" aria-label={`${metric.label} by ${dimension.label} — all rows ranked`} className="report-result-bars">
          {top.map((r, i) => (
            <div key={i} title={`${r.label}: ${r.value ?? 'Unknown'} ${metric.unit}`}>
              <div className="report-result-bar-label">
                <span>{r.label}</span>
                <strong>{valueText(r.value, metric.unit)}</strong>
              </div>
              <div className="report-result-track">
                {r.value !== null && (
                  <span
                    style={{
                      width: `${(Math.abs(r.value ?? 0) / range) * 100}%`,
                      marginLeft: `${((Math.min(0, r.value ?? 0) - minimum) / range) * 100}%`,
                    }}
                  />
                )}
              </div>
            </div>
          ))}
        </div>
      ) : view === 'table' ? (
        <div className="report-result-table-scroll">
          <table>
            <caption>{title} summary</caption>
            <thead>
              <tr>
                <th>{dimension.label}</th>
                <th>{metric.label}</th>
              </tr>
            </thead>
            <tbody>
              {(temporal ? observations.map((r) => ({ label: r.date, value: r.value })) : top).map((r, i) => (
                <tr key={i}>
                  <td>{r.label}</td>
                  <td title={`${r.value ?? 'Unknown'} ${metric.unit}`}>{valueText(r.value, metric.unit)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      {(temporal || hasUnknown || minimum < 0) && (
        <details className="report-result-chart-notes">
          <summary>Chart notes</summary>
          {temporal && (
            <p>
              Gaps mean no returned row, not zero. Boundary periods may be partial. Dates use source bucket labels; exact values
              in Complete data.
            </p>
          )}
          {hasUnknown && <p>Unknown measurements are unplotted; zero is a known measurement.</p>}
          {minimum < 0 && <p>{temporal ? 'Negative values extend below zero.' : 'Negative values extend left of zero.'}</p>}
        </details>
      )}
    </section>
  );
}
export function ReportResultView({
  result,
  runID,
  onDrillDown,
  drillableDimensions,
}: {
  result: ReportResult;
  runID: string;
  onDrillDown?: (dimension: string, value: string) => void;
  drillableDimensions?: string[];
}): ReactNode {
  const localOnly = useLocalOnly();
  // Defense in depth for old frozen runs. Server downloads enforce their own policy.
  const hidden = (text: string) =>
    /prompt|user[_ -]?agent|\bip[_ -]?address\b/i.test(text) ||
    (localOnly &&
      (isCostMetric(text) ||
        /budget|scenario|billing|currency|revenue|charge|amount|\b(?:eur|gbp|jpy|dollars?|cents?)\b|[$€£¥]/i.test(text)));
  const safeColumns = (columns: Column[]) => columns.filter((column) => !hidden(`${column.key} ${column.label} ${column.unit}`));
  const columns = safeColumns(result.columns);
  const warnings = [...new Set([...result.warnings, ...(result.comparison_warnings ?? [])])].map((warning) =>
    hidden(warning) ? 'A report warning was withheld under the current visibility policy.' : warning,
  );
  const sections = result.sections
    .filter((section) => !hidden(`${section.id} ${section.title}`))
    .map((section) => ({
      ...section,
      columns: safeColumns(section.columns),
      notes: section.notes.filter((note) => !hidden(note)),
    }));
  const executive = sections.find((s) => s.id === 'executive' && s.rows.length === 1);
  const kpiValues = { ...(executive?.rows[0]?.values ?? {}), ...result.totals };
  const metrics = [
    ...columns.filter((column) => Object.hasOwn(result.totals, column.key)),
    ...(executive?.columns.filter((c) => !columns.some((column) => column.key === c.key) && Object.hasOwn(kpiValues, c.key)) ??
      []),
  ];
  const primaryKeys = ['requests', metrics.find((c) => /^(?:nano_?usd|USD)$/i.test(c.unit))?.key, 'success_rate', 'active_users'];
  const primaryMetrics = executive ? primaryKeys.flatMap((key) => metrics.filter((c) => c.key === key)) : metrics;
  const secondaryMetrics = metrics.filter((c) => !primaryMetrics.includes(c));
  return (
    <article className="report-result">
      <header className="report-result-header">
        <div>
          <p>
            Frozen report · <span>Version {result.version}</span>
          </p>
          <h2>{hidden(result.definition.name) ? 'Frozen report' : result.definition.name}</h2>
        </div>
        <nav aria-label="Export all rows">
          <strong>Export all rows</strong>
          {['csv', 'xlsx', 'pdf', 'json'].map((format) => (
            <a key={format} href={`/api/v1/reports/runs/${encodeURIComponent(runID)}/download?format=${format}`}>
              {format.toUpperCase()}
            </a>
          ))}
        </nav>
      </header>
      <p className="report-result-audience">
        {result.definition.scope === 'organization'
          ? 'Organization scope'
          : result.definition.scope === 'team'
            ? 'Team scope'
            : 'Self scope'}{' '}
        ·{' '}
        {new Date(result.start).toLocaleDateString('en-US', {
          month: 'short',
          day: 'numeric',
          year: 'numeric',
          timeZone: result.definition.timezone,
        })}{' '}
        –{' '}
        {new Date(result.end).toLocaleDateString('en-US', {
          month: 'short',
          day: 'numeric',
          year: 'numeric',
          timeZone: result.definition.timezone,
        })}{' '}
        · {result.definition.timezone} (end exclusive)
      </p>
      <section aria-label="Overview" className="report-result-metrics">
        {primaryMetrics.map((column: Column) => (
          <div className="report-result-card" key={column.key}>
            <h3>{column.label}</h3>
            <strong title={`${kpiValues[column.key] ?? 'Unknown'} ${column.unit}`}>
              {valueText(kpiValues[column.key], column.unit)}
            </strong>
            {result.definition.compare && (
              <p>
                {result.comparison_reliable === false
                  ? 'Comparison unavailable: incomplete coverage'
                  : changeText(result.totals[column.key], result.previous_totals?.[column.key], column.unit)}
              </p>
            )}
          </div>
        ))}
      </section>
      {!localOnly && columns.some((c) => /^(?:nano_?usd|USD)$/i.test(c.unit)) && (
        <p className="report-result-cost-note">
          Recorded amounts do not establish complete pricing coverage. Zero recorded cost is not a claim of free usage; unknown
          measurements are not zero.
        </p>
      )}
      {warnings.length > 0 && (
        <details className="report-result-warnings">
          <summary>{warnings.length} coverage notices · review limitations</summary>
          <ul>
            {warnings.map((warning, i) => (
              <li key={i}>{warning}</li>
            ))}
          </ul>
        </details>
      )}
      <div className="report-result-visuals">
        <SummaryChart
          key={runID}
          name={panelName(runID, 'data')}
          title="Report data"
          placement="report-result-primary"
          columns={columns}
          rows={result.rows}
          start={result.start}
          end={result.end}
          timezone={result.definition.timezone}
        />
        {sections
          .filter((s) => /model|user/i.test(s.id))
          .sort((a, b) => Number(/model/i.test(b.id)) - Number(/model/i.test(a.id)))
          .slice(0, 2)
          .map((s) => (
            <SummaryChart
              key={`${runID}-${s.id}`}
              name={panelName(runID, 'section', s.id)}
              title={s.title}
              placement={/user/i.test(s.id) ? 'report-result-users' : 'report-result-models'}
              columns={s.columns}
              rows={s.rows}
              start={result.start}
              end={result.end}
              timezone={result.definition.timezone}
            />
          ))}
      </div>
      <details className="report-result-complete">
        <summary>Complete data and provenance</summary>
        <dl className="report-result-provenance">
          <div>
            <dt>Period (start inclusive, end exclusive)</dt>
            <dd>
              {result.start} — {result.end}
            </dd>
          </div>
          <div>
            <dt>Timezone</dt>
            <dd>{result.definition.timezone}</dd>
          </div>
          <div>
            <dt>Generated at</dt>
            <dd>
              <time dateTime={result.generated_at}>{result.generated_at}</time>
            </dd>
          </div>
          <div>
            <dt>Data cutoff</dt>
            <dd>
              <time dateTime={result.data_cutoff}>{result.data_cutoff}</time>
            </dd>
          </div>
          <div>
            <dt>Source coverage</dt>
            <dd>{formatNumber(result.source_rows)} source rows</dd>
          </div>
        </dl>
        {secondaryMetrics.length > 0 && (
          <DataPanel
            name={panelName(runID, 'totals')}
            title="Additional totals"
            columns={secondaryMetrics}
            rows={[{ dimensions: {}, values: kpiValues }]}
          />
        )}
        <DataPanel
          key={runID}
          name={panelName(runID, 'data')}
          title="Report data"
          columns={columns}
          rows={result.rows}
          onDrillDown={onDrillDown}
        />
        {sections.map((section, i) => (
          <div key={`${runID}-${section.id}-${i}`} className="report-result-section">
            <DataPanel
              name={panelName(runID, 'section', section.id)}
              title={section.title}
              columns={section.columns}
              rows={section.rows}
              onDrillDown={
                section.columns.some((c) => (drillableDimensions ?? result.definition.dimensions).includes(c.key))
                  ? onDrillDown
                  : undefined
              }
            />
            <ul>
              {section.notes.map((note, j) => (
                <li key={j}>{note}</li>
              ))}
            </ul>
          </div>
        ))}
        <section className="report-result-methodology">
          <h3>Methodology</h3>
          <p>
            This is a frozen result, not a live query. Generation time and data cutoff above remain unchanged when searching,
            sorting, paging or changing charts. Exports download the complete frozen run, not the visible page.
          </p>
          <p>
            Totals and prior-period comparisons are supplied by the report engine. Group rows may overlap: never add their values
            to reconstruct deduplicated totals. Unknown values mean unavailable measurements, not zero. Ratios are supplied
            measurements, not recomputed from displayed groups.
          </p>
          <p>
            Membership mode: {result.definition.group_mode}. Scope: {result.definition.scope}. Template:{' '}
            {hidden(result.definition.template) ? 'Restricted' : result.definition.template}.
          </p>
        </section>
      </details>
    </article>
  );
}
