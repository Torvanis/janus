import { useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { getModelPriceHistory, hasRateCard, type ModelRatePatch, type RateCardVersion } from '../../api/client';
import type { MetadataSource, MetadataSourceDetail, Model, ModelMetadataField } from '../../lib/types';
import {
  MAX_PRICE_USD,
  PRICE_UNITS,
  compareLines,
  expectedTokenRates,
  formatNanoUSD,
  isBaseTokenRate,
  lineKey,
  modelPriceType,
  nanoToInput,
  parseLineKey,
  parseUSD,
  priceLineLabel,
  priceUnit,
  relevantBases,
  validConditionValue,
  whenText,
  type PriceError,
} from '../../lib/priceLines';
import { t } from '../../lib/i18n';

// The pricing builder: a model's price lines, each "rate per <unit> [when
// <condition>]" with where its value comes from. The drawer owns the draft
// and saves it with the rest of the model in one PATCH.

/** A line's unsaved change: a typed price, or back to the automatic value. */
export type LineEdit = { kind: 'set'; text: string } | { kind: 'reset' };

export interface PriceDraft {
  edits: Record<string, LineEdit>;
  /** Lines the administrator added in this draft (not in the model yet). */
  added: string[];
}

export const emptyDraft: PriceDraft = { edits: {}, added: [] };

export function draftChangeCount(draft: PriceDraft): number {
  return Object.keys(draft.edits).length;
}

function errorText(error: PriceError): string {
  return error === 'max'
    ? t('adminModels.priceBuilder.errors.max', { max: formatNanoUSD(MAX_PRICE_USD * 1e9) })
    : t(`adminModels.priceBuilder.errors.${error}`);
}

/** Each invalid typed price, by line, in words. */
export function draftErrors(draft: PriceDraft): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, edit] of Object.entries(draft.edits)) {
    if (edit.kind !== 'set') continue;
    const parsed = parseUSD(edit.text);
    if ('error' in parsed) out[key] = errorText(parsed.error);
  }
  return out;
}

/**
 * The PATCH fields for a valid draft: the five token rates through their
 * rate_*_usd_per_mtok fields (kept for API clients), every other line
 * through price_lines.
 */
export function draftPatch(draft: PriceDraft): ModelRatePatch {
  const body: ModelRatePatch = {};
  const lines: Record<string, number | null> = {};
  for (const [key, edit] of Object.entries(draft.edits)) {
    let usd: number | null = null;
    if (edit.kind === 'set') {
      const parsed = parseUSD(edit.text);
      if ('error' in parsed) continue;
      usd = parsed.usd;
    }
    const field = isBaseTokenRate(key) ? priceUnit(key)?.ratePatch : undefined;
    if (field) body[field] = usd;
    else lines[key] = usd;
  }
  if (Object.keys(lines).length > 0) body.price_lines = lines;
  return body;
}

function formatDay(day?: string): string {
  if (!day) return '';
  const d = new Date(`${day.slice(0, 10)}T00:00:00Z`);
  if (Number.isNaN(d.getTime())) return day;
  return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric', timeZone: 'UTC' });
}

/** Where a value comes from, in words: label, date, and the page it was read from. */
function sourceOf(
  model: Model,
  source: MetadataSource,
  detail?: MetadataSourceDetail,
): { label: string; date: string; url?: string } {
  switch (source) {
    case 'admin':
      return { label: t('adminModels.priceBuilder.sourceAdmin'), date: formatDay(detail?.set_at) };
    case 'upstream':
      return {
        label: t('adminModels.priceBuilder.sourceUpstream', { provider: model.upstream_name }),
        date: formatDay(detail?.read_at),
      };
    case 'reference':
      return {
        label: detail?.label || t('adminModels.priceBuilder.sourceCatalog'),
        date: formatDay(detail?.read_at),
        url: detail?.url,
      };
    default:
      return { label: t('adminModels.priceBuilder.sourceNone'), date: '' };
  }
}

/** A price in words: Not priced, Not charged, or the amount. */
export function priceText(nano: number | null): string {
  if (nano === null) return t('adminModels.priceBuilder.notPriced');
  if (nano === 0) return t('adminModels.priceBuilder.notCharged');
  return formatNanoUSD(nano);
}

/** "Per 1M input tokens" for a select option. */
function capitalized(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

interface RowState {
  key: string;
  field?: ModelMetadataField;
  edit?: LineEdit;
  added: boolean;
  /** The value in force once the draft is saved; null = not priced. */
  value: number | null;
  invalid: boolean;
  source: MetadataSource;
  detail?: MetadataSourceDetail;
  /** The line goes away on save (a reset with nothing automatic beneath). */
  removing: boolean;
}

function rowState(model: Model, draft: PriceDraft, key: string): RowState {
  const field = model.metadata?.[key];
  const edit = draft.edits[key];
  const added = draft.added.includes(key);
  if (edit?.kind === 'set') {
    const parsed = parseUSD(edit.text);
    return {
      key,
      field,
      edit,
      added,
      value: 'error' in parsed ? null : parsed.nano,
      invalid: 'error' in parsed,
      source: 'admin',
      removing: false,
    };
  }
  if (edit?.kind === 'reset') {
    const auto = field?.automatic_value ?? null;
    return {
      key,
      field,
      edit,
      added,
      value: auto,
      invalid: false,
      source: field?.automatic_source ?? 'unknown',
      detail: field?.automatic_detail,
      removing: auto === null && !isBaseTokenRate(key),
    };
  }
  return {
    key,
    field,
    added,
    value: field?.value ?? null,
    invalid: false,
    source: field?.source ?? 'unknown',
    detail: field?.source_detail,
    removing: false,
  };
}

export function PriceBuilder({
  model,
  draft,
  onChange,
  showErrors,
  disabled,
}: {
  model: Model;
  draft: PriceDraft;
  onChange: (draft: PriceDraft) => void;
  /** Show "enter a price" on empty inputs (after a save attempt). */
  showErrors: boolean;
  disabled: boolean;
}): ReactNode {
  const [open, setOpen] = useState<string[]>([]);
  const [confirmRemove, setConfirmRemove] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const type = modelPriceType(model.modalities ?? []);
  const errors = draftErrors(draft);

  const setEdit = (key: string, edit: LineEdit | undefined) => {
    const edits = { ...draft.edits };
    if (edit) edits[key] = edit;
    else delete edits[key];
    onChange({ ...draft, edits });
  };
  const toggle = (key: string, to = !open.includes(key)) =>
    setOpen(to ? [...open.filter((k) => k !== key), key] : open.filter((k) => k !== key));

  const known = new Set<string>();
  for (const [key, field] of Object.entries(model.metadata ?? {})) {
    if (key !== 'context_window' && (field.value !== null || draft.edits[key])) known.add(key);
  }
  for (const key of draft.added) known.add(key);
  const fold: string[] = [];
  for (const key of expectedTokenRates(type)) {
    if (known.has(key)) continue;
    if (open.includes(key) || draft.edits[key]) known.add(key);
    else fold.push(key);
  }
  const keys = [...known].sort(compareLines);
  const defaultType = keys
    .map((k) => {
      const f = model.metadata?.[k];
      return f?.automatic_detail?.kind === 'default' ? f.automatic_detail.type : undefined;
    })
    .find((x) => x);

  const remove = (row: RowState) => {
    if (row.added) {
      onChange({
        edits: Object.fromEntries(Object.entries(draft.edits).filter(([k]) => k !== row.key)),
        added: draft.added.filter((k) => k !== row.key),
      });
      return;
    }
    if (row.field?.automatic_value != null) {
      setConfirmRemove(row.key);
      return;
    }
    setEdit(row.key, row.field?.override != null ? { kind: 'reset' } : undefined);
  };

  return (
    <section className="price-builder" aria-labelledby="price-builder-heading">
      <h3 id="price-builder-heading" className="small" style={{ margin: 0 }}>
        {t('adminModels.priceBuilder.title')}
      </h3>
      <p className="small muted" style={{ margin: 0 }}>
        {t('adminModels.priceBuilder.intro')}
      </p>
      {model.cost_quoted ? (
        <div className="banner banner-info" data-testid="quote-note">
          <p className="small" style={{ margin: 0 }}>
            {t('adminModels.priceBuilder.quoteNote')}{' '}
            <Link to="/admin/models/pricing-accuracy">{t('adminModels.priceBuilder.quoteNoteLink')}</Link>.
          </p>
        </div>
      ) : null}
      {defaultType ? (
        <p className="small muted" style={{ margin: 0 }} data-testid="defaults-note">
          {t('adminModels.priceBuilder.defaultsNote', { name: defaultType, provider: model.upstream_name })}
        </p>
      ) : null}
      <ul className="price-lines" aria-label={t('adminModels.priceBuilder.linesLabel')}>
        {keys.map((key) => {
          const row = rowState(model, draft, key);
          const label = priceLineLabel(key);
          const when = whenText(key);
          const per = priceUnit(parseLineKey(key).base)?.per ?? parseLineKey(key).base;
          const src = sourceOf(model, row.source, row.detail);
          const typed = row.edit?.kind === 'set' && row.edit.text.trim() !== '';
          const error = errors[key] && (showErrors || typed) ? errors[key] : undefined;
          const isOpen = open.includes(key) || !!error;
          const auto = row.field?.automatic_value ?? null;
          const adminNow = row.source === 'admin';
          const inputText = row.edit?.kind === 'set' ? row.edit.text : row.value === null ? '' : nanoToInput(row.value);
          return (
            <li
              key={key}
              className="price-row"
              data-line={key}
              data-source={row.source}
              data-pending={row.edit ? '' : undefined}
              data-removing={row.removing ? '' : undefined}
            >
              <div className="price-main">
                <span className="price-rate" data-unpriced={row.value === null && !row.invalid ? '' : undefined}>
                  {row.removing ? t('adminModels.priceBuilder.willRemove') : row.invalid ? '—' : priceText(row.value)}
                </span>{' '}
                <span className="price-unit">{per}</span>
                {when ? <span className="price-when"> {when}</span> : null}
              </div>
              <div className="price-action">
                {row.removing ? (
                  <button
                    className="btn btn-ghost btn-sm"
                    type="button"
                    disabled={disabled}
                    onClick={() => setEdit(key, undefined)}
                  >
                    {t('adminModels.priceBuilder.undo')}
                  </button>
                ) : (
                  <button
                    className="btn btn-ghost btn-sm"
                    type="button"
                    aria-expanded={isOpen}
                    aria-label={t(isOpen ? 'adminModels.priceBuilder.closeFor' : 'adminModels.priceBuilder.changeFor', {
                      name: label,
                    })}
                    disabled={disabled}
                    onClick={() => toggle(key, !isOpen)}
                  >
                    {t(isOpen ? 'adminModels.priceBuilder.close' : 'adminModels.priceBuilder.change')}
                  </button>
                )}
              </div>
              <div className={`price-source${adminNow ? ' price-source-admin' : ''}`}>
                <span className="price-source-label">{src.label}</span>
                {row.edit && !row.removing ? <span> · {t('adminModels.priceBuilder.notSaved')}</span> : null}
                {src.date && !(row.edit && adminNow) ? <span> · {src.date}</span> : null}
                {src.url ? (
                  <>
                    {' · '}
                    <a href={src.url} target="_blank" rel="noreferrer">
                      {t('adminModels.priceBuilder.sourcePage')}
                    </a>
                  </>
                ) : null}
              </div>
              {isOpen && !row.removing ? (
                <div className="price-edit">
                  <div className="price-input">
                    <span aria-hidden="true">$</span>
                    <input
                      className="input mono"
                      inputMode="decimal"
                      autoComplete="off"
                      aria-label={t('adminModels.priceBuilder.inputFor', { name: label })}
                      aria-invalid={error ? true : undefined}
                      value={inputText}
                      placeholder={t('adminModels.priceBuilder.notPriced')}
                      disabled={disabled}
                      onChange={(event) => setEdit(key, { kind: 'set', text: event.target.value })}
                    />
                    <span className="small">{per}</span>
                  </div>
                  {error ? (
                    <p className="field-error small" role="alert" style={{ margin: 0 }}>
                      {error}
                    </p>
                  ) : null}
                  {confirmRemove === key ? (
                    <div className="price-confirm" role="group" aria-label={t('adminModels.priceBuilder.removeTitle')}>
                      <strong className="small">{t('adminModels.priceBuilder.removeTitle')}</strong>
                      <p className="small" style={{ margin: 0 }}>
                        {t('adminModels.priceBuilder.removeBody', {
                          name: sourceOf(model, row.field?.automatic_source ?? 'unknown', row.field?.automatic_detail).label,
                          value: formatNanoUSD(auto ?? 0),
                        })}
                      </p>
                      <div className="row" style={{ flexWrap: 'wrap' }}>
                        <button
                          className="btn btn-danger btn-sm"
                          type="button"
                          onClick={() => {
                            setEdit(key, { kind: 'set', text: '0' });
                            setConfirmRemove(null);
                          }}
                        >
                          {t('adminModels.priceBuilder.removeConfirm')}
                        </button>
                        <button className="btn btn-sm" type="button" data-autofocus onClick={() => setConfirmRemove(null)}>
                          {t('adminModels.priceBuilder.keep')}
                        </button>
                      </div>
                    </div>
                  ) : (
                    <div className="row" style={{ flexWrap: 'wrap', gap: 'var(--janus-space-2)' }}>
                      {auto !== null && adminNow ? (
                        <button
                          className="btn btn-sm"
                          type="button"
                          disabled={disabled}
                          onClick={() => setEdit(key, row.field?.override != null ? { kind: 'reset' } : undefined)}
                        >
                          {t('adminModels.priceBuilder.resetAutomatic')}
                        </button>
                      ) : null}
                      {row.value !== null || row.added || row.invalid ? (
                        <button
                          className="btn btn-ghost btn-sm price-remove"
                          type="button"
                          disabled={disabled}
                          onClick={() => remove(row)}
                        >
                          {t('adminModels.priceBuilder.remove')}
                        </button>
                      ) : null}
                    </div>
                  )}
                  {auto !== null && adminNow && confirmRemove !== key ? (
                    <p className="small muted" style={{ margin: 0 }}>
                      {t('adminModels.priceBuilder.resetPreview', {
                        value: priceText(auto),
                        name: [
                          sourceOf(model, row.field?.automatic_source ?? 'unknown', row.field?.automatic_detail).label,
                          formatDay(row.field?.automatic_detail?.read_at),
                        ]
                          .filter(Boolean)
                          .join(', '),
                      })}
                    </p>
                  ) : null}
                </div>
              ) : null}
            </li>
          );
        })}
      </ul>
      {fold.length > 0 ? (
        <p className="small price-fold">
          {t('adminModels.priceBuilder.notPricedFold', { count: fold.length, list: '' })}
          {fold.map((key, i) => (
            <span key={key}>
              {i > 0 ? ', ' : ''}
              <button className="price-fold-link" type="button" disabled={disabled} onClick={() => toggle(key, true)}>
                {priceLineLabel(key)}
              </button>
            </span>
          ))}
        </p>
      ) : null}
      {adding ? (
        <AddLine
          type={type}
          taken={(key) =>
            keys.includes(key) && !(isBaseTokenRate(key) && rowState(model, draft, key).value === null && !draft.edits[key])
          }
          onCancel={() => setAdding(false)}
          onAdd={(key, text) => {
            const inModel = model.metadata?.[key] !== undefined;
            onChange({
              edits: { ...draft.edits, [key]: { kind: 'set', text } },
              added: inModel || draft.added.includes(key) ? draft.added : [...draft.added, key],
            });
            setAdding(false);
          }}
        />
      ) : (
        <div>
          <button className="btn btn-sm" type="button" disabled={disabled} onClick={() => setAdding(true)}>
            + {t('adminModels.priceBuilder.addLine')}
          </button>
        </div>
      )}
      {hasRateCard(model) ? <PriceHistory model={model} /> : null}
    </section>
  );
}

function AddLine({
  type,
  taken,
  onAdd,
  onCancel,
}: {
  type: string;
  taken: (key: string) => boolean;
  onAdd: (key: string, text: string) => void;
  onCancel: () => void;
}): ReactNode {
  const relevant = relevantBases(type);
  const [more, setMore] = useState(false);
  // The model's own units first, in the order that matters for its type.
  const units = [
    ...relevant.map((b) => priceUnit(b)).filter((u): u is NonNullable<typeof u> => !!u),
    ...(more ? PRICE_UNITS.filter((u) => !relevant.includes(u.base)) : []),
  ];
  const [base, setBase] = useState(units[0]?.base ?? 'rate_in_nanousd');
  const [conds, setConds] = useState<Record<string, string>>({});
  const [text, setText] = useState('');
  const [error, setError] = useState<string | null>(null);
  const unit = priceUnit(base);
  const cond = (name: string) => conds[name] ?? '';
  const setCond = (name: string, value: string) => {
    setConds({ ...conds, [name]: value });
    setError(null);
  };
  const submit = () => {
    const clean: Record<string, string> = {};
    for (const name of unit?.conditions ?? []) {
      const v = cond(name).trim().toLowerCase();
      if (v === '') continue;
      if (name === 'prompt_over') {
        if (!/^\d+$/.test(v) || Number(v) <= 0) return setError(t('adminModels.priceBuilder.errors.promptOver'));
      } else if (!validConditionValue(v)) {
        return setError(t('adminModels.priceBuilder.errors.condition'));
      }
      clean[name] = name === 'prompt_over' ? String(Number(v)) : v;
    }
    const key = lineKey(base, clean);
    if (taken(key)) return setError(t('adminModels.priceBuilder.errors.duplicate'));
    const parsed = parseUSD(text);
    if ('error' in parsed) return setError(errorText(parsed.error));
    onAdd(key, text.trim());
  };
  const field = (name: string, label: string, placeholder: string) => (
    <label className="field" key={name}>
      <span className="field-label">{label}</span>
      <input
        className="input"
        value={cond(name)}
        placeholder={placeholder}
        inputMode={name === 'prompt_over' ? 'numeric' : undefined}
        onChange={(event) => setCond(name, event.target.value)}
      />
    </label>
  );
  return (
    <div
      className="price-add"
      role="group"
      aria-label={t('adminModels.priceBuilder.addTitle')}
      onKeyDown={(event) => {
        if (event.key === 'Enter' && (event.target as HTMLElement).tagName === 'INPUT') {
          event.preventDefault();
          submit();
        }
      }}
    >
      <strong className="small">{t('adminModels.priceBuilder.addTitle')}</strong>
      <div className="price-add-grid">
        <label className="field">
          <span className="field-label">{t('adminModels.priceBuilder.unit')}</span>
          <select
            className="select"
            value={base}
            onChange={(event) => {
              setBase(event.target.value);
              setConds({});
              setError(null);
            }}
          >
            {units.map((u) => (
              <option key={u.base} value={u.base}>
                {capitalized(u.per)}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span className="field-label">{t('adminModels.priceBuilder.price')}</span>
          <span className="price-input">
            <span aria-hidden="true">$</span>
            <input
              className="input mono"
              inputMode="decimal"
              autoComplete="off"
              aria-label={t('adminModels.priceBuilder.inputFor', { name: unit?.per ?? base })}
              value={text}
              onChange={(event) => {
                setText(event.target.value);
                setError(null);
              }}
            />
          </span>
        </label>
        {unit?.conditions.includes('prompt_over')
          ? field('prompt_over', t('adminModels.priceBuilder.promptOver'), '200000')
          : null}
        {unit?.conditions.includes('quality') ? field('quality', t('adminModels.priceBuilder.quality'), 'high') : null}
        {unit?.conditions.includes('size') ? field('size', t('adminModels.priceBuilder.size'), '1024x1024') : null}
        {unit?.conditions.includes('resolution') ? field('resolution', t('adminModels.priceBuilder.resolution'), '720p') : null}
        {unit?.conditions.includes('audio') ? (
          <label className="field">
            <span className="field-label">{t('adminModels.priceBuilder.sound')}</span>
            <select className="select" value={cond('audio')} onChange={(event) => setCond('audio', event.target.value)}>
              <option value="">{t('adminModels.priceBuilder.soundAny')}</option>
              <option value="yes">{t('adminModels.priceBuilder.soundYes')}</option>
              <option value="no">{t('adminModels.priceBuilder.soundNo')}</option>
            </select>
          </label>
        ) : null}
      </div>
      {unit && unit.conditions.length > 0 ? (
        <p className="small muted" style={{ margin: 0 }}>
          {t('adminModels.priceBuilder.conditionHint')}
        </p>
      ) : null}
      {error ? (
        <p className="field-error small" role="alert" style={{ margin: 0 }}>
          {error}
        </p>
      ) : null}
      <div className="row" style={{ flexWrap: 'wrap', gap: 'var(--janus-space-2)' }}>
        <button className="btn btn-primary btn-sm" type="button" onClick={submit}>
          {t('adminModels.priceBuilder.add')}
        </button>
        <button className="btn btn-sm" type="button" onClick={onCancel}>
          {t('common.cancel')}
        </button>
        <button
          className="btn btn-ghost btn-sm"
          type="button"
          aria-pressed={more}
          onClick={() => {
            const next = !more;
            setMore(next);
            if (!next && !relevant.includes(base)) {
              setBase(relevant[0] ?? 'rate_in_nanousd');
              setConds({});
            }
          }}
        >
          {t(more ? 'adminModels.priceBuilder.fewerUnits' : 'adminModels.priceBuilder.moreUnits')}
        </button>
      </div>
    </div>
  );
}

const RATE_FIELDS = [
  'rate_in_nanousd',
  'rate_cached_nanousd',
  'rate_cache_write_5m_nanousd',
  'rate_cache_write_1h_nanousd',
  'rate_out_nanousd',
] as const;

function versionValues(v: RateCardVersion): Map<string, number> {
  const out = new Map<string, number>();
  for (const f of RATE_FIELDS) if (v[f]) out.set(f, v[f]);
  for (const [k, n] of Object.entries(v.lines ?? {})) out.set(k, n);
  return out;
}

/** What changed from older to newer, one phrase per line. */
function versionChanges(newer: RateCardVersion, older?: RateCardVersion): string[] {
  const now = versionValues(newer);
  if (!older) return [...now.keys()].sort(compareLines).map((k) => `${priceLineLabel(k)}: ${formatNanoUSD(now.get(k) ?? 0)}`);
  const before = versionValues(older);
  const keys = [...new Set([...now.keys(), ...before.keys()])].sort(compareLines);
  const fmt = (n?: number) => (n === undefined ? t('adminModels.priceBuilder.notPriced') : formatNanoUSD(n));
  return keys
    .filter((k) => now.get(k) !== before.get(k))
    .map((k) => `${priceLineLabel(k)}: ${fmt(before.get(k))} → ${fmt(now.get(k))}`);
}

function PriceHistory({ model }: { model: Model }): ReactNode {
  const [open, setOpen] = useState(false);
  const history = useQuery({
    queryKey: ['admin', 'model-price-history', model.id, model.rate_effective_from],
    queryFn: () => getModelPriceHistory(model.id),
    enabled: open,
  });
  const versions = history.data?.versions ?? [];
  return (
    <details className="price-history" onToggle={(event) => setOpen((event.currentTarget as HTMLDetailsElement).open)}>
      <summary className="small">{t('adminModels.priceBuilder.history')}</summary>
      {history.isPending && open ? <p className="small muted">{t('adminModels.priceBuilder.historyLoading')}</p> : null}
      {history.error ? (
        <p className="small" role="alert">
          {history.error.message}
        </p>
      ) : null}
      {history.data && versions.length === 0 ? <p className="small muted">{t('adminModels.priceBuilder.historyEmpty')}</p> : null}
      {versions.length > 0 ? (
        <ol className="price-history-list">
          {versions.map((v, i) => {
            const changes = versionChanges(v, versions[i + 1]);
            return (
              <li key={`${v.effective_from}-${i}`}>
                <span className="small" style={{ fontWeight: 600 }}>
                  {new Date(v.effective_from).toLocaleString('en-US', { dateStyle: 'medium', timeStyle: 'short' })}
                </span>
                {i === versions.length - 1 ? (
                  <span className="small muted"> · {t('adminModels.priceBuilder.historyFirst')}</span>
                ) : null}
                <ul className="small muted">
                  {changes.length ? (
                    changes.map((c) => <li key={c}>{c}</li>)
                  ) : (
                    <li>{t('adminModels.priceBuilder.historyNoChange')}</li>
                  )}
                </ul>
              </li>
            );
          })}
        </ol>
      ) : null}
    </details>
  );
}
