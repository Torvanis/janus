import type { ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link, Navigate, useParams } from 'react-router-dom';
import { api } from '../../lib/api';
import { formatDateTime, formatNumber, formatUSDPrecise as formatUSD } from '../../lib/format';
import { useUrlState } from '../../lib/hooks';
import { t } from '../../lib/i18n';
import { useLocalOnly } from '../../app/session';
import { AsyncSection, Badge } from '../../components/ui';
import { NotFoundPage } from '../Errors';
import { RangePicker, type RangeKey } from '../shared';
import { AdminModelsPage } from './Models';
import { ModelsTabs } from './ModelsTabs';
import './pricing-accuracy.css';
import type { PriceAccuracyCall, PriceAccuracyGroup, PriceAccuracyReport } from '../../lib/types';

/*
 * Administration → Models → Pricing accuracy.
 *
 * Some providers say what each call cost (xAI, OpenRouter, Requesty,
 * Perplexity, DeepInfra, LiteLLM). That quote is what the call is billed.
 * Janus also works out its own price for the same call from the model's
 * prices, and this page compares the two, so an administrator can see which
 * prices are wrong or missing before they matter for models that are not
 * quoted.
 */

/** /admin/models and /admin/models/:tab. */
export function AdminModelsSection(): ReactNode {
  const { tab } = useParams();
  const localOnly = useLocalOnly();
  if (!tab) return <AdminModelsPage />;
  if (tab !== 'pricing-accuracy') return <NotFoundPage />;
  if (localOnly) return <Navigate to="/admin/models" replace />;
  return <PricingAccuracyPage />;
}

function PricingAccuracyPage(): ReactNode {
  const [range, setRange] = useUrlState<RangeKey>('range', 'month');
  const report = useQuery({
    queryKey: ['admin', 'pricing-accuracy', range],
    queryFn: () => api.get<{ range: string; report: PriceAccuracyReport }>(`/api/v1/admin/pricing/accuracy?range=${range}`),
  });
  return (
    <div className="page pricing-accuracy">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminModels.title')}</h1>
          <p className="page-subtitle">
            Some providers tell Janus what each call cost: xAI, OpenRouter, Requesty, Perplexity, DeepInfra and LiteLLM. That
            amount is what the call is billed. Janus also works out its own price for the same call from the model&apos;s prices.
            Comparing the two shows which prices are wrong or missing.
          </p>
        </div>
        <RangePicker value={range} onChange={setRange} />
      </header>
      <ModelsTabs active="pricing-accuracy" />
      <AsyncSection
        query={report}
        empty={{
          when: (d) => d.report.totals.quoted_calls === 0,
          title: 'No calls with a provider price in this period',
          body: 'Calls to xAI, OpenRouter, Requesty, Perplexity, DeepInfra or LiteLLM show up here. Try a longer period.',
        }}
      >
        {(data) => <ReportBody report={data.report} />}
      </AsyncSection>
    </div>
  );
}

function ReportBody({ report }: { report: PriceAccuracyReport }): ReactNode {
  const tot = report.totals;
  const notCompared = tot.quoted_calls - tot.compared_calls;
  const sharedNames = new Set(report.models.map((m) => m.model ?? '').filter((name, i, all) => all.indexOf(name) !== i));
  return (
    <>
      <div className="grid grid-tiles">
        <Tile
          label="Calls with a provider price"
          value={formatNumber(tot.quoted_calls)}
          meta={
            notCompared > 0
              ? `${formatNumber(tot.compared_calls)} compared; ${formatNumber(notCompared)} without a Janus price on record`
              : 'All compared with Janus’s price'
          }
        />
        <Tile label="Providers charged" value={formatUSD(tot.quoted_nanousd)} meta="For the compared calls" />
        <Tile label="Janus would have charged" value={formatUSD(tot.estimate_nanousd)} meta={differenceText(tot)} />
        <Tile
          label="Calls within 1% / 5%"
          value={`${share(tot.within_1_percent, tot.compared_calls)} / ${share(tot.within_5_percent, tot.compared_calls)}`}
          meta={
            tot.no_price_calls > 0
              ? `${formatNumber(tot.no_price_calls)} ${tot.no_price_calls === 1 ? 'call' : 'calls'} with no Janus price`
              : 'Of the compared calls'
          }
        />
      </div>
      {notCompared > 0 ? (
        <p className="small muted" style={{ margin: 0 }}>
          Calls without a Janus price on record were made before Janus kept its own price beside the provider&apos;s, and the
          price history could not price them (no price was set yet, or the call was an image or video job). They count as calls
          but not in the comparison.
        </p>
      ) : null}

      <section className="card card-flush" aria-labelledby="pa-providers">
        <h2 id="pa-providers" className="pa-heading">
          By provider
        </h2>
        <p className="small muted pa-note">
          Difference is Janus&apos;s price minus what the provider charged: below zero, Janus prices the calls lower than the
          provider did.
        </p>
        <GroupTable rows={report.providers} kind="provider" sharedNames={sharedNames} />
      </section>

      <section className="card card-flush" aria-labelledby="pa-models">
        <h2 id="pa-models" className="pa-heading">
          By model
        </h2>
        <GroupTable rows={report.models} kind="model" sharedNames={sharedNames} />
      </section>

      <section className="card card-flush" aria-labelledby="pa-worst">
        <h2 id="pa-worst" className="pa-heading">
          Largest differences
        </h2>
        {report.worst.length === 0 ? (
          <p className="small muted pa-note">No compared calls in this period.</p>
        ) : (
          <WorstTable calls={report.worst} />
        )}
      </section>
    </>
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

function share(n: number, of: number): string {
  if (of === 0) return '—';
  return `${Math.round((n * 100) / of)}%`;
}

function percentText(p: number | null): string {
  if (p === null) return '';
  const abs = Math.abs(p);
  return abs < 0.05 ? '0%' : `${abs < 10 ? abs.toFixed(1) : Math.round(abs)}%`;
}

/** "3.2% less than providers charged", in words. */
function differenceText(g: { diff_nanousd: number; diff_percent: number | null; quoted_nanousd: number }): string {
  if (g.diff_nanousd === 0) return 'The same as providers charged';
  const dir = g.diff_nanousd > 0 ? 'more' : 'less';
  if (g.diff_percent === null) return `${formatUSD(Math.abs(g.diff_nanousd))} ${dir} (providers charged $0)`;
  return `${formatUSD(Math.abs(g.diff_nanousd))} (${percentText(g.diff_percent)}) ${dir} than providers charged`;
}

function DiffCell({ diff, percent }: { diff: number; percent: number | null }): ReactNode {
  if (diff === 0) return <span className="muted">Same</span>;
  const tone = percent === null || Math.abs(percent) > 5 ? 'pa-diff-far' : Math.abs(percent) > 1 ? 'pa-diff-near' : '';
  return (
    <span className={tone}>
      {diff > 0 ? '+' : '−'}
      {formatUSD(Math.abs(diff))}
      {percent !== null ? <span className="muted small"> ({percentText(percent)})</span> : null}
    </span>
  );
}

function GroupTable({
  rows,
  kind,
  sharedNames,
}: {
  rows: PriceAccuracyGroup[];
  kind: 'model' | 'provider';
  sharedNames: Set<string>;
}): ReactNode {
  return (
    <div className="table-wrap">
      <table className="data pa-table">
        <thead>
          <tr>
            <th scope="col">{kind === 'model' ? 'Model' : 'Provider'}</th>
            <th scope="col" className="num">
              Calls
            </th>
            <th scope="col" className="num">
              Provider charged
            </th>
            <th scope="col" className="num">
              Janus price
            </th>
            <th scope="col" className="num">
              Difference
            </th>
            <th scope="col" className="num">
              Within 1%
            </th>
            <th scope="col" className="num">
              Within 5%
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((g) => {
            const name = kind === 'model' ? (g.model ?? '') : g.upstream_name || 'Deleted upstream';
            const shared = kind === 'model' && sharedNames.has(name);
            return (
              <tr key={`${g.upstream_id ?? ''}/${g.model_id ?? ''}/${g.model ?? ''}`}>
                <th scope="row">
                  <div className="pa-name">
                    <span className="mono">{name}</span>
                    {kind === 'model' ? (
                      <span className="small muted">
                        {shared ? '· ' : ''}
                        {g.upstream_name || 'Deleted upstream'}
                      </span>
                    ) : null}
                  </div>
                  {kind === 'model' && g.no_price_calls > 0 ? <NoPriceFlag group={g} /> : null}
                </th>
                <td className="num" data-label="Calls">
                  {formatNumber(g.compared_calls)}
                  {g.quoted_calls > g.compared_calls ? (
                    <span className="small muted" title="Calls with a provider price but no Janus price on record">
                      {' '}
                      of {formatNumber(g.quoted_calls)}
                    </span>
                  ) : null}
                </td>
                {g.compared_calls === 0 ? (
                  <td className="muted small pa-not-compared" colSpan={3}>
                    Not compared: no Janus price on record for these calls
                  </td>
                ) : (
                  <>
                    <td className="num" data-label="Provider charged">
                      {formatUSD(g.quoted_nanousd)}
                    </td>
                    <td className="num" data-label="Janus price">
                      {formatUSD(g.estimate_nanousd)}
                    </td>
                    <td className="num" data-label="Difference">
                      <DiffCell diff={g.diff_nanousd} percent={g.diff_percent} />
                    </td>
                  </>
                )}
                <td className="num" data-label="Within 1%">
                  {share(g.within_1_percent, g.compared_calls)}
                </td>
                <td className="num" data-label="Within 5%">
                  {share(g.within_5_percent, g.compared_calls)}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

/** Janus priced some of this model's calls at $0 while the provider charged. */
function NoPriceFlag({ group }: { group: PriceAccuracyGroup }): ReactNode {
  const calls = `${formatNumber(group.no_price_calls)} ${group.no_price_calls === 1 ? 'call' : 'calls'}`;
  return (
    <div className="pa-flag">
      <Badge tone="warning">No Janus price</Badge>{' '}
      <span className="small muted">
        {group.has_price_now ? `${calls}; prices are set now` : calls}
        {!group.has_price_now && group.model_id ? (
          <>
            {' · '}
            <Link to={`/admin/models?edit=${encodeURIComponent(group.model_id)}`}>Set prices</Link>
          </>
        ) : null}
      </span>
    </div>
  );
}

function WorstTable({ calls }: { calls: PriceAccuracyCall[] }): ReactNode {
  return (
    <div className="table-wrap">
      <table className="data pa-table">
        <thead>
          <tr>
            <th scope="col">When</th>
            <th scope="col">Model</th>
            <th scope="col" className="num">
              Provider charged
            </th>
            <th scope="col" className="num">
              Janus price
            </th>
            <th scope="col" className="num">
              Difference
            </th>
            <th scope="col">
              <span className="sr-only">Request</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {calls.map((c) => (
            <tr key={c.id}>
              <td data-label="When">{formatDateTime(c.created_at)}</td>
              <th scope="row">
                <div className="pa-name">
                  <span className="mono">{c.model}</span>
                  <span className="small muted">{c.upstream_name || 'Deleted upstream'}</span>
                </div>
              </th>
              <td className="num" data-label="Provider charged">
                {formatUSD(c.quoted_nanousd)}
              </td>
              <td className="num" data-label="Janus price">
                {c.estimate_nanousd === 0 && c.quoted_nanousd > 0 ? (
                  <Badge tone="warning">No Janus price</Badge>
                ) : (
                  formatUSD(c.estimate_nanousd)
                )}
              </td>
              <td className="num" data-label="Difference">
                <DiffCell diff={c.diff_nanousd} percent={c.diff_percent} />
              </td>
              <td className="pa-link">
                {c.request_id ? (
                  <Link
                    className="small"
                    to={`/admin/requests?request_id=${encodeURIComponent(c.request_id)}${c.internal ? '&include_internal=true' : ''}`}
                  >
                    View request
                  </Link>
                ) : null}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
