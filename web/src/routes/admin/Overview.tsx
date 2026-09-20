import type { ReactNode } from 'react';
import { Collection } from '../../components/Collection';
import { sortCollection } from '../../lib/collections';
import { Link } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api, qs } from '../../lib/api';
import type { Breakdown, QuotaStatus, TimePoint, Totals } from '../../lib/types';
import { formatNumber, formatUSD } from '../../lib/format';
import { cacheMeta } from '../../lib/cache';
import { AreaChart, BarList } from '../../components/charts';
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
          <p className="page-subtitle">{t('adminOverview.subtitle')}</p>
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
                    {t('adminOverview.pendingBannerBody')}
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

function Tile({ label, value, meta }: { label: string; value: string; meta?: string }): ReactNode {
  return (
    <article className="tile">
      <div className="overline">{label}</div>
      <div className="tile-value">{value}</div>
      {meta ? <div className="tile-meta">{meta}</div> : null}
    </article>
  );
}
