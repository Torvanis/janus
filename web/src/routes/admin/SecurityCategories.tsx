/**
 * Security → Categories: the categories a content-safety check with the
 * Categories method asks a decision model about. Three groups (Safety and
 * Family built in, the admin's own), an editor drawer, a try box and a
 * batch test of every category's examples. Modeled on Auto-routing's
 * Categories tab and Batch test.
 *
 * Hazard codes are never shown: categories are named.
 */
import { Fragment, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { formatDuration, formatNumber } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, Drawer, Field, useToast } from '../../components/ui';
import { IconButton, RowActions } from '../../components/IconButton';
import { t, type MessageKey } from '../../lib/i18n';
import type {
  SecgwCategoryGroup,
  SecurityCategoriesDoc,
  SecurityCategory,
  SecurityCategoryBatch,
  SecurityCategoryTest,
} from '../../lib/types';

const BASE = '/api/v1/admin/secgw/categories';
export const CATEGORIES_KEY = ['admin', 'secgw', 'categories'] as const;

export function useSecurityCategories() {
  return useQuery({ queryKey: CATEGORIES_KEY, queryFn: () => api.get<SecurityCategoriesDoc>(BASE) });
}

function pct(v: number): string {
  return `${Math.round(v * 100)}%`;
}

function firstSentence(text: string): string {
  const trimmed = text.trim();
  const m = /^(.+?[.!?])(\s|$)/.exec(trimmed);
  return m?.[1] ?? trimmed;
}

const GROUPS: Array<[SecgwCategoryGroup, MessageKey, MessageKey]> = [
  ['safety', 'adminSecurity.categories.groupSafety', 'adminSecurity.categories.groupSafetyHint'],
  ['family', 'adminSecurity.categories.groupFamily', 'adminSecurity.categories.groupFamilyHint'],
  ['custom', 'adminSecurity.categories.groupCustom', 'adminSecurity.categories.groupCustomHint'],
];

interface Draft {
  id?: string;
  name: string;
  includes: string;
  excludes: string;
  linked_slug: string;
  min_confidence: number;
  examples: string;
  enabled: boolean;
  builtin: boolean;
}

function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

export function CategoriesTab(): ReactNode {
  const cats = useSecurityCategories();
  const qc = useQueryClient();
  const toast = useToast();
  const [editing, setEditing] = useState<Draft | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<SecurityCategory | null>(null);
  const [classifier, setClassifier] = useState('');
  const invalidate = () => void qc.invalidateQueries({ queryKey: ['admin', 'secgw'] });

  const save = useMutation({
    mutationFn: (d: Draft) => {
      const body = {
        name: d.name,
        includes: d.includes,
        excludes: d.excludes,
        linked_slug: d.linked_slug,
        min_confidence: d.min_confidence,
        enabled: d.enabled,
        examples: d.examples
          .split('\n')
          .map((x) => x.trim())
          .filter(Boolean),
      };
      return d.id ? api.patch(`${BASE}/${d.id}`, body) : api.post(BASE, body);
    },
    onSuccess: () => {
      invalidate();
      setEditing(null);
      setError(null);
      toast(t('adminSecurity.categories.saved'), 'success');
    },
    onError: (e) => setError(errorMessage(e)),
  });
  const toggle = useMutation({
    mutationFn: (c: SecurityCategory) => api.patch(`${BASE}/${c.id}`, { enabled: !c.enabled }),
    onSuccess: invalidate,
    onError: (e) => toast(errorMessage(e), 'danger'),
  });
  const reset = useMutation({
    mutationFn: (id: string) => api.post(`${BASE}/${id}/reset`),
    onSuccess: () => {
      invalidate();
      setEditing(null);
      toast(t('adminSecurity.categories.resetDone'), 'success');
    },
    onError: (e) => toast(errorMessage(e), 'danger'),
  });
  const del = useMutation({
    mutationFn: (id: string) => api.del(`${BASE}/${id}`),
    onSuccess: () => {
      invalidate();
      setDeleting(null);
      toast(t('adminSecurity.categories.deleted'), 'success');
    },
    onError: (e) => {
      setDeleting(null);
      toast(errorMessage(e), 'danger');
    },
  });

  const open = (c?: SecurityCategory) => {
    setError(null);
    setEditing(
      c
        ? {
            id: c.id,
            name: c.name,
            includes: c.linked_slug ? '' : c.includes,
            excludes: c.linked_slug ? '' : c.excludes,
            linked_slug: c.linked_slug,
            min_confidence: c.min_confidence,
            examples: (c.examples ?? []).join('\n'),
            enabled: c.enabled,
            builtin: c.builtin,
          }
        : {
            name: '',
            includes: '',
            excludes: '',
            linked_slug: '',
            min_confidence: 0.5,
            examples: '',
            enabled: true,
            builtin: false,
          },
    );
  };

  const classifiers = cats.data?.classifiers ?? [];
  const chosen = classifier || classifiers[0]?.id || '';

  return (
    <div className="stack" style={{ gap: 'var(--janus-space-4)' }}>
      <section className="card stack" style={{ gap: 'var(--janus-space-3)' }}>
        <div className="row-between" style={{ flexWrap: 'wrap', gap: 'var(--janus-space-3)' }}>
          <div>
            <h2>{t('adminSecurity.categories.testTitle')}</h2>
            <p className="small secondary" style={{ margin: 0 }}>
              {t('adminSecurity.categories.tryIntro')}
            </p>
          </div>
          {classifiers.length > 0 ? (
            <Field label={t('adminSecurity.categories.classifierLabel')}>
              <select
                className="select"
                aria-label={t('adminSecurity.categories.classifierLabel')}
                value={chosen}
                onChange={(e) => setClassifier(e.target.value)}
              >
                {classifiers.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.name} · {c.upstream_name}
                  </option>
                ))}
              </select>
            </Field>
          ) : null}
        </div>
        {cats.data && classifiers.length === 0 ? (
          <div className="banner banner-info">{t('adminSecurity.categories.noClassifiers')}</div>
        ) : null}
        <div className="grid grid-halves" style={{ alignItems: 'start' }}>
          <TryCategories classifierId={chosen} />
          <BatchCategories classifierId={chosen} />
        </div>
      </section>

      <section className="card card-flush">
        <div className="card-header" style={{ padding: 'var(--janus-card-padding)' }}>
          <div>
            <h2>{t('adminSecurity.tabs.categories')}</h2>
            <p className="small secondary" style={{ margin: 0 }}>
              {t('adminSecurity.categories.intro')}
            </p>
          </div>
          <button type="button" className="btn btn-primary" onClick={() => open()}>
            {t('adminSecurity.categories.newCategory')}
          </button>
        </div>
        <AsyncSection query={cats}>
          {(data) => (
            <div className="table-wrap">
              <table className="data routing-table">
                <thead>
                  <tr>
                    <th scope="col">{t('adminSecurity.categories.colName')}</th>
                    <th scope="col" className="num">
                      {t('adminSecurity.categories.colMinConfidence')}
                    </th>
                    <th scope="col">{t('adminSecurity.categories.colUsedBy')}</th>
                    <th scope="col">{t('adminSecurity.categories.colEnabled')}</th>
                    <th scope="col" aria-label={t('tables.actions')} />
                  </tr>
                </thead>
                {GROUPS.map(([group, label, hint]) => {
                  const rows = (data.categories ?? []).filter((c) => c.group === group);
                  return (
                    <tbody key={group}>
                      <tr className="routing-group-row">
                        <th scope="rowgroup" colSpan={5}>
                          <span>{t(label)}</span> <span className="small muted">· {t(hint)}</span>
                        </th>
                      </tr>
                      {rows.length === 0 ? (
                        <tr>
                          <td colSpan={5} className="small muted">
                            {t('adminSecurity.categories.groupCustomEmpty')}
                          </td>
                        </tr>
                      ) : null}
                      {rows.map((c) => (
                        <tr key={c.id} data-clickable="true" onClick={() => open(c)}>
                          <th scope="row">
                            <div>{c.name}</div>
                            <div className="small muted routing-cell-note">
                              {c.linked_slug
                                ? t('adminSecurity.categories.follows', { name: c.linked_name ?? c.linked_slug })
                                : firstSentence(c.includes)}
                            </div>
                          </th>
                          <td className="num">{pct(c.min_confidence)}</td>
                          <td className="small">
                            {(c.used_by ?? []).length === 0 ? <span className="muted">—</span> : (c.used_by ?? []).join(', ')}
                          </td>
                          <td onClick={(e) => e.stopPropagation()}>
                            <label className="switch">
                              <input
                                type="checkbox"
                                aria-label={t('adminSecurity.categories.enable', { name: c.name })}
                                checked={c.enabled}
                                onChange={() => toggle.mutate(c)}
                              />
                            </label>
                          </td>
                          <td onClick={(e) => e.stopPropagation()}>
                            <RowActions>
                              <IconButton
                                icon="edit"
                                label={t('adminSecurity.categories.edit', { name: c.name })}
                                onClick={() => open(c)}
                              />
                              {c.builtin ? (
                                <IconButton
                                  icon="rotate"
                                  label={t('adminSecurity.categories.reset', { name: c.name })}
                                  onClick={() => reset.mutate(c.id)}
                                />
                              ) : (
                                <IconButton
                                  icon="delete"
                                  danger
                                  label={t('adminSecurity.categories.remove', { name: c.name })}
                                  onClick={() => setDeleting(c)}
                                />
                              )}
                            </RowActions>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  );
                })}
              </table>
            </div>
          )}
        </AsyncSection>
      </section>

      <Drawer
        open={!!editing}
        onClose={() => setEditing(null)}
        title={editing?.id ? editing.name || t('adminSecurity.categories.editorNew') : t('adminSecurity.categories.editorNew')}
        description={t('adminSecurity.categories.editorDescription')}
        wide
        footer={
          <div className="row-between">
            <div>
              {editing?.builtin && editing.id ? (
                <button type="button" className="btn btn-ghost" onClick={() => editing.id && reset.mutate(editing.id)}>
                  {t('adminSecurity.categories.resetBuiltin')}
                </button>
              ) : null}
            </div>
            <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
              <button type="button" className="btn btn-ghost" onClick={() => setEditing(null)}>
                {t('common.cancel')}
              </button>
              <button
                type="button"
                className="btn btn-primary"
                disabled={save.isPending || !editing?.name.trim()}
                onClick={() => editing && save.mutate(editing)}
              >
                {t('adminSecurity.categories.save')}
              </button>
            </div>
          </div>
        }
      >
        {editing ? (
          <div className="stack" style={{ gap: 'var(--janus-space-4)' }}>
            {error ? (
              <div className="banner banner-danger" role="alert">
                {error}
              </div>
            ) : null}
            <Field label={t('adminSecurity.categories.nameLabel')}>
              <input
                className="input"
                aria-label={t('adminSecurity.categories.nameLabel')}
                value={editing.name}
                onChange={(e) => setEditing({ ...editing, name: e.target.value })}
              />
            </Field>
            <div className="field">
              <span className="field-label">{t('adminSecurity.categories.definitionLabel')}</span>
              <div className="segmented" role="radiogroup" aria-label={t('adminSecurity.categories.definitionLabel')}>
                <button
                  type="button"
                  role="radio"
                  aria-checked={!editing.linked_slug}
                  aria-pressed={!editing.linked_slug}
                  onClick={() => setEditing({ ...editing, linked_slug: '' })}
                >
                  {t('adminSecurity.categories.definitionWrite')}
                </button>
                <button
                  type="button"
                  role="radio"
                  aria-checked={!!editing.linked_slug}
                  aria-pressed={!!editing.linked_slug}
                  disabled={(cats.data?.insights_categories ?? []).length === 0}
                  onClick={() =>
                    setEditing({
                      ...editing,
                      linked_slug: editing.linked_slug || (cats.data?.insights_categories?.[0]?.slug ?? ''),
                    })
                  }
                >
                  {t('adminSecurity.categories.definitionFollow')}
                </button>
              </div>
            </div>
            {editing.linked_slug ? (
              <Field label={t('adminSecurity.categories.insightsLabel')} hint={t('adminSecurity.categories.insightsHint')}>
                <select
                  className="select"
                  aria-label={t('adminSecurity.categories.insightsLabel')}
                  value={editing.linked_slug}
                  onChange={(e) => setEditing({ ...editing, linked_slug: e.target.value })}
                >
                  {(cats.data?.insights_categories ?? []).map((c) => (
                    <option key={c.slug} value={c.slug}>
                      {c.parent_slug ? '  › ' : ''}
                      {c.name}
                    </option>
                  ))}
                </select>
              </Field>
            ) : (
              <>
                <Field label={t('adminSecurity.categories.includesLabel')} hint={t('adminSecurity.categories.includesHint')}>
                  <textarea
                    className="textarea"
                    rows={4}
                    aria-label={t('adminSecurity.categories.includesLabel')}
                    value={editing.includes}
                    onChange={(e) => setEditing({ ...editing, includes: e.target.value })}
                  />
                </Field>
                <Field label={t('adminSecurity.categories.excludesLabel')} hint={t('adminSecurity.categories.excludesHint')}>
                  <textarea
                    className="textarea"
                    rows={3}
                    aria-label={t('adminSecurity.categories.excludesLabel')}
                    value={editing.excludes}
                    onChange={(e) => setEditing({ ...editing, excludes: e.target.value })}
                  />
                </Field>
              </>
            )}
            <Field
              label={t('adminSecurity.categories.minConfidenceLabel', { value: pct(editing.min_confidence) })}
              hint={t('adminSecurity.categories.minConfidenceHint')}
            >
              <input
                type="range"
                min={0.05}
                max={0.95}
                step={0.05}
                aria-label={t('adminSecurity.categories.colMinConfidence')}
                value={editing.min_confidence}
                onChange={(e) => setEditing({ ...editing, min_confidence: Number(e.target.value) })}
              />
            </Field>
            <Field label={t('adminSecurity.categories.examplesLabel')} hint={t('adminSecurity.categories.examplesHint')}>
              <textarea
                className="textarea"
                rows={6}
                aria-label={t('adminSecurity.categories.examplesLabel')}
                value={editing.examples}
                onChange={(e) => setEditing({ ...editing, examples: e.target.value })}
              />
            </Field>
            <label className="switch">
              <input
                type="checkbox"
                checked={editing.enabled}
                onChange={(e) => setEditing({ ...editing, enabled: e.target.checked })}
              />
              <span className="small">{t('adminSecurity.categories.enabledLabel')}</span>
            </label>
          </div>
        ) : null}
      </Drawer>

      <ConfirmDialog
        open={!!deleting}
        title={t('adminSecurity.categories.deleteTitle', { name: deleting?.name ?? '' })}
        consequence={t('adminSecurity.categories.deleteConsequence')}
        confirmLabel={t('tables.delete')}
        danger
        busy={del.isPending}
        onClose={() => setDeleting(null)}
        onConfirm={() => {
          if (deleting) del.mutate(deleting.id);
        }}
      />
    </div>
  );
}

function TryCategories({ classifierId }: { classifierId: string }): ReactNode {
  const [text, setText] = useState('');
  const test = useMutation({
    mutationFn: () => api.post<SecurityCategoryTest>(`${BASE}/test`, { text, classifier_model_id: classifierId }),
  });
  const res = test.data;
  const scores = (res?.scores ?? []).slice(0, 8);
  const counted = (res?.scores ?? []).filter((s) => s.counts).map((s) => s.name);
  return (
    <div className="stack" style={{ gap: 'var(--janus-space-3)' }}>
      <h3 style={{ margin: 0 }}>{t('adminSecurity.categories.tryTitle')}</h3>
      <textarea
        className="textarea"
        rows={4}
        aria-label={t('adminSecurity.categories.tryLabel')}
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
          {test.isPending ? t('adminSecurity.categories.tryRunning') : t('adminSecurity.categories.tryRun')}
        </button>
      </div>
      {test.isError ? (
        <div className="banner banner-danger" role="alert">
          {errorMessage(test.error)}
        </div>
      ) : null}
      {res && res.status !== 'ok' ? (
        <div className="banner banner-danger" role="alert">
          {t('adminSecurity.categories.tryFailed', { time: formatDuration(res.latency_ms), error: res.error ?? '' })}
        </div>
      ) : null}
      {res && res.status === 'ok' ? (
        <div className="stack" style={{ gap: 'var(--janus-space-2)' }} role="status">
          <p className="small" style={{ margin: 0 }}>
            <strong>
              {counted.length > 0
                ? t('adminSecurity.categories.tryCounted', { list: counted.join(', ') })
                : t('adminSecurity.categories.tryNothing')}
            </strong>{' '}
            <span className="muted">
              {t('adminSecurity.categories.tryMeta', { time: formatDuration(res.latency_ms), count: res.calls })}
            </span>
          </p>
          <div className="table-wrap">
            <table className="data data-compact routing-table">
              <thead>
                <tr>
                  <th scope="col">{t('adminSecurity.categories.colName')}</th>
                  <th scope="col" className="num">
                    {t('adminSecurity.categories.colProbability')}
                  </th>
                  <th scope="col" className="num">
                    {t('adminSecurity.categories.colMinConfidence')}
                  </th>
                  <th scope="col">{t('adminSecurity.categories.colCounts')}</th>
                </tr>
              </thead>
              <tbody>
                {scores.map((s) => (
                  <tr key={s.slug}>
                    <th scope="row" className="small">
                      {s.name}
                    </th>
                    <td className="num small">{(s.probability * 100).toFixed(1)}%</td>
                    <td className="num small">{pct(s.min_confidence)}</td>
                    <td className="small">
                      {s.counts ? <Badge tone="danger">{t('tables.yes')}</Badge> : <span className="muted">—</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      ) : null}
    </div>
  );
}

function BatchCategories({ classifierId }: { classifierId: string }): ReactNode {
  const run = useMutation({
    mutationFn: () => api.post<SecurityCategoryBatch>(`${BASE}/test-batch`, { classifier_model_id: classifierId }),
  });
  const [open, setOpen] = useState<string | null>(null);
  const res = run.data;
  return (
    <div className="stack" style={{ gap: 'var(--janus-space-3)' }}>
      <div className="row-between">
        <div>
          <h3 style={{ margin: 0 }}>{t('adminSecurity.categories.batchTitle')}</h3>
          <p className="small muted" style={{ margin: 0 }}>
            {t('adminSecurity.categories.batchIntro')}
          </p>
        </div>
        <button type="button" className="btn" disabled={!classifierId || run.isPending} onClick={() => run.mutate()}>
          {run.isPending ? t('adminSecurity.categories.batchRunning') : t('adminSecurity.categories.batchRun')}
        </button>
      </div>
      {run.isError ? (
        <div className="banner banner-danger" role="alert">
          {errorMessage(run.error)}
        </div>
      ) : null}
      {res ? (
        <>
          <p className="small" style={{ margin: 0 }}>
            {t('adminSecurity.categories.batchSummary', {
              percent: pct(res.accuracy),
              found: formatNumber(res.correct),
              total: formatNumber(res.total),
              time: formatDuration(res.elapsed_ms),
            })}
            {res.failed > 0 ? ` · ${t('adminSecurity.categories.batchFailed', { count: res.failed })}` : ''} · {res.classifier}
          </p>
          <div className="table-wrap">
            <table className="data data-compact routing-table">
              <thead>
                <tr>
                  <th scope="col">{t('adminSecurity.categories.colName')}</th>
                  <th scope="col" className="num">
                    {t('adminSecurity.categories.colCaught')}
                  </th>
                  <th scope="col">{t('adminSecurity.categories.colMisses')}</th>
                </tr>
              </thead>
              <tbody>
                {(res.categories ?? []).map((c) => (
                  <Fragment key={c.slug}>
                    <tr
                      data-clickable={c.misses.length > 0 ? 'true' : undefined}
                      onClick={() => setOpen(open === c.slug ? null : c.slug)}
                    >
                      <th scope="row" className="small">
                        {c.name}
                      </th>
                      <td className="num small">
                        <Badge tone={c.accuracy >= 0.9 ? 'success' : c.accuracy >= 0.7 ? 'warning' : 'danger'}>
                          {c.correct}/{c.total}
                        </Badge>
                      </td>
                      <td className="small">
                        {(c.misses ?? []).map((m) => (m.error ? m.error : m.got_name)).join(', ') || (
                          <span className="muted">—</span>
                        )}
                      </td>
                    </tr>
                    {open === c.slug
                      ? (c.misses ?? []).map((m, i) => (
                          <tr key={i} className="routing-miss">
                            <td colSpan={3} className="small">
                              <span className="muted">“{m.text.length > 160 ? `${m.text.slice(0, 160)}…` : m.text}”</span> →{' '}
                              {m.error
                                ? m.error
                                : t('adminSecurity.categories.missLine', { name: m.got_name, percent: pct(m.confidence) })}
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
            {t('adminSecurity.categories.batchHint')}
          </p>
        </>
      ) : null}
    </div>
  );
}
