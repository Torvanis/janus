/**
 * Auto-routing (Administration → Auto-routing).
 *
 * Overview → Routers (list + full-page builder) → Categories → Classifier
 * (with Try routing and the batch test) → Evaluations. A router is a managed
 * model of kind router: callers address it by name, and it is granted,
 * bound to security policies and scoped by quotas like any alias.
 */
import { Fragment, useEffect, useMemo, useState, type ReactNode } from 'react';
import { Link, useLocation, useNavigate, useParams } from 'react-router-dom';
import { useUnsavedGuard, useUnsavedLinkGuard } from '../../lib/hooks';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import { formatDateTime, formatDuration, formatNumber, formatRelative, formatUSD } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, Drawer, Field, useToast, type Tone } from '../../components/ui';
import { IconButton, RowActions } from '../../components/IconButton';
import { RetainedPanel, RouteTopNav } from '../../components/RouteTopNav';
import { seriesColor } from '../../components/charts';
import { useLocalOnly } from '../../app/session';
import { NotFoundPage } from '../Errors';
import {
  bandOf,
  DEMO_PROMPTS,
  DIFFICULTY_BANDS,
  difficultyLabel,
  FAILURE_REASONS,
  newRoute,
  pct,
  reasonLabel,
  ROUTER_TEMPLATES,
  routeEvalPairings,
  routingHealth,
  thinkingLabel,
  type BatchResult,
  type Router,
  type RouterConfig,
  type RouterRoute,
  type RouterStatus,
  type RouterTemplate,
  type RoutingCategory,
  type RoutingDecisionRow,
  type RoutingDocument,
  type RoutingEvalRow,
  type RoutingSettings,
  type RoutingTarget,
  type RoutingTestResult,
  type RouteEvalSummary,
} from '../../lib/routing';
import type { ClassificationCategory } from '../../lib/types';

const BASE = '/api/v1/admin/routing';
const KEY = ['admin', 'routing'] as const;

export const routingTabs = [
  { id: 'overview', label: 'Overview' },
  { id: 'routers', label: 'Routers' },
  { id: 'categories', label: 'Categories' },
  { id: 'classifier', label: 'Classifier' },
  { id: 'evaluations', label: 'Evaluations' },
] as const;

function errorMessage(err: unknown): string {
  return err instanceof ApiError ? err.message : err instanceof Error ? err.message : 'Something went wrong.';
}

function useRoutingDoc() {
  return useQuery({ queryKey: [...KEY, 'doc'], queryFn: () => api.get<RoutingDocument>(BASE) });
}

function useRouters() {
  return useQuery({ queryKey: [...KEY, 'routers'], queryFn: () => api.get<{ routers: Router[] }>(`${BASE}/routers`) });
}

function useRoutingCategories() {
  return useQuery({
    queryKey: [...KEY, 'categories'],
    queryFn: () =>
      api.get<{ categories: RoutingCategory[]; insights_categories: ClassificationCategory[]; max_enabled: number }>(
        `${BASE}/categories`,
      ),
  });
}

function useTargets() {
  return useQuery({ queryKey: [...KEY, 'targets'], queryFn: () => api.get<{ targets: RoutingTarget[] }>(`${BASE}/targets`) });
}

function useCategoryName(): (slug: string) => string {
  const cats = useRoutingCategories();
  return useMemo(() => {
    const names = new Map((cats.data?.categories ?? []).map((c) => [c.slug, c.name]));
    return (slug: string) => (slug === 'none' ? 'None of these' : (names.get(slug) ?? (slug ? slug.replace(/_/g, ' ') : '—')));
  }, [cats.data]);
}

function statusTone(s: RouterStatus): Tone {
  return s === 'live' ? 'success' : s === 'shadow' ? 'info' : 'neutral';
}

const STATUS_LABEL: Record<RouterStatus, string> = { draft: 'Draft', shadow: 'Shadow', live: 'Live' };

function hostingBadge(h?: string): ReactNode {
  if (h === 'self_hosted') return <Badge tone="success">self-hosted</Badge>;
  if (h === 'external') return <Badge tone="neutral">hosted</Badge>;
  if (h === 'hybrid') return <Badge tone="neutral">hybrid</Badge>;
  return null;
}

/* --- Page ------------------------------------------------------------------- */

export function RoutingPage(): ReactNode {
  const { tab = 'overview', id } = useParams();
  const location = useLocation();
  if (!routingTabs.some((item) => item.id === tab)) return <NotFoundPage />;
  // A router opened from the Routers tab stays inside the section: same
  // title and tabs, with a way back to the list.
  const builder = tab === 'routers' && !!id;
  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">Auto-routing</h1>
          <p className="page-subtitle">
            Let Janus pick the model for each request. A classifier reads the new turn, decides what kind of work it is and how
            hard, and a router sends it to the model you chose for that work.
          </p>
        </div>
      </header>
      <RouteTopNav
        label="Auto-routing"
        active={tab}
        items={routingTabs.map((item) => ({
          ...item,
          to: { pathname: `/admin/routing/${item.id}`, search: builder ? '' : location.search },
        }))}
      />
      {builder ? <RouterBuilderPage id={id} /> : null}
      <RetainedPanel active={tab === 'overview'}>
        <OverviewPanel />
      </RetainedPanel>
      <RetainedPanel active={tab === 'routers' && !builder}>
        <RoutersPanel />
      </RetainedPanel>
      <RetainedPanel active={tab === 'categories'}>
        <CategoriesPanel />
      </RetainedPanel>
      <RetainedPanel active={tab === 'classifier'}>
        <ClassifierPanel />
      </RetainedPanel>
      <RetainedPanel active={tab === 'evaluations'}>
        <EvaluationsPanel />
      </RetainedPanel>
    </div>
  );
}

/* --- Overview --------------------------------------------------------------- */

function OverviewPanel(): ReactNode {
  const navigate = useNavigate();
  const doc = useRoutingDoc();
  const routers = useRouters();
  const cats = useRoutingCategories();
  const catName = useCategoryName();
  return (
    <AsyncSection query={doc}>
      {(d) => {
        const list = routers.data?.routers ?? [];
        const health = routingHealth(d, list);
        const totals = list.reduce(
          (acc, r) => {
            const s = r.stats;
            if (!s) return acc;
            acc.requests += s.requests;
            acc.routed += s.routed;
            acc.fell += s.timeouts + s.errors;
            acc.shadow += s.shadow;
            acc.p95 = Math.max(acc.p95, s.p95_ms);
            return acc;
          },
          { requests: 0, routed: 0, fell: 0, shadow: 0, p95: 0 },
        );
        const steps: Array<{ done: boolean; title: string; body: string; to: string }> = [
          {
            done: !!d.settings.classifier_model_id && d.running,
            title: 'Choose a classifier',
            body: 'A decision model (Jev, Clef or Nimble) answers “what kind of work, how hard”.',
            to: 'classifier',
          },
          {
            done: (cats.data?.categories ?? []).some((c) => !c.builtin) || (cats.data?.categories ?? []).length > 0,
            title: 'Review the categories',
            body: 'Fifteen are built in. Add your own, or link one to a Usage insights category.',
            to: 'categories',
          },
          { done: list.length > 0, title: 'Build a router', body: 'Start from a template; pick your models.', to: 'routers' },
          {
            done: list.some((r) => r.status !== 'draft'),
            title: 'Run it in shadow',
            body: 'Callers get the default; Janus records what it would have done.',
            to: 'routers',
          },
          {
            done: list.some((r) => r.config.routes.some((x) => x.eval.enabled)),
            title: 'Evaluate a route',
            body: 'Compare a sample of routed answers with the default’s, judged blind.',
            to: 'evaluations',
          },
          {
            done: list.some((r) => r.status === 'live'),
            title: 'Go live',
            body: 'Grant the router and point callers at its name.',
            to: 'routers',
          },
        ];
        return (
          <div className="stack" style={{ gap: 'var(--janus-space-5)' }}>
            <section className="secgw-hero" aria-live="polite" data-testid="routing-health">
              <div
                className={`secgw-hero-lamp${health.level === 'off' ? ' is-off' : health.level === 'attention' ? ' is-watch' : health.level === 'down' ? ' is-down' : ''}`}
                aria-hidden="true"
              >
                {health.level === 'ok' ? '✓' : health.level === 'down' ? '✕' : health.level === 'attention' ? '!' : '○'}
              </div>
              <div>
                <h2 className="secgw-hero-title">{health.title}</h2>
                <ul className="routing-reasons">
                  {health.reasons.map((r) => (
                    <li key={r}>{r}</li>
                  ))}
                </ul>
                <div className="secgw-hero-meta">
                  <span>
                    <strong>{formatNumber(totals.requests)}</strong> decisions in 24 h
                  </span>
                  <span>
                    <strong>{totals.requests ? pct(totals.routed / totals.requests) : '—'}</strong> sent to a route
                  </span>
                  <span>
                    <strong>{formatNumber(totals.fell)}</strong> fell back (slow or failed)
                  </span>
                  <span>
                    p95 decision <strong>{totals.p95 ? formatDuration(totals.p95) : '—'}</strong>
                  </span>
                </div>
              </div>
              <button type="button" className="btn" onClick={() => navigate('/admin/routing/routers')}>
                Routers
              </button>
            </section>

            <div className="grid grid-halves" style={{ alignItems: 'start' }}>
              <section className="card">
                <div className="card-header">
                  <div>
                    <h2>Routers in the last 24 hours</h2>
                    <p className="small secondary" style={{ margin: 0 }}>
                      Where each router sent its requests.
                    </p>
                  </div>
                </div>
                {list.length === 0 ? (
                  <p className="muted small" style={{ margin: 0 }}>
                    No routers yet.
                  </p>
                ) : (
                  <div className="stack" style={{ gap: 'var(--janus-space-4)' }}>
                    {list.map((r) => (
                      <RouterMix key={r.id} router={r} />
                    ))}
                  </div>
                )}
                {list.some((r) => (r.stats?.categories.length ?? 0) > 0) ? (
                  <p className="small muted" style={{ margin: 'var(--janus-space-4) 0 0' }}>
                    Most common:{' '}
                    {Object.entries(
                      list
                        .flatMap((r) => r.stats?.categories ?? [])
                        .filter((c) => c.reason)
                        .reduce<Record<string, number>>(
                          (acc, c) => ({ ...acc, [c.reason]: (acc[c.reason] ?? 0) + c.requests }),
                          {},
                        ),
                    )
                      .sort((a, b) => b[1] - a[1])
                      .slice(0, 4)
                      .map(([slug, n]) => `${catName(slug)} (${formatNumber(n)})`)
                      .join(' · ')}
                  </p>
                ) : null}
              </section>
              <section className="card">
                <div className="card-header">
                  <div>
                    <h2>Getting started</h2>
                    <p className="small secondary" style={{ margin: 0 }}>
                      From nothing to a live router, safely.
                    </p>
                  </div>
                </div>
                <ol className="secgw-steps">
                  {steps.map((step, i) => (
                    <li key={step.title} className={`secgw-step${step.done ? ' is-done' : ''}`}>
                      <span className="secgw-step-mark" aria-hidden="true">
                        {step.done ? '✓' : i + 1}
                      </span>
                      <div>
                        <div className="secgw-step-title">{step.title}</div>
                        <div className="secgw-step-body">{step.body}</div>
                      </div>
                      <button
                        type="button"
                        className="btn btn-ghost btn-sm"
                        onClick={() => navigate(`/admin/routing/${step.to}`)}
                        aria-label={`${step.title}: ${step.done ? 'review' : 'go'}`}
                      >
                        {step.done ? 'Review' : 'Go'}
                      </button>
                    </li>
                  ))}
                </ol>
              </section>
            </div>

            <section className="card">
              <div className="card-header">
                <div>
                  <h2>How a request is routed</h2>
                  <p className="small secondary" style={{ margin: 0 }}>
                    What happens between the caller and the model, and what it costs the request.
                  </p>
                </div>
              </div>
              <div className="routing-flow">
                {[
                  [
                    '1',
                    'Grant and policies',
                    'The caller needs a grant on the router. Security policies bound to the router (and later to the model it picks) apply.',
                  ],
                  [
                    '2',
                    'Same conversation?',
                    'A later turn of a conversation the router already placed reuses that route. No classifier call.',
                  ],
                  [
                    '3',
                    'Classify',
                    'The classifier answers category and difficulty, sharing the security check’s call when it can. Capped by the router’s time budget.',
                  ],
                  [
                    '4',
                    'First route that fits',
                    'Category, difficulty, context window, grant and quota are checked route by route, in order.',
                  ],
                  [
                    '5',
                    'Default',
                    'Nothing fits, the classifier is unsure, slow or down: the default serves. A router never fails a request.',
                  ],
                  [
                    '6',
                    'Later, out of band',
                    'A sample of routed answers is compared with the default’s by a judge model, when the gateway is quiet.',
                  ],
                ].map(([n, title, body]) => (
                  <div key={n} className="routing-flow-step">
                    <span className="secgw-step-mark" aria-hidden="true">
                      {n}
                    </span>
                    <div className="small" style={{ fontWeight: 600 }}>
                      {title}
                    </div>
                    <div className="small secondary">{body}</div>
                  </div>
                ))}
              </div>
            </section>
          </div>
        );
      }}
    </AsyncSection>
  );
}

function RouterMix({ router }: { router: Router }): ReactNode {
  const s = router.stats;
  const total = s?.requests ?? 0;
  return (
    <div>
      <div className="row-between small" style={{ marginBottom: 4 }}>
        <span className="row" style={{ gap: 6 }}>
          <Link to={`/admin/routing/routers/${router.id}`} className="mono">
            {router.name}
          </Link>
          <Badge tone={statusTone(router.status)}>{STATUS_LABEL[router.status]}</Badge>
        </span>
        <span className="muted num">{formatNumber(total)} requests</span>
      </div>
      {total === 0 ? (
        <div className="meter" aria-hidden="true" />
      ) : (
        <div className="routing-mixbar" role="img" aria-label={`Route mix for ${router.name}`}>
          {(s?.mix ?? []).map((m, i) => (
            <span
              key={m.route_id + m.model_name}
              style={{
                width: `${(m.requests / total) * 100}%`,
                background: m.route_id === 'default' ? 'var(--janus-color-border-strong)' : seriesColor(i),
              }}
              title={`${m.route_name || 'default'} → ${m.model_name}: ${formatNumber(m.requests)}`}
            />
          ))}
        </div>
      )}
      {total > 0 ? (
        <div className="small muted" style={{ marginTop: 4 }}>
          {(s?.mix ?? [])
            .slice(0, 4)
            .map((m) => `${m.route_name || 'default'} ${pct(m.requests / total)}`)
            .join(' · ')}
        </div>
      ) : null}
    </div>
  );
}

/* --- Routers list ------------------------------------------------------------ */

function RoutersPanel(): ReactNode {
  const navigate = useNavigate();
  const routers = useRouters();
  const qc = useQueryClient();
  const toast = useToast();
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<Router | null>(null);
  const del = useMutation({
    mutationFn: (id: string) => api.del(`${BASE}/routers/${id}`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: KEY });
      toast('Router deleted.', 'success');
      setDeleting(null);
    },
    onError: (e) => toast(errorMessage(e), 'danger'),
  });
  return (
    <section className="card card-flush">
      <div className="card-header" style={{ padding: 'var(--janus-card-padding)' }}>
        <div>
          <h2>Routers</h2>
          <p className="small secondary" style={{ margin: 0 }}>
            Each router is a model name callers use, like <span className="mono">auto/legal</span>. Grant it on the Grants page.
          </p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
          New router
        </button>
      </div>
      <AsyncSection query={routers}>
        {(data) =>
          data.routers.length === 0 ? (
            <div style={{ padding: 'var(--janus-card-padding)' }}>
              <TemplatePicker onPick={(t) => navigate(`/admin/routing/routers/new?template=${t.id}`)} />
            </div>
          ) : (
            <div className="table-wrap">
              <table className="data routing-table">
                <thead>
                  <tr>
                    <th>Router</th>
                    <th>Status</th>
                    <th>Routes</th>
                    <th>Default</th>
                    <th className="num">Grants</th>
                    <th className="num">24 h</th>
                    <th>Evaluation</th>
                    <th aria-label="Actions" />
                  </tr>
                </thead>
                <tbody>
                  {data.routers.map((r) => {
                    const current = (r.evaluations ?? []).filter((e) => e.current !== false);
                    const losing = current.filter((e) => e.verdict === 'losing').length;
                    const keep = current.filter((e) => e.verdict === 'keep').length;
                    return (
                      <tr key={r.id} data-clickable="true" onClick={() => navigate(`/admin/routing/routers/${r.id}`)}>
                        <th scope="row">
                          <div className="mono">{r.name}</div>
                          {r.problems.length > 0 ? (
                            <div className="small routing-cell-note" style={{ color: 'var(--janus-color-warning-fg)' }}>
                              ⚠ {r.problems[0]}
                            </div>
                          ) : null}
                          {r.description ? <div className="small muted routing-cell-note">{r.description}</div> : null}
                        </th>
                        <td>
                          <Badge tone={statusTone(r.status)}>{STATUS_LABEL[r.status]}</Badge>
                        </td>
                        <td className="small">
                          {r.config.routes.length === 0
                            ? 'none'
                            : r.config.routes
                                .slice(0, 3)
                                .map((x) => x.name || x.model_name)
                                .join(', ')}
                          {r.config.routes.length > 3 ? ` +${r.config.routes.length - 3}` : ''}
                        </td>
                        <td className="small">{r.config.default.model_name}</td>
                        <td className="num">
                          {r.grant_count === 0 ? <Badge tone="warning">0</Badge> : formatNumber(r.grant_count)}
                        </td>
                        <td className="num">{formatNumber(r.stats?.requests ?? 0)}</td>
                        <td className="small">
                          {losing > 0 ? (
                            <Badge tone="danger">{losing} losing</Badge>
                          ) : keep > 0 ? (
                            <Badge tone="success">{keep} keep</Badge>
                          ) : r.config.routes.some((x) => x.eval.enabled) ? (
                            <span className="muted">collecting</span>
                          ) : (
                            <span className="muted">off</span>
                          )}
                        </td>
                        <td onClick={(e) => e.stopPropagation()}>
                          <RowActions>
                            <IconButton
                              icon="edit"
                              label={`Edit ${r.name}`}
                              onClick={() => navigate(`/admin/routing/routers/${r.id}`)}
                            />
                            <IconButton icon="delete" danger label={`Delete ${r.name}`} onClick={() => setDeleting(r)} />
                          </RowActions>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )
        }
      </AsyncSection>
      <Drawer
        open={creating}
        onClose={() => setCreating(false)}
        title="New router"
        description="Start from a template or a blank router."
        wide
      >
        <TemplatePicker
          onPick={(t) => {
            setCreating(false);
            navigate(`/admin/routing/routers/new?template=${t.id}`);
          }}
          blank={() => {
            setCreating(false);
            navigate('/admin/routing/routers/new');
          }}
        />
      </Drawer>
      <ConfirmDialog
        open={!!deleting}
        title={`Delete ${deleting?.name ?? ''}?`}
        consequence={`Callers using ${deleting?.name ?? ''} will get “model not found”. Its grants go with it; decisions and evaluation verdicts stay until retention.`}
        confirmLabel="Delete router"
        danger
        busy={del.isPending}
        onClose={() => setDeleting(null)}
        onConfirm={() => {
          if (deleting) del.mutate(deleting.id);
        }}
      />
    </section>
  );
}

function TemplatePicker({ onPick, blank }: { onPick: (t: RouterTemplate) => void; blank?: () => void }): ReactNode {
  return (
    <div className="stack" style={{ gap: 'var(--janus-space-3)' }}>
      <p className="small secondary" style={{ margin: 0 }}>
        Templates fill in categories, difficulty and thinking. You choose the models: Janus does not know which of yours is local,
        cheap or best. New routers start as drafts.
      </p>
      <div className="routing-templates">
        {ROUTER_TEMPLATES.map((t) => (
          <button key={t.id} type="button" className="routing-template" onClick={() => onPick(t)}>
            <span className="routing-template-name">{t.name}</span>
            <span className="small secondary">{t.summary}</span>
            <span className="small muted mono">{t.modelName}</span>
          </button>
        ))}
        {blank ? (
          <button type="button" className="routing-template is-blank" onClick={blank}>
            <span className="routing-template-name">Blank router</span>
            <span className="small secondary">A default and no routes. Add routes yourself.</span>
          </button>
        ) : null}
      </div>
    </div>
  );
}

/* --- Router builder ------------------------------------------------------------ */

interface BuilderDraft {
  name: string;
  description: string;
  status: RouterStatus;
  config: RouterConfig;
}

function emptyDraft(): BuilderDraft {
  return {
    name: '',
    description: '',
    status: 'draft',
    config: {
      routes: [],
      default: { model_id: '', thinking: 'passthrough' },
      budget_ms: 800,
      sticky: true,
      sticky_minutes: 30,
      disclose: true,
    },
  };
}

function draftFromTemplate(t: RouterTemplate): BuilderDraft {
  const d = emptyDraft();
  d.name = t.modelName;
  d.description = t.summary;
  d.config.default.thinking = t.defaultThinking;
  d.config.routes = t.routes.map((r) => ({ ...newRoute(), ...r }) as RouterRoute);
  return d;
}

function RouterBuilderPage({ id }: { id: string }): ReactNode {
  const isNew = id === 'new';
  const location = useLocation();
  const template = new URLSearchParams(location.search).get('template');
  const router = useQuery({
    queryKey: [...KEY, 'router', id],
    queryFn: () =>
      api.get<{ router: Router; grants: Array<{ id: string; grantee_name: string; grantee_type: string }> }>(
        `${BASE}/routers/${id}`,
      ),
    enabled: !isNew,
  });
  if (isNew) {
    const t = ROUTER_TEMPLATES.find((x) => x.id === template);
    return <RouterBuilder key={`new-${template ?? ''}`} initial={t ? draftFromTemplate(t) : emptyDraft()} templateHints={t} />;
  }
  return (
    <div>
      <AsyncSection query={router}>
        {(data) => (
          <RouterBuilder
            key={data.router.id + data.router.updated_at}
            routerId={data.router.id}
            initial={{
              name: data.router.name,
              description: data.router.description,
              status: data.router.status,
              config: data.router.config,
            }}
            problems={data.router.problems}
            grants={data.grants}
            evals={data.router.evaluations}
          />
        )}
      </AsyncSection>
    </div>
  );
}

function RouterBuilder({
  routerId,
  initial,
  problems,
  grants,
  templateHints,
}: {
  routerId?: string;
  initial: BuilderDraft;
  problems?: string[];
  grants?: Array<{ id: string; grantee_name: string; grantee_type: string }>;
  evals?: RouteEvalSummary[];
  templateHints?: RouterTemplate;
}): ReactNode {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const toast = useToast();
  const [draft, setDraft] = useState<BuilderDraft>(initial);
  const [error, setError] = useState<{ field?: string; message: string } | null>(null);
  const [editing, setEditing] = useState<number | null>(null);
  const targets = useTargets();
  const doc = useRoutingDoc();
  const cats = useRoutingCategories();
  const catName = useCategoryName();
  const dirty = JSON.stringify(draft) !== JSON.stringify(initial);
  // Unsaved edits are guarded on new routers too, and against in-app links
  // (tabs, sidebar, back), not only closing the tab.
  useUnsavedGuard(dirty);
  useUnsavedLinkGuard(dirty, 'This router has unsaved changes. Leave and discard them?');
  const targetList = targets.data?.targets ?? [];
  const target = (mid: string) => targetList.find((t) => t.id === mid);
  const classifier = (doc.data?.classifiers ?? []).find((c) => c.id === doc.data?.settings.classifier_model_id);

  const save = useMutation({
    mutationFn: (d: BuilderDraft) =>
      routerId
        ? api.put<{ router: Router }>(`${BASE}/routers/${routerId}`, d)
        : api.post<{ router: Router }>(`${BASE}/routers`, d),
    onSuccess: (res) => {
      void qc.invalidateQueries({ queryKey: KEY });
      void qc.invalidateQueries({ queryKey: ['admin', 'managed-models'] });
      setError(null);
      toast(routerId ? 'Router saved.' : 'Router created.', 'success');
      if (!routerId) navigate(`/admin/routing/routers/${res.router.id}`, { replace: true });
    },
    onError: (e) =>
      setError(e instanceof ApiError ? { field: e.payload.param, message: e.message } : { message: errorMessage(e) }),
  });

  const setCfg = (patch: Partial<RouterConfig>) => setDraft((d) => ({ ...d, config: { ...d.config, ...patch } }));
  const setRoute = (i: number, patch: Partial<RouterRoute>) =>
    setCfg({ routes: draft.config.routes.map((r, j) => (j === i ? { ...r, ...patch } : r)) });
  const move = (i: number, by: number) => {
    const routes = [...draft.config.routes];
    const [r] = routes.splice(i, 1);
    if (r) routes.splice(i + by, 0, r);
    setCfg({ routes });
  };
  const onPremWithHostedClassifier =
    classifier?.hosting === 'external' && draft.config.routes.some((r) => target(r.model_id)?.hosting === 'self_hosted');
  // Evaluating an on-prem route copies a sample of its prompts to the
  // default and the judge: say so when either is hosted elsewhere.
  const judgeHosting = (id: string) => (doc.data?.judge_models ?? []).find((j) => j.id === id)?.hosting;
  const defaultHosted = target(draft.config.default.model_id)?.hosting === 'external';
  const onPremEvaluatedOffSite = draft.config.routes.filter(
    (r) =>
      r.eval.enabled &&
      target(r.model_id)?.hosting === 'self_hosted' &&
      (defaultHosted || judgeHosting(r.eval.judge_model_id || doc.data?.settings.eval_judge_model_id || '') === 'external'),
  );

  return (
    <div className="stack" style={{ gap: 'var(--janus-space-4)' }}>
      <div className="routing-builder-bar">
        <Link to="/admin/routing/routers" className="btn btn-ghost btn-sm" aria-label="Back to all routers">
          ← All routers
        </Link>
        <div className="routing-builder-title">
          <h2 className="mono">{draft.name || 'New router'}</h2>
          <p className="small secondary" style={{ margin: 0 }}>
            Routes are tried top to bottom; the first that fits serves. Anything else goes to the default.
          </p>
        </div>
        <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
          <button type="button" className="btn btn-ghost" disabled={!dirty || save.isPending} onClick={() => setDraft(initial)}>
            Discard
          </button>
          <button
            type="button"
            className="btn btn-primary"
            disabled={(!dirty && !!routerId) || save.isPending}
            onClick={() => save.mutate(draft)}
          >
            {save.isPending ? 'Saving…' : routerId ? 'Save' : 'Create router'}
          </button>
        </div>
      </div>

      {error ? (
        <div className="banner banner-danger" role="alert" style={{ marginBottom: 'var(--janus-space-4)' }}>
          {error.message}
        </div>
      ) : null}
      {(problems ?? []).length > 0 && !dirty ? (
        <div className="banner banner-warning" style={{ marginBottom: 'var(--janus-space-4)' }}>
          <strong>Needs attention.</strong> {(problems ?? []).join(' ')}
        </div>
      ) : null}
      {onPremWithHostedClassifier ? (
        <div className="banner banner-warning" style={{ marginBottom: 'var(--janus-space-4)' }}>
          The routing classifier ({classifier?.name}) is externally hosted, and this router sends some requests to a self-hosted
          model. Those prompts are read by the classifier’s provider before they reach your model. Use a self-hosted classifier
          (Clef or Nimble) if that matters.
        </div>
      ) : null}

      {onPremEvaluatedOffSite.length > 0 ? (
        <div className="banner banner-warning" style={{ marginBottom: 'var(--janus-space-4)' }}>
          {onPremEvaluatedOffSite.map((r) => r.name || target(r.model_id)?.name || 'A route').join(', ')}{' '}
          {onPremEvaluatedOffSite.length === 1 ? 'keeps' : 'keep'} work on a self-hosted model but{' '}
          {onPremEvaluatedOffSite.length === 1 ? 'is' : 'are'} evaluated against an externally hosted default or judge: evaluation
          sends a sample of those prompts (and the answers) there. Security Gateway policies on the default and the judge still
          apply, and a sample they would block or redact is skipped. Turn evaluation off, or use a self-hosted default and judge,
          if those prompts must stay on site.
        </div>
      ) : null}

      <div className="routing-builder">
        <div className="stack" style={{ gap: 'var(--janus-space-4)', minWidth: 0 }}>
          <section className="card stack" style={{ gap: 'var(--janus-space-4)' }}>
            <div className="routing-form-grid">
              <Field
                label="Model name callers use"
                error={error?.field === 'name' ? error.message : undefined}
                hint="For example auto/legal. Lowercase, no spaces."
              >
                <input
                  className="input mono"
                  aria-label="Model name callers use"
                  value={draft.name}
                  onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                />
              </Field>
              <Field label="Status">
                <div className="segmented" role="radiogroup" aria-label="Status">
                  {(['draft', 'shadow', 'live'] as RouterStatus[]).map((s) => (
                    <button
                      key={s}
                      type="button"
                      role="radio"
                      aria-checked={draft.status === s}
                      aria-pressed={draft.status === s}
                      onClick={() => setDraft({ ...draft, status: s })}
                    >
                      {STATUS_LABEL[s]}
                    </button>
                  ))}
                </div>
              </Field>
            </div>
            <p className="small muted" style={{ margin: 0 }}>
              {draft.status === 'draft'
                ? 'Draft: not callable. Test it with the panel on the right.'
                : draft.status === 'shadow'
                  ? 'Shadow: callable, always served by the default. Janus classifies in the background and records which route would have served, so you can see the effect before it is real. No added latency.'
                  : 'Live: requests are routed. The decision waits at most the time budget below.'}
            </p>
            <Field label="Description" hint="Shown to callers in the model list.">
              <input
                className="input"
                aria-label="Description"
                value={draft.description}
                onChange={(e) => setDraft({ ...draft, description: e.target.value })}
              />
            </Field>
          </section>

          <section className="card card-flush">
            <div className="card-header" style={{ padding: 'var(--janus-card-padding)' }}>
              <div>
                <h2>Routes</h2>
                <p className="small secondary" style={{ margin: 0 }}>
                  First match wins. The same model can appear in several routes, for example with different thinking.
                </p>
              </div>
              <button
                type="button"
                className="btn"
                disabled={draft.config.routes.length >= 32}
                onClick={() => {
                  setCfg({ routes: [...draft.config.routes, newRoute()] });
                  setEditing(draft.config.routes.length);
                }}
              >
                Add route
              </button>
            </div>
            <div className="table-wrap">
              <table className="data routing-table routing-routes">
                <thead>
                  <tr>
                    <th className="num">#</th>
                    <th>When</th>
                    <th>Send to</th>
                    <th aria-label="Actions" />
                  </tr>
                </thead>
                <tbody>
                  {draft.config.routes.map((r, i) => {
                    const tgt = target(r.model_id);
                    const hint = templateHints?.routes[i]?.hint;
                    return (
                      <tr key={r.id || `new-${i}`} data-clickable="true" onClick={() => setEditing(i)}>
                        <td className="num muted">{i + 1}</td>
                        <th scope="row" style={{ fontWeight: 400 }}>
                          {r.name ? <div style={{ fontWeight: 600 }}>{r.name}</div> : null}
                          <div className="small">
                            {r.categories.length === 0 ? 'Any category' : r.categories.map(catName).join(', ')}
                            {r.difficulty_min || r.difficulty_max
                              ? ` · ${difficultyLabel(r.difficulty_min, r.difficulty_max)}`
                              : ''}
                          </div>
                          {r.restrict_to_granted || r.skip_when_over_quota ? (
                            <div className="small muted">
                              {[
                                r.restrict_to_granted ? 'granted callers only' : '',
                                r.skip_when_over_quota ? 'skip when over quota' : '',
                              ]
                                .filter(Boolean)
                                .join(' · ')}
                            </div>
                          ) : null}
                        </th>
                        <td className="small routing-target-cell">
                          {tgt ? (
                            <span className="routing-target">
                              <span className="mono">{tgt.name}</span> {hostingBadge(tgt.hosting)}
                            </span>
                          ) : r.model_id ? (
                            <span style={{ color: 'var(--janus-color-danger-fg)' }}>{r.model_name || 'unavailable'}</span>
                          ) : (
                            <span style={{ color: 'var(--janus-color-warning-fg)' }}>Choose {hint ?? 'a model'}</span>
                          )}
                          <div className="muted">
                            Thinking: {thinkingLabel(r.thinking)}
                            {r.eval.enabled ? ` · evaluating ${r.eval.sample_pct}%` : ''}
                          </div>
                        </td>
                        <td onClick={(e) => e.stopPropagation()}>
                          <RowActions>
                            <IconButton icon="edit" label={`Edit route ${i + 1}`} onClick={() => setEditing(i)} />
                            <button
                              type="button"
                              className="icon-btn"
                              aria-label={`Move route ${i + 1} up`}
                              data-tooltip="Move up"
                              disabled={i === 0}
                              onClick={() => move(i, -1)}
                            >
                              ↑
                            </button>
                            <button
                              type="button"
                              className="icon-btn"
                              aria-label={`Move route ${i + 1} down`}
                              data-tooltip="Move down"
                              disabled={i === draft.config.routes.length - 1}
                              onClick={() => move(i, 1)}
                            >
                              ↓
                            </button>
                            <IconButton
                              icon="delete"
                              danger
                              label={`Remove route ${i + 1}`}
                              onClick={() => setCfg({ routes: draft.config.routes.filter((_, j) => j !== i) })}
                            />
                          </RowActions>
                        </td>
                      </tr>
                    );
                  })}
                  <tr className="routing-default-row">
                    <td className="num muted">⤓</td>
                    <th scope="row" style={{ fontWeight: 400 }}>
                      <div style={{ fontWeight: 600 }}>Default</div>
                      <div className="small muted">No route fits, classifier unsure, slow or down</div>
                    </th>
                    <td className="routing-target-cell">
                      <div className="stack" style={{ gap: 6 }}>
                        <TargetSelect
                          label="Default model"
                          value={draft.config.default.model_id}
                          targets={targetList}
                          placeholder={templateHints ? `Choose ${templateHints.defaultHint}` : 'Choose the default model'}
                          onChange={(v) => setCfg({ default: { ...draft.config.default, model_id: v } })}
                        />
                        <ThinkingSelect
                          label="Default thinking"
                          value={draft.config.default.thinking}
                          levels={doc.data?.thinking_levels ?? []}
                          onChange={(v) => setCfg({ default: { ...draft.config.default, thinking: v } })}
                        />
                      </div>
                    </td>
                    <td />
                  </tr>
                </tbody>
              </table>
            </div>
          </section>

          <details className="card">
            <summary className="routing-summary">
              <h2 style={{ display: 'inline' }}>Router settings</h2>
              <span className="small muted">
                {' '}
                · {draft.config.budget_ms} ms budget ·{' '}
                {draft.config.sticky ? `sticky ${draft.config.sticky_minutes} min` : 'not sticky'}
              </span>
            </summary>
            <div className="routing-form-grid" style={{ marginTop: 'var(--janus-space-4)' }}>
              <Field
                label="Decision time budget (ms)"
                hint="The most a request waits for the classifier before the default serves it."
              >
                <input
                  className="input"
                  type="number"
                  min={50}
                  max={30000}
                  aria-label="Decision time budget (ms)"
                  value={draft.config.budget_ms}
                  onChange={(e) => setCfg({ budget_ms: Number(e.target.value) })}
                />
              </Field>
              <Field
                label="Remember conversations (minutes)"
                hint="Later turns of a conversation keep its route: no model flip mid-task, prompt caches stay warm."
              >
                <div className="row" style={{ gap: 'var(--janus-space-3)' }}>
                  <label className="switch">
                    <input
                      type="checkbox"
                      aria-label="Keep conversations on one route"
                      checked={draft.config.sticky}
                      onChange={(e) => setCfg({ sticky: e.target.checked })}
                    />
                  </label>
                  <input
                    className="input"
                    type="number"
                    min={1}
                    max={1440}
                    aria-label="Remember conversations (minutes)"
                    disabled={!draft.config.sticky}
                    value={draft.config.sticky_minutes}
                    onChange={(e) => setCfg({ sticky_minutes: Number(e.target.value) })}
                  />
                </div>
              </Field>
              <Field
                label="Tell callers which route served"
                hint="Adds X-Janus-Route, X-Janus-Route-Reason and X-Janus-Route-Model response headers."
              >
                <label className="switch">
                  <input
                    type="checkbox"
                    aria-label="Tell callers which route served"
                    checked={draft.config.disclose}
                    onChange={(e) => setCfg({ disclose: e.target.checked })}
                  />
                </label>
              </Field>
            </div>
          </details>

          {routerId ? (
            <section className="card">
              <div className="card-header">
                <div>
                  <h2>Access and controls</h2>
                  <p className="small secondary" style={{ margin: 0 }}>
                    A router is a managed model. These pages treat it like one.
                  </p>
                </div>
              </div>
              <ul className="routing-links small">
                <li>
                  <strong>Grants:</strong>{' '}
                  {grants && grants.length > 0 ? grants.map((g) => g.grantee_name).join(', ') : 'nobody can call it yet'} ·{' '}
                  <Link to="/admin/grants">Grants</Link>
                </li>
                <li>
                  <strong>Security:</strong> bind policies to <span className="mono">{draft.name}</span> (scope: managed model).
                  Policies on the model a route picks apply too. · <Link to="/admin/security/bindings">Bindings</Link>
                </li>
                <li>
                  <strong>Quotas:</strong> a quota on the router counts everything sent through it; a quota on a route’s model
                  still applies, or makes the route skip when “skip when over quota” is on. ·{' '}
                  <Link to="/admin/quotas">Quotas</Link>
                </li>
                <li>
                  <strong>Request log:</strong> filter by model <span className="mono">{draft.name}</span>; each row shows the
                  route reason. · <Link to={`/admin/requests?model=${encodeURIComponent(draft.name)}`}>Requests</Link>
                </li>
              </ul>
            </section>
          ) : null}
        </div>

        <aside className="stack routing-side" style={{ gap: 'var(--janus-space-4)' }}>
          <RouterTryBox routerId={routerId} name={draft.name} config={draft.config} />
          <section className="card stack" style={{ gap: 'var(--janus-space-2)' }}>
            <h2>What this router asks</h2>
            <p className="small secondary" style={{ margin: 0 }}>
              {routeNeeds(draft.config)}
            </p>
            {classifier ? (
              <p className="small muted" style={{ margin: 0 }}>
                Classifier: {classifier.name} ({classifier.style || 'auto'} questions)
                {classifier.used_by_security ? ' · shares the security check’s call' : ''}.
              </p>
            ) : (
              <p className="small" style={{ margin: 0, color: 'var(--janus-color-warning-fg)' }}>
                No classifier chosen. <Link to="/admin/routing/classifier">Choose one</Link>; until then every request is served
                by the default.
              </p>
            )}
          </section>
        </aside>
      </div>

      <Drawer
        open={editing !== null}
        onClose={() => setEditing(null)}
        title={editing !== null ? `Route ${editing + 1}` : 'Route'}
        description="When this route serves, and how."
        wide
        footer={
          <div className="row" style={{ justifyContent: 'flex-end' }}>
            <button type="button" className="btn btn-primary" onClick={() => setEditing(null)}>
              Done
            </button>
          </div>
        }
      >
        {editing !== null && draft.config.routes[editing] ? (
          <RouteEditor
            route={draft.config.routes[editing]!}
            hint={templateHints?.routes[editing]?.hint}
            targets={targetList}
            categories={(cats.data?.categories ?? []).filter((c) => c.enabled)}
            levels={doc.data?.thinking_levels ?? []}
            judges={doc.data?.judge_models ?? []}
            defaultJudge={doc.data?.settings.eval_judge_model_id ?? ''}
            defaultModelId={draft.config.default.model_id}
            onChange={(patch) => setRoute(editing, patch)}
          />
        ) : null}
      </Drawer>
    </div>
  );
}

function routeNeeds(cfg: RouterConfig): string {
  const cat = cfg.routes.some((r) => r.categories.length > 0);
  const diff =
    cfg.default.thinking === 'scale' ||
    cfg.routes.some((r) => r.difficulty_min > 0 || r.difficulty_max > 0 || r.thinking === 'scale');
  if (cfg.routes.length === 0) return 'Nothing: with no routes every request goes to the default, with no classifier call.';
  if (cat && diff) return 'Two questions per request: which category, and how difficult.';
  if (cat) return 'One question per request: which category.';
  if (diff) return 'One question per request: how difficult.';
  return 'Nothing to decide: every route matches any request, so the first one always serves.';
}

function TargetSelect({
  label,
  value,
  targets,
  placeholder,
  onChange,
}: {
  label: string;
  value: string;
  targets: RoutingTarget[];
  placeholder: string;
  onChange: (v: string) => void;
}): ReactNode {
  const managed = targets.filter((t) => t.kind === 'managed');
  const models = targets.filter((t) => t.kind === 'model');
  return (
    <select className="select" aria-label={label} value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="">{placeholder}</option>
      {managed.length > 0 ? (
        <optgroup label="Managed models">
          {managed.map((t) => (
            <option key={t.id} value={t.id}>
              {t.name}
              {t.pool ? ' (pool)' : ''}
              {t.upstream_name ? ` · ${t.upstream_name}` : ''}
            </option>
          ))}
        </optgroup>
      ) : null}
      <optgroup label="Models">
        {models.map((t) => (
          <option key={t.id} value={t.id}>
            {t.name} · {t.upstream_name}
            {t.hosting === 'self_hosted' ? ' · self-hosted' : ''}
          </option>
        ))}
      </optgroup>
    </select>
  );
}

function ThinkingSelect({
  label,
  value,
  levels,
  onChange,
}: {
  label: string;
  value: string;
  levels: string[];
  onChange: (v: string) => void;
}): ReactNode {
  return (
    <select className="select" aria-label={label} value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="passthrough">{thinkingLabel('passthrough')}</option>
      <option value="scale">{thinkingLabel('scale')}</option>
      {levels.map((l) => (
        <option key={l} value={l}>
          {thinkingLabel(l)}
        </option>
      ))}
    </select>
  );
}

function RouteEditor({
  route,
  hint,
  targets,
  categories,
  levels,
  judges,
  defaultJudge,
  defaultModelId,
  onChange,
}: {
  route: RouterRoute;
  hint?: string;
  targets: RoutingTarget[];
  categories: RoutingCategory[];
  levels: string[];
  judges: RoutingTarget[];
  defaultJudge: string;
  defaultModelId: string;
  onChange: (patch: Partial<RouterRoute>) => void;
}): ReactNode {
  const band = bandOf(route.difficulty_min, route.difficulty_max);
  const set = new Set(route.categories);
  const judge = route.eval.judge_model_id || defaultJudge;
  const judgeIsContestant = judge !== '' && (judge === route.model_id || judge === defaultModelId);
  return (
    <div className="stack" style={{ gap: 'var(--janus-space-5)' }}>
      <div className="routing-form-grid">
        <Field label="Route name" hint="Optional; shown in headers and reports.">
          <input
            className="input"
            aria-label="Route name"
            value={route.name}
            onChange={(e) => onChange({ name: e.target.value })}
          />
        </Field>
        <Field label="Send to" hint={hint ? `Suggested: ${hint}.` : 'A model or a managed model (alias or pool).'}>
          <TargetSelect
            label="Send to"
            value={route.model_id}
            targets={targets}
            placeholder="Choose a model"
            onChange={(v) => onChange({ model_id: v })}
          />
        </Field>
      </div>

      <fieldset className="routing-fieldset">
        <legend>Categories</legend>
        <p className="small muted" style={{ margin: '0 0 var(--janus-space-2)' }}>
          The route serves requests the classifier puts in any of these. None selected means any category.
        </p>
        <div className="secgw-chips">
          {categories.map((c) => (
            <button
              key={c.slug}
              type="button"
              className="secgw-chip"
              aria-pressed={set.has(c.slug)}
              title={c.linked_slug ? `Follows Usage insights: ${c.linked_name ?? c.linked_slug}` : c.includes}
              onClick={() =>
                onChange({
                  categories: set.has(c.slug) ? route.categories.filter((x) => x !== c.slug) : [...route.categories, c.slug],
                })
              }
            >
              {c.name}
              {c.linked_slug ? ' ↗' : ''}
            </button>
          ))}
        </div>
      </fieldset>

      <fieldset className="routing-fieldset">
        <legend>Difficulty</legend>
        <div className="segmented" role="radiogroup" aria-label="Difficulty">
          {DIFFICULTY_BANDS.map((b) => (
            <button
              key={b.id}
              type="button"
              role="radio"
              aria-checked={band === b.id}
              aria-pressed={band === b.id}
              onClick={() => onChange({ difficulty_min: b.min, difficulty_max: b.max })}
            >
              {b.label}
            </button>
          ))}
        </div>
        <div className="routing-form-grid" style={{ marginTop: 'var(--janus-space-3)' }}>
          <Field label="From (1–5, 0 = no minimum)">
            <input
              className="input"
              type="number"
              step={0.5}
              min={0}
              max={5}
              aria-label="Minimum difficulty"
              value={route.difficulty_min}
              onChange={(e) => onChange({ difficulty_min: Number(e.target.value) })}
            />
          </Field>
          <Field label="To (1–5, 0 = no maximum)">
            <input
              className="input"
              type="number"
              step={0.5}
              min={0}
              max={5}
              aria-label="Maximum difficulty"
              value={route.difficulty_max}
              onChange={(e) => onChange({ difficulty_max: Number(e.target.value) })}
            />
          </Field>
        </div>
        <p className="small muted" style={{ margin: 0 }}>
          The classifier rates each request 1 (trivial) to 5 (expert). Ratings are a weighted average, so 2.5 sits between easy
          and moderate.
        </p>
      </fieldset>

      <div className="routing-form-grid">
        <Field label="Thinking" hint="As requested keeps the caller’s setting. Scale with difficulty sends low / medium / high.">
          <ThinkingSelect label="Thinking" value={route.thinking} levels={levels} onChange={(v) => onChange({ thinking: v })} />
        </Field>
        <div className="stack" style={{ gap: 'var(--janus-space-2)' }}>
          <label className="row small" style={{ gap: 8 }}>
            <input
              type="checkbox"
              aria-label="Only for callers granted this model"
              checked={route.restrict_to_granted}
              onChange={(e) => onChange({ restrict_to_granted: e.target.checked })}
            />
            Only for callers also granted this model
          </label>
          <label className="row small" style={{ gap: 8 }}>
            <input
              type="checkbox"
              aria-label="Skip when over quota"
              checked={route.skip_when_over_quota}
              onChange={(e) => onChange({ skip_when_over_quota: e.target.checked })}
            />
            Skip this route when the caller is over a quota on its model
          </label>
        </div>
      </div>

      <fieldset className="routing-fieldset">
        <legend>
          <label className="row" style={{ gap: 8 }}>
            <input
              type="checkbox"
              aria-label="Evaluate this route"
              checked={route.eval.enabled}
              onChange={(e) => onChange({ eval: { ...route.eval, enabled: e.target.checked } })}
            />
            Evaluate against the default
          </label>
        </legend>
        <p className="small muted" style={{ margin: '0 0 var(--janus-space-3)' }}>
          After the caller has their answer, a sample of this route’s requests is sent to the default model too, and a judge
          compares the two answers blind, twice with the order swapped. Only the verdict is kept. Text chats without tools or
          images only.
        </p>
        {route.eval.enabled ? (
          <div className="routing-form-grid">
            <Field label="Sample (%)">
              <input
                className="input"
                type="number"
                min={0.1}
                max={100}
                step={0.5}
                aria-label="Sample percent"
                value={route.eval.sample_pct}
                onChange={(e) => onChange({ eval: { ...route.eval, sample_pct: Number(e.target.value) } })}
              />
            </Field>
            <Field label="Most per day">
              <input
                className="input"
                type="number"
                min={1}
                aria-label="Most evaluations per day"
                value={route.eval.max_per_day}
                onChange={(e) => onChange({ eval: { ...route.eval, max_per_day: Number(e.target.value) } })}
              />
            </Field>
            <Field label="Daily budget (USD)" hint="Default model’s answer plus both judge calls.">
              <input
                className="input"
                type="number"
                min={0}
                step={1}
                aria-label="Daily evaluation budget"
                value={route.eval.daily_budget_usd}
                onChange={(e) => onChange({ eval: { ...route.eval, daily_budget_usd: Number(e.target.value) } })}
              />
            </Field>
            <Field label="Run until" hint="Empty: 14 days from saving.">
              <input
                className="input"
                type="date"
                aria-label="Evaluate until"
                value={route.eval.until ? route.eval.until.slice(0, 10) : ''}
                onChange={(e) =>
                  onChange({ eval: { ...route.eval, until: e.target.value ? `${e.target.value}T23:59:59Z` : '' } })
                }
              />
            </Field>
            <Field
              label="Judge"
              hint={judgeIsContestant ? undefined : 'Defaults to the judge on the Evaluations tab.'}
              error={
                judgeIsContestant
                  ? 'The judge is one of the two models it compares; judges favour their own answers. Pick a third model.'
                  : undefined
              }
            >
              <select
                className="select"
                aria-label="Judge model"
                value={route.eval.judge_model_id}
                onChange={(e) => onChange({ eval: { ...route.eval, judge_model_id: e.target.value } })}
              >
                <option value="">Routing-wide judge</option>
                {judges.map((j) => (
                  <option key={j.id} value={j.id}>
                    {j.name} · {j.upstream_name}
                  </option>
                ))}
              </select>
            </Field>
          </div>
        ) : null}
      </fieldset>
    </div>
  );
}

/* --- Try routing --------------------------------------------------------------- */

function RouterTryBox({ routerId, name, config }: { routerId?: string; name: string; config: RouterConfig }): ReactNode {
  const [text, setText] = useState('');
  const catName = useCategoryName();
  const test = useMutation({
    mutationFn: () => api.post<RoutingTestResult>(`${BASE}/test`, { text, router_id: routerId ?? '', router_name: name, config }),
  });
  const res = test.data;
  const r = res?.routers[0];
  return (
    <section className="card stack" style={{ gap: 'var(--janus-space-3)' }}>
      <h2>Test this router</h2>
      <p className="small muted" style={{ margin: 0 }}>
        Uses the routes as they are on screen, saved or not. Nothing is sent to the models and nothing is stored.
      </p>
      <div className="secgw-chips">
        {DEMO_PROMPTS.map((d) => (
          <button key={d.label} type="button" className="secgw-chip" onClick={() => setText(d.text)}>
            {d.label}
          </button>
        ))}
      </div>
      <textarea
        className="textarea"
        rows={4}
        aria-label="Prompt to route"
        placeholder="Paste a prompt…"
        value={text}
        onChange={(e) => setText(e.target.value)}
      />
      <div className="row" style={{ justifyContent: 'flex-end' }}>
        <button
          type="button"
          className="btn"
          disabled={!text.trim() || test.isPending || !config.default.model_id}
          onClick={() => test.mutate()}
        >
          {test.isPending ? 'Routing…' : 'Route it'}
        </button>
      </div>
      {test.isError ? <div className="banner banner-danger">{errorMessage(test.error)}</div> : null}
      {res && res.status !== 'ok' ? (
        <div className="banner banner-danger">
          Classifier failed after {formatDuration(res.latency_ms)}: {res.error}. A live request would go to the default.
        </div>
      ) : null}
      {res && res.status === 'ok' && r ? (
        <div className="stack" style={{ gap: 'var(--janus-space-2)' }} data-testid="try-result">
          <div className="routing-verdict">
            <div className="overline">Would be served by</div>
            <div style={{ fontWeight: 600 }}>{r.model_name || '—'}</div>
            <div className="small muted">
              {r.route_name} · {reasonLabel(r.reason)}
              {r.thinking ? ` · thinking ${thinkingLabel(r.thinking)}` : ''}
            </div>
          </div>
          <div className="small">
            {catName(res.category)} <span className="muted">{pct(res.confidence)}</span> · difficulty{' '}
            <span className="num">{res.difficulty.toFixed(1)}</span> · {formatDuration(res.latency_ms)}
            {res.calls > 1 ? ` · ${res.calls} calls` : ''}
          </div>
          <ol className="routing-trace">
            {r.trace.map((s, i) => (
              <li key={i} className={s.outcome === 'matched' ? 'is-match' : ''}>
                <span className="mono small">{s.index < 0 ? 'default' : `route ${s.index + 1}`}</span>{' '}
                <span className="small">
                  {s.outcome === 'matched' ? '✓' : '✕'} {s.detail}
                </span>
              </li>
            ))}
          </ol>
        </div>
      ) : null}
    </section>
  );
}

/* --- Categories ------------------------------------------------------------------ */

interface CategoryDraft {
  id?: string;
  name: string;
  includes: string;
  excludes: string;
  linked_slug: string;
  min_confidence: number;
  examples: string;
  builtin: boolean;
}

function CategoriesPanel(): ReactNode {
  const cats = useRoutingCategories();
  const qc = useQueryClient();
  const toast = useToast();
  const [editing, setEditing] = useState<CategoryDraft | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<RoutingCategory | null>(null);
  const invalidate = () => void qc.invalidateQueries({ queryKey: KEY });
  const save = useMutation({
    mutationFn: (d: CategoryDraft) => {
      const body = {
        name: d.name,
        includes: d.includes,
        excludes: d.excludes,
        linked_slug: d.linked_slug,
        min_confidence: d.min_confidence,
        examples: d.examples
          .split('\n')
          .map((x) => x.trim())
          .filter(Boolean),
      };
      return d.id ? api.patch(`${BASE}/categories/${d.id}`, body) : api.post(`${BASE}/categories`, body);
    },
    onSuccess: () => {
      invalidate();
      setEditing(null);
      setError(null);
      toast('Category saved.', 'success');
    },
    onError: (e) => setError(errorMessage(e)),
  });
  const toggle = useMutation({
    mutationFn: (c: RoutingCategory) => api.patch(`${BASE}/categories/${c.id}`, { enabled: !c.enabled }),
    onSuccess: invalidate,
    onError: (e) => toast(errorMessage(e), 'danger'),
  });
  const reset = useMutation({
    mutationFn: (id: string) => api.post(`${BASE}/categories/${id}/reset`),
    onSuccess: () => {
      invalidate();
      setEditing(null);
      toast('Restored the built-in definition.', 'success');
    },
    onError: (e) => setError(errorMessage(e)),
  });
  const del = useMutation({
    mutationFn: (id: string) => api.del(`${BASE}/categories/${id}`),
    onSuccess: () => {
      invalidate();
      setDeleting(null);
    },
    onError: (e) => toast(errorMessage(e), 'danger'),
  });
  const open = (c?: RoutingCategory) => {
    setError(null);
    setEditing(
      c
        ? {
            id: c.id,
            name: c.name,
            includes: c.includes,
            excludes: c.excludes,
            linked_slug: c.linked_slug,
            min_confidence: c.min_confidence,
            examples: c.examples.join('\n'),
            builtin: c.builtin,
          }
        : { name: '', includes: '', excludes: '', linked_slug: '', min_confidence: 0.5, examples: '', builtin: false },
    );
  };
  return (
    <section className="card card-flush">
      <div className="card-header" style={{ padding: 'var(--janus-card-padding)' }}>
        <div>
          <h2>Routing categories</h2>
          <p className="small secondary" style={{ margin: 0 }}>
            What the classifier chooses between. Each description is what it reads, so say what belongs and which neighbours do
            not. All enabled categories are offered on every request, so near neighbours help it decide.
          </p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => open()}>
          New category
        </button>
      </div>
      <AsyncSection query={cats}>
        {(data) => (
          <div className="table-wrap">
            <table className="data routing-table">
              <thead>
                <tr>
                  <th>Category</th>
                  <th>Defined by</th>
                  <th className="num">Min. confidence</th>
                  <th className="num">Test examples</th>
                  <th>Used by</th>
                  <th>Enabled</th>
                  <th aria-label="Actions" />
                </tr>
              </thead>
              <tbody>
                {data.categories.map((c) => (
                  <tr key={c.id} data-clickable="true" onClick={() => open(c)}>
                    <th scope="row">
                      <div>{c.name}</div>
                      <div className="small muted routing-cell-note">
                        {c.linked_slug ? `Follows Usage insights: ${c.linked_name ?? c.linked_slug}` : c.includes}
                      </div>
                    </th>
                    <td className="small">
                      {c.linked_slug ? (
                        <Badge tone="info">linked</Badge>
                      ) : c.builtin ? (
                        <Badge>built-in</Badge>
                      ) : (
                        <Badge tone="primary">custom</Badge>
                      )}
                    </td>
                    <td className="num">{pct(c.min_confidence)}</td>
                    <td className="num">
                      {c.examples.length === 0 ? <span className="muted">0</span> : formatNumber(c.examples.length)}
                    </td>
                    <td className="small mono">
                      {c.used_by.length === 0 ? <span className="muted">—</span> : c.used_by.join(', ')}
                    </td>
                    <td onClick={(e) => e.stopPropagation()}>
                      <label className="switch">
                        <input
                          type="checkbox"
                          aria-label={`Enable ${c.name}`}
                          checked={c.enabled}
                          onChange={() => toggle.mutate(c)}
                        />
                      </label>
                    </td>
                    <td onClick={(e) => e.stopPropagation()}>
                      <RowActions>
                        <IconButton icon="edit" label={`Edit ${c.name}`} onClick={() => open(c)} />
                        {!c.builtin ? (
                          <IconButton icon="delete" danger label={`Delete ${c.name}`} onClick={() => setDeleting(c)} />
                        ) : null}
                      </RowActions>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </AsyncSection>
      <Drawer
        open={!!editing}
        onClose={() => setEditing(null)}
        title={editing?.id ? editing.name || 'Category' : 'New category'}
        description="The classifier reads the name and the descriptions; write them for it."
        wide
        footer={
          <div className="row-between">
            <div>
              {editing?.builtin && editing.id ? (
                <button type="button" className="btn btn-ghost" onClick={() => editing.id && reset.mutate(editing.id)}>
                  Restore built-in
                </button>
              ) : null}
            </div>
            <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
              <button type="button" className="btn btn-ghost" onClick={() => setEditing(null)}>
                Cancel
              </button>
              <button
                type="button"
                className="btn btn-primary"
                disabled={save.isPending || !editing?.name.trim()}
                onClick={() => editing && save.mutate(editing)}
              >
                Save
              </button>
            </div>
          </div>
        }
      >
        {editing ? (
          <div className="stack" style={{ gap: 'var(--janus-space-4)' }}>
            {error ? <div className="banner banner-danger">{error}</div> : null}
            <Field label="Name">
              <input
                className="input"
                aria-label="Category name"
                value={editing.name}
                onChange={(e) => setEditing({ ...editing, name: e.target.value })}
              />
            </Field>
            <Field label="Definition">
              <div className="segmented" role="radiogroup" aria-label="Definition">
                <button
                  type="button"
                  role="radio"
                  aria-checked={!editing.linked_slug}
                  aria-pressed={!editing.linked_slug}
                  onClick={() => setEditing({ ...editing, linked_slug: '' })}
                >
                  Write a description
                </button>
                <button
                  type="button"
                  role="radio"
                  aria-checked={!!editing.linked_slug}
                  aria-pressed={!!editing.linked_slug}
                  onClick={() =>
                    setEditing({
                      ...editing,
                      linked_slug: editing.linked_slug || (data(cats)?.insights_categories[0]?.slug ?? ''),
                    })
                  }
                >
                  Follow a Usage insights category
                </button>
              </div>
            </Field>
            {editing.linked_slug ? (
              <Field label="Usage insights category" hint="This category uses that one’s description and changes when it does.">
                <select
                  className="select"
                  aria-label="Usage insights category"
                  value={editing.linked_slug}
                  onChange={(e) => setEditing({ ...editing, linked_slug: e.target.value })}
                >
                  {(data(cats)?.insights_categories ?? []).map((c) => (
                    <option key={c.slug} value={c.slug}>
                      {c.parent_slug ? '  › ' : ''}
                      {c.name}
                    </option>
                  ))}
                </select>
              </Field>
            ) : (
              <>
                <Field label="Includes" hint="What belongs here. Be concrete: kinds of request, not topics.">
                  <textarea
                    className="textarea"
                    rows={4}
                    aria-label="Includes"
                    value={editing.includes}
                    onChange={(e) => setEditing({ ...editing, includes: e.target.value })}
                  />
                </Field>
                <Field
                  label="Does not include"
                  hint="Near neighbours and where they belong instead. This is what stops misfiling."
                >
                  <textarea
                    className="textarea"
                    rows={3}
                    aria-label="Does not include"
                    value={editing.excludes}
                    onChange={(e) => setEditing({ ...editing, excludes: e.target.value })}
                  />
                </Field>
              </>
            )}
            <Field
              label={`Minimum confidence: ${pct(editing.min_confidence)}`}
              hint="Below this, the answer does not count as this category and the request is not routed by it."
            >
              <input
                type="range"
                min={0}
                max={0.95}
                step={0.05}
                aria-label="Minimum confidence"
                value={editing.min_confidence}
                onChange={(e) => setEditing({ ...editing, min_confidence: Number(e.target.value) })}
              />
            </Field>
            <Field
              label="Test examples (one per line)"
              hint="Prompts that SHOULD land here. The Classifier tab’s batch test scores them."
            >
              <textarea
                className="textarea mono"
                rows={6}
                aria-label="Test examples"
                value={editing.examples}
                onChange={(e) => setEditing({ ...editing, examples: e.target.value })}
              />
            </Field>
          </div>
        ) : null}
      </Drawer>
      <ConfirmDialog
        open={!!deleting}
        title={`Delete ${deleting?.name ?? ''}?`}
        consequence="The category is removed from the classifier's choices."
        confirmLabel="Delete"
        danger
        busy={del.isPending}
        onClose={() => setDeleting(null)}
        onConfirm={() => {
          if (deleting) del.mutate(deleting.id);
        }}
      />
    </section>
  );
}

function data<T>(q: { data?: T }): T | undefined {
  return q.data;
}

/* --- Classifier ---------------------------------------------------------------------- */

function ClassifierPanel(): ReactNode {
  const doc = useRoutingDoc();
  return <AsyncSection query={doc}>{(d) => <ClassifierForm doc={d} />}</AsyncSection>;
}

function ClassifierForm({ doc }: { doc: RoutingDocument }): ReactNode {
  const qc = useQueryClient();
  const toast = useToast();
  const [draft, setDraft] = useState<RoutingSettings>(doc.settings);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => setDraft(doc.settings), [doc.settings]);
  const dirty = JSON.stringify(draft) !== JSON.stringify(doc.settings);
  const save = useMutation({
    mutationFn: (s: RoutingSettings) => api.put<RoutingDocument>(`${BASE}/settings`, s),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: KEY });
      setError(null);
      toast('Routing settings saved.', 'success');
    },
    onError: (e) => setError(errorMessage(e)),
  });
  const chosen = doc.classifiers.find((c) => c.id === draft.classifier_model_id);
  return (
    <div className="grid grid-halves" style={{ alignItems: 'start' }}>
      <section className="card stack" style={{ gap: 'var(--janus-space-4)' }}>
        <div>
          <h2>Routing classifier</h2>
          <p className="small secondary" style={{ margin: 0 }}>
            A decision model answers which category and how difficult. Any model with the decision role on the Models page can be
            used.
          </p>
        </div>
        {error ? <div className="banner banner-danger">{error}</div> : null}
        {!doc.running && doc.problem && !dirty ? (
          <div className="banner banner-warning">{doc.problem} Every router serves its default until this is fixed.</div>
        ) : null}
        {doc.classifiers.length === 0 ? (
          <div className="banner banner-info">
            No decision model yet. Add an upstream that serves Jev (TypeSafe or OpenRouter), Clef or Nimble (llama.cpp), then set
            its role to “decision” on the <Link to="/admin/models">Models</Link> page.
          </div>
        ) : (
          <div className="stack" role="radiogroup" aria-label="Routing classifier" style={{ gap: 'var(--janus-space-2)' }}>
            {doc.classifiers.map((c) => (
              <label key={c.id} className={`routing-option${draft.classifier_model_id === c.id ? ' is-selected' : ''}`}>
                <input
                  type="radio"
                  name="routing-classifier"
                  checked={draft.classifier_model_id === c.id}
                  onChange={() => setDraft({ ...draft, classifier_model_id: c.id })}
                />
                <span className="stack" style={{ gap: 2 }}>
                  <span className="row" style={{ gap: 6 }}>
                    <strong>{c.name}</strong> <span className="small muted">{c.upstream_name}</span> {hostingBadge(c.hosting)}
                  </span>
                  <span className="small secondary">
                    {c.style === 'combined'
                      ? 'Answers all questions in one request.'
                      : 'Answers one question per request, in parallel (more accurate for this model).'}
                    {c.used_by_security ? ' Already used by a security policy: routing questions can share that call.' : ''}
                  </span>
                </span>
              </label>
            ))}
          </div>
        )}
        {chosen?.hosting === 'external' ? (
          <p className="small" style={{ margin: 0, color: 'var(--janus-color-warning-fg)' }}>
            This classifier is externally hosted: it reads every prompt sent to a router, including ones you route to self-hosted
            models.
          </p>
        ) : null}
        <p className="small muted" style={{ margin: 0 }}>
          In our tests the hosted Jev model was the fastest (a few hundred milliseconds) and as accurate as the larger self-hosted
          Clef; Clef-Flash and Nimble are faster self-hosted choices that misfile more borderline requests. Run the batch test
          below on your own examples.
        </p>
        <details>
          <summary className="small">Advanced</summary>
          <div className="routing-form-grid" style={{ marginTop: 'var(--janus-space-3)' }}>
            <Field label="Difficulty question">
              <select
                className="select"
                aria-label="Difficulty question"
                value={draft.difficulty_style}
                onChange={(e) => setDraft({ ...draft, difficulty_style: e.target.value as RoutingSettings['difficulty_style'] })}
              >
                <option value="general">One general scale</option>
                <option value="category">Coding-specific scale for coding (one more question)</option>
              </select>
            </Field>
            <Field
              label="Decisions at once (per replica)"
              hint="Above this, requests skip routing and go to the default rather than queue."
            >
              <input
                className="input"
                type="number"
                min={1}
                max={1024}
                aria-label="Decisions at once"
                value={draft.max_concurrent_decisions}
                onChange={(e) => setDraft({ ...draft, max_concurrent_decisions: Number(e.target.value) })}
              />
            </Field>
            <Field label="Characters read" hint="The most recent text of the new turn is kept.">
              <input
                className="input"
                type="number"
                min={200}
                max={200000}
                aria-label="Characters read"
                value={draft.max_chars}
                onChange={(e) => setDraft({ ...draft, max_chars: Number(e.target.value) })}
              />
            </Field>
            <Field
              label="Keep decisions (days)"
              hint="0 keeps them. Callers are no longer named once their usage events are purged."
            >
              <input
                className="input"
                type="number"
                min={0}
                max={3650}
                aria-label="Keep decisions"
                value={draft.retention_days}
                onChange={(e) => setDraft({ ...draft, retention_days: Number(e.target.value) })}
              />
            </Field>
          </div>
        </details>
        <div className="row" style={{ justifyContent: 'flex-end', gap: 'var(--janus-space-2)' }}>
          <button type="button" className="btn btn-ghost" disabled={!dirty} onClick={() => setDraft(doc.settings)}>
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
      <div className="stack" style={{ gap: 'var(--janus-space-4)' }}>
        <TryRouting classifierId={draft.classifier_model_id} />
        <BatchTest classifierId={draft.classifier_model_id} />
      </div>
    </div>
  );
}

function TryRouting({ classifierId }: { classifierId: string }): ReactNode {
  const [text, setText] = useState('');
  const catName = useCategoryName();
  const test = useMutation({
    mutationFn: () => api.post<RoutingTestResult>(`${BASE}/test`, { text, classifier_model_id: classifierId }),
  });
  const res = test.data;
  const probs = Object.entries(res?.probabilities ?? {})
    .sort((a, b) => b[1] - a[1])
    .slice(0, 5);
  return (
    <section className="card stack" style={{ gap: 'var(--janus-space-3)' }}>
      <h2>Try routing</h2>
      <p className="small muted" style={{ margin: 0 }}>
        Classifies a prompt with the classifier selected on the left (saved or not) and shows where every router would send it.
      </p>
      <div className="secgw-chips">
        {DEMO_PROMPTS.map((d) => (
          <button key={d.label} type="button" className="secgw-chip" onClick={() => setText(d.text)}>
            {d.label}
          </button>
        ))}
      </div>
      <textarea
        className="textarea"
        rows={4}
        aria-label="Prompt to classify"
        value={text}
        onChange={(e) => setText(e.target.value)}
      />
      <div className="row" style={{ justifyContent: 'flex-end' }}>
        <button
          type="button"
          className="btn"
          disabled={!text.trim() || !classifierId || test.isPending}
          onClick={() => test.mutate()}
        >
          {test.isPending ? 'Classifying…' : 'Classify and route'}
        </button>
      </div>
      {test.isError ? <div className="banner banner-danger">{errorMessage(test.error)}</div> : null}
      {res && res.status !== 'ok' ? (
        <div className="banner banner-danger">
          Classifier failed after {formatDuration(res.latency_ms)}: {res.error}
        </div>
      ) : null}
      {res && res.status === 'ok' ? (
        <div className="stack" style={{ gap: 'var(--janus-space-3)' }}>
          <div className="grid routing-tiles">
            <MiniTile label="Category" value={catName(res.category)} meta={pct(res.confidence)} />
            <MiniTile label="Difficulty" value={res.difficulty.toFixed(1)} meta="1 trivial · 5 expert" />
            <MiniTile
              label="Classifier time"
              value={formatDuration(res.latency_ms)}
              meta={`${res.calls} call${res.calls === 1 ? '' : 's'}`}
            />
          </div>
          {probs.length > 1 ? (
            <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0, gap: 'var(--janus-space-2)' }}>
              {probs.map(([slug, p], i) => (
                <li key={slug}>
                  <div className="row-between small" style={{ marginBottom: 2 }}>
                    <span>{catName(slug)}</span>
                    <span className="num muted">{(p * 100).toFixed(1)}%</span>
                  </div>
                  <div className="meter" aria-hidden="true">
                    <div className="meter-fill" style={{ width: `${Math.max(1, p * 100)}%`, background: seriesColor(i) }} />
                  </div>
                </li>
              ))}
            </ul>
          ) : null}
          {res.routers.length > 0 ? (
            <div className="table-wrap">
              <table className="data data-compact routing-table">
                <thead>
                  <tr>
                    <th>Router</th>
                    <th>Served by</th>
                    <th>Why</th>
                  </tr>
                </thead>
                <tbody>
                  {res.routers.map((r) => (
                    <tr key={r.router_id}>
                      <th scope="row" className="mono small">
                        {r.router_name}
                      </th>
                      <td className="small">
                        {r.model_name}
                        {r.thinking ? <span className="muted"> · thinking {thinkingLabel(r.thinking)}</span> : null}
                      </td>
                      <td
                        className="small"
                        title={r.trace.map((s) => `${s.index < 0 ? 'default' : `route ${s.index + 1}`}: ${s.detail}`).join('\n')}
                      >
                        {r.route_name} · {reasonLabel(r.reason)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <p className="small muted" style={{ margin: 0 }}>
              No routers yet.
            </p>
          )}
        </div>
      ) : null}
    </section>
  );
}

function MiniTile({ label, value, meta }: { label: string; value: string; meta?: string }): ReactNode {
  return (
    <article className="tile">
      <div className="overline">{label}</div>
      <div className="tile-value routing-tile-value">{value}</div>
      {meta ? <div className="tile-meta">{meta}</div> : null}
    </article>
  );
}

function BatchTest({ classifierId }: { classifierId: string }): ReactNode {
  const run = useMutation({
    mutationFn: () => api.post<BatchResult>(`${BASE}/test-batch`, { classifier_model_id: classifierId }),
  });
  const [open, setOpen] = useState<string | null>(null);
  const res = run.data;
  return (
    <section className="card stack" style={{ gap: 'var(--janus-space-3)' }}>
      <div className="row-between">
        <div>
          <h2>Batch test</h2>
          <p className="small muted" style={{ margin: 0 }}>
            Runs every enabled category’s test examples and shows which ones the classifier misfiles, and where to.
          </p>
        </div>
        <button type="button" className="btn" disabled={!classifierId || run.isPending} onClick={() => run.mutate()}>
          {run.isPending ? 'Running…' : 'Run'}
        </button>
      </div>
      {run.isError ? <div className="banner banner-danger">{errorMessage(run.error)}</div> : null}
      {res ? (
        <>
          <p className="small" style={{ margin: 0 }}>
            <strong>{pct(res.accuracy)}</strong> correct ({formatNumber(res.correct)} of {formatNumber(res.total)}) in{' '}
            {formatDuration(res.elapsed_ms)}
            {res.failed > 0 ? ` · ${res.failed} failed` : ''} · {res.classifier}
          </p>
          <div className="table-wrap">
            <table className="data data-compact routing-table">
              <thead>
                <tr>
                  <th>Category</th>
                  <th className="num">Correct</th>
                  <th>Misfiled as</th>
                </tr>
              </thead>
              <tbody>
                {res.categories.map((c) => (
                  <Fragment key={c.slug}>
                    <tr
                      data-clickable={c.misses.length > 0 ? 'true' : undefined}
                      onClick={() => setOpen(open === c.slug ? null : c.slug)}
                    >
                      <th scope="row" className="small">
                        {c.name || c.slug}
                      </th>
                      <td className="num small">
                        <Badge tone={c.accuracy >= 0.9 ? 'success' : c.accuracy >= 0.7 ? 'warning' : 'danger'}>
                          {c.correct}/{c.total}
                        </Badge>
                      </td>
                      <td className="small">
                        {c.misses.map((m) => m.got || 'error').join(', ') || <span className="muted">—</span>}
                      </td>
                    </tr>
                    {open === c.slug
                      ? c.misses.map((m, i) => (
                          <tr key={i} className="routing-miss">
                            <td colSpan={3} className="small">
                              <span className="muted">“{m.text.length > 160 ? `${m.text.slice(0, 160)}…` : m.text}”</span> →{' '}
                              {m.error ? m.error : `${m.got} at ${pct(m.confidence)}`}
                            </td>
                          </tr>
                        ))
                      : null}
                  </Fragment>
                ))}
              </tbody>
            </table>
          </div>
          <p className="small muted" style={{ margin: 0 }}>
            Fix a misfile by naming the neighbour in the category’s “Does not include”, or lower its minimum confidence if the
            answer was right but unsure.
          </p>
        </>
      ) : null}
    </section>
  );
}

/* --- Evaluations ----------------------------------------------------------------------- */

const VERDICT_TONE: Record<string, Tone> = { keep: 'success', losing: 'danger', needs_data: 'neutral' };
const VERDICT_LABEL: Record<string, string> = { keep: 'Keep', losing: 'Losing', needs_data: 'Needs more data' };

function EvaluationsPanel(): ReactNode {
  const routers = useRouters();
  const doc = useRoutingDoc();
  const targets = useTargets();
  const localOnly = useLocalOnly();
  const qc = useQueryClient();
  const toast = useToast();
  const [detail, setDetail] = useState<EvalDetailTarget | null>(null);
  const [judge, setJudge] = useState<string | null>(null);
  const [limits, setLimits] = useState<Pick<
    RoutingSettings,
    'eval_max_concurrent' | 'eval_pause_above_in_flight' | 'eval_max_output_tokens'
  > | null>(null);
  const saveSettings = useMutation({
    mutationFn: (patch: Partial<RoutingSettings>) => api.put(`${BASE}/settings`, { ...(doc.data?.settings ?? {}), ...patch }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: KEY });
      setJudge(null);
      setLimits(null);
      toast('Evaluation settings saved.', 'success');
    },
    onError: (e) => toast(errorMessage(e), 'danger'),
  });
  const judgeValue = judge ?? doc.data?.settings.eval_judge_model_id ?? '';
  const settings = doc.data?.settings;
  const limitValues = limits ?? {
    eval_max_concurrent: settings?.eval_max_concurrent ?? 2,
    eval_pause_above_in_flight: settings?.eval_pause_above_in_flight ?? 50,
    eval_max_output_tokens: settings?.eval_max_output_tokens ?? 2048,
  };
  const setLimit = (k: keyof typeof limitValues, v: number) => setLimits({ ...limitValues, [k]: v });
  const nameOf = (id?: string) =>
    !id
      ? '—'
      : ((targets.data?.targets ?? []).find((t) => t.id === id)?.name ??
        (doc.data?.judge_models ?? []).find((j) => j.id === id)?.name ??
        'a model no longer in the catalog');
  const rows = (routers.data?.routers ?? []).flatMap((r) =>
    r.config.routes
      .filter((x) => x.eval.enabled || (r.evaluations ?? []).some((e) => e.route_id === x.id))
      .map((x) => ({ router: r, route: x, ...routeEvalPairings(r.evaluations, x.id) })),
  );
  const evalWarnings = doc.data?.eval_warnings ?? [];
  return (
    <div className="stack" style={{ gap: 'var(--janus-space-4)' }}>
      <section className="card stack" style={{ gap: 'var(--janus-space-3)' }}>
        <div>
          <h2>How routes are evaluated</h2>
          <p className="small secondary" style={{ margin: 0 }}>
            For a sample of a route’s requests, after the caller has their answer, Janus asks the router’s default model the same
            question (as the default would have been asked, with its own thinking setting) and has a judge model compare the two
            answers without knowing which is which, twice, with the order swapped. If the two verdicts disagree it counts as a
            tie. Only verdicts, scores and cost are kept, never the prompt or answers. Evaluation calls are platform overhead and
            never count against the caller’s quota.
          </p>
          <p className="small secondary" style={{ margin: 'var(--janus-space-2) 0 0' }}>
            The copies sent to the default and the judge go through the Security Gateway policies that would apply to the caller
            sending them there. A sample a policy would block or redact is skipped and counted, never sent.
          </p>
        </div>
        {evalWarnings.length > 0 ? (
          <div className="banner banner-warning" role="status">
            <ul style={{ margin: 0, paddingLeft: '1.2em' }}>
              {evalWarnings.map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          </div>
        ) : null}
        <div className="routing-form-grid">
          <Field
            label="Judge model"
            hint="Pick a strong model that is not one of the two being compared: judges favour their own answers."
          >
            <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
              <select className="select" aria-label="Judge model" value={judgeValue} onChange={(e) => setJudge(e.target.value)}>
                <option value="">Choose a judge</option>
                {(doc.data?.judge_models ?? []).map((j) => (
                  <option key={j.id} value={j.id}>
                    {j.name} · {j.upstream_name}
                  </option>
                ))}
              </select>
              <button
                type="button"
                className="btn"
                disabled={judge === null || saveSettings.isPending}
                onClick={() => saveSettings.mutate({ eval_judge_model_id: judgeValue })}
              >
                Save
              </button>
            </div>
          </Field>
          <div className="small secondary">
            {formatNumber(doc.data?.eval_running ?? 0)} running and {formatNumber(doc.data?.eval_queue ?? 0)} waiting on this
            replica now.
          </div>
        </div>
        <fieldset className="routing-fieldset">
          <legend>Limits</legend>
          <div className="routing-form-grid">
            <Field label="At once per replica" hint="Evaluations running at the same time on each replica (1–32).">
              <input
                className="input"
                type="number"
                min={1}
                max={32}
                aria-label="Evaluations at once per replica"
                value={limitValues.eval_max_concurrent}
                onChange={(e) => setLimit('eval_max_concurrent', Number(e.target.value))}
              />
            </Field>
            <Field
              label="Pause above (requests in flight)"
              hint="Evaluations wait while the replica is this busy. 0 never pauses."
            >
              <input
                className="input"
                type="number"
                min={0}
                aria-label="Pause evaluations above requests in flight"
                value={limitValues.eval_pause_above_in_flight}
                onChange={(e) => setLimit('eval_pause_above_in_flight', Number(e.target.value))}
              />
            </Field>
            <Field label="Default answer cap (tokens)" hint="Longest answer asked of the default (64–32,000).">
              <input
                className="input"
                type="number"
                min={64}
                max={32000}
                aria-label="Default answer cap in tokens"
                value={limitValues.eval_max_output_tokens}
                onChange={(e) => setLimit('eval_max_output_tokens', Number(e.target.value))}
              />
            </Field>
          </div>
          <div className="row" style={{ justifyContent: 'flex-end', gap: 'var(--janus-space-2)' }}>
            <button type="button" className="btn btn-ghost" disabled={limits === null} onClick={() => setLimits(null)}>
              Discard
            </button>
            <button
              type="button"
              className="btn"
              disabled={limits === null || saveSettings.isPending}
              onClick={() => saveSettings.mutate(limitValues)}
            >
              Save limits
            </button>
          </div>
        </fieldset>
      </section>
      <section className="stack" style={{ gap: 'var(--janus-space-3)' }} aria-label="Routes under evaluation">
        <div>
          <h2 style={{ margin: 0 }}>Routes under evaluation</h2>
          <p className="small secondary" style={{ margin: 0 }}>
            Last 30 days. Each card asks one question: is the model this route sends work to as good as the router’s default for
            that work? A card counts only comparisons between the models the route uses now; after you change the route’s model,
            the default or the judge, the tally starts again and earlier comparisons are kept as history. Open a card for the
            judge’s reasons.
          </p>
        </div>
        <AsyncSection query={routers}>
          {() =>
            rows.length === 0 ? (
              <div className="card">
                <p className="muted small" style={{ margin: 0 }}>
                  No route is being evaluated. Open a router, edit a route and tick “Evaluate against the default”.
                </p>
              </div>
            ) : (
              <div className="routing-eval-cards">
                {rows.map(({ router, route, current, older }) => (
                  <EvalCard
                    key={router.id + route.id}
                    router={router}
                    route={route}
                    summary={current}
                    older={older}
                    nameOf={nameOf}
                    localOnly={localOnly}
                    judgeName={
                      (doc.data?.judge_models ?? []).find((j) => j.id === (route.eval.judge_model_id || judgeValue))?.name
                    }
                    onOpen={(pairing) => {
                      // No tally yet for the current models: list only
                      // comparisons between them, never older pairings.
                      const p = pairing ?? {
                        routed_model_id: route.model_id,
                        default_model_id: router.config.default.model_id,
                        judge_model_id: route.eval.judge_model_id || (doc.data?.settings.eval_judge_model_id ?? ''),
                      };
                      const isCurrent = !pairing || pairing.current !== false;
                      setDetail({
                        router,
                        routeId: route.id,
                        pairing: p,
                        routedName: isCurrent ? route.model_name || nameOf(p.routed_model_id) : nameOf(p.routed_model_id),
                        defaultName: isCurrent
                          ? router.config.default.model_name || nameOf(p.default_model_id)
                          : nameOf(p.default_model_id),
                      });
                    }}
                  />
                ))}
              </div>
            )
          }
        </AsyncSection>
      </section>
      <EvalDetail detail={detail} onClose={() => setDetail(null)} localOnly={localOnly} />
    </div>
  );
}

interface EvalDetailTarget {
  router: Router;
  routeId: string;
  /** The pairing (route model, default, judge) whose comparisons to list. */
  pairing?: Pick<RouteEvalSummary, 'routed_model_id' | 'default_model_id' | 'judge_model_id'>;
  routedName: string;
  defaultName: string;
}

const MIN_COMPARISONS = 10;

/** One route's evaluation, in words first and numbers second. */
function EvalCard({
  router,
  route,
  summary: s,
  older,
  nameOf,
  localOnly,
  judgeName,
  onOpen,
}: {
  router: Router;
  route: RouterConfig['routes'][number];
  summary?: RouteEvalSummary;
  older: RouteEvalSummary[];
  nameOf: (id?: string) => string;
  localOnly: boolean;
  judgeName?: string;
  onOpen: (pairing?: RouteEvalSummary) => void;
}): ReactNode {
  const n = s?.evaluated ?? 0;
  const verdict = s?.verdict ?? 'needs_data';
  const expired = !!route.eval.until && new Date(route.eval.until) < new Date();
  const catName = useCategoryName();
  const what = route.categories.length > 0 ? route.categories.map(catName).join(', ') : 'the requests this route matches';
  const routed = route.model_name || 'the routed model';
  const def = router.config.default.model_name || 'the default';
  const perReq = (nano: number) => (n > 0 ? formatUSD(nano / n) : '—');
  const saving = s && s.default_cost_nanousd > 0 ? 1 - s.routed_cost_nanousd / s.default_cost_nanousd : null;
  const status = !route.eval.enabled
    ? 'Evaluation stopped'
    : expired
      ? `Ended ${formatRelative(route.eval.until)}`
      : router.status !== 'live'
        ? 'Starts when the router is live'
        : `Sampling ${route.eval.sample_pct}% of requests${route.eval.until ? ` until ${formatDateTime(route.eval.until).split(',')[0]}` : ''}`;
  const meaning =
    verdict === 'keep'
      ? `${routed} is holding up: across ${n} comparisons it was at least as good as ${def} often enough to keep the route.`
      : verdict === 'losing'
        ? `${routed} is losing to ${def} for this work. Consider sending it to the default, or to a stronger model.`
        : n < MIN_COMPARISONS
          ? `Too early to call: ${n} of the ${MIN_COMPARISONS} comparisons needed so far.`
          : `Still too close to call after ${n} comparisons. It keeps sampling until the result is clear.`;
  return (
    <article className="card routing-eval-card" aria-label={`${router.name}: ${route.name || routed}`}>
      <header className="routing-eval-card-head">
        <div style={{ minWidth: 0 }}>
          <div className="small muted mono">
            {router.name} · {route.name || 'route'}
          </div>
          <h3 className="routing-eval-question">
            Is <strong>{routed}</strong> as good as <strong>{def}</strong> for {what}?
          </h3>
        </div>
        <Badge tone={VERDICT_TONE[verdict] ?? 'neutral'}>{VERDICT_LABEL[verdict]}</Badge>
      </header>
      <p className="small" style={{ margin: 0 }}>
        {meaning}
      </p>
      {n > 0 && s ? (
        <>
          <div>
            <div
              className="routing-mixbar routing-eval-bar"
              role="img"
              aria-label={`${s.better} better, ${s.ties} tie, ${s.worse} worse`}
            >
              <span style={{ width: `${(s.better / n) * 100}%`, background: 'var(--janus-color-success-solid)' }} />
              <span style={{ width: `${(s.ties / n) * 100}%`, background: 'var(--janus-color-border-strong)' }} />
              <span style={{ width: `${(s.worse / n) * 100}%`, background: 'var(--janus-color-danger-solid)' }} />
            </div>
            <div className="routing-eval-legend small">
              <span>
                <i style={{ background: 'var(--janus-color-success-solid)' }} /> {routed} better: {s.better}
              </span>
              <span>
                <i style={{ background: 'var(--janus-color-border-strong)' }} /> Tie: {s.ties}
              </span>
              <span>
                <i style={{ background: 'var(--janus-color-danger-solid)' }} /> {def} better: {s.worse}
              </span>
            </div>
          </div>
          <dl className="routing-eval-facts">
            <div>
              <dt>{routed} at least as good</dt>
              <dd>
                {pct((s.better + s.ties / 2) / n)}
                <span className="small muted">
                  {' '}
                  of comparisons (likely {pct(s.at_least_as_good_low)}–{pct(s.at_least_as_good_high)}; keep needs over 45%)
                </span>
              </dd>
            </div>
            {!localOnly ? (
              <div>
                <dt>Cost per request</dt>
                <dd>
                  {perReq(s.routed_cost_nanousd)} vs {perReq(s.default_cost_nanousd)}
                  {saving !== null ? (
                    <span className="small muted"> ({saving >= 0 ? `${pct(saving)} cheaper` : `${pct(-saving)} dearer`})</span>
                  ) : null}
                </dd>
              </div>
            ) : null}
            <div>
              <dt>Judge consistent</dt>
              <dd>
                {pct(s.consistency)}
                <span className="small muted"> of verdicts agreed with the order swapped</span>
              </dd>
            </div>
            {s.failed > 0 ? (
              <div>
                <dt>Failed</dt>
                <dd>{formatNumber(s.failed)} (not counted)</dd>
              </div>
            ) : null}
          </dl>
        </>
      ) : s && s.failed > 0 ? (
        <p className="small" style={{ margin: 0, color: 'var(--janus-color-warning-fg)' }}>
          {formatNumber(s.failed)} comparison{s.failed === 1 ? '' : 's'} failed. Open the card to see why.
        </p>
      ) : null}
      {s && (s.skipped ?? 0) > 0 ? (
        <p className="small" style={{ margin: 0 }}>
          {formatNumber(s.skipped ?? 0)} sampled request{s.skipped === 1 ? ' was' : 's were'} not evaluated: a Security Gateway
          policy on the default or the judge would have blocked or redacted the copy, so it was never sent.
        </p>
      ) : null}
      {older.length > 0 ? (
        <details className="small">
          <summary>Earlier comparisons with other models ({older.length}): not counted above</summary>
          <ul style={{ margin: 'var(--janus-space-2) 0 0', paddingLeft: '1.2em' }}>
            {older.map((o) => (
              <li key={`${o.routed_model_id}|${o.default_model_id}|${o.judge_model_id}`}>
                {nameOf(o.routed_model_id)} vs {nameOf(o.default_model_id)}, judged by {nameOf(o.judge_model_id)}:{' '}
                {VERDICT_LABEL[o.verdict] ?? o.verdict} after {formatNumber(o.evaluated)} comparison{o.evaluated === 1 ? '' : 's'}
                {o.last_at ? ` (last ${formatRelative(o.last_at)})` : ''}{' '}
                <button type="button" className="btn btn-ghost btn-sm" onClick={() => onOpen(o)}>
                  Show
                </button>
              </li>
            ))}
          </ul>
        </details>
      ) : null}
      <footer className="routing-eval-card-foot small muted">
        <span>
          {status}
          {judgeName ? ` · judged by ${judgeName}` : ' · no judge chosen'}
        </span>
        <button type="button" className="btn btn-sm" onClick={() => onOpen(s)}>
          Verdicts and reasons
        </button>
      </footer>
    </article>
  );
}

function EvalDetail({
  detail,
  onClose,
  localOnly,
}: {
  detail: EvalDetailTarget | null;
  onClose: () => void;
  localOnly: boolean;
}): ReactNode {
  const pairing = detail?.pairing;
  const pairQuery = pairing
    ? `&routed_model_id=${encodeURIComponent(pairing.routed_model_id ?? '')}&default_model_id=${encodeURIComponent(
        pairing.default_model_id ?? '',
      )}&judge_model_id=${encodeURIComponent(pairing.judge_model_id ?? '')}`
    : '';
  const results = useQuery({
    queryKey: [...KEY, 'eval-results', detail?.router.id, detail?.routeId, pairQuery],
    queryFn: () =>
      api.get<{ results: RoutingEvalRow[] }>(
        `${BASE}/evaluations/${detail?.router.id}?route_id=${detail?.routeId}&limit=50${pairQuery}`,
      ),
    enabled: !!detail,
  });
  const route = detail?.router.config.routes.find((r) => r.id === detail.routeId);
  const routed = detail?.routedName || 'Routed model';
  const def = detail?.defaultName || 'Default';
  return (
    <Drawer
      open={!!detail}
      onClose={onClose}
      title={`${detail?.router.name ?? ''} · ${route?.name || routed}`}
      description={`Each row is one request answered by both ${routed} and ${def}, compared blind by the judge. Prompts and answers are never stored.`}
      wide
    >
      <AsyncSection query={results}>
        {(d) =>
          d.results.length === 0 ? (
            <p className="muted small">No comparisons yet. They appear a minute or so after a sampled request finishes.</p>
          ) : (
            <div className="table-wrap">
              <table className="data data-compact routing-table">
                <thead>
                  <tr>
                    <th>When</th>
                    <th>Result</th>
                    <th className="num">{routed}</th>
                    <th className="num">{def}</th>
                    <th>Judge’s reason</th>
                    {!localOnly ? <th className="num">Cost to judge</th> : null}
                  </tr>
                </thead>
                <tbody>
                  {d.results.map((r) => (
                    <tr key={r.id}>
                      <td className="small">{formatRelative(r.created_at)}</td>
                      <td>
                        {r.status === 'skipped' ? (
                          <Badge tone="neutral">Not sent</Badge>
                        ) : r.status !== 'ok' ? (
                          <Badge tone="warning">Failed</Badge>
                        ) : (
                          <Badge tone={r.verdict === 'better' ? 'success' : r.verdict === 'worse' ? 'danger' : 'neutral'}>
                            {r.verdict === 'better' ? `${routed} better` : r.verdict === 'worse' ? `${def} better` : 'Tie'}
                          </Badge>
                        )}
                        {r.status === 'ok' && !r.consistent ? (
                          <div className="small muted">judge disagreed with itself</div>
                        ) : null}
                      </td>
                      <td className="num small">{r.status === 'ok' ? `${r.routed_score.toFixed(1)} / 5` : '—'}</td>
                      <td className="num small">{r.status === 'ok' ? `${r.default_score.toFixed(1)} / 5` : '—'}</td>
                      <td className="small routing-eval-reason">{r.status === 'ok' ? r.rationale : r.error}</td>
                      {!localOnly ? <td className="num small">{formatUSD(r.eval_cost_nanousd)}</td> : null}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )
        }
      </AsyncSection>
    </Drawer>
  );
}

/** Recent decisions for a request (request log drawer). */
export function RouteDecisionLine({ requestId }: { requestId: string }): ReactNode {
  const q = useQuery({
    queryKey: [...KEY, 'decision', requestId],
    queryFn: () =>
      api.get<{ decisions: RoutingDecisionRow[] }>(`${BASE}/decisions?request_id=${encodeURIComponent(requestId)}&days=366`),
    enabled: !!requestId,
  });
  const d = q.data?.decisions[0];
  if (!d) return null;
  return (
    <span className="small">
      {d.mode === 'shadow' ? 'Shadow: would have used ' : ''}
      {d.route_name} → {d.model_name} · {reasonLabel(d.reason)}
      {d.category ? ` · ${d.category.replace(/_/g, ' ')} ${pct(d.confidence)}` : ''}
      {d.difficulty ? ` · difficulty ${d.difficulty.toFixed(1)}` : ''}
      {d.decision_ms ? ` · ${formatDuration(d.decision_ms)}` : ''}
      {FAILURE_REASONS.has(d.reason) && d.error ? ` · ${d.error}` : ''}
    </span>
  );
}
