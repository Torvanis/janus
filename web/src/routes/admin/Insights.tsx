import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { Link, useLocation, useParams } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import { formatDateTime, formatDuration, formatNumber, formatRelative, formatUSD } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, CopyButton, Drawer, Field, useToast } from '../../components/ui';
import { IconButton, RowActions } from '../../components/IconButton';
import { RetainedPanel, RouteTopNav } from '../../components/RouteTopNav';
import { AreaChart, seriesColor, type AreaSeries } from '../../components/charts';
import { useLocalOnly } from '../../app/session';
import { NotFoundPage } from '../Errors';
import { PerformancePanel } from './InsightsPerformance';
import { RangePicker, type RangeKey } from '../shared';
import { useUrlState } from '../../lib/hooks';
import type {
  ClassificationCategory,
  ClassificationDocument,
  ClassificationReport,
  ClassificationResult,
  ClassificationSettings,
  ClassificationTestResult,
  ClassifierUpstream,
} from '../../lib/types';

export const insightsTabs = [
  { id: 'overview', label: 'Overview' },
  { id: 'categories', label: 'Categories' },
  { id: 'classifier', label: 'Classifier' },
  { id: 'performance', label: 'Performance' },
] as const;

const RANGE_DAYS: Record<RangeKey, { days?: number; hours?: number }> = {
  day: { hours: 24 },
  week: { days: 7 },
  month: { days: 30 },
  quarter: { days: 90 },
};

function rangeQuery(range: RangeKey): string {
  const r = RANGE_DAYS[range];
  return r.hours ? `hours=${r.hours}` : `days=${r.days ?? 30}`;
}

/** Usage insights: what kinds of work flow through the gateway. */
export function InsightsPage(): ReactNode {
  const { tab = 'overview' } = useParams();
  const location = useLocation();
  if (!insightsTabs.some((item) => item.id === tab)) return <NotFoundPage />;
  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">Usage insights</h1>
          <p className="page-subtitle">
            What people use AI for. A classifier labels requests with a category; only the label is kept, never the prompt.
          </p>
        </div>
      </header>
      <RouteTopNav
        label="Usage insights"
        active={tab}
        items={insightsTabs.map((item) => ({ ...item, to: { pathname: `/admin/insights/${item.id}`, search: location.search } }))}
      />
      <RetainedPanel active={tab === 'overview'}>
        <InsightsOverview />
      </RetainedPanel>
      <RetainedPanel active={tab === 'categories'}>
        <CategoriesPanel />
      </RetainedPanel>
      <RetainedPanel active={tab === 'classifier'}>
        <ClassifierPanel />
      </RetainedPanel>
      <RetainedPanel active={tab === 'performance'}>
        <PerformancePanel />
      </RetainedPanel>
    </div>
  );
}

/* --- Overview ------------------------------------------------------------- */

function useCategories() {
  return useQuery({
    queryKey: ['classification', 'categories'],
    queryFn: () => api.get<{ categories: ClassificationCategory[] }>('/api/v1/admin/classification/categories'),
  });
}

function useCategoryNames(): (slug: string) => string {
  const cats = useCategories();
  return useMemo(() => {
    const names = new Map((cats.data?.categories ?? []).map((c) => [c.slug, c.name]));
    return (slug: string) => names.get(slug) ?? (slug ? slug.replace(/_/g, ' ') : 'Unlabelled');
  }, [cats.data]);
}

function percent(value: number): string {
  return `${Math.round(value * 100)}%`;
}

function InsightsOverview(): ReactNode {
  const [range, setRange] = useUrlState('range', 'week') as [RangeKey, (next: RangeKey) => void];
  const name = useCategoryNames();
  const localOnly = useLocalOnly();
  const settings = useQuery({
    queryKey: ['classification', 'settings'],
    queryFn: () => api.get<ClassificationDocument>('/api/v1/admin/classification'),
  });
  const report = useQuery({
    queryKey: ['classification', 'report', range],
    queryFn: () => api.get<ClassificationReport>(`/api/v1/admin/classification/report?${rangeQuery(range)}`),
    refetchInterval: 30_000,
  });
  const [open, setOpen] = useState<string | null>(null);

  return (
    <div className="stack" style={{ gap: 'var(--janus-space-5)' }}>
      <ClassifierStatusBanner doc={settings.data} />
      <div className="row-between">
        <span className="small muted">
          Counts are classified requests
          {settings.data && settings.data.settings.sample_rate < 100 ? ` (a ${settings.data.settings.sample_rate}% sample)` : ''}.
        </span>
        <RangePicker value={range} onChange={setRange} />
      </div>
      <AsyncSection query={report}>
        {(data) => {
          const top = data.categories.slice(0, 8);
          const stacked: AreaSeries[] = top.map((c, i) => ({
            key: c.category,
            label: name(c.category),
            color: seriesColor(i),
            values: data.series.map((p) => p.counts[c.category] ?? 0),
          }));
          const h = data.health;
          return (
            <>
              <div className="grid grid-tiles">
                <Tile
                  label="Classified requests"
                  value={formatNumber(data.classified, { compact: true })}
                  meta={`${data.categories.length} categories seen`}
                />
                <Tile
                  label="Top category"
                  value={data.categories[0] ? name(data.categories[0].category) : '—'}
                  meta={
                    data.categories[0] ? `${percent(data.categories[0].share)} of classified requests` : 'Nothing classified yet'
                  }
                />
                <Tile
                  label="Classifier time"
                  value={h.attempts > h.dropped ? formatDuration(h.p50_ms) : '—'}
                  meta={h.attempts > h.dropped ? `median · p95 ${formatDuration(h.p95_ms)}` : 'No calls in this range'}
                />
                <Tile
                  label="Classifier misses"
                  value={formatNumber(h.errors + h.timeouts + h.dropped)}
                  meta={`${formatNumber(h.errors)} errors · ${formatNumber(h.timeouts)} timeouts · ${formatNumber(h.dropped)} dropped`}
                />
              </div>

              <section className="card">
                <div className="card-header">
                  <h2>Categories over time</h2>
                  <span className="small muted">by {data.bucket}</span>
                </div>
                {stacked.length ? (
                  <>
                    <AreaChart series={stacked} metric="requests" height={200} label="Classified requests by category" />
                    {data.series.filter((p) => Object.keys(p.counts).length > 0).length <= 1 ? (
                      <p className="small muted" style={{ margin: 0 }}>
                        All classified requests so far fall in one {data.bucket}; the chart fills in as traffic is classified.
                      </p>
                    ) : null}
                  </>
                ) : (
                  <p className="small muted">No classified requests in this range yet.</p>
                )}
              </section>

              <section className="card card-flush">
                <div className="card-header" style={{ padding: 'var(--janus-card-padding) var(--janus-card-padding) 0' }}>
                  <h2>Categories</h2>
                  <span className="small muted">Select a row for its subcategories, models and people.</span>
                </div>
                <div className="table-wrap">
                  <table className="data">
                    <thead>
                      <tr>
                        <th scope="col">Category</th>
                        <th scope="col" className="num">
                          Requests
                        </th>
                        <th scope="col">Share</th>
                        <th scope="col" className="num">
                          Avg confidence
                        </th>
                        <th scope="col">Top subcategory</th>
                        <th scope="col">Top model</th>
                      </tr>
                    </thead>
                    {data.categories.length === 0 ? (
                      <tbody>
                        <tr>
                          <td colSpan={6} className="muted">
                            Nothing classified in this range.
                          </td>
                        </tr>
                      </tbody>
                    ) : (
                      data.categories.map((c, i) => (
                        <tbody key={c.category}>
                          <tr data-clickable="true" onClick={() => setOpen(open === c.category ? null : c.category)}>
                            <th scope="row">
                              <button
                                type="button"
                                className="insights-row-toggle"
                                aria-expanded={open === c.category}
                                onClick={(e) => {
                                  e.stopPropagation();
                                  setOpen(open === c.category ? null : c.category);
                                }}
                              >
                                <span
                                  className="insights-swatch"
                                  style={{ background: i < 8 ? seriesColor(i) : 'var(--janus-color-border-strong)' }}
                                  aria-hidden="true"
                                />
                                {name(c.category)}
                              </button>
                            </th>
                            <td className="num">{formatNumber(c.count)}</td>
                            <td>
                              <div className="insights-share">
                                <div className="meter" aria-hidden="true">
                                  <div
                                    className="meter-fill"
                                    style={{
                                      width: `${Math.max(2, c.share * 100)}%`,
                                      background: i < 8 ? seriesColor(i) : undefined,
                                    }}
                                  />
                                </div>
                                <span className="num small">{percent(c.share)}</span>
                              </div>
                            </td>
                            <td className="num">
                              {percent(c.avg_confidence)}
                              {c.low_confidence > 0 ? (
                                <span className="small muted" title="Requests labelled with under 50% confidence">
                                  {' '}
                                  · {formatNumber(c.low_confidence)} low
                                </span>
                              ) : null}
                            </td>
                            <td>
                              {c.subcategories[0] ? (
                                `${name(c.subcategories[0].key)} (${formatNumber(c.subcategories[0].count)})`
                              ) : (
                                <span className="muted">—</span>
                              )}
                            </td>
                            <td className="truncate">{c.top_models[0]?.key ?? '—'}</td>
                          </tr>
                          {open === c.category ? (
                            <tr className="insights-detail-row">
                              <td colSpan={6}>
                                <div className="grid insights-thirds">
                                  <MiniList
                                    title="Subcategories"
                                    rows={c.subcategories.map((s) => ({ key: s.key, label: name(s.key), count: s.count }))}
                                    total={c.count}
                                    empty="No subcategory picked."
                                  />
                                  <MiniList
                                    title="Models"
                                    rows={c.top_models.map((s) => ({ key: s.key, label: s.key, count: s.count }))}
                                    total={c.count}
                                  />
                                  <MiniList
                                    title="People and service tokens"
                                    rows={c.top_people.map((s) => ({ key: s.key, label: s.label || 'Unknown', count: s.count }))}
                                    total={c.count}
                                  />
                                </div>
                              </td>
                            </tr>
                          ) : null}
                        </tbody>
                      ))
                    )}
                  </table>
                </div>
              </section>

              {data.by_team.length ? <TeamMix rows={data.by_team} categories={top.map((c) => c.category)} name={name} /> : null}

              <section className="card">
                <div className="card-header">
                  <h2>Classifier performance</h2>
                  <span className="small muted">Time spent at the classifier for each request, excluding queue wait.</span>
                </div>
                <div className="grid grid-tiles">
                  <Tile
                    label="Classifications"
                    value={formatNumber(h.attempts)}
                    meta={`${formatNumber(h.ok)} labelled · ${formatNumber(h.calls ?? h.attempts)} classifier requests`}
                  />
                  <Tile
                    label="Average"
                    value={h.attempts > h.dropped ? formatDuration(h.avg_ms) : '—'}
                    meta={`max ${formatDuration(h.max_ms)}`}
                  />
                  <Tile label="Queue wait" value={formatDuration(h.avg_queue_ms)} meta="average, async mode" />
                  {localOnly ? (
                    <Tile label="Classifier tokens" value={formatNumber(h.tokens_in + h.tokens_out, { compact: true })} />
                  ) : (
                    <Tile
                      label="Classifier cost"
                      value={formatUSD(h.cost_nanousd)}
                      meta={`${formatNumber(h.tokens_in + h.tokens_out, { compact: true })} tokens`}
                    />
                  )}
                </div>
                {h.by_classifier.length ? (
                  <div className="table-wrap" style={{ marginTop: 'var(--janus-space-4)' }}>
                    <table className="data">
                      <thead>
                        <tr>
                          <th scope="col">Classifier</th>
                          <th scope="col">Mode</th>
                          <th scope="col" className="num">
                            Calls
                          </th>
                          <th scope="col" className="num">
                            Average
                          </th>
                          <th scope="col" className="num">
                            Median
                          </th>
                          <th scope="col" className="num">
                            p95
                          </th>
                        </tr>
                      </thead>
                      <tbody>
                        {h.by_classifier.map((c) => (
                          <tr key={`${c.classifier}-${c.mode}`}>
                            <td>{c.classifier}</td>
                            <td>{c.mode === 'inline' ? 'Inline' : 'Background'}</td>
                            <td className="num">{formatNumber(c.count)}</td>
                            <td className="num">{formatDuration(c.avg_ms)}</td>
                            <td className="num">{formatDuration(c.p50_ms)}</td>
                            <td className="num">{formatDuration(c.p95_ms)}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                ) : null}
              </section>
              <RecentResults range={range} name={name} />
            </>
          );
        }}
      </AsyncSection>
    </div>
  );
}

function ClassifierStatusBanner({ doc }: { doc?: ClassificationDocument }): ReactNode {
  if (!doc) return null;
  if (!doc.settings.enabled) {
    return (
      <div className="banner banner-info">
        <div className="row-between" style={{ width: '100%' }}>
          <span>Classification is off. Choose a classifier to start labelling requests.</span>
          <Link className="btn btn-sm" to="/admin/insights/classifier">
            Set up classifier
          </Link>
        </div>
      </div>
    );
  }
  if (!doc.running) {
    return (
      <div className="banner banner-warning">
        <div className="row-between" style={{ width: '100%' }}>
          <span>
            <strong>Classification is on but not running.</strong> {doc.problem}
          </span>
          <Link className="btn btn-sm" to="/admin/insights/classifier">
            Fix classifier
          </Link>
        </div>
      </div>
    );
  }
  return null;
}

function MiniList({
  title,
  rows,
  total,
  empty = 'Nothing yet.',
}: {
  title: string;
  rows: Array<{ key: string; label: string; count: number }>;
  total: number;
  empty?: string;
}): ReactNode {
  return (
    <div>
      <h3 className="overline" style={{ marginBottom: 'var(--janus-space-2)' }}>
        {title}
      </h3>
      {rows.length === 0 ? (
        <p className="small muted">{empty}</p>
      ) : (
        <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0, gap: 'var(--janus-space-2)' }}>
          {rows.map((r) => (
            <li key={r.key} className="row-between small">
              <span className="truncate" title={r.label}>
                {r.label}
              </span>
              <span className="num muted">
                {formatNumber(r.count)} · {total ? percent(r.count / total) : '—'}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function TeamMix({
  rows,
  categories,
  name,
}: {
  rows: ClassificationReport['by_team'];
  categories: string[];
  name: (slug: string) => string;
}): ReactNode {
  return (
    <section className="card">
      <div className="card-header">
        <h2>Category mix by team</h2>
        <span className="small muted">Each bar is one team's classified requests, split by category.</span>
      </div>
      <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0, gap: 'var(--janus-space-3)' }}>
        {rows.map((team) => {
          const other = team.total - categories.reduce((sum, c) => sum + (team.counts[c] ?? 0), 0);
          return (
            <li key={team.team_id}>
              <div className="row-between small" style={{ marginBottom: 4 }}>
                <span className="truncate">{team.team || 'Unknown team'}</span>
                <span className="num muted">{formatNumber(team.total)}</span>
              </div>
              <div
                className="insights-stacked-bar"
                role="img"
                aria-label={`${team.team}: ${categories
                  .filter((c) => team.counts[c])
                  .map((c) => `${name(c)} ${team.counts[c]}`)
                  .join(', ')}`}
              >
                {categories.map((c, i) =>
                  team.counts[c] ? (
                    <span
                      key={c}
                      title={`${name(c)}: ${team.counts[c]}`}
                      style={{ flexGrow: team.counts[c], background: seriesColor(i) }}
                    />
                  ) : null,
                )}
                {other > 0 ? (
                  <span
                    title={`Other categories: ${other}`}
                    style={{ flexGrow: other, background: 'var(--janus-color-border-strong)' }}
                  />
                ) : null}
              </div>
            </li>
          );
        })}
      </ul>
      <div className="row" style={{ flexWrap: 'wrap', gap: 'var(--janus-space-3)', marginTop: 'var(--janus-space-3)' }}>
        {categories.map((c, i) => (
          <span key={c} className="small row" style={{ gap: 6 }}>
            <span className="insights-swatch" style={{ background: seriesColor(i) }} aria-hidden="true" />
            {name(c)}
          </span>
        ))}
      </div>
    </section>
  );
}

const STATUS_TONE: Record<string, 'success' | 'warning' | 'danger' | 'neutral'> = {
  ok: 'success',
  timeout: 'warning',
  error: 'danger',
  dropped: 'neutral',
};

function RecentResults({ range, name }: { range: RangeKey; name: (slug: string) => string }): ReactNode {
  const [status, setStatus] = useState('');
  const results = useQuery({
    queryKey: ['classification', 'results', range, status],
    queryFn: () =>
      api.get<{ results: ClassificationResult[] }>(
        `/api/v1/admin/classification/results?${rangeQuery(range)}&limit=25${status ? `&status=${status}` : ''}`,
      ),
    refetchInterval: 30_000,
  });
  return (
    <section className="card card-flush">
      <div className="card-header" style={{ padding: 'var(--janus-card-padding) var(--janus-card-padding) 0' }}>
        <h2>Recent classifications</h2>
        <div className="segmented" role="group" aria-label="Result status">
          {[
            ['', 'All'],
            ['ok', 'Labelled'],
            ['error', 'Errors'],
            ['timeout', 'Timeouts'],
            ['dropped', 'Dropped'],
          ].map(([value, label]) => (
            <button key={value} type="button" aria-pressed={status === value} onClick={() => setStatus(value ?? '')}>
              {label}
            </button>
          ))}
        </div>
      </div>
      <AsyncSection query={results}>
        {(data) => (
          <div className="table-wrap">
            <table className="data">
              <thead>
                <tr>
                  <th scope="col">When</th>
                  <th scope="col">Category</th>
                  <th scope="col" className="num">
                    Confidence
                  </th>
                  <th scope="col">Who</th>
                  <th scope="col">Model</th>
                  <th scope="col" className="num">
                    Classifier time
                  </th>
                  <th scope="col">Request ID</th>
                </tr>
              </thead>
              <tbody>
                {data.results.length === 0 ? (
                  <tr>
                    <td colSpan={7} className="muted">
                      No results.
                    </td>
                  </tr>
                ) : (
                  data.results.map((r) => (
                    <tr key={r.id}>
                      <td className="small nowrap" title={formatDateTime(r.created_at)}>
                        {formatRelative(r.created_at)}
                      </td>
                      <td>
                        {r.status === 'ok' ? (
                          <>
                            {name(r.category)}
                            {r.subcategory ? <span className="muted"> › {name(r.subcategory)}</span> : null}
                          </>
                        ) : (
                          <span title={r.error}>
                            <Badge tone={STATUS_TONE[r.status] ?? 'neutral'}>{r.status}</Badge>{' '}
                            <span className="small muted">{r.error}</span>
                          </span>
                        )}
                      </td>
                      <td className="num">{r.status === 'ok' ? percent(r.confidence) : '—'}</td>
                      <td className="truncate">{r.user_name || (r.service_token_id ? 'Service token' : '—')}</td>
                      <td className="truncate">{r.model_name}</td>
                      <td className="num nowrap">
                        {r.status === 'dropped' ? '—' : formatDuration(r.latency_ms)}
                        {r.queue_ms > 0 ? <div className="small muted">+{formatDuration(r.queue_ms)} in queue</div> : null}
                      </td>
                      <td className="nowrap">
                        {r.request_id ? (
                          <span className="row" style={{ gap: 4 }}>
                            <span className="mono small" title={r.request_id}>
                              {r.request_id.slice(0, 8)}
                            </span>
                            <CopyButton value={r.request_id} label="Copy request ID" />
                          </span>
                        ) : (
                          '—'
                        )}
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
        )}
      </AsyncSection>
    </section>
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

/* --- Categories ----------------------------------------------------------- */

interface CategoryDraft {
  id?: string;
  name: string;
  description: string;
  parent_slug: string;
  builtin: boolean;
}

function CategoriesPanel(): ReactNode {
  const cats = useCategories();
  const doc = useQuery({
    queryKey: ['classification', 'settings'],
    queryFn: () => api.get<ClassificationDocument>('/api/v1/admin/classification'),
  });
  const queryClient = useQueryClient();
  const toast = useToast();
  const [draft, setDraft] = useState<CategoryDraft | null>(null);
  const [error, setError] = useState<{ field?: string; message: string } | null>(null);
  const [deleting, setDeleting] = useState<ClassificationCategory | null>(null);

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['classification'] });
  const save = useMutation({
    mutationFn: (d: CategoryDraft) =>
      d.id
        ? api.patch(`/api/v1/admin/classification/categories/${d.id}`, { name: d.name, description: d.description })
        : api.post('/api/v1/admin/classification/categories', {
            name: d.name,
            description: d.description,
            parent_slug: d.parent_slug,
          }),
    onSuccess: () => {
      toast(draft?.id ? 'Category saved.' : 'Category added.', 'success');
      setDraft(null);
      void invalidate();
    },
    onError: (err: unknown) => setError(apiErrorOf(err)),
  });
  const toggle = useMutation({
    mutationFn: (c: ClassificationCategory) =>
      api.patch(`/api/v1/admin/classification/categories/${c.id}`, { enabled: !c.enabled }),
    onSuccess: () => void invalidate(),
    onError: (err: unknown) => toast(apiErrorOf(err).message, 'danger'),
  });
  const remove = useMutation({
    mutationFn: (c: ClassificationCategory) => api.del(`/api/v1/admin/classification/categories/${c.id}`),
    onSuccess: () => {
      toast('Category deleted.', 'success');
      setDeleting(null);
      void invalidate();
    },
    onError: (err: unknown) => toast(apiErrorOf(err).message, 'danger'),
  });

  const all = cats.data?.categories ?? [];
  const roots = all.filter((c) => !c.parent_slug);
  const enabledRoots = roots.filter((c) => c.enabled).length;
  const limit = doc.data?.limits.top_level ?? 26;

  return (
    <div className="stack" style={{ gap: 'var(--janus-space-4)' }}>
      <div className="row-between">
        <p className="small muted" style={{ margin: 0 }}>
          The classifier picks one category per request, then a subcategory when the category has any. Descriptions are what the
          classifier reads, so say what belongs and what does not. {enabledRoots} of {limit} top-level categories enabled.
        </p>
        <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
          <button
            type="button"
            className="btn"
            onClick={() => {
              setError(null);
              setDraft({ name: '', description: '', parent_slug: 'coding', builtin: false });
            }}
          >
            Add subcategory
          </button>
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => {
              setError(null);
              setDraft({ name: '', description: '', parent_slug: '', builtin: false });
            }}
          >
            Add category
          </button>
        </div>
      </div>
      <AsyncSection query={cats}>
        {() => (
          <section className="card card-flush">
            <div className="table-wrap">
              <table className="data">
                <thead>
                  <tr>
                    <th scope="col">Category</th>
                    <th scope="col">What belongs here</th>
                    <th scope="col">Source</th>
                    <th scope="col">Enabled</th>
                    <th scope="col">
                      <span className="sr-only">Actions</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {all.map((c) => (
                    <tr key={c.id} className={c.parent_slug ? 'insights-child' : undefined}>
                      <th scope="row">
                        {c.parent_slug ? (
                          <span className="muted" aria-hidden="true">
                            ↳{' '}
                          </span>
                        ) : null}
                        {c.name}
                      </th>
                      <td className="small secondary">{c.description}</td>
                      <td>{c.builtin ? <Badge>Built-in</Badge> : <Badge tone="info">Custom</Badge>}</td>
                      <td>
                        <label className="switch">
                          <input
                            type="checkbox"
                            aria-label={`${c.name} enabled`}
                            checked={c.enabled}
                            disabled={c.slug === 'other' || toggle.isPending}
                            onChange={() => toggle.mutate(c)}
                          />
                        </label>
                      </td>
                      <td>
                        <RowActions>
                          <IconButton
                            label={`Edit ${c.name}`}
                            icon="edit"
                            onClick={() => {
                              setError(null);
                              setDraft({
                                id: c.id,
                                name: c.name,
                                description: c.description,
                                parent_slug: c.parent_slug,
                                builtin: c.builtin,
                              });
                            }}
                          />
                          {!c.builtin ? (
                            <IconButton label={`Delete ${c.name}`} icon="delete" danger onClick={() => setDeleting(c)} />
                          ) : null}
                        </RowActions>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </section>
        )}
      </AsyncSection>

      <Drawer
        open={draft !== null}
        onClose={() => setDraft(null)}
        title={draft?.id ? `Edit ${draft.name}` : draft?.parent_slug ? 'Add subcategory' : 'Add category'}
        description={
          draft?.builtin
            ? 'Built-in category: the name and description can change; its identifier stays, so reports keep their history.'
            : undefined
        }
        footer={
          <div className="row" style={{ justifyContent: 'flex-end', gap: 'var(--janus-space-2)' }}>
            <button type="button" className="btn btn-ghost" onClick={() => setDraft(null)}>
              Cancel
            </button>
            <button
              type="button"
              className="btn btn-primary"
              disabled={save.isPending || !draft?.name.trim() || !draft?.description.trim()}
              onClick={() => draft && save.mutate(draft)}
            >
              {draft?.id ? 'Save' : 'Add'}
            </button>
          </div>
        }
      >
        {draft ? (
          <div className="stack" style={{ gap: 'var(--janus-space-4)' }}>
            {error && !error.field ? <div className="banner banner-danger">{error.message}</div> : null}
            {!draft.id ? (
              <Field label="Kind">
                <select
                  className="select"
                  aria-label="Kind"
                  value={draft.parent_slug ? 'sub' : 'top'}
                  onChange={(e) =>
                    setDraft({
                      ...draft,
                      parent_slug: e.target.value === 'sub' ? (roots.find((r) => r.slug !== 'other')?.slug ?? '') : '',
                    })
                  }
                >
                  <option value="top">Top-level category</option>
                  <option value="sub">Subcategory of an existing category</option>
                </select>
              </Field>
            ) : null}
            {!draft.id && draft.parent_slug ? (
              <Field
                label="Parent category"
                error={error?.field === 'parent_slug' ? error.message : undefined}
                hint={
                  roots.find((r) => r.slug === draft.parent_slug)?.enabled === false
                    ? 'This category is disabled, so its subcategories are not asked until it is enabled again.'
                    : undefined
                }
              >
                <select
                  className="select"
                  aria-label="Parent category"
                  value={draft.parent_slug}
                  onChange={(e) => setDraft({ ...draft, parent_slug: e.target.value })}
                >
                  {roots
                    .filter((r) => r.slug !== 'other')
                    .map((r) => (
                      <option key={r.slug} value={r.slug}>
                        {r.name}
                        {r.enabled ? '' : ' (disabled)'}
                      </option>
                    ))}
                </select>
              </Field>
            ) : null}
            <Field label="Name" required error={error?.field === 'name' || error?.field === 'slug' ? error.message : undefined}>
              <input
                className="input"
                aria-label="Name"
                value={draft.name}
                maxLength={80}
                placeholder={draft.parent_slug ? 'e.g. Rust, Reverse engineering' : 'e.g. Clinical trials'}
                onChange={(e) => setDraft({ ...draft, name: e.target.value })}
              />
            </Field>
            <Field
              label="What belongs here"
              required
              hint="The classifier reads this to decide. Say what belongs and, if a neighbouring category is easy to confuse, what does not."
              error={error?.field === 'description' || error?.field === 'enabled' ? error.message : undefined}
            >
              <textarea
                className="textarea"
                aria-label="What belongs here"
                rows={4}
                maxLength={600}
                value={draft.description}
                onChange={(e) => setDraft({ ...draft, description: e.target.value })}
              />
            </Field>
          </div>
        ) : null}
      </Drawer>

      <ConfirmDialog
        open={deleting !== null}
        title={`Delete ${deleting?.name ?? ''}?`}
        consequence={
          deleting?.parent_slug
            ? 'New requests will no longer be labelled with this subcategory. Requests already labelled keep their label in reports.'
            : 'This also deletes its subcategories. New requests will no longer be labelled with it; requests already labelled keep their label in reports.'
        }
        confirmLabel="Delete category"
        danger
        busy={remove.isPending}
        onConfirm={() => deleting && remove.mutate(deleting)}
        onClose={() => setDeleting(null)}
      />
    </div>
  );
}

function apiErrorOf(err: unknown): { field?: string; message: string } {
  if (err instanceof ApiError) return { field: err.payload.param, message: err.payload.message };
  return { message: err instanceof Error ? err.message : 'Something went wrong.' };
}

/* --- Classifier ----------------------------------------------------------- */

const SCOPES = [
  { value: 'last_user', label: 'Latest user message' },
  { value: 'all_user', label: 'Every user message' },
  { value: 'conversation', label: 'Whole conversation' },
];

function ClassifierPanel(): ReactNode {
  const doc = useQuery({
    queryKey: ['classification', 'settings'],
    queryFn: () => api.get<ClassificationDocument>('/api/v1/admin/classification'),
  });
  return <AsyncSection query={doc}>{(data) => <ClassifierForm doc={data} />}</AsyncSection>;
}

function ClassifierForm({ doc }: { doc: ClassificationDocument }): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const localOnly = useLocalOnly();
  const [draft, setDraft] = useState<ClassificationSettings>(doc.settings);
  const [error, setError] = useState<{ field?: string; message: string } | null>(null);
  const [sample, setSample] = useState(
    'Our Rust service panics with "already borrowed: BorrowMutError" when two requests hit the cache at once. How do I fix it?',
  );
  const [trial, setTrial] = useState<ClassificationTestResult | null>(null);
  useEffect(() => setDraft(doc.settings), [doc.settings]);

  const upstream: ClassifierUpstream | undefined = doc.upstreams.find((u) => u.id === draft.upstream_id);
  const modelOptions = upstream ? (draft.protocol === 'systemone' ? upstream.decision_models : upstream.models) : [];
  const dirty = JSON.stringify({ ...draft, updated_at: '' }) !== JSON.stringify({ ...doc.settings, updated_at: '' });

  const save = useMutation({
    mutationFn: (s: ClassificationSettings) => api.put<ClassificationDocument>('/api/v1/admin/classification', s),
    onSuccess: (next) => {
      queryClient.setQueryData(['classification', 'settings'], next);
      void queryClient.invalidateQueries({ queryKey: ['classification'] });
      setError(null);
      toast(
        next.settings.enabled
          ? next.running
            ? 'Classifier saved and running.'
            : 'Saved, but not running yet.'
          : 'Classification turned off.',
        'success',
      );
    },
    onError: (err: unknown) => setError(apiErrorOf(err)),
  });
  const test = useMutation({
    mutationFn: () => api.post<ClassificationTestResult>('/api/v1/admin/classification/test', { text: sample, settings: draft }),
    onSuccess: (r) => setTrial(r),
    onError: (err: unknown) => {
      setTrial(null);
      toast(apiErrorOf(err).message, 'danger');
    },
  });
  const set = <K extends keyof ClassificationSettings>(key: K, value: ClassificationSettings[K]) =>
    setDraft((d) => ({ ...d, [key]: value }));
  const fieldError = (f: string) => (error?.field === f ? error.message : undefined);

  return (
    <div className="grid grid-halves" style={{ alignItems: 'start' }}>
      <section className="card stack" style={{ gap: 'var(--janus-space-4)' }}>
        <div className="row-between">
          <h2>Classifier</h2>
          {doc.settings.enabled ? (
            doc.running ? (
              <Badge tone="success" dot>
                Running
              </Badge>
            ) : (
              <Badge tone="warning" dot>
                Not running
              </Badge>
            )
          ) : (
            <Badge>Off</Badge>
          )}
        </div>
        {doc.settings.enabled && !doc.running && doc.problem ? <div className="banner banner-warning">{doc.problem}</div> : null}
        {error && !error.field ? <div className="banner banner-danger">{error.message}</div> : null}

        <label className="switch">
          <input
            type="checkbox"
            aria-label="Classify requests"
            checked={draft.enabled}
            onChange={(e) => set('enabled', e.target.checked)}
          />
          <span>Classify requests</span>
        </label>

        <Field
          label="How it runs"
          hint={
            draft.mode === 'inline'
              ? 'Inline: the request waits for its label (at most the timeout) and the label is returned in the X-Janus-Category header, so the caller can act on it. Adds the classifier time to each sampled request.'
              : 'Background: requests are labelled after they are sent on, from a queue. Adds no latency; when the queue is full a sample is skipped and counted.'
          }
        >
          <div className="segmented" role="group" aria-label="How it runs">
            <button type="button" aria-pressed={draft.mode === 'async'} onClick={() => set('mode', 'async')}>
              Background
            </button>
            <button type="button" aria-pressed={draft.mode === 'inline'} onClick={() => set('mode', 'inline')}>
              Inline
            </button>
          </div>
        </Field>

        <Field
          label="Classifier type"
          hint={
            draft.protocol === 'systemone'
              ? 'Decision model: TypeSafe Jev, Bespoke Nimble, Laya and others on the System One API (TypeSafe, OpenRouter, Ollama, llama.cpp), or OpenAI decision models on /v1/decisions. Fast, with calibrated confidence.'
              : 'Chat model: any model Janus can reach is asked to answer in JSON. Works without a decision model; slower, and its confidence is its own estimate.'
          }
        >
          <div className="segmented" role="group" aria-label="Classifier type">
            <button type="button" aria-pressed={draft.protocol === 'systemone'} onClick={() => set('protocol', 'systemone')}>
              Decision model
            </button>
            <button type="button" aria-pressed={draft.protocol === 'chat'} onClick={() => set('protocol', 'chat')}>
              Chat model
            </button>
          </div>
        </Field>

        <Field label="Provider" error={fieldError('upstream_id')}>
          <select
            className="select"
            aria-label="Provider"
            value={draft.upstream_id}
            onChange={(e) => setDraft((d) => ({ ...d, upstream_id: e.target.value, model: '' }))}
          >
            <option value="">Choose a provider…</option>
            {doc.upstreams.map((u) => (
              <option key={u.id} value={u.id} disabled={!u.enabled}>
                {u.name}
                {u.enabled ? '' : ' (disabled)'}
              </option>
            ))}
          </select>
        </Field>
        {upstream && draft.protocol === 'systemone' ? (
          <p className="small muted" style={{ marginTop: -8 }}>
            Decision calls go to {upstream.systemone_url}
          </p>
        ) : null}

        <Field
          label="Model"
          error={fieldError('model')}
          hint={
            draft.protocol === 'systemone'
              ? 'Pick a suggestion or type the decision model name the provider serves.'
              : 'Smaller, fast models are usually enough.'
          }
        >
          <input
            className="input"
            aria-label="Model"
            list="classifier-models"
            value={draft.model}
            placeholder={draft.protocol === 'systemone' ? 'e.g. ~typesafe/jev-latest or nimble' : 'e.g. gpt-4.1-mini'}
            onChange={(e) => set('model', e.target.value)}
          />
          <datalist id="classifier-models">
            {modelOptions.map((m) => (
              <option key={m} value={m} />
            ))}
          </datalist>
        </Field>

        <details className="insights-more">
          <summary>Sampling, text and limits</summary>
          <div className="stack" style={{ gap: 'var(--janus-space-4)', marginTop: 'var(--janus-space-3)' }}>
            <Field
              label="Requests to classify"
              hint="A share of chat requests, chosen at random."
              error={fieldError('sample_rate')}
            >
              <div className="row" style={{ gap: 'var(--janus-space-3)' }}>
                <input
                  type="range"
                  min={1}
                  max={100}
                  value={draft.sample_rate}
                  aria-label="Requests to classify (percent)"
                  onChange={(e) => set('sample_rate', Number(e.target.value))}
                  style={{ flex: 1 }}
                />
                <span className="num" style={{ width: 48, textAlign: 'right' }}>
                  {draft.sample_rate}%
                </span>
              </div>
            </Field>
            <Field label="Text to classify" error={fieldError('text_scope')}>
              <select
                className="select"
                aria-label="Text to classify"
                value={draft.text_scope}
                onChange={(e) => set('text_scope', e.target.value as ClassificationSettings['text_scope'])}
              >
                {SCOPES.map((s) => (
                  <option key={s.value} value={s.value}>
                    {s.label}
                  </option>
                ))}
              </select>
            </Field>
            <label className="switch">
              <input
                type="checkbox"
                aria-label="Include the system prompt"
                checked={draft.include_system_prompt}
                onChange={(e) => set('include_system_prompt', e.target.checked)}
              />
              <span>Include the system prompt (helps when apps put the task there)</span>
            </label>
            <label className="switch">
              <input
                type="checkbox"
                aria-label="Include service tokens"
                checked={draft.include_service_tokens}
                onChange={(e) => set('include_service_tokens', e.target.checked)}
              />
              <span>Include service-token (automated) traffic</span>
            </label>
            <label className="switch">
              <input
                type="checkbox"
                aria-label="Pick subcategories"
                checked={draft.subcategories}
                onChange={(e) => set('subcategories', e.target.checked)}
              />
              <span>Pick subcategories (asked together with the category when the classifier allows enough choices)</span>
            </label>
            <div className="grid grid-halves">
              <Field
                label="Timeout (ms)"
                error={fieldError('timeout_ms')}
                hint={draft.mode === 'inline' ? 'Most a request waits.' : 'Per classification.'}
              >
                <input
                  className="input"
                  type="number"
                  min={100}
                  max={60000}
                  aria-label="Timeout (ms)"
                  value={draft.timeout_ms}
                  onChange={(e) => set('timeout_ms', Number(e.target.value))}
                />
              </Field>
              <Field
                label="Most recent characters sent"
                error={fieldError('max_chars')}
                hint="Nimble reads up to about 8,000 tokens."
              >
                <input
                  className="input"
                  type="number"
                  min={200}
                  max={200000}
                  aria-label="Most recent characters sent"
                  value={draft.max_chars}
                  onChange={(e) => set('max_chars', Number(e.target.value))}
                />
              </Field>
              <Field label="Parallel calls per replica" error={fieldError('concurrency')}>
                <input
                  className="input"
                  type="number"
                  min={1}
                  max={64}
                  aria-label="Parallel calls per replica"
                  value={draft.concurrency}
                  onChange={(e) => set('concurrency', Number(e.target.value))}
                />
              </Field>
              <Field
                label="Queue size per replica"
                error={fieldError('queue_size')}
                hint={`${formatNumber(doc.queue_depth)} waiting now.`}
              >
                <input
                  className="input"
                  type="number"
                  min={10}
                  max={100000}
                  aria-label="Queue size per replica"
                  value={draft.queue_size}
                  onChange={(e) => set('queue_size', Number(e.target.value))}
                />
              </Field>
              <Field label="Keep results (days)" error={fieldError('retention_days')} hint="0 keeps them.">
                <input
                  className="input"
                  type="number"
                  min={0}
                  max={3650}
                  aria-label="Keep results (days)"
                  value={draft.retention_days}
                  onChange={(e) => set('retention_days', Number(e.target.value))}
                />
              </Field>
            </div>
          </div>
        </details>

        <div className="row" style={{ justifyContent: 'flex-end', gap: 'var(--janus-space-2)' }}>
          <button
            type="button"
            className="btn btn-ghost"
            disabled={!dirty || save.isPending}
            onClick={() => {
              setDraft(doc.settings);
              setError(null);
            }}
          >
            Discard
          </button>
          <button
            type="button"
            className="btn btn-primary"
            disabled={!dirty || save.isPending}
            onClick={() => save.mutate(draft)}
          >
            Save
          </button>
        </div>
      </section>

      <section className="card stack" style={{ gap: 'var(--janus-space-4)' }}>
        <h2>Try it</h2>
        <p className="small muted" style={{ margin: 0 }}>
          Classifies this text with the settings on the left, saved or not. Nothing is stored. Use it to compare classifiers and
          their speed.
        </p>
        <Field label="Sample request">
          <textarea
            className="textarea"
            aria-label="Sample request"
            rows={5}
            value={sample}
            onChange={(e) => setSample(e.target.value)}
          />
        </Field>
        <div className="row" style={{ justifyContent: 'flex-end' }}>
          <button
            type="button"
            className="btn"
            disabled={test.isPending || !sample.trim() || !draft.upstream_id || !draft.model.trim()}
            onClick={() => test.mutate()}
          >
            {test.isPending ? 'Classifying…' : 'Classify'}
          </button>
        </div>
        {trial ? <TrialResult r={trial} localOnly={localOnly} /> : null}
      </section>
    </div>
  );
}

function TrialResult({ r, localOnly }: { r: ClassificationTestResult; localOnly: boolean }): ReactNode {
  const name = useCategoryNames();
  if (r.status !== 'ok') {
    return (
      <div className="banner banner-danger">
        <strong>{r.status === 'timeout' ? 'Timed out' : 'Classifier error'}</strong> after {formatDuration(r.latency_ms)}:{' '}
        {r.error}
      </div>
    );
  }
  const probs = Object.entries(r.probabilities ?? {})
    .sort((a, b) => b[1] - a[1])
    .slice(0, 5);
  return (
    <div className="stack" style={{ gap: 'var(--janus-space-3)' }}>
      <div className="grid insights-trial-tiles">
        <Tile label="Category" value={name(r.category)} meta={r.subcategory ? `› ${name(r.subcategory)}` : 'no subcategory'} />
        <Tile
          label="Confidence"
          value={percent(r.confidence)}
          meta={r.protocol === 'chat' ? "the model's own estimate" : 'calibrated'}
        />
        <Tile label="Classifier time" value={formatDuration(r.latency_ms)} meta={r.model || r.classifier} />
      </div>
      {probs.length > 1 ? (
        <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0, gap: 'var(--janus-space-2)' }}>
          {probs.map(([slug, p], i) => (
            <li key={slug}>
              <div className="row-between small" style={{ marginBottom: 2 }}>
                <span>{name(slug)}</span>
                <span className="num muted">{(p * 100).toFixed(1)}%</span>
              </div>
              <div className="meter" aria-hidden="true">
                <div className="meter-fill" style={{ width: `${Math.max(1, p * 100)}%`, background: seriesColor(i) }} />
              </div>
            </li>
          ))}
        </ul>
      ) : null}
      <p className="small muted" style={{ margin: 0 }}>
        {formatNumber(r.input_chars)} characters · {formatNumber(r.tokens_in)} tokens in
        {!localOnly && r.cost_nanousd > 0 ? ` · ${formatUSD(r.cost_nanousd)}` : ''}
      </p>
    </div>
  );
}
