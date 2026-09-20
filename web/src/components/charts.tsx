import { useId, useMemo, type ReactNode } from 'react';
import type { Breakdown, TimePoint } from '../lib/types';
import { formatNumber, formatUSD } from '../lib/format';
import { sortCollection } from '../lib/collections';
import { BreakdownDetails } from './BreakdownDetails';

const SERIES_VARS = [
  'var(--janus-chart-1)',
  'var(--janus-chart-2)',
  'var(--janus-chart-3)',
  'var(--janus-chart-4)',
  'var(--janus-chart-5)',
  'var(--janus-chart-6)',
  'var(--janus-chart-7)',
  'var(--janus-chart-8)',
];

// Dash patterns pair with colour so a series is still distinguishable when hue
// alone is not perceivable.
const DASH_PATTERNS = ['0', '0', '5 3', '2 3', '8 3', '5 3 1 3', '1 3', '10 4'];

// Defaults for stacked layers: every consecutive layer gets a DISTINCT pattern
// ('0' = solid) so hue is never the sole encoding between adjacent layers.
const STACK_DASH_PATTERNS = ['0', '5 3', '2 3', '8 3', '5 3 1 3', '1 3', '10 4', '3 2'];

export function seriesColor(index: number): string {
  return SERIES_VARS[index % SERIES_VARS.length]!;
}

/** Default stroke-dash pattern for stacked layer `index` ('0' means solid). */
export function seriesDash(index: number): string {
  return STACK_DASH_PATTERNS[index % STACK_DASH_PATTERNS.length]!;
}

/**
 * One sub-series layer of a stacked AreaChart, ordered bottom-up in the array
 * passed to the chart: the first entry is drawn at the baseline, each later
 * entry stacks on top, and the top boundary equals the sum of all layers.
 */
export interface AreaSeries {
  /** Stable identifier for the layer (React key). */
  key: string;
  /** Legend label, e.g. t('explore.tokensIn'). */
  label: string;
  /** Layer colour — pass a chart palette token via seriesColor(i), never raw hex. */
  color: string;
  /**
   * Optional stroke-dasharray for the layer boundary ("5 3" or "5,3"). When
   * omitted the layer gets a distinct default from seriesDash(i) so colour is
   * never the only thing telling two layers apart.
   */
  dash?: string;
  /** Y-values, one per x bucket; all layers must share the same length. */
  values: number[];
}

// Accepts "5,3" (CSS-ish) as well as "5 3" (SVG); SVG wants space separation.
function normalizeDash(dash: string): string {
  return dash.replace(/,/g, ' ').replace(/\s+/g, ' ').trim();
}

function isStackedSeries(series: readonly TimePoint[] | readonly AreaSeries[]): series is AreaSeries[] {
  const first = series[0];
  return first !== undefined && 'values' in first;
}

// 'cost' stays a valid metric for flag-off deployments; pages exclude it from
// their metric pickers in local-only mode (useLocalOnly), so these components
// are never asked to chart cost there.
export type MetricKey = 'requests' | 'tokens' | 'tokens_out' | 'cost';

const METRIC_LABEL: Record<MetricKey, string> = {
  requests: 'Requests',
  tokens: 'Tokens',
  tokens_out: 'Tokens out',
  cost: 'Spend',
};

function metricValue(point: TimePoint, metric: MetricKey): number {
  switch (metric) {
    case 'requests':
      return point.totals.request_count;
    case 'tokens':
      return point.totals.tokens_in + point.totals.tokens_out;
    case 'tokens_out':
      return point.totals.tokens_out;
    case 'cost':
      return point.totals.cost_nanousd;
  }
}

/** Value of one breakdown row under a metric — shared by BarList and DonutChart. */
function breakdownValue(row: Breakdown, metric: MetricKey): number {
  switch (metric) {
    case 'requests':
      return row.totals.request_count;
    case 'tokens':
      return row.totals.tokens_in + row.totals.tokens_out;
    case 'tokens_out':
      return row.totals.tokens_out;
    case 'cost':
      return row.totals.cost_nanousd;
  }
}

function formatMetric(value: number, metric: MetricKey): string {
  return metric === 'cost' ? formatUSD(value, { compact: true }) : formatNumber(value, { compact: true });
}

/**
 * Nice round axis maximum at or above `value`, so gridlines land on numbers a
 * human would choose (10, 25, 50, 100, 250…) instead of the raw peak.
 */
export function niceAxisMax(value: number): number {
  if (!Number.isFinite(value) || value <= 0) return 1;
  const magnitude = Math.pow(10, Math.floor(Math.log10(value)));
  const normalized = value / magnitude;
  const step = normalized <= 1 ? 1 : normalized <= 2 ? 2 : normalized <= 2.5 ? 2.5 : normalized <= 5 ? 5 : 10;
  return step * magnitude;
}

/**
 * The y-axis scale for a chart: gridline values rendered as HTML beside the
 * plot. Charts stretch their viewBox (preserveAspectRatio="none"), which would
 * squash SVG <text>, so the labels live outside the SVG and are positioned by
 * percentage against the same 0..max range the paths are drawn in.
 *
 * Without this a reader can see a shape but not read a magnitude — the whole
 * point of the axis.
 */
function ChartScale({
  max,
  metric,
  height,
  children,
}: {
  max: number;
  metric: MetricKey;
  height: number;
  children: ReactNode;
}): ReactNode {
  // Fractions match the 25/50/75 gridlines the charts already draw, plus the
  // top and the zero baseline.
  const ticks = [1, 0.75, 0.5, 0.25, 0];
  return (
    <div style={{ display: 'flex', gap: 6, alignItems: 'stretch' }}>
      <div
        aria-hidden="true"
        style={{
          position: 'relative',
          height,
          minWidth: 34,
          flex: '0 0 auto',
          fontSize: 'var(--janus-text-2xs, 10px)',
          color: 'var(--janus-color-text-muted)',
          fontVariantNumeric: 'tabular-nums',
        }}
      >
        {ticks.map((fraction) => (
          <span
            key={fraction}
            style={{
              position: 'absolute',
              right: 0,
              // The plot area occupies 92% of the viewBox height (the paths
              // reserve 8% of headroom), so labels track that same band.
              top: `${(1 - fraction) * 92}%`,
              transform: 'translateY(-50%)',
              lineHeight: 1,
              whiteSpace: 'nowrap',
            }}
          >
            {formatMetric(max * fraction, metric)}
          </span>
        ))}
      </div>
      <div style={{ flex: 1, minWidth: 0 }}>{children}</div>
    </div>
  );
}

/**
 * Area chart for a usage time series. Rendered as inline SVG so it inherits
 * design tokens, needs no charting dependency, and prints correctly.
 *
 * Two modes, discriminated by the shape of `series`:
 * - `TimePoint[]` (the historical contract) — single-series render, unchanged.
 * - `AreaSeries[]` — stacked mode: layers are drawn bottom-up as cumulative
 *   areas summing to the total, each boundary gets a distinct colour AND a
 *   distinct dash pattern, and an inline legend (swatch + dash sample + label)
 *   is rendered under the chart.
 */
export function AreaChart({
  series,
  metric,
  height = 180,
  label,
}: {
  series: TimePoint[] | AreaSeries[];
  metric: MetricKey;
  height?: number;
  label?: string;
}): ReactNode {
  if (isStackedSeries(series)) {
    return <StackedAreaChart series={series} metric={metric} height={height} label={label} />;
  }
  return <SingleAreaChart series={series} metric={metric} height={height} label={label} />;
}

// The pre-stacking implementation, verbatim: when callers pass TimePoint[]
// (Dashboard, Explore, Team, admin Overview, and the admin People user-detail
// drawer for the Requests/Spend metrics; Team's sparklines) the rendered
// markup is byte-identical to the single-series chart that shipped before
// stacked mode. Token time-series charts instead build AreaSeries[] via
// tokenDirectionSeries() in routes/shared.tsx, whose In/Out modes render a
// single stacked layer and whose Total mode stacks tokens-in under tokens-out.
function SingleAreaChart({
  series,
  metric,
  height,
  label,
}: {
  series: TimePoint[];
  metric: MetricKey;
  height: number;
  label?: string;
}): ReactNode {
  const gradientId = useId();
  const points = useMemo(() => series.map((point) => metricValue(point, metric)), [series, metric]);
  const peak = Math.max(1, ...points);
  // Paths are drawn against a rounded axis top, so the gridline labels the
  // scale renders are round numbers rather than an arbitrary peak.
  const max = niceAxisMax(peak);
  const width = 100;

  const path = useMemo(() => {
    if (points.length === 0) return '';
    return points
      .map((value, index) => {
        const x = points.length === 1 ? 0 : (index / (points.length - 1)) * width;
        const y = 100 - (value / max) * 92;
        return `${index === 0 ? 'M' : 'L'}${x.toFixed(2)},${y.toFixed(2)}`;
      })
      .join(' ');
  }, [points, max]);

  const area = path ? `${path} L${width},100 L0,100 Z` : '';
  const total = points.reduce((sum, value) => sum + value, 0);

  if (series.length === 0) {
    return (
      <div className="state" style={{ padding: 'var(--janus-space-8)' }}>
        <p className="state-body small">No activity in this range yet.</p>
      </div>
    );
  }

  return (
    <figure style={{ margin: 0 }}>
      <figcaption className="sr-only">
        {label ?? METRIC_LABEL[metric]} over time. Total {formatMetric(total, metric)} across {series.length} buckets.
      </figcaption>
      <ChartScale max={max} metric={metric} height={height}>
        <svg
          viewBox={`0 0 ${width} 100`}
          preserveAspectRatio="none"
          style={{ width: '100%', height, display: 'block' }}
          role="img"
          aria-label={`${label ?? METRIC_LABEL[metric]} trend`}
        >
          <defs>
            <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--janus-chart-1)" stopOpacity="0.42" />
              <stop offset="100%" stopColor="var(--janus-chart-1)" stopOpacity="0" />
            </linearGradient>
          </defs>
          {[25, 50, 75].map((y) => (
            <line key={y} x1="0" y1={y} x2={width} y2={y} stroke="var(--janus-chart-grid)" strokeWidth="0.3" />
          ))}
          {area ? <path d={area} fill={`url(#${gradientId})`} /> : null}
          {path ? (
            <path d={path} fill="none" stroke="var(--janus-chart-1)" strokeWidth="0.8" vectorEffect="non-scaling-stroke" />
          ) : null}
        </svg>
      </ChartScale>
      <div className="row-between small muted" style={{ marginTop: 6 }}>
        <span>peak {formatMetric(peak, metric)}</span>
        <span>total {formatMetric(total, metric)}</span>
      </div>
    </figure>
  );
}

// Stacked mode: cumulative areas drawn bottom-up. Layer i's top boundary is
// sum(values[0..i]); the last layer's boundary is the grand total, so the
// stack visually sums to the same line a single-series chart of the totals
// would draw. No path animation, so reduced-motion needs no extra gate here
// (matches the existing chart transitions, which only animate QuotaRing).
function StackedAreaChart({
  series,
  metric,
  height,
  label,
}: {
  series: AreaSeries[];
  metric: MetricKey;
  height: number;
  label?: string;
}): ReactNode {
  const width = 100;
  const bucketCount = series.reduce((longest, layer) => Math.max(longest, layer.values.length), 0);

  const layers = useMemo(() => {
    const running = new Array<number>(bucketCount).fill(0);
    return series.map((layer, index) => {
      const bases = [...running];
      const tops = bases.map((base, x) => base + (layer.values[x] ?? 0));
      tops.forEach((top, x) => {
        running[x] = top;
      });
      return {
        key: layer.key,
        label: layer.label,
        color: layer.color,
        dash: normalizeDash(layer.dash ?? seriesDash(index)),
        bases,
        tops,
      };
    });
  }, [series, bucketCount]);

  const totals = layers.length > 0 ? layers[layers.length - 1]!.tops : [];
  const peak = Math.max(1, ...totals);
  // Rounded axis top so the scale labels read as round numbers.
  const max = niceAxisMax(peak);
  const total = totals.reduce((sum, value) => sum + value, 0);

  const x = (index: number): number => (bucketCount === 1 ? 0 : (index / (bucketCount - 1)) * width);
  const y = (value: number): number => 100 - (value / max) * 92;
  const boundaryPath = (values: number[]): string =>
    values.map((value, index) => `${index === 0 ? 'M' : 'L'}${x(index).toFixed(2)},${y(value).toFixed(2)}`).join(' ');
  const bandPath = (tops: number[], bases: number[]): string => {
    const forward = boundaryPath(tops);
    const back = [...bases]
      .reverse()
      .map((value, index) => `L${x(bases.length - 1 - index).toFixed(2)},${y(value).toFixed(2)}`)
      .join(' ');
    return `${forward} ${back} Z`;
  };

  if (bucketCount === 0) {
    return (
      <div className="state" style={{ padding: 'var(--janus-space-8)' }}>
        <p className="state-body small">No activity in this range yet.</p>
      </div>
    );
  }

  return (
    <figure style={{ margin: 0 }}>
      <figcaption className="sr-only">
        {label ?? METRIC_LABEL[metric]} over time, split by {layers.map((layer) => layer.label).join(', ')}. Total{' '}
        {formatMetric(total, metric)} across {bucketCount} buckets.
      </figcaption>
      <ChartScale max={max} metric={metric} height={height}>
        <svg
          viewBox={`0 0 ${width} 100`}
          preserveAspectRatio="none"
          style={{ width: '100%', height, display: 'block' }}
          role="img"
          aria-label={`${label ?? METRIC_LABEL[metric]} trend`}
        >
          {[25, 50, 75].map((gridY) => (
            <line key={gridY} x1="0" y1={gridY} x2={width} y2={gridY} stroke="var(--janus-chart-grid)" strokeWidth="0.3" />
          ))}
          {layers.map((layer) => (
            <path key={`${layer.key}-fill`} d={bandPath(layer.tops, layer.bases)} fill={layer.color} fillOpacity="0.28" />
          ))}
          {layers.map((layer) => (
            <path
              key={`${layer.key}-line`}
              data-series={layer.key}
              d={boundaryPath(layer.tops)}
              fill="none"
              stroke={layer.color}
              strokeWidth="0.8"
              strokeDasharray={layer.dash === '0' ? undefined : layer.dash}
              vectorEffect="non-scaling-stroke"
            />
          ))}
        </svg>
      </ChartScale>
      <ul
        className="row small"
        aria-label="Chart series"
        style={{ listStyle: 'none', margin: '6px 0 0', padding: 0, gap: 'var(--janus-space-4)', flexWrap: 'wrap' }}
      >
        {layers.map((layer) => (
          <li key={layer.key} className="row" style={{ gap: 6, alignItems: 'center' }}>
            <span aria-hidden="true" style={{ width: 10, height: 10, borderRadius: 3, background: layer.color, flex: 'none' }} />
            <svg width="24" height="8" viewBox="0 0 24 8" aria-hidden="true" style={{ flex: 'none' }}>
              <line
                x1="1"
                y1="4"
                x2="23"
                y2="4"
                stroke={layer.color}
                strokeWidth="2"
                strokeDasharray={layer.dash === '0' ? undefined : layer.dash}
              />
            </svg>
            <span className="truncate">{layer.label}</span>
          </li>
        ))}
      </ul>
      <div className="row-between small muted" style={{ marginTop: 6 }}>
        <span>peak {formatMetric(peak, metric)}</span>
        <span>total {formatMetric(total, metric)}</span>
      </div>
    </figure>
  );
}

/** Horizontal ranked bars — the readable default for categorical breakdowns. */
export function BarList({
  items,
  metric,
  emptyLabel = 'Nothing recorded yet.',
  max: maxItems = 8,
}: {
  items: Breakdown[];
  metric: MetricKey;
  emptyLabel?: string;
  max?: number;
}): ReactNode {
  const ordered = sortCollection(items, (row) => breakdownValue(row, metric), false);
  const rows = ordered.slice(0, maxItems);
  const values = rows.map((row) => breakdownValue(row, metric));
  const max = Math.max(1, ...values);

  if (rows.length === 0) {
    return <p className="small muted">{emptyLabel}</p>;
  }

  return (
    <div className="stack">
      <p className="small muted">
        Top {rows.length} of {items.length} returned categories by {METRIC_LABEL[metric].toLowerCase()}, ranked within the
        returned server summary — not the full inventory.
      </p>
      <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0, gap: 'var(--janus-space-3)' }}>
        {rows.map((row, index) => {
          const value = values[index] ?? 0;
          return (
            <li key={row.key || row.label || index}>
              <div className="row-between small" style={{ marginBottom: 4 }}>
                <span className="truncate" title={row.label}>
                  {row.label || '—'}
                </span>
                <span className="num muted">{formatMetric(value, metric)}</span>
              </div>
              <div className="meter" aria-hidden="true">
                <div
                  className="meter-fill"
                  style={{ width: `${Math.max(2, (value / max) * 100)}%`, background: seriesColor(index) }}
                />
              </div>
            </li>
          );
        })}
      </ul>
      <BreakdownDetails
        key={metric}
        rows={ordered.map((row) => ({ key: row.key, label: row.label, value: breakdownValue(row, metric) }))}
        metricLabel={METRIC_LABEL[metric]}
        format={(value) => formatMetric(value, metric)}
      />
    </div>
  );
}

/** Donut used for modality mix; every slice is also labelled in the legend. */
export function DonutChart({ items, metric }: { items: Breakdown[]; metric: MetricKey }): ReactNode {
  const ordered = sortCollection(items, (row) => breakdownValue(row, metric), false);
  // Preserve the denominator instead of making the displayed subset look like 100%.
  const rows =
    ordered.length <= 8
      ? ordered
      : [
          ...ordered.slice(0, 7),
          {
            key: '__other_returned',
            label: 'Other returned categories',
            totals: ordered.slice(7).reduce(
              (a, row) => ({
                request_count: a.request_count + row.totals.request_count,
                tokens_in: a.tokens_in + row.totals.tokens_in,
                tokens_out: a.tokens_out + row.totals.tokens_out,
                tokens_cached: a.tokens_cached + row.totals.tokens_cached,
                tokens_cache_write_5m: a.tokens_cache_write_5m + row.totals.tokens_cache_write_5m,
                tokens_cache_write_1h: a.tokens_cache_write_1h + row.totals.tokens_cache_write_1h,
                cost_nanousd: a.cost_nanousd + row.totals.cost_nanousd,
                error_count: a.error_count + row.totals.error_count,
              }),
              {
                request_count: 0,
                tokens_in: 0,
                tokens_out: 0,
                tokens_cached: 0,
                tokens_cache_write_5m: 0,
                tokens_cache_write_1h: 0,
                cost_nanousd: 0,
                error_count: 0,
              },
            ),
          },
        ];
  const values = rows.map((row) => breakdownValue(row, metric));
  const total = values.reduce((sum, value) => sum + value, 0);

  if (total === 0) {
    return <p className="small muted">No breakdown available yet.</p>;
  }

  const radius = 42;
  const circumference = 2 * Math.PI * radius;
  let offset = 0;

  return (
    <div className="stack">
      <p className="small muted">
        Ranked within the returned server summary by {METRIC_LABEL[metric].toLowerCase()}. Percentages cover the returned summary,
        not the full inventory.
      </p>
      <div className="row" style={{ gap: 'var(--janus-space-5)', alignItems: 'center', flexWrap: 'wrap' }}>
        <svg width="132" height="132" viewBox="0 0 120 120" role="img" aria-label="Usage by category">
          <circle cx="60" cy="60" r={radius} fill="none" stroke="var(--janus-color-bg-surface-active)" strokeWidth="14" />
          {rows.map((row, index) => {
            const value = values[index] ?? 0;
            const length = (value / total) * circumference;
            const dash = `${length} ${circumference - length}`;
            const element = (
              <circle
                key={row.key || row.label || index}
                cx="60"
                cy="60"
                r={radius}
                fill="none"
                stroke={seriesColor(index)}
                strokeWidth="14"
                strokeDasharray={dash}
                strokeDashoffset={-offset}
                transform="rotate(-90 60 60)"
              />
            );
            offset += length;
            return element;
          })}
        </svg>
        <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0, gap: 6, flex: 1, minWidth: 160 }}>
          {rows.map((row, index) => (
            <li key={row.key || row.label || index} className="row-between small">
              <span className="row" style={{ gap: 8 }}>
                <span
                  aria-hidden="true"
                  style={{
                    width: 10,
                    height: 10,
                    borderRadius: 3,
                    background: seriesColor(index),
                    border: `1px dashed ${DASH_PATTERNS[index] === '0' ? 'transparent' : 'var(--janus-color-bg-canvas)'}`,
                  }}
                />
                <span className="truncate">{row.label || '—'}</span>
              </span>
              <span className="num muted">{Math.round(((values[index] ?? 0) / total) * 100)}%</span>
            </li>
          ))}
        </ul>
      </div>
      <BreakdownDetails
        key={metric}
        rows={ordered.map((row) => ({ key: row.key, label: row.label, value: breakdownValue(row, metric) }))}
        metricLabel={METRIC_LABEL[metric]}
        format={(value) => formatMetric(value, metric)}
      />
    </div>
  );
}

/** Circular quota gauge with 80% / 95% / breach colouring. */
export function QuotaRing({ percent, label, caption }: { percent: number; label: string; caption?: string }): ReactNode {
  const clamped = Math.max(0, Math.min(100, percent));
  const radius = 34;
  const circumference = 2 * Math.PI * radius;
  const filled = (clamped / 100) * circumference;
  const tone = percent >= 100 ? 'danger' : percent >= 80 ? 'warning' : 'success';
  const stroke =
    tone === 'danger'
      ? 'var(--janus-color-danger-solid)'
      : tone === 'warning'
        ? 'var(--janus-color-warning-solid)'
        : 'var(--janus-color-success-solid)';

  return (
    <div className="row" style={{ gap: 'var(--janus-space-3)' }}>
      <svg width="88" height="88" viewBox="0 0 88 88" role="img" aria-label={`${label}: ${Math.round(percent)}% used`}>
        <circle cx="44" cy="44" r={radius} fill="none" stroke="var(--janus-color-bg-surface-active)" strokeWidth="9" />
        <circle
          cx="44"
          cy="44"
          r={radius}
          fill="none"
          stroke={stroke}
          strokeWidth="9"
          strokeLinecap="round"
          strokeDasharray={`${filled} ${circumference - filled}`}
          transform="rotate(-90 44 44)"
          style={{ transition: 'stroke-dasharray var(--janus-duration-slower) var(--janus-ease-decelerate)' }}
        />
        <text
          x="44"
          y="49"
          textAnchor="middle"
          fill="var(--janus-color-text-primary)"
          fontSize="17"
          fontFamily="var(--janus-font-mono)"
        >
          {Math.round(clamped)}%
        </text>
      </svg>
      <div>
        <div style={{ fontWeight: 600 }}>{label}</div>
        {caption ? <div className="small muted">{caption}</div> : null}
      </div>
    </div>
  );
}
