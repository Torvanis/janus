import type { ReactNode } from 'react';
import { Collection } from '../../components/Collection';
import { sortCollection } from '../../lib/collections';
import { Link } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api, qs } from '../../lib/api';
import type { Breakdown, ModelPerformanceSeries, QuotaStatus, TimePoint, Totals } from '../../lib/types';
import { formatNumber, formatUSD } from '../../lib/format';
import { cacheMeta } from '../../lib/cache';
import { AreaChart, BarList, LineChart } from '../../components/charts';
import { AsyncSection, Badge } from '../../components/ui';
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

  const overview = useQuery({
    queryKey: ['admin', 'overview', range],
    queryFn: () => api.get<AdminOverviewData>(`/api/v1/admin/overview${qs({ range })}`),
    refetchInterval: 20_000,
  });

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

      <AsyncSection query={overview}>
        {(data) => (
          <>
            {(data.model_counts.pending_approval ?? 0) > 0 ? (
              <div className="banner banner-info">
                <div className="row-between" style={{ width: '100%' }}>
                  <span>
                    <strong>{t('adminOverview.pendingBanner', { count: data.model_counts.pending_approval ?? 0 })}</strong>{' '}
                    {t(localOnly ? 'adminOverview.pendingBannerBodyLocalOnly' : 'adminOverview.pendingBannerBody')}
                  </span>
                  <Link className="btn btn-sm" to="/admin/models?status=pending_approval">
                    {t('adminOverview.reviewNow')}
                  </Link>
                </div>
              </div>
            ) : null}

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
              {metric === 'tokens' ? (
                <AreaChart series={tokenDirectionSeries(data.series, direction)} metric="tokens" height={200} />
              ) : (
                <AreaChart series={data.series} metric={metric} height={200} />
              )}
            </section>

            <ModelPerformanceCard perf={data.model_performance} />

            {(data.top_teams?.length ?? 0) > 0 ? (
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
            ) : null}

            <div className="grid grid-halves">
              {localOnly ? null : (
                <section className="card">
                  <div className="card-header">
                    <h2>{t('adminOverview.topSpenders')}</h2>
                    <Link className="small" to="/admin/users">
                      {t('adminOverview.allPeople')}
                    </Link>
                  </div>
                  <BarList items={data.top_spenders} metric="tokens_out" emptyLabel={t('adminOverview.noAttributedSpend')} />
                </section>
              )}

              <section className="card">
                <div className="card-header">
                  <h2>{t('adminOverview.mostUsedModels')}</h2>
                  <Link className="small" to="/admin/models">
                    {t('adminOverview.manageModels')}
                  </Link>
                </div>
                <BarList items={data.top_models} metric={metric} emptyLabel={t('adminOverview.noModelUsage')} />
              </section>
            </div>

            <div className="grid grid-halves">
              <section className="card">
                <div className="card-header">
                  <h2>{t('adminOverview.outcomes')}</h2>
                </div>
                <BarList items={data.by_status} metric="requests" emptyLabel={t('adminOverview.noRequests')} />
              </section>

              <section className="card">
                <div className="card-header">
                  <h2>{t('adminOverview.nearBreach')}</h2>
                  <Link className="small" to="/admin/quotas">
                    {t('adminOverview.manageQuotas')}
                  </Link>
                </div>
                {data.near_breach.length === 0 ? (
                  <p className="small muted">{t('adminOverview.noneNearBreach')}</p>
                ) : (
                  <>
                    <p className="small muted">Current evaluated at-risk quotas; independent of the usage time range.</p>
                    <Collection
                      name="At-risk quotas"
                      rows={sortCollection(data.near_breach, (quota) => quota.percent, false)}
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
                          render: (quota) => (
                            <Badge tone={quota.breached ? 'danger' : 'warning'}>{Math.round(quota.percent)}%</Badge>
                          ),
                        },
                      ]}
                    />
                  </>
                )}
              </section>
            </div>
          </>
        )}
      </AsyncSection>
    </div>
  );
}

/**
 * Performance of the most-used models over the selected range: average
 * concurrency, generation speed and input context, one line per model on the
 * same buckets as the organisation trend above.
 */
function ModelPerformanceCard({ perf }: { perf: ModelPerformanceSeries | undefined }): ReactNode {
  if (!perf) return null;
  const models = perf.models;
  // Two decimals below 1 (concurrency 0.25), one below 10, whole numbers above.
  const decimal = (value: number): string => {
    if (value <= 0) return '0';
    if (value < 1) return String(Number(value.toFixed(2)));
    if (value < 10) return String(Number(value.toFixed(1)));
    return formatNumber(Math.round(value), { compact: true });
  };
  const charts: Array<{ key: string; title: string; hint: string; format: (v: number) => string; lines: LinePick }> = [
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
  ];
  return (
    <section className="card" data-testid="model-performance">
      <div className="card-header">
        <h2>{t('adminOverview.modelPerformance')}</h2>
        <Link className="small" to="/admin/models">
          {t('adminOverview.manageModels')}
        </Link>
      </div>
      {models.length === 0 ? (
        <p className="small muted">{t('adminOverview.perfNoData')}</p>
      ) : (
        <>
          <p className="small muted">{t('adminOverview.modelPerformanceSubtitle', { count: models.length })}</p>
          <div className="perf-grid">
            {charts.map((chart) => (
              <div key={chart.key} className="perf-chart">
                <h3>{chart.title}</h3>
                <p className="small muted perf-hint">{chart.hint}</p>
                <LineChart
                  label={chart.title}
                  height={150}
                  format={chart.format}
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

/** Model name, plus its upstream when another plotted model shares the name. */
function perfLabel(m: ModelPerformanceSeries['models'][number], all: ModelPerformanceSeries['models']): string {
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
