import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Collection } from '../../components/Collection';
import { sortCollection } from '../../lib/collections';
import { Link } from 'react-router-dom';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { api, qs } from '../../lib/api';
import type { Breakdown, ModelPerformanceChoice, ModelPerformanceSeries, QuotaStatus, TimePoint, Totals } from '../../lib/types';
import { formatDuration, formatNumber, formatUSD } from '../../lib/format';
import { cacheMeta } from '../../lib/cache';
import { AreaChart, BarList, LineChart } from '../../components/charts';
import { Badge } from '../../components/ui';
import { ChartPlaceholder, ListPlaceholder, Piece, TilePlaceholder } from '../../components/progressive';
import { useUrlState } from '../../lib/hooks';
import { t } from '../../lib/i18n';
import { useDefaultMetric, useLocalOnly, type DashboardMetric } from '../../app/session';
import { RangePicker, TokenDirectionPicker, tokenDirectionSeries, type RangeKey, type TokenDirection } from '../shared';

interface AdminOverviewData {
  range: string;
  totals: Totals;
  series: TimePoint[];
  /** Top users, server-ranked by output-token volume (payload key kept for API stability). */
  top_spenders: Breakdown[];
  top_teams?: Breakdown[];
  top_models: Breakdown[];
  by_status: Breakdown[];
  model_counts: Record<string, number>;
  near_breach: QuotaStatus[];
  user_count: number;
  active_users_15m: number;
  /** Absent from servers older than this UI. */
  model_performance?: ModelPerformanceSeries;
}

export function AdminOverview(): ReactNode {
  const localOnly = useLocalOnly();
  const [range, setRange] = useUrlState<RangeKey>('range', 'month');
  // Spend vs usage emphasis (the spend_emphasis admin flag) picks the
  // default; URL state and the segmented selector still override it per view.
  const emphasisDefault = useDefaultMetric();
  // Local-only mode has no cost surface at all. Neither a cost emphasis
  // default nor a stale ?metric=cost may select it, and because 'tokens' is
  // not offered as this page's opening metric there, both fall to requests.
  const [rawMetric, setMetric] = useUrlState<DashboardMetric>('metric', localOnly ? 'requests' : emphasisDefault);
  const metric = localOnly && rawMetric === 'cost' ? 'requests' : rawMetric;
  const [direction, setDirection] = useUrlState<TokenDirection>('direction', 'total');
  // The admin's own choice of models for the performance charts (comma
  // separated ids); empty = the most-used models.
  const [modelsParam, setModelsParam] = useUrlState<string>('models', '');

  // Each piece loads on its own (the server computes them independently):
  // the tiles paint first and every chart fills in as its data arrives.
  const part = (name: string) => ({
    queryKey: ['admin', 'overview', range, name],
    queryFn: () => api.get<AdminOverviewData>(`/api/v1/admin/overview${qs({ range, part: name })}`),
    refetchInterval: 20_000,
  });
  const totalsQ = useQuery(part('totals'));
  const seriesQ = useQuery(part('series'));
  const leadersQ = useQuery(part('leaders'));
  const perfQ = useQuery({
    queryKey: ['admin', 'overview', range, 'performance', modelsParam],
    queryFn: () => api.get<AdminOverviewData>(`/api/v1/admin/overview${qs({ range, part: 'performance', models: modelsParam })}`),
    refetchInterval: 20_000,
    // Keep the card (and an open model picker) on screen while a new choice loads.
    placeholderData: keepPreviousData,
  });
  const quotasQ = useQuery(part('quotas'));
  const pending = totalsQ.data?.model_counts?.pending_approval ?? 0;

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminOverview.title')}</h1>
          <p className="page-subtitle">{t(localOnly ? 'adminOverview.subtitleLocalOnly' : 'adminOverview.subtitle')}</p>
        </div>
        <div className="row">
          <RangePicker value={range} onChange={setRange} />
        </div>
      </header>

      {pending > 0 ? (
        <div className="banner banner-info">
          <div className="row-between" style={{ width: '100%' }}>
            <span>
              <strong>{t('adminOverview.pendingBanner', { count: pending })}</strong>{' '}
              {t(localOnly ? 'adminOverview.pendingBannerBodyLocalOnly' : 'adminOverview.pendingBannerBody')}
            </span>
            <Link className="btn btn-sm" to="/admin/models?status=pending_approval">
              {t('adminOverview.reviewNow')}
            </Link>
          </div>
        </div>
      ) : null}

      <Piece
        query={totalsQ}
        placeholder={
          <div className="grid grid-tiles">
            <TilePlaceholder label={localOnly ? t('adminOverview.accounts') : t('dashboard.spend')} />
            <TilePlaceholder label={t('dashboard.requests')} />
            <TilePlaceholder label={t('tables.tokens')} />
            <TilePlaceholder label={t('adminOverview.activeUsers')} />
          </div>
        }
      >
        {(data) => (
          <div className="grid grid-tiles">
            {localOnly ? (
              <Tile label={t('adminOverview.accounts')} value={formatNumber(data.user_count)} />
            ) : (
              <Tile
                label={t('dashboard.spend')}
                value={formatUSD(data.totals.cost_nanousd)}
                meta={t('adminOverview.acrossAccounts', { count: data.user_count })}
              />
            )}
            <Tile
              label={t('dashboard.requests')}
              value={formatNumber(data.totals.request_count, { compact: true })}
              meta={t('adminOverview.failedCount', { count: formatNumber(data.totals.error_count) })}
            />
            <Tile
              label={t('tables.tokens')}
              value={formatNumber(data.totals.tokens_in + data.totals.tokens_out, { compact: true })}
              meta={cacheMeta(data.totals)}
            />
            <Tile
              label={t('adminOverview.activeUsers')}
              value={formatNumber(data.active_users_15m)}
              meta={t('adminOverview.last15m')}
            />
          </div>
        )}
      </Piece>

      <section className="card">
        <div className="card-header">
          <h2>{t('adminOverview.orgTrend')}</h2>
          <div className="row" style={{ gap: 'var(--janus-space-3)', flexWrap: 'wrap' }}>
            {metric === 'tokens' ? <TokenDirectionPicker value={direction} onChange={setDirection} /> : null}
            {/* Metric selector: opens on the emphasis default resolved
                above; a click writes ?metric= which then overrides the
                instance-wide setting for this view. Local-only mode has
                no spend surface, so 'cost' is dropped there. */}
            <div className="segmented" role="group" aria-label={t('tables.metric')}>
              {(localOnly ? (['requests', 'tokens'] as const) : (['cost', 'requests', 'tokens'] as const)).map((key) => (
                <button key={key} type="button" aria-pressed={metric === key} onClick={() => setMetric(key)}>
                  {key === 'cost' ? t('dashboard.spend') : key === 'tokens' ? t('tables.tokens') : t('dashboard.requests')}
                </button>
              ))}
            </div>
          </div>
        </div>
        <Piece query={seriesQ} placeholder={<ChartPlaceholder height={200} label="Loading the trend…" />}>
          {(data) =>
            metric === 'tokens' ? (
              <AreaChart series={tokenDirectionSeries(data.series, direction)} metric="tokens" height={200} />
            ) : (
              <AreaChart series={data.series} metric={metric} height={200} />
            )
          }
        </Piece>
      </section>

      <Piece
        query={perfQ}
        placeholder={
          <section className="card" data-testid="model-performance-loading">
            <div className="card-header">
              <h2>{t('adminOverview.modelPerformance')}</h2>
            </div>
            <ChartPlaceholder height={150} label="Loading model performance…" />
          </section>
        }
      >
        {(data) => (
          <ModelPerformanceCard
            perf={data.model_performance}
            chosen={modelsParam ? modelsParam.split(',') : []}
            onChoose={(ids) => setModelsParam(ids.join(','))}
          />
        )}
      </Piece>

      <Piece query={leadersQ} placeholder={null}>
        {(data) =>
          (data.top_teams?.length ?? 0) > 0 ? (
            <section className="card">
              <div className="card-header">
                <h2>Most popular teams</h2>
                <Link className="small" to="/reports">
                  Team reports
                </Link>
              </div>
              <p className="small muted">Ranked by output tokens</p>
              <BarList items={data.top_teams ?? []} metric="tokens_out" max={10} />
            </section>
          ) : null
        }
      </Piece>

      <div className="grid grid-halves">
        {localOnly ? null : (
          <section className="card">
            <div className="card-header">
              <h2>{t('adminOverview.topSpenders')}</h2>
              <Link className="small" to="/admin/users">
                {t('adminOverview.allPeople')}
              </Link>
            </div>
            <Piece query={leadersQ} placeholder={<ListPlaceholder label="Loading top people…" />}>
              {(data) => (
                <BarList items={data.top_spenders} metric="tokens_out" emptyLabel={t('adminOverview.noAttributedSpend')} />
              )}
            </Piece>
          </section>
        )}

        <section className="card">
          <div className="card-header">
            <h2>{t('adminOverview.mostUsedModels')}</h2>
            <Link className="small" to="/admin/models">
              {t('adminOverview.manageModels')}
            </Link>
          </div>
          <Piece query={leadersQ} placeholder={<ListPlaceholder label="Loading top models…" />}>
            {(data) => <BarList items={data.top_models} metric={metric} emptyLabel={t('adminOverview.noModelUsage')} />}
          </Piece>
        </section>
      </div>

      <div className="grid grid-halves">
        <section className="card">
          <div className="card-header">
            <h2>{t('adminOverview.outcomes')}</h2>
          </div>
          <Piece query={leadersQ} placeholder={<ListPlaceholder rows={3} label="Loading outcomes…" />}>
            {(data) => <BarList items={data.by_status} metric="requests" emptyLabel={t('adminOverview.noRequests')} />}
          </Piece>
        </section>

        <section className="card">
          <div className="card-header">
            <h2>{t('adminOverview.nearBreach')}</h2>
            <Link className="small" to="/admin/quotas">
              {t('adminOverview.manageQuotas')}
            </Link>
          </div>
          <Piece query={quotasQ} placeholder={<ListPlaceholder rows={2} label="Checking quotas…" />}>
            {(data) => <NearBreach rows={data.near_breach} />}
          </Piece>
        </section>
      </div>
    </div>
  );
}

function NearBreach({ rows }: { rows: QuotaStatus[] }): ReactNode {
  if (rows.length === 0) return <p className="small muted">{t('adminOverview.noneNearBreach')}</p>;
  return (
    <>
      <p className="small muted">Current evaluated at-risk quotas; independent of the usage time range.</p>
      <Collection
        name="At-risk quotas"
        rows={sortCollection(rows, (quota) => quota.percent, false)}
        rowKey={(quota) => quota.id}
        columns={[
          {
            id: 'subject',
            label: 'Subject',
            value: (quota) => quota.subject_name || quota.subject_type,
            render: (quota) => quota.subject_name || quota.subject_type,
          },
          {
            id: 'metric',
            label: 'Metric / window',
            value: (quota) => `${quota.metric_label} ${quota.window_label}`,
            render: (quota) => `${quota.metric_label} ${quota.window_label}`,
          },
          {
            id: 'usage',
            label: 'Utilization',
            value: (quota) => quota.percent,
            render: (quota) => <Badge tone={quota.breached ? 'danger' : 'warning'}>{Math.round(quota.percent)}%</Badge>,
          },
        ]}
      />
    </>
  );
}

/**
 * Performance of the most-used (or picked) models over the selected range:
 * average concurrency, generation speed, input context, time to first token,
 * output size and cache hit rate, one line per model on the same buckets as
 * the organization trend above. A model that never reports cached tokens
 * reads "not reported" in the cache legend, never 0%.
 */
function ModelPerformanceCard({
  perf,
  chosen,
  onChoose,
}: {
  perf: ModelPerformanceSeries | undefined;
  chosen: string[];
  onChoose: (ids: string[]) => void;
}): ReactNode {
  if (!perf) return null;
  const models = perf.models;
  const percent = (fraction: number): string => `${Number((fraction * 100).toFixed(1))}%`;
  // Two decimals below 1 (concurrency 0.25), one below 10, whole numbers above.
  const decimal = (value: number): string => {
    if (value <= 0) return '0';
    if (value < 1) return String(Number(value.toFixed(2)));
    if (value < 10) return String(Number(value.toFixed(1)));
    return formatNumber(Math.round(value), { compact: true });
  };
  const charts: Array<{
    key: string;
    title: string;
    hint: string;
    format: (v: number) => string;
    lines: LinePick;
    empty?: string;
  }> = [
    {
      key: 'concurrency',
      title: t('adminOverview.perfConcurrency'),
      hint: t('adminOverview.perfConcurrencyHint'),
      format: decimal,
      lines: (m) => ({ values: m.concurrency, summary: decimal(m.avg_concurrency) }),
    },
    {
      key: 'speed',
      title: t('adminOverview.perfSpeed'),
      hint: t('adminOverview.perfSpeedHint'),
      format: decimal,
      lines: (m) => ({
        values: m.tokens_per_second_series,
        summary: m.tokens_per_second > 0 ? `${decimal(m.tokens_per_second)} tok/s` : '—',
      }),
    },
    {
      key: 'input',
      title: t('adminOverview.perfInput'),
      hint: t('adminOverview.perfInputHint'),
      format: (v) => formatNumber(Math.round(v), { compact: true }),
      lines: (m) => ({
        values: m.input_tokens_series,
        summary: m.avg_input_tokens > 0 ? `${formatNumber(Math.round(m.avg_input_tokens), { compact: true })} tok` : '—',
      }),
    },
    {
      key: 'ttft',
      title: t('adminOverview.perfTTFT'),
      hint: t('adminOverview.perfTTFTHint'),
      format: formatDuration,
      empty: t('adminOverview.perfNoStreamed'),
      lines: (m) => ({
        values: m.ttft_ms_series ?? [],
        summary: m.avg_ttft_ms != null ? formatDuration(m.avg_ttft_ms) : '—',
      }),
    },
    {
      key: 'output',
      title: t('adminOverview.perfOutput'),
      hint: t('adminOverview.perfOutputHint'),
      format: (v) => formatNumber(Math.round(v), { compact: true }),
      lines: (m) => ({
        values: m.output_tokens_series ?? [],
        summary: m.avg_output_tokens != null ? `${formatNumber(Math.round(m.avg_output_tokens), { compact: true })} tok` : '—',
      }),
    },
    {
      key: 'cache',
      title: t('adminOverview.perfCache'),
      hint: t('adminOverview.perfCacheHint'),
      // Plotted as 0..100 so the axis reads in whole percents.
      format: (v) => `${Number(v.toFixed(1))}%`,
      empty: t('adminOverview.perfNoCache'),
      lines: (m) => ({
        values: (m.cache_hit_rate_series ?? []).map((v) => (v == null ? null : v * 100)),
        summary: !m.cache_reported
          ? t('adminOverview.perfNotReported')
          : m.cache_hit_rate != null
            ? percent(m.cache_hit_rate)
            : '—',
      }),
    },
  ];
  return (
    <section className="card" data-testid="model-performance">
      <div className="card-header">
        <h2>{t('adminOverview.modelPerformance')}</h2>
        <div className="row" style={{ gap: 'var(--janus-space-3)' }}>
          {(perf.available?.length ?? 0) > 0 ? (
            <ModelPicker available={perf.available ?? []} chosen={chosen} onChoose={onChoose} />
          ) : null}
          <Link className="small" to="/admin/models">
            {t('adminOverview.manageModels')}
          </Link>
        </div>
      </div>
      {models.length === 0 ? (
        <p className="small muted">{t('adminOverview.perfNoData')}</p>
      ) : (
        <>
          <p className="small muted">
            {t(chosen.length > 0 ? 'adminOverview.perfChosenSubtitle' : 'adminOverview.modelPerformanceSubtitle', {
              count: models.length,
            })}
          </p>
          <div className="perf-grid">
            {charts.map((chart) => (
              <div key={chart.key} className="perf-chart">
                <h3>{chart.title}</h3>
                <p className="small muted perf-hint">{chart.hint}</p>
                <LineChart
                  label={chart.title}
                  height={150}
                  format={chart.format}
                  emptyLabel={chart.empty}
                  series={models.map((m) => ({ key: m.key, label: perfLabel(m, models), ...chart.lines(m) }))}
                />
              </div>
            ))}
          </div>
        </>
      )}
    </section>
  );
}

/** Most an admin may plot at once; more overlaid lines cannot be told apart (the server caps it too). */
const MAX_CHOSEN_MODELS = 8;

/**
 * Picks which models the performance charts plot. Nothing checked means the
 * most-used models; the choice lives in the URL (?models=) so a view can be
 * shared.
 */
function ModelPicker({
  available,
  chosen,
  onChoose,
}: {
  available: ModelPerformanceChoice[];
  chosen: string[];
  onChoose: (ids: string[]) => void;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (event: MouseEvent | KeyboardEvent) => {
      if (event instanceof KeyboardEvent ? event.key === 'Escape' : !ref.current?.contains(event.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener('mousedown', close);
    document.addEventListener('keydown', close);
    return () => {
      document.removeEventListener('mousedown', close);
      document.removeEventListener('keydown', close);
    };
  }, [open]);
  const toggle = (id: string) =>
    onChoose(chosen.includes(id) ? chosen.filter((c) => c !== id) : [...chosen, id].slice(0, MAX_CHOSEN_MODELS));
  const full = chosen.length >= MAX_CHOSEN_MODELS;
  return (
    <div ref={ref} style={{ position: 'relative' }}>
      <button type="button" className="btn btn-sm" aria-haspopup="true" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
        {t('adminOverview.perfPick')}: {chosen.length > 0 ? formatNumber(chosen.length) : t('adminOverview.perfMostUsed')}
      </button>
      {open ? (
        <div className="menu perf-picker" role="group" aria-label={t('adminOverview.perfPickLabel')}>
          <div className="row-between perf-picker-head">
            <span className="small muted">{t('adminOverview.perfPickHint', { count: MAX_CHOSEN_MODELS })}</span>
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              aria-pressed={chosen.length === 0}
              onClick={() => onChoose([])}
            >
              {t('adminOverview.perfMostUsed')}
            </button>
          </div>
          <div className="perf-picker-list">
            {available.map((m) => {
              const checked = chosen.includes(m.key);
              return (
                <label key={m.key} className="perf-picker-item">
                  <input type="checkbox" checked={checked} disabled={!checked && full} onChange={() => toggle(m.key)} />
                  <span className="truncate small" title={perfLabel(m, available)}>
                    {perfLabel(m, available)}
                  </span>
                  <span className="small muted num">{formatNumber(m.requests, { compact: true })}</span>
                </label>
              );
            })}
          </div>
        </div>
      ) : null}
    </div>
  );
}

/** Model name, plus its upstream when another listed model shares the name. */
function perfLabel(
  m: Pick<ModelPerformanceChoice, 'key' | 'model_name' | 'upstream_name'>,
  all: Array<Pick<ModelPerformanceChoice, 'key' | 'model_name'>>,
): string {
  const clash = all.some((other) => other.key !== m.key && other.model_name === m.model_name);
  return clash && m.upstream_name ? `${m.model_name} · ${m.upstream_name}` : m.model_name;
}

type LinePick = (m: ModelPerformanceSeries['models'][number]) => { values: (number | null)[]; summary: string };

function Tile({ label, value, meta }: { label: string; value: string; meta?: string }): ReactNode {
  return (
    <article className="tile">
      <div className="overline">{label}</div>
      <div className="tile-value">{value}</div>
      {meta ? <div className="tile-meta">{meta}</div> : null}
    </article>
  );
}
