import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import { isRenamedModel, publicModelName, type GlobalDashboard } from '../lib/types';
import { formatDate, formatNumber, formatUSD } from '../lib/format';
import { cacheMeta } from '../lib/cache';
import { AreaChart, BarList } from '../components/charts';
import { AsyncSection, Badge } from '../components/ui';
import { useUrlState } from '../lib/hooks';
import { t } from '../lib/i18n';
import { useDefaultMetric, useLocalOnly, type DashboardMetric } from '../app/session';
import { TokenDirectionPicker, tokenDirectionSeries, type TokenDirection } from './shared';

// tokenDirectionSeries moved to routes/shared.tsx on main so the Dashboard,
// Explore and admin Overview charts share one implementation; the local copy
// that lived here is therefore dropped.

export function ExplorePage(): ReactNode {
  const localOnly = useLocalOnly();
  // The instance-wide emphasis setting (spend vs usage) picks the metric this
  // page opens on via useDefaultMetric() (session feature_flags.spend_emphasis);
  // an explicit ?metric= in the URL or a selector click overrides it. In
  // local-only mode there is no spend surface, so cost degrades to requests
  // (matching the single-series chart that mode's tests pin).
  const [rawMetric, setMetric] = useUrlState<DashboardMetric>('metric', useDefaultMetric());
  const metric = localOnly && rawMetric === 'cost' ? 'requests' : rawMetric;
  const [direction, setDirection] = useUrlState<TokenDirection>('direction', 'total');

  const pulse = useQuery({
    queryKey: ['dashboard', 'global', 'page'],
    queryFn: () => api.get<GlobalDashboard>('/api/v1/dashboard/global'),
    refetchInterval: 10_000,
  });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('explore.title')}</h1>
          <p className="page-subtitle">{t('explore.subtitle')}</p>
        </div>
        {/* Segmented metric selector: the pressed segment starts on the
            emphasis default above and a click re-writes ?metric=, which then
            wins over the instance-wide default for this view. */}
        <div className="segmented" role="group" aria-label={t('explore.metric')}>
          {(localOnly ? (['requests', 'tokens'] as const) : (['requests', 'tokens', 'cost'] as const)).map((key) => (
            <button key={key} type="button" aria-pressed={metric === key} onClick={() => setMetric(key)}>
              {key === 'cost' ? t('dashboard.spend') : key === 'tokens' ? t('tables.tokens') : t('dashboard.requests')}
            </button>
          ))}
        </div>
      </header>

      <AsyncSection query={pulse}>
        {(data) => (
          <>
            <div className="grid grid-tiles">
              <article className="tile">
                <div className="overline">{t('explore.activeUsers')}</div>
                <div className="tile-value">{formatNumber(data.active_users_15m)}</div>
                <div className="tile-meta">{t('explore.last15m')}</div>
              </article>
              <article className="tile">
                <div className="overline">{t('explore.requests30d')}</div>
                <div className="tile-value">{formatNumber(data.totals_30d.request_count, { compact: true })}</div>
                <div className="tile-meta">{t('explore.errorsCount', { count: formatNumber(data.totals_30d.error_count) })}</div>
              </article>
              <article className="tile">
                <div className="overline">{t('explore.tokens30d')}</div>
                <div className="tile-value">
                  {formatNumber(data.totals_30d.tokens_in + data.totals_30d.tokens_out, { compact: true })}
                </div>
                <div className="tile-meta">
                  {t('explore.tokensSplit', {
                    tokensIn: formatNumber(data.totals_30d.tokens_in),
                    tokensOut: formatNumber(data.totals_30d.tokens_out),
                  })}
                </div>
                {cacheMeta(data.totals_30d) ? <div className="tile-meta">{cacheMeta(data.totals_30d)}</div> : null}
              </article>
              {localOnly ? null : (
                <article className="tile">
                  <div className="overline">{t('explore.spend30d')}</div>
                  <div className="tile-value">{formatUSD(data.totals_30d.cost_nanousd, { compact: true })}</div>
                  <div className="tile-meta">{t('explore.acrossUpstreams')}</div>
                </article>
              )}
            </div>

            <section className="card">
              <div className="card-header">
                <div>
                  <h2>{t('explore.liveThroughput')}</h2>
                  <p className="small muted" style={{ margin: 0 }}>
                    {t('explore.liveThroughputSubtitle')}
                  </p>
                </div>
                {metric === 'tokens' ? <TokenDirectionPicker value={direction} onChange={setDirection} /> : null}
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
                  <h2>{t('explore.mostUsedModels')}</h2>
                </div>
                <BarList items={data.top_models} metric={metric} emptyLabel={t('explore.noOrgUsage')} />
              </section>

              <section className="card">
                <div className="card-header">
                  <h2>{t('explore.recentlyAvailable')}</h2>
                  <Link className="small" to="/models">
                    Browse models available to you
                  </Link>
                </div>
                <p className="small muted">
                  Latest {data.new_models.length} enabled models (up to 5), newest discovery first. Availability to your account
                  may differ.
                </p>
                {data.new_models.length === 0 ? (
                  <p className="small muted">{t('explore.noModelsEnabled')}</p>
                ) : (
                  <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0 }}>
                    {data.new_models.map((model) => (
                      <li key={model.id} className="row-between">
                        <span>
                          <span
                            style={{ fontWeight: 500 }}
                            title={isRenamedModel(model) ? t('adminModels.upstreamNameTitle', { name: model.name }) : undefined}
                          >
                            {publicModelName(model)}
                          </span>{' '}
                          <span className="small muted">{t('explore.onUpstream', { name: model.upstream_name })}</span>
                        </span>
                        <span className="small muted">{formatDate(model.discovered_at)}</span>
                      </li>
                    ))}
                  </ul>
                )}
              </section>
            </div>

            {data.leaderboards_enabled ? (
              <div className="grid grid-halves">
                <section className="card">
                  <div className="card-header">
                    <h2>{t('explore.topUsers')}</h2>
                    <Badge tone="warning">{t('explore.namesVisible')}</Badge>
                  </div>
                  {/* Ranked by tokens_out on main, so this leaderboard is
                      already cost-free and needs no local-only variant beyond
                      its empty-state wording. */}
                  <BarList
                    items={data.top_users ?? []}
                    metric="tokens_out"
                    emptyLabel={localOnly ? t('explore.noOrgUsage') : t('explore.noSpend')}
                    max={5}
                  />
                </section>
                <section className="card">
                  <div className="card-header">
                    <h2>{t('explore.topTeams')}</h2>
                    <Badge tone="warning">{t('explore.namesVisible')}</Badge>
                  </div>
                  <BarList
                    items={data.top_teams ?? []}
                    metric="tokens_out"
                    emptyLabel={localOnly ? t('explore.noOrgUsage') : t('explore.noTeamSpend')}
                    max={5}
                  />
                </section>
              </div>
            ) : null}
          </>
        )}
      </AsyncSection>
    </div>
  );
}
