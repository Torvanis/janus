import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { useToast } from '../../components/ui';
import {
  CAPABILITY_GROUPS,
  CAPABILITY_SOURCE_LABEL,
  capabilityEnumLabel,
  capabilityLabel,
  capabilityValueText,
  probeInFlight,
  probeSummary,
  type CapabilityKey,
  type CapabilityValue,
  type ModelCapabilityView,
} from '../../lib/capabilities';
import { formatRelative } from '../../lib/format';
import type { Model } from '../../lib/types';

// What Janus knows about one model's capabilities, with the source of each
// value, and the administrator's override. A value set here beats what the
// provider says and what a probe measured.

/** Absent = untouched, null = back to automatic, a value = override. */
type Draft = Record<string, CapabilityValue | null>;

function parseList(text: string): string[] {
  if (text.trim().toLowerCase() === 'none') return [];
  return text
    .split(',')
    .map((v) => v.trim().toLowerCase())
    .filter(Boolean);
}

const viewKey = (id: string) => ['admin', 'model-capabilities', id];

/**
 * The last measurement of one model and the button that starts another.
 * While one is waiting or under way the view is re-read every two seconds.
 * Administrators only: it reads the admin capability view.
 */
export function CapabilityMeasure({
  model,
  disabled,
}: {
  model: Pick<Model, 'id' | 'modalities'>;
  disabled?: boolean;
}): ReactNode {
  const toast = useToast();
  const client = useQueryClient();
  const path = `/api/v1/admin/models/${model.id}/capabilities`;
  const view = useQuery({
    queryKey: viewKey(model.id),
    queryFn: () => api.get<ModelCapabilityView>(path),
    refetchInterval: (query) => (probeInFlight(query.state.data?.probed) ? 2000 : false),
  });
  const run = view.data?.probed;
  const inFlight = probeInFlight(run);
  const was = useRef(false);
  useEffect(() => {
    if (was.current && !inFlight) {
      // Finished: the lists carry capabilities too.
      void client.invalidateQueries({ queryKey: ['admin', 'models'] });
      void client.invalidateQueries({ queryKey: ['models'] });
    }
    was.current = inFlight;
  }, [inFlight, client]);
  const start = useMutation({
    mutationFn: () => api.post<ModelCapabilityView>(`${path}/probe`),
    onSuccess: (next) => client.setQueryData(viewKey(model.id), next),
    onError: (failure: Error) => toast(failure.message, 'danger'),
  });
  if (!(model.modalities ?? []).includes('chat')) {
    return <p className="small muted">Only chat models are measured. This one is described by its provider and by you.</p>;
  }
  if (view.isPending || !view.data || !Array.isArray(view.data.keys)) return null;
  return (
    <div className="cap-measure">
      <span className="small" role="status">
        {probeSummary(run, run ? formatRelative(run.at) : '')}
      </span>
      <button
        className="btn btn-sm"
        type="button"
        disabled={disabled || inFlight || start.isPending}
        onClick={() => start.mutate()}
      >
        {inFlight || start.isPending ? 'Measuring…' : run ? 'Measure again' : 'Measure now'}
      </button>
    </div>
  );
}

export function ModelCapabilities({ model, disabled }: { model: Model; disabled?: boolean }): ReactNode {
  const toast = useToast();
  const client = useQueryClient();
  const path = `/api/v1/admin/models/${model.id}/capabilities`;
  const view = useQuery({
    queryKey: viewKey(model.id),
    queryFn: () => api.get<ModelCapabilityView>(path),
  });
  const [draft, setDraft] = useState<Draft>({});
  const [text, setText] = useState<Record<string, string>>({});
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const [error, setError] = useState<string | null>(null);
  const save = useMutation({
    mutationFn: (overrides: Draft) => api.put<ModelCapabilityView>(path, { overrides }),
    onSuccess: (next) => {
      client.setQueryData(viewKey(model.id), next);
      void client.invalidateQueries({ queryKey: ['admin', 'models'] });
      setDraft({});
      setText({});
      setOpen({});
      setError(null);
      toast('Capabilities saved');
    },
    onError: (failure: Error) => setError(failure.message),
  });

  if (view.isPending) return <p className="small muted">Loading capabilities…</p>;
  // An older server answers this path with something else.
  if (view.isError || !view.data || !Array.isArray(view.data.keys))
    return <p className="small muted">Capabilities could not be loaded.</p>;
  const d = view.data;
  const byKey = new Map<string, CapabilityKey>(d.keys.map((k) => [k.key, k]));
  const busy = disabled || save.isPending;
  const set = (key: string, value: CapabilityValue | null | undefined) => {
    setError(null);
    setDraft((cur) => {
      const next = { ...cur };
      if (value === undefined) delete next[key];
      else next[key] = value;
      return next;
    });
  };
  /** What would be in force with the override removed. */
  const automatic = (key: string) => d.probe[key] ?? d.provider[key];
  const dirty = Object.keys(draft).length > 0;

  const control = (def: CapabilityKey): ReactNode => {
    const key = def.key;
    const edit = draft[key];
    const overridden = edit === undefined ? d.admin[key]?.value : (edit ?? undefined);
    const label = capabilityLabel(key);
    if (def.kind === 'bool' || def.enum) {
      const auto = automatic(key);
      const options = def.enum ?? ['true', 'false'];
      const current = overridden === undefined ? '' : String(overridden);
      return (
        <select
          className="input"
          style={{ width: '100%', minWidth: 0 }}
          aria-label={label}
          title={`Automatic: ${capabilityValueText(auto?.value)}`}
          value={current}
          disabled={busy}
          onChange={(event) => {
            const v = event.target.value;
            if (v === '') set(key, d.admin[key] ? null : undefined);
            else set(key, def.kind === 'bool' ? v === 'true' : v);
          }}
        >
          <option value="">Automatic</option>
          {options.map((o) => (
            <option key={o} value={o}>
              {def.kind === 'bool' ? (o === 'true' ? 'Yes' : 'No') : capabilityEnumLabel(o)}
            </option>
          ))}
        </select>
      );
    }
    const shown =
      text[key] ??
      (overridden === undefined
        ? ''
        : Array.isArray(overridden)
          ? overridden.length
            ? overridden.join(', ')
            : 'none'
          : String(overridden));
    return (
      <>
        <input
          className="input"
          style={{ width: '100%', minWidth: 0 }}
          aria-label={label}
          inputMode={def.kind === 'int' ? 'numeric' : undefined}
          placeholder={`Automatic (${capabilityValueText(automatic(key)?.value)})`}
          value={shown}
          disabled={busy}
          onChange={(event) => {
            const raw = event.target.value;
            setText((cur) => ({ ...cur, [key]: raw }));
            if (raw.trim() === '') set(key, d.admin[key] ? null : undefined);
            else if (def.kind === 'list') set(key, parseList(raw));
            else if (def.kind === 'int') set(key, /^\d+$/.test(raw.trim()) ? Number(raw.trim()) : raw.trim());
            else set(key, raw.trim().toLowerCase());
          }}
        />
        {def.kind === 'list' ? (
          <span className="small muted">Separate values with commas. Type none for an empty list.</span>
        ) : null}
      </>
    );
  };

  const row = (key: string): ReactNode => {
    const def = byKey.get(key);
    if (!def) return null;
    const edit = draft[key];
    const inForce = edit === undefined ? d.resolved[key] : undefined;
    const pendingValue = edit === undefined ? undefined : edit === null ? automatic(key)?.value : edit;
    const source =
      edit === undefined
        ? inForce?.source
        : edit === null
          ? d.probe[key]
            ? 'probe'
            : d.provider[key]
              ? 'provider'
              : undefined
          : 'admin';
    const label = capabilityLabel(key);
    const editing = open[key] === true || edit !== undefined;
    const value = edit === undefined ? inForce?.value : pendingValue;
    const overridden = edit === undefined ? Boolean(d.admin[key]) : edit !== null;
    return (
      <div key={key} className="cap-row" role="row" data-cap={key} data-unknown={value === undefined ? 'true' : undefined}>
        <div className="cap-label" role="rowheader">
          {label}
        </div>
        <div className="cap-value" role="cell">
          <span className="cap-value-text">{capabilityValueText(value)}</span>
          {source ? <span className={`cap-source cap-source-${source}`}>{CAPABILITY_SOURCE_LABEL[source]}</span> : null}
          {edit !== undefined ? <span className="cap-source cap-source-pending">Not saved yet</span> : null}
        </div>
        <div className="cap-action" role="cell">
          {editing ? null : (
            <button
              className="btn btn-ghost btn-sm"
              type="button"
              aria-label={`Change ${label}`}
              disabled={busy}
              onClick={() => setOpen((cur) => ({ ...cur, [key]: true }))}
            >
              Change
            </button>
          )}
        </div>
        {inForce?.source === 'probe' && inForce.note ? <div className="cap-note small muted">How: {inForce.note}</div> : null}
        {editing ? (
          <div className="cap-edit">
            <span className="small muted">Your value (leave on automatic to use what Janus finds)</span>
            {control(def)}
            <div className="row">
              {overridden ? (
                <button
                  className="btn btn-ghost btn-sm"
                  type="button"
                  aria-label={`Use the automatic value for ${label}`}
                  disabled={busy}
                  onClick={() => {
                    setText((cur) => ({ ...cur, [key]: '' }));
                    set(key, d.admin[key] ? null : undefined);
                  }}
                >
                  Use automatic ({capabilityValueText(automatic(key)?.value)})
                </button>
              ) : null}
              {edit === undefined ? (
                <button
                  className="btn btn-ghost btn-sm"
                  type="button"
                  disabled={busy}
                  onClick={() => setOpen((cur) => ({ ...cur, [key]: false }))}
                >
                  Close
                </button>
              ) : null}
            </div>
          </div>
        ) : null}
      </div>
    );
  };
  const changes = Object.keys(draft).length;

  return (
    <section className="cap" aria-labelledby={`caps-${model.id}`} data-testid="model-capabilities-editor">
      <h3 id={`caps-${model.id}`} style={{ margin: 0 }}>
        Capabilities
      </h3>
      <p className="small muted" style={{ margin: 0 }}>
        What this model can take in, give back and be asked. Janus fits each request to it. Change a value only where it is wrong:
        yours wins.
      </p>
      <CapabilityMeasure model={model} disabled={disabled} />
      <details className="cap-help small muted">
        <summary>Where these values come from</summary>
        <p>
          Janus first reads what the provider publishes about the model. What the provider does not say, Janus measures by sending
          the model a few small requests (a few hundred tokens, recorded as platform usage), among them a test image, a short
          spoken clip and a one-second video sent the way Chat sends attachments, to check whether it sees images, hears sound and
          watches video. Only a reply that shows the behavior, or a refusal that names the setting, is recorded, so a value can
          stay not known.
        </p>
        <p>A value you set wins over both, and stays until you return it to automatic.</p>
      </details>
      {CAPABILITY_GROUPS.map((group) => {
        const keys = group.keys.filter((key) => byKey.has(key));
        // A row is listed when something is known about it, or it is being changed.
        const listed = keys.filter((key) => d.resolved[key] !== undefined || draft[key] !== undefined || open[key]);
        const unknown = keys.filter((key) => !listed.includes(key));
        if (keys.length === 0) return null;
        return (
          <div key={group.id} className="cap-group" role="table" aria-label={group.label}>
            <h4 className="overline" style={{ margin: 0 }}>
              {group.label}
            </h4>
            {listed.map(row)}
            {unknown.length > 0 ? (
              <details className="cap-unknown">
                <summary>
                  <span className="cap-unknown-title">Not known ({unknown.length})</span>
                  <span className="cap-unknown-names">{unknown.map((key) => capabilityLabel(key)).join(', ')}</span>
                </summary>
                {unknown.map(row)}
              </details>
            ) : null}
          </div>
        );
      })}
      {error ? (
        <p role="alert" className="small" style={{ color: 'var(--janus-color-danger-fg)', margin: 0 }}>
          {error}
        </p>
      ) : null}
      {dirty ? (
        <div className="cap-savebar">
          <span className="small">{changes === 1 ? '1 change not saved' : `${changes} changes not saved`}</span>
          <div className="row">
            <button
              className="btn"
              type="button"
              disabled={busy}
              onClick={() => {
                setDraft({});
                setText({});
                setOpen({});
                setError(null);
              }}
            >
              Discard
            </button>
            <button className="btn btn-primary" type="button" disabled={busy} onClick={() => save.mutate(draft)}>
              {save.isPending ? 'Saving…' : 'Save capabilities'}
            </button>
          </div>
        </div>
      ) : null}
    </section>
  );
}
