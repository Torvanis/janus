import type { ReactNode } from 'react';
import { sortCollection } from '../lib/collections';
import { Link } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api, qs } from '../lib/api';
import type { PersonalDashboard, QuotaStatus } from '../lib/types';
import { formatDateTime, formatDuration, formatNumber, formatUSD, statusTone } from '../lib/format';
import { cacheMeta } from '../lib/cache';
import { AreaChart, BarList, DonutChart, QuotaRing } from '../components/charts';
import { AsyncSection, Badge, EmptyState, SkeletonRows } from '../components/ui';
import { ChargedTeam } from '../components/ChargedTeam';
import { useUrlState } from '../lib/hooks';
import { t } from '../lib/i18n';
import { useDefaultMetric, useLocalOnly, useMe, type DashboardMetric } from '../app/session';
import { RangePicker, TokenDirectionPicker, tokenDirectionSeries, type RangeKey, type TokenDirection } from './shared';

export function DashboardPage(): ReactNode {
  const me = useMe();
  const localOnly = useLocalOnly();
  const [range, setRange] = useUrlState<RangeKey>('range', 'day');
  // Spend vs usage emphasis (admin spend_emphasis flag) picks the default;
  // URL state and the segmented selector still override it per view.
  const [rawMetric, setMetric] = useUrlState<DashboardMetric>('metric', useDefaultMetric());
  // Local-only mode has no cost surface: a stale ?metric=cost URL (or a cost
  // emphasis default) degrades to tokens.
  const metric = localOnly && rawMetric === 'cost' ? 'tokens' : rawMetric;
  const [direction, setDirection] = useUrlState<TokenDirection>('direction', 'total');

  const dashboard = useQuery({
    queryKey: ['dashboard', 'personal', range],
    queryFn: () => api.get<PersonalDashboard>(`/api/v1/dashboard/personal${qs({ range })}`),
    refetchInterval: 10_000,
  });

  const quotas = useQuery({
    queryKey: ['dashboard', 'quota'],
    queryFn: () => api.get<{ quotas: QuotaStatus[] }>('/api/v1/dashboard/quota'),
    refetchInterval: 15_000,
  });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">
            {t('dashboard.welcome', { name: me?.name?.split(' ')[0] ?? t('dashboard.welcomeFallbackName') })}
          </h1>
          <p className="page-subtitle">{t('dashboard.subtitle')}</p>
        </div>
        <RangePicker value={range} onChange={setRange} />
      </header>

      <AsyncSection
        query={dashboard}
        skeleton={
          <div className="grid grid-tiles">
            {Array.from({ length: 4 }, (_, index) => (
              <div key={index} className="tile">
                <SkeletonRows rows={2} height={26} />
              </div>
            ))}
          </div>
        }
        empty={{
          when: (data) => data.totals.request_count === 0,
          title: t('dashboard.emptyTitle'),
          body: t('dashboard.emptyBody'),
          action: (
            <div className="row">
              <Link className="btn btn-primary" to="/tokens">
                {t('dashboard.createToken')}
              </Link>
              <Link className="btn" to="/help">
                {t('dashboard.showMeHow')}
              </Link>
            </div>
          ),
        }}
      >
        {(data) => (
          <>
            <div className="grid grid-tiles">
              <MetricTile
                label={t('requests.inputTokens')}
                value={formatNumber(data.totals.tokens_in)}
                meta={cacheMeta(data.totals)}
              />
              <MetricTile
                label={t('requests.outputTokens')}
                value={formatNumber(data.totals.tokens_out)}
                meta={t('dashboard.generatedByModels')}
              />
              {localOnly ? null : (
                <MetricTile
                  label={t('dashboard.spend')}
                  value={formatUSD(data.totals.cost_nanousd)}
                  meta={t('dashboard.spendMeta')}
                />
              )}
              <MetricTile
                label={t('dashboard.requests')}
                value={formatNumber(data.totals.request_count)}
                meta={
                  data.totals.error_count > 0
                    ? t('dashboard.returnedError', { count: formatNumber(data.totals.error_count) })
                    : t('dashboard.allSucceeded')
                }
                tone={data.totals.error_count > 0 ? 'warning' : undefined}
              />
            </div>

            <section className="card">
              <div className="card-header">
                <div>
                  <h2>{t('dashboard.throughput')}</h2>
                  <p className="small muted" style={{ margin: 0 }}>
                    {t('dashboard.throughputSubtitle')}
                  </p>
                </div>
                <div className="row" style={{ gap: 'var(--janus-space-3)', flexWrap: 'wrap' }}>
                  {metric === 'tokens' ? <TokenDirectionPicker value={direction} onChange={setDirection} /> : null}
                  {/* Metric selector: opens on the emphasis default resolved
                      above; clicking a segment writes ?metric= so the choice
                      sticks for this view regardless of the instance setting.
                      Local-only mode drops 'cost' entirely — that instance has
                      no spend surface to select. */}
                  <div className="segmented" role="group" aria-label={t('dashboard.chartMetric')}>
                    {(localOnly ? (['tokens', 'requests'] as const) : (['tokens', 'requests', 'cost'] as const)).map((key) => (
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

            <div className="grid grid-halves">
              <section className="card">
                <div className="card-header">
                  <h2>{t('dashboard.quotaStatus')}</h2>
                  <Link className="small" to="/quota">
                    {t('dashboard.allQuotas')}
                  </Link>
                </div>
                <AsyncSection query={quotas} skeleton={<SkeletonRows rows={2} height={40} />}>
                  {(snapshot) => {
                    const visible = snapshot.quotas.filter((quota) => !localOnly || quota.unit !== 'usd');
                    return visible.length === 0 ? (
                      <p className="small muted">{t('dashboard.noQuotas')}</p>
                    ) : (
                      <div className="stack" style={{ gap: 'var(--janus-space-4)' }}>
                        <p className="small muted">
                          Showing the {Math.min(3, visible.length)} most utilized of {visible.length} current quotas. Open all
                          quotas for the complete list.
                        </p>
                        {sortCollection(visible, (quota) => quota.percent, false)
                          .slice(0, 3)
                          .map((quota) => (
                            <QuotaRing
                              key={quota.id}
                              percent={quota.percent}
                              label={`${quota.metric_label} ${quota.window_label}`}
                              caption={t('dashboard.quotaCaption', {
                                current:
                                  quota.unit === 'usd' ? formatUSD(quota.current_value) : formatNumber(quota.current_value),
                                limit: quota.unit === 'usd' ? formatUSD(quota.limit_value) : formatNumber(quota.limit_value),
                                time: formatDateTime(quota.reset_at),
                              })}
                            />
                          ))}
                      </div>
                    );
                  }}
                </AsyncSection>
              </section>

              <section className="card">
                <div className="card-header">
                  <h2>{t('dashboard.whereItWent')}</h2>
                </div>
                <DonutChart items={data.per_modality} metric={metric} />
              </section>
            </div>

            <div className="grid grid-halves">
              <section className="card">
                <div className="card-header">
                  <h2>{t('dashboard.topModels')}</h2>
                </div>
                <BarList items={data.per_model} metric={metric} emptyLabel={t('dashboard.noModelUsage')} />
              </section>

              <section className="card">
                <div className="card-header">
                  <h2>{t('dashboard.byToken')}</h2>
                  <Link className="small" to="/tokens">
                    {t('dashboard.manageTokens')}
                  </Link>
                </div>
                <BarList items={data.per_token} metric={metric} emptyLabel={t('dashboard.noTokenUsage')} />
              </section>
            </div>

            <section className="card card-flush">
              <div className="card-header" style={{ padding: 'var(--janus-card-padding)', marginBottom: 0 }}>
                <h2>{t('dashboard.recentRequests')}</h2>
                <Link className="small" to="/requests">
                  {t('dashboard.fullRequestLog')}
                </Link>
              </div>
              <p className="small muted" style={{ padding: '0 var(--janus-card-padding)' }}>
                Latest {data.recent_requests.length} requests across all dates (up to 5), newest first. This preview is
                independent of the dashboard time range; use the request log for filters and paging.
              </p>
              {data.recent_requests.length === 0 ? (
                <EmptyState title={t('dashboard.recentEmptyTitle')} body={t('dashboard.recentEmptyBody')} />
              ) : (
                <div className="table-wrap">
                  <table className="data">
                    <thead>
                      <tr>
                        <th scope="col">{t('tables.time')}</th>
                        <th scope="col">{t('tables.model')}</th>
                        <th scope="col">Charged to</th>
                        <th scope="col">{t('tables.modality')}</th>
                        <th scope="col">{t('tables.status')}</th>
                        <th scope="col">{t('tables.latency')}</th>
                        <th scope="col">{t('tables.tokens')}</th>
                        {localOnly ? null : <th scope="col">{t('tables.cost')}</th>}
                      </tr>
                    </thead>
                    <tbody>
                      {data.recent_requests.map((event) => (
                        <tr key={event.id}>
                          <td className="small muted">{formatDateTime(event.created_at)}</td>
                          <td className="truncate">{event.model || '—'}</td>
                          <td className="small">
                            <ChargedTeam event={event} />
                          </td>
                          <td className="small">{event.modality}</td>
                          <td>
                            <Badge tone={statusTone(event.http_status)}>{event.http_status}</Badge>
                          </td>
                          <td className="num small">{formatDuration(event.latency_ms)}</td>
                          <td className="num small">
                            {formatNumber(event.tokens_in)} / {formatNumber(event.tokens_out)}
                          </td>
                          {localOnly ? null : <td className="num small">{formatUSD(event.cost_nanousd)}</td>}
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </section>
          </>
        )}
      </AsyncSection>
    </div>
  );
}

function MetricTile({
  label,
  value,
  meta,
  tone,
}: {
  label: string;
  value: string;
  meta?: string;
  tone?: 'warning' | 'danger';
}): ReactNode {
  return (
    <article className="tile">
      <div className="overline">{label}</div>
      <div className="tile-value">{value}</div>
      {meta ? (
        <div className="tile-meta" style={tone ? { color: `var(--janus-color-${tone}-fg)` } : undefined}>
          {meta}
        </div>
      ) : null}
    </article>
  );
}
