import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import { formatDateTime, formatDuration, formatNumber, formatRelative } from '../../lib/format';
import { AsyncSection, Badge, Field, useToast } from '../../components/ui';
import type {
  BenchmarkConfig,
  BenchmarkLevel,
  ClassifierBenchmark,
  ClassifierProfileView,
  DecisionModelView,
  Model,
} from '../../lib/types';

/*
 * Administration → Usage insights → Performance.
 *
 * Two things decide what classifiers cost every request: how each classifier
 * is asked several questions (its call profile), and which classifiers run
 * at all. The profiles card sets the first; the benchmark measures the
 * result against the real classifiers.
 */

export function PerformancePanel(): ReactNode {
  return (
    <div className="stack" style={{ gap: 'var(--janus-space-5)' }}>
      <BenchmarkCard />
      <ProfilesCard />
    </div>
  );
}

/* --- Call profiles --------------------------------------------------------- */

const STYLE_LABEL: Record<string, string> = {
  combined: 'All questions in one request',
  separate: 'One request per question, in parallel',
};

function ProfilesCard(): ReactNode {
  const profiles = useQuery({
    queryKey: ['classification', 'profiles'],
    queryFn: () => api.get<{ profiles: ClassifierProfileView[] }>('/api/v1/admin/classification/profiles'),
  });
  const [showAll, setShowAll] = useState(false);
  const decisionModels = useQuery({
    queryKey: ['classification', 'decision-models'],
    queryFn: () => api.get<{ models: DecisionModelView[] }>('/api/v1/admin/decision-models'),
  });
  return (
    <section className="card stack" style={{ gap: 'var(--janus-space-4)' }}>
      <div>
        <h2 style={{ margin: 0 }}>Decision models</h2>
        <p className="small muted" style={{ margin: '4px 0 0' }}>
          How Janus talks to each decision model, what its calls cost, and whether people can call it themselves. Janus speaks two
          formats: OpenAI Decisions (OpenAI) and System One (TypeSafe, OpenRouter, llama.cpp). Callers can use either format with
          any model; Janus translates.
        </p>
        <p className="small muted" style={{ margin: '4px 0 0' }}>
          A request can need several answers from the same classifier. Hosted decision APIs answer each question independently, so
          Janus sends them together. Self-hosted models such as Nimble let questions in the same request influence each other, so
          Janus sends each one on its own, at the same time.
        </p>
      </div>
      <AsyncSection
        query={profiles}
        empty={{
          when: (d) => d.profiles.length === 0,
          title: 'No decision classifiers yet',
          body: 'Mark a model as a decision model under Security → Classifiers, or choose one for Usage insights.',
        }}
      >
        {(data) => {
          const byId = new Map((decisionModels.data?.models ?? []).map((d) => [d.id, d]));
          // Discovery adds every decision model a provider offers; show the
          // ones in use or enabled, and fold the rest away.
          const active = (p: ClassifierProfileView): boolean =>
            p.used_by.length > 0 || !p.model_id || byId.get(p.model_id)?.status === 'enabled';
          const shown = data.profiles.filter((p) => showAll || active(p));
          const hidden = data.profiles.length - data.profiles.filter(active).length;
          return (
            <div className="stack" style={{ gap: 'var(--janus-space-3)' }}>
              {shown.map((p) => (
                <ProfileRow
                  key={`${p.upstream_id}/${p.model}`}
                  profile={p}
                  decision={p.model_id ? byId.get(p.model_id) : undefined}
                />
              ))}
              {hidden > 0 ? (
                <button
                  type="button"
                  className="btn btn-ghost btn-sm"
                  style={{ alignSelf: 'flex-start' }}
                  onClick={() => setShowAll(!showAll)}
                >
                  {showAll ? 'Show only models in use' : `Show ${hidden} more not enabled (enable them under Models)`}
                </button>
              ) : null}
            </div>
          );
        }}
      </AsyncSection>
    </section>
  );
}

const DIALECT_LABEL: Record<string, string> = {
  systemone: 'System One',
  openai: 'OpenAI Decisions',
};

const COST_LABEL: Record<string, string> = {
  '': 'Automatic',
  provider: 'What the provider reports',
  card: 'The rate card',
  none: 'Free',
};

function usd(n: number): string {
  if (n === 0) return '$0';
  // Decision prices are small ($0.042 per million): keep the digits that matter.
  return n >= 1 ? `$${n.toFixed(2)}` : `$${Number(n.toPrecision(3))}`;
}

/** Says, in words, how a decision model's calls will be priced. */
function costHint(d: DecisionModelView, source: string, fee: number): string {
  const card =
    d.usd_per_mtok_in || d.usd_per_mtok_out
      ? `${usd(d.usd_per_mtok_in)} per million tokens read${d.usd_per_mtok_out ? `, ${usd(d.usd_per_mtok_out)} written` : ''}`
      : 'no rate card set';
  const feeText = fee > 0 && source !== 'none' ? ` plus ${usd(fee)} per call` : '';
  switch (source) {
    case 'provider':
      return d.provider_reports_cost
        ? `The charge ${d.upstream_name} reports with each answer${feeText}.`
        : `${d.upstream_name} does not report a charge, so calls will cost $0${feeText}. Use the rate card instead.`;
    case 'card':
      return `Rate card: ${card}${feeText}.`;
    case 'none':
      return 'Calls are free (for example a model you host yourself).';
    default:
      return d.provider_reports_cost
        ? `The charge ${d.upstream_name} reports with each answer, or the rate card (${card}) when it sends none${feeText}.`
        : `Rate card: ${card}${feeText}.`;
  }
}

function ProfileRow({ profile, decision }: { profile: ClassifierProfileView; decision?: DecisionModelView }): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [style, setStyle] = useState(profile.call_style);
  const [maxOptions, setMaxOptions] = useState(profile.max_options ? String(profile.max_options) : '');
  const [wire, setWire] = useState(profile.wire_model);
  const [dialect, setDialect] = useState(profile.dialect ?? '');
  const [costSource, setCostSource] = useState(decision?.cost_source ?? '');
  const [fee, setFee] = useState(decision?.request_fee_usd ? String(decision.request_fee_usd) : '');
  const [grantWhenGuarding, setGrantWhenGuarding] = useState(decision?.grant_when_guarding ?? false);
  const reset = (): void => {
    setStyle(profile.call_style);
    setMaxOptions(profile.max_options ? String(profile.max_options) : '');
    setWire(profile.wire_model);
    setDialect(profile.dialect ?? '');
    setCostSource(decision?.cost_source ?? '');
    setFee(decision?.request_fee_usd ? String(decision.request_fee_usd) : '');
    setGrantWhenGuarding(decision?.grant_when_guarding ?? false);
  };
  useEffect(reset, [profile, decision]);
  const profileDirty =
    style !== profile.call_style ||
    (Number(maxOptions) || 0) !== profile.max_options ||
    wire.trim() !== profile.wire_model ||
    dialect !== (profile.dialect ?? '');
  const decisionDirty =
    !!decision &&
    (costSource !== decision.cost_source ||
      (Number(fee) || 0) !== decision.request_fee_usd ||
      grantWhenGuarding !== decision.grant_when_guarding);
  const dirty = profileDirty || decisionDirty;
  const save = useMutation({
    mutationFn: async () => {
      if (profileDirty) {
        await api.put('/api/v1/admin/classification/profiles', {
          upstream_id: profile.upstream_id,
          model: profile.model,
          call_style: style,
          max_options: Number(maxOptions) || 0,
          wire_model: wire.trim(),
          dialect,
        });
      }
      if (decisionDirty && decision) {
        await api.put(`/api/v1/admin/decision-models/${decision.id}/settings`, {
          cost_source: costSource,
          request_fee_usd: Number(fee) || 0,
          grant_when_guarding: grantWhenGuarding,
        });
      }
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['classification'] });
      toast('Decision model saved.', 'success');
    },
    onError: (err: unknown) => toast(err instanceof ApiError ? err.message : 'Could not save the profile.', 'danger'),
  });
  return (
    <div className="insights-profile">
      <div className="row-between wrap" style={{ gap: 'var(--janus-space-2)' }}>
        <div>
          <strong>{profile.model}</strong>
          <span className="small muted"> · {profile.upstream_name}</span>
        </div>
        <div className="row wrap" style={{ gap: 6 }}>
          {profile.used_by.length ? (
            profile.used_by.map((u) => (
              <Badge key={u} tone="info">
                {u}
              </Badge>
            ))
          ) : (
            <Badge>Not in use</Badge>
          )}
        </div>
      </div>
      <div className="insights-profile-grid">
        <Field
          label="Several questions"
          hint={
            style === ''
              ? `Automatic: ${(STYLE_LABEL[profile.effective_style] ?? profile.effective_style).toLowerCase()}.`
              : undefined
          }
        >
          <select
            className="select"
            aria-label={`How ${profile.model} is asked several questions`}
            value={style}
            onChange={(e) => setStyle(e.target.value as ClassifierProfileView['call_style'])}
          >
            <option value="">Automatic</option>
            <option value="combined">{STYLE_LABEL.combined}</option>
            <option value="separate">{STYLE_LABEL.separate}</option>
          </select>
        </Field>
        <Field
          label="Most choices per question"
          hint={`Detected: ${profile.effective_max_options}. Larger category lists are asked in two steps.`}
        >
          <input
            className="input"
            type="number"
            min={0}
            max={1000}
            aria-label={`Most choices per question for ${profile.model}`}
            placeholder={String(profile.effective_max_options)}
            value={maxOptions}
            onChange={(e) => setMaxOptions(e.target.value)}
          />
        </Field>
        <Field label="Name sent to the provider" hint="Only when the decision API knows the model by another name.">
          <input
            className="input"
            aria-label={`Name sent to the provider for ${profile.model}`}
            placeholder={decision?.wire_model || profile.model}
            value={wire}
            onChange={(e) => setWire(e.target.value)}
          />
        </Field>
        <Field
          label="Format"
          hint={
            dialect === ''
              ? `Automatic: ${DIALECT_LABEL[profile.effective_dialect] ?? profile.effective_dialect}, from the provider's address.`
              : undefined
          }
        >
          <select
            className="select"
            aria-label={`Decision format for ${profile.model}`}
            value={dialect}
            onChange={(e) => setDialect(e.target.value as ClassifierProfileView['dialect'])}
          >
            <option value="">Automatic</option>
            <option value="systemone">{DIALECT_LABEL.systemone}</option>
            <option value="openai">{DIALECT_LABEL.openai}</option>
          </select>
        </Field>
      </div>
      {decision ? (
        <div className="insights-profile-grid">
          <Field label="Cost of each call" hint={costHint(decision, costSource, Number(fee) || 0)}>
            <select
              className="select"
              aria-label={`How calls to ${profile.model} are priced`}
              value={costSource}
              onChange={(e) => setCostSource(e.target.value as DecisionModelView['cost_source'])}
            >
              {Object.entries(COST_LABEL).map(([v, label]) => (
                <option key={v} value={v}>
                  {label}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Fee per call (USD)" hint="Added to every call, for example to recover hosting costs.">
            <input
              className="input"
              type="number"
              min={0}
              step="0.0001"
              aria-label={`Fee per call for ${profile.model}`}
              placeholder="0"
              disabled={costSource === 'none'}
              value={fee}
              onChange={(e) => setFee(e.target.value)}
            />
          </Field>
          <div className="field">
            <span className="field-label">People calling it directly</span>
            <DecisionAccess decision={decision} allow={grantWhenGuarding} setAllow={setGrantWhenGuarding} />
          </div>
        </div>
      ) : null}
      {dirty ? (
        <div className="row" style={{ justifyContent: 'flex-end', gap: 'var(--janus-space-2)' }}>
          <button type="button" className="btn btn-ghost btn-sm" onClick={reset}>
            Discard
          </button>
          <button type="button" className="btn btn-primary btn-sm" disabled={save.isPending} onClick={() => save.mutate()}>
            Save
          </button>
        </div>
      ) : null}
    </div>
  );
}

/** Who can call a decision model, and the guard rule, in words. */
function DecisionAccess({
  decision,
  allow,
  setAllow,
}: {
  decision: DecisionModelView;
  allow: boolean;
  setAllow: (v: boolean) => void;
}): ReactNode {
  const granted =
    decision.grant_count > 0
      ? `Granted ${decision.grant_count} time${decision.grant_count === 1 ? '' : 's'}.`
      : 'Not granted to anyone yet. Grant it under Grants.';
  if (decision.guarding.length === 0) {
    return (
      <p className="small" style={{ margin: 0 }}>
        {granted} People call it at /v1/decisions or /v1/systemone.
      </p>
    );
  }
  return (
    <div className="stack" style={{ gap: 4 }}>
      <p className="small" style={{ margin: 0 }}>
        It blocks requests in {decision.guarding.join(', ')}. People who can call it could test what gets past that check, so it
        cannot be granted{allow ? ' unless you allow it below' : ''}.
      </p>
      <label className="row small" style={{ gap: 6 }}>
        <input type="checkbox" checked={allow} onChange={(e) => setAllow(e.target.checked)} />
        Allow granting it anyway
      </label>
      {decision.grant_count > 0 && !allow ? (
        <p className="small muted" style={{ margin: 0 }}>
          {decision.grant_count} existing grant{decision.grant_count === 1 ? '' : 's'} will be refused while it guards.
        </p>
      ) : null}
    </div>
  );
}

/* --- Benchmark ------------------------------------------------------------- */

const LEVEL_PRESETS: Array<{ id: string; label: string; levels: number[] }> = [
  { id: 'small', label: 'Up to 8 at once', levels: [1, 2, 4, 8] },
  { id: 'medium', label: 'Up to 32 at once', levels: [1, 2, 4, 8, 16, 32] },
  { id: 'large', label: 'Up to 128 at once', levels: [1, 4, 16, 32, 64, 128] },
];

function BenchmarkCard(): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const list = useQuery({
    queryKey: ['classification', 'benchmarks'],
    queryFn: () =>
      api.get<{ benchmarks: ClassifierBenchmark[]; defaults: BenchmarkConfig }>('/api/v1/admin/classification/benchmarks'),
    refetchInterval: (q) => (q.state.data?.benchmarks.some((b) => b.status === 'running') ? 2000 : false),
  });
  const classifiers = useQuery({
    queryKey: ['admin', 'secgw', 'classifiers', 'benchmark'],
    queryFn: () => api.get<{ classifiers: Model[] }>('/api/v1/admin/secgw/classifiers'),
    retry: false,
  });
  const [draft, setDraft] = useState<BenchmarkConfig | null>(null);
  const [preset, setPreset] = useState('medium');
  const [selectedId, setSelectedId] = useState<string | null>(null);
  useEffect(() => {
    if (!draft && list.data) setDraft(list.data.defaults);
  }, [list.data, draft]);

  const start = useMutation({
    mutationFn: (cfg: BenchmarkConfig) => api.post<ClassifierBenchmark>('/api/v1/admin/classification/benchmarks', cfg),
    onSuccess: (b) => {
      setSelectedId(b.id);
      void queryClient.invalidateQueries({ queryKey: ['classification', 'benchmarks'] });
    },
    onError: (err: unknown) => toast(err instanceof ApiError ? err.message : 'Could not start the benchmark.', 'danger'),
  });

  const runs = list.data?.benchmarks ?? [];
  const selected = runs.find((b) => b.id === selectedId) ?? runs[0];
  const running = runs.some((b) => b.status === 'running');
  const injectionModels = (classifiers.data?.classifiers ?? []).filter((m) =>
    ['text_classification', 'decision', ''].includes(m.classifier_role ?? ''),
  );
  const safetyModels = (classifiers.data?.classifiers ?? []).filter((m) =>
    ['generative_guard', 'decision'].includes(m.classifier_role ?? ''),
  );
  const set = <K extends keyof BenchmarkConfig>(k: K, v: BenchmarkConfig[K]) => setDraft((d) => (d ? { ...d, [k]: v } : d));
  const nothingChosen = draft ? !draft.injection_model_id && !draft.safety_model_id && !draft.usage_insights : true;

  return (
    <section className="card stack" style={{ gap: 'var(--janus-space-4)' }}>
      <div>
        <h2 style={{ margin: 0 }}>Benchmark</h2>
        <p className="small muted" style={{ margin: '4px 0 0' }}>
          Measures what your classifier choices cost each request: how long a request waits before it is sent on, and how many
          requests can be classified at once before that wait grows past your budget. Sample prompts go through the real
          classifiers exactly as live traffic does; nothing is sent to a model and nothing appears in the request log. Run it when
          traffic is light: the benchmark shares the classifiers with live requests.
        </p>
      </div>
      {draft ? (
        <div className="insights-bench-form">
          <Field label="Prompt injection">
            <select
              className="select"
              aria-label="Prompt-injection classifier"
              value={draft.injection_model_id}
              onChange={(e) => set('injection_model_id', e.target.value)}
            >
              <option value="">Not included</option>
              {injectionModels.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.display_name || m.name}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Content safety">
            <select
              className="select"
              aria-label="Content-safety classifier"
              value={draft.safety_model_id}
              onChange={(e) => set('safety_model_id', e.target.value)}
            >
              <option value="">Not included</option>
              {safetyModels.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.display_name || m.name}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Usage insights">
            <select
              className="select"
              aria-label="Usage insights in the benchmark"
              value={draft.usage_insights ? draft.insights_mode || 'saved' : 'off'}
              onChange={(e) => {
                const v = e.target.value;
                setDraft((d) =>
                  d
                    ? {
                        ...d,
                        usage_insights: v !== 'off',
                        insights_mode: v === 'inline' || v === 'async' ? v : '',
                      }
                    : d,
                );
              }}
            >
              <option value="off">Not included</option>
              <option value="saved">As configured</option>
              <option value="inline">Inline (request waits for the label)</option>
              <option value="async">Background</option>
            </select>
          </Field>
          <Field label="Several questions">
            <select
              className="select"
              aria-label="Question grouping for the benchmark"
              value={draft.style}
              onChange={(e) => set('style', e.target.value as BenchmarkConfig['style'])}
            >
              <option value="">Each classifier's profile</option>
              <option value="combined">{STYLE_LABEL.combined}</option>
              <option value="separate">{STYLE_LABEL.separate}</option>
            </select>
          </Field>
          <Field label="Simultaneous requests">
            <select
              className="select"
              aria-label="Simultaneous requests"
              value={preset}
              onChange={(e) => {
                setPreset(e.target.value);
                const p = LEVEL_PRESETS.find((x) => x.id === e.target.value);
                if (p) set('levels', p.levels);
              }}
            >
              {LEVEL_PRESETS.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.label}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Acceptable wait (ms)" hint="Per request, 95th percentile.">
            <input
              className="input"
              type="number"
              min={50}
              max={60000}
              aria-label="Acceptable wait (ms)"
              value={draft.budget_ms}
              onChange={(e) => set('budget_ms', Number(e.target.value))}
            />
          </Field>
        </div>
      ) : null}
      <div className="row" style={{ justifyContent: 'flex-end' }}>
        <button
          type="button"
          className="btn btn-primary"
          disabled={!draft || running || start.isPending || nothingChosen}
          onClick={() => draft && start.mutate(draft)}
        >
          {running ? 'Benchmark running…' : 'Run benchmark'}
        </button>
      </div>

      <AsyncSection query={list}>
        {() =>
          selected ? (
            <BenchmarkView run={selected} />
          ) : (
            <p className="small muted" style={{ margin: 0 }}>
              No benchmark yet. Choose the classifiers you use (or are considering) and run one.
            </p>
          )
        }
      </AsyncSection>

      {runs.length > 1 ? (
        <details>
          <summary className="small">Earlier runs</summary>
          <ul className="insights-bench-history">
            {runs.map((b) => (
              <li key={b.id}>
                <button
                  type="button"
                  className="btn btn-ghost btn-sm"
                  aria-pressed={b.id === selected?.id}
                  onClick={() => setSelectedId(b.id)}
                >
                  {formatDateTime(b.created_at)}
                </button>
                <span className="small muted">
                  {b.status === 'running'
                    ? 'running'
                    : b.status === 'failed'
                      ? 'failed'
                      : `${formatNumber(b.result.max_concurrent ?? 0)} at once within ${formatDuration(b.config.budget_ms ?? 0)}`}
                </span>
              </li>
            ))}
          </ul>
        </details>
      ) : null}
    </section>
  );
}

function BenchmarkView({ run }: { run: ClassifierBenchmark }): ReactNode {
  const levels = run.result.levels ?? [];
  const budget = run.config.budget_ms ?? 0;
  const background = levels.some((l) => (l.background_p50_ms ?? 0) > 0);
  const best = useMemo(() => levels.filter((l) => l.within_budget).at(-1), [levels]);
  return (
    <div className="stack" style={{ gap: 'var(--janus-space-4)' }}>
      <div className="row-between wrap" style={{ gap: 'var(--janus-space-2)' }}>
        <span className="small muted">
          Started {formatRelative(run.created_at)}
          {run.status === 'running' ? ' · measuring…' : run.finished_at ? ` · finished ${formatRelative(run.finished_at)}` : ''}
        </span>
        {run.status === 'running' ? (
          <Badge tone="info" dot>
            Running
          </Badge>
        ) : run.status === 'failed' ? (
          <Badge tone="danger">Failed</Badge>
        ) : (
          <Badge tone="success">Done</Badge>
        )}
      </div>
      {run.error ? <div className="banner banner-danger">{run.error}</div> : null}

      {levels.length ? (
        <div className="grid insights-bench-tiles">
          <BenchTile
            label="Added wait, one at a time"
            value={run.result.single_ms ? formatDuration(run.result.single_ms) : '—'}
            meta="Typical wait for one request with nothing else running"
          />
          <BenchTile
            label="Simultaneous requests within budget"
            value={best ? formatNumber(best.concurrency) : 'None'}
            meta={
              best
                ? `95% of requests waited ${formatDuration(best.p95_ms)} or less (budget ${formatDuration(budget)})`
                : `Even the lowest level waited longer than ${formatDuration(budget)}`
            }
          />
          <BenchTile
            label="Classifier calls per request"
            value={String(levels[0]?.calls_per_request ?? 0)}
            meta={
              best
                ? `${formatNumber(best.per_second)} requests a second at ${formatNumber(best.concurrency)} at once`
                : 'Requests the classifiers received for each user request'
            }
          />
        </div>
      ) : null}

      {run.result.plan?.length ? (
        <div className="small">
          <div className="muted" style={{ marginBottom: 4 }}>
            What was measured
          </div>
          <ul style={{ margin: 0, paddingLeft: 18 }}>
            {run.result.plan.map((line) => (
              <li key={line}>{line}</li>
            ))}
          </ul>
        </div>
      ) : null}

      {levels.length ? <LatencyChart levels={levels} budget={budget} /> : null}

      {levels.length ? (
        <div className="table-wrap">
          <table className="data">
            <thead>
              <tr>
                <th scope="col">At once</th>
                <th scope="col" className="num">
                  Typical wait
                </th>
                <th scope="col" className="num">
                  95% within
                </th>
                <th scope="col" className="num">
                  Slowest
                </th>
                {background ? (
                  <th scope="col" className="num">
                    Background label
                  </th>
                ) : null}
                <th scope="col" className="num">
                  Per second
                </th>
                <th scope="col" className="num">
                  Errors
                </th>
                <th scope="col">Within budget</th>
              </tr>
            </thead>
            <tbody>
              {levels.map((l) => (
                <tr key={l.concurrency}>
                  <td className="num">{l.concurrency}</td>
                  <td className="num">{formatDuration(l.p50_ms)}</td>
                  <td className="num">{formatDuration(l.p95_ms)}</td>
                  <td className="num">{formatDuration(l.max_ms)}</td>
                  {background ? <td className="num">{l.background_p50_ms ? formatDuration(l.background_p50_ms) : '—'}</td> : null}
                  <td className="num">{formatNumber(l.per_second)}</td>
                  <td className="num">{l.errors ? <Badge tone="danger">{l.errors}</Badge> : '0'}</td>
                  <td>{l.within_budget ? <Badge tone="success">Yes</Badge> : <Badge tone="warning">No</Badge>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : run.status === 'running' ? (
        <p className="small muted" style={{ margin: 0 }}>
          Warming up the classifiers…
        </p>
      ) : null}
      {background ? (
        <p className="small muted" style={{ margin: 0 }}>
          Background labelling does not delay requests, but each label still takes classifier time; if it is slower than requests
          arrive, the queue grows and samples are skipped.
        </p>
      ) : null}
      {run.result.stopped ? (
        <p className="small muted" style={{ margin: 0 }}>
          {run.result.stopped}
        </p>
      ) : null}
    </div>
  );
}

function BenchTile({ label, value, meta }: { label: string; value: string; meta?: string }): ReactNode {
  return (
    <article className="tile">
      <div className="overline">{label}</div>
      <div className="tile-value">{value}</div>
      {meta ? <div className="tile-meta">{meta}</div> : null}
    </article>
  );
}

/**
 * Wait against simultaneous requests: one bar pair per level (typical and
 * 95th percentile) with the budget as a line, so the point where the
 * classifiers saturate is visible at a glance.
 */
function LatencyChart({ levels, budget }: { levels: BenchmarkLevel[]; budget: number }): ReactNode {
  const top = Math.max(budget, ...levels.map((l) => l.p95_ms)) * 1.1 || 1;
  const h = 160;
  // Slots never get wider than a quarter of the chart, so one or two
  // levels read as bars rather than a filled block.
  const slot = Math.min(100 / levels.length, 25);
  const offset = (100 - slot * levels.length) / 2;
  const y = (ms: number) => h - (ms / top) * h;
  return (
    <figure className="insights-bench-chart" aria-label="Wait by number of simultaneous requests">
      <div className="insights-bench-plot">
        <div className="insights-bench-scale" aria-hidden="true">
          <span>{formatDuration(top)}</span>
          <span>{formatDuration(top / 2)}</span>
          <span>0</span>
        </div>
        <svg viewBox={`0 0 100 ${h}`} preserveAspectRatio="none" role="img">
          <line x1={0} x2={100} y1={h / 2} y2={h / 2} className="insights-bench-grid" />
          {levels.map((l, i) => {
            const x = offset + i * slot;
            return (
              <g key={l.concurrency}>
                <rect
                  x={x + slot * 0.2}
                  width={slot * 0.28}
                  y={y(l.p50_ms)}
                  height={h - y(l.p50_ms)}
                  className="insights-bench-bar-p50"
                >
                  <title>{`${l.concurrency} at once: typical ${formatDuration(l.p50_ms)}`}</title>
                </rect>
                <rect
                  x={x + slot * 0.52}
                  width={slot * 0.28}
                  y={y(l.p95_ms)}
                  height={h - y(l.p95_ms)}
                  className={l.within_budget ? 'insights-bench-bar-p95' : 'insights-bench-bar-over'}
                >
                  <title>{`${l.concurrency} at once: 95% within ${formatDuration(l.p95_ms)}`}</title>
                </rect>
              </g>
            );
          })}
          {budget > 0 ? <line x1={0} x2={100} y1={y(budget)} y2={y(budget)} className="insights-bench-budget" /> : null}
        </svg>
      </div>
      <div className="insights-bench-axis" style={{ paddingLeft: `calc(var(--bench-scale-w) + ${offset}%)` }}>
        {levels.map((l) => (
          <span key={l.concurrency} style={{ width: `${slot}%` }}>
            {l.concurrency}
          </span>
        ))}
      </div>
      <figcaption className="small muted row wrap" style={{ gap: 'var(--janus-space-3)' }}>
        <span>
          <i className="insights-bench-key insights-bench-bar-p50" /> Typical wait
        </span>
        <span>
          <i className="insights-bench-key insights-bench-bar-p95" /> 95% within budget
        </span>
        <span>
          <i className="insights-bench-key insights-bench-bar-over" /> 95% over budget
        </span>
        <span>
          <i className="insights-bench-key insights-bench-budget-key" /> Budget {formatDuration(budget)}
        </span>
        <span>Simultaneous requests along the bottom</span>
      </figcaption>
    </figure>
  );
}
