import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/api';
import { ConfirmDialog, useToast } from '../components/ui';
import {
  capabilityLabel,
  capabilityValueText,
  planSourceLabel,
  probeInFlight,
  probeSummary,
  vendorShortName,
  type CapabilityResolved,
  type PlanCapabilitiesResponse,
  type PlanCapabilityView,
} from '../lib/capabilities';
import { formatDate, formatRelative } from '../lib/format';

// What a person's own plan models can take, where each answer came from,
// and the two things they can do about it: measure the model through their
// own plan, or say Yes / No themselves. Owner ruling: measuring and setting
// happen only on the Subscriptions page (mode="manage"). Everywhere else
// (the Models page pop-up for my/<provider>/<model> models) shows the values
// read-only (mode="view") with a link to change them on Subscriptions.

/** The values a person sets, in the order they matter to Chat. */
const SETTABLE: { key: string; label: string; hint: string }[] = [
  { key: 'input.image', label: 'Images', hint: 'Chat sends attached pictures to the model.' },
  { key: 'input.audio', label: 'Sound', hint: 'Chat sends attached sound to the model.' },
  { key: 'input.video', label: 'Video', hint: 'Chat sends attached video to the model.' },
  { key: 'request.tools', label: 'Function tools', hint: 'The model can call tools.' },
];

export const planCapabilitiesKey = (subscriptionId: string) => ['plan-capabilities', subscriptionId];

/** The capabilities of every model on one connection, re-read while one is being measured. */
export function usePlanCapabilities(subscriptionId: string | undefined) {
  return useQuery({
    queryKey: planCapabilitiesKey(subscriptionId ?? ''),
    queryFn: () =>
      api.get<PlanCapabilitiesResponse>(`/api/v1/me/subscriptions/${encodeURIComponent(subscriptionId ?? '')}/capabilities`),
    enabled: !!subscriptionId,
    refetchInterval: (q) => (q.state.data?.models?.some((m) => probeInFlight(m.probed)) ? 2000 : false),
  });
}

/** Short chips for a model row: only what is known to be yes. */
export function planCapabilityChips(view: PlanCapabilityView | undefined): string[] {
  if (!view) return [];
  const yes = (key: string) => view.resolved[key]?.value === true;
  const out: string[] = [];
  if (yes('input.image')) out.push('Sees images');
  if (yes('input.audio')) out.push('Hears sound');
  if (yes('input.video')) out.push('Watches video');
  if (yes('request.tools')) out.push('Tools');
  return out;
}

function sourceText(r: CapabilityResolved | undefined, vendor: string): string {
  if (!r) return 'Nobody has said';
  return planSourceLabel(r.source, vendor, r.source === 'probe' ? formatDate(r.at) : '');
}

/** Where a plan model's capabilities are changed: its provider tab on Subscriptions, with its pop-up open. */
export function planCapabilitiesHref(provider: string, model: string): string {
  return `/subscriptions/${encodeURIComponent(provider)}?capabilities=${encodeURIComponent(model)}`;
}

/**
 * The Capabilities section for one plan model. `subscriptionId` is the
 * connection; `model` the native model name. `mode="manage"` (Subscriptions
 * only) adds Yes / No / Use default and Measure; the default `view` shows the
 * values with their sources and links to Subscriptions to change them.
 */
export function PlanCapabilities({
  subscriptionId,
  model,
  mode = 'view',
}: {
  subscriptionId: string;
  model: string;
  mode?: 'view' | 'manage';
}): ReactNode {
  const manage = mode === 'manage';
  const toast = useToast();
  const client = useQueryClient();
  const caps = usePlanCapabilities(subscriptionId);
  const [confirm, setConfirm] = useState(false);
  const path = `/api/v1/me/subscriptions/${encodeURIComponent(subscriptionId)}/capabilities`;
  const view = caps.data?.models?.find((m) => m.model === model);
  const vendor = vendorShortName(caps.data?.provider_name ?? 'the vendor');
  const run = view?.probed;
  const inFlight = probeInFlight(run);

  const replace = (next: PlanCapabilityView) => {
    client.setQueryData<PlanCapabilitiesResponse>(planCapabilitiesKey(subscriptionId), (cur) =>
      cur ? { ...cur, models: cur.models.map((m) => (m.model === next.model ? next : m)) } : cur,
    );
  };
  // When a measurement ends, the model list (Chat, Models page) reads the new answers.
  const was = useRef(false);
  useEffect(() => {
    if (was.current && !inFlight) void client.invalidateQueries({ queryKey: ['models'] });
    was.current = inFlight;
  }, [inFlight, client]);

  const set = useMutation({
    mutationFn: (change: Record<string, boolean | null>) => api.patch<PlanCapabilityView>(path, { model, set: change }),
    onSuccess: (next) => {
      replace(next);
      void client.invalidateQueries({ queryKey: ['models'] });
    },
    onError: (failure: Error) => toast(failure.message, 'danger'),
  });
  const measure = useMutation({
    mutationFn: () => api.post<PlanCapabilityView>(`${path}/measure`, { model }),
    onSuccess: (next) => {
      replace(next);
      setConfirm(false);
    },
    onError: (failure: Error) => {
      setConfirm(false);
      toast(failure.message, 'danger');
    },
  });

  if (caps.isPending) return <p className="small muted">Loading capabilities…</p>;
  if (caps.isError || !caps.data || !Array.isArray(caps.data.models))
    return <p className="small muted">Capabilities could not be loaded.</p>;
  if (!view) return <p className="small muted">This plan no longer offers {model}.</p>;

  const waitUntil = view.measure_after ? new Date(view.measure_after) : null;
  const waiting = waitUntil !== null && waitUntil.getTime() > Date.now();
  const others = Object.entries(view.resolved)
    .filter(([key]) => !SETTABLE.some((s) => s.key === key))
    .sort(([a], [b]) => a.localeCompare(b));

  return (
    <section className="cap plan-cap" aria-labelledby={`plan-cap-${model}`}>
      <div>
        <h3 id={`plan-cap-${model}`} style={{ margin: 0 }}>
          Capabilities
        </h3>
        <p className="small secondary" style={{ margin: 'var(--janus-space-1) 0 0' }}>
          What this model takes on your {vendor} plan, and where each answer comes from. Chat sends pictures, sound and video
          straight to the model only where the answer is Yes; otherwise it describes them in words.
        </p>
      </div>

      <div className="cap-group" role="list" aria-label="What it takes">
        {SETTABLE.map(({ key, label, hint }) => {
          const r = view.resolved[key];
          const mine = view.set[key] !== undefined;
          return (
            <div key={key} className="cap-row" role="listitem" data-unknown={r ? undefined : ''}>
              <div className="cap-label" title={hint}>
                {label}
              </div>
              <div className="cap-value">
                <span className="cap-value-text">{r ? capabilityValueText(r.value) : 'Not known'}</span>
                <span className={mine ? 'cap-source cap-source-admin' : 'cap-source'} title={r?.note || undefined}>
                  {sourceText(r, vendor)}
                </span>
              </div>
              {manage ? (
                <div className="cap-action segmented plan-cap-set" role="group" aria-label={`Set ${label}`}>
                  <button
                    type="button"
                    aria-pressed={mine && r?.value === true}
                    disabled={set.isPending}
                    onClick={() => set.mutate({ [key]: true })}
                  >
                    Yes
                  </button>
                  <button
                    type="button"
                    aria-pressed={mine && r?.value === false}
                    disabled={set.isPending}
                    onClick={() => set.mutate({ [key]: false })}
                  >
                    No
                  </button>
                  <button
                    type="button"
                    aria-pressed={!mine}
                    disabled={set.isPending}
                    onClick={() => mine && set.mutate({ [key]: null })}
                    title="Use what was measured, reported by the vendor or assumed"
                  >
                    Use default
                  </button>
                </div>
              ) : null}
              {r?.note ? <div className="cap-note small secondary">{r.note}</div> : null}
            </div>
          );
        })}
      </div>

      {others.length > 0 ? (
        <details className="cap-group cap-unknown">
          <summary>
            <span className="cap-unknown-title">More details</span>
            <span className="cap-unknown-names">{others.map(([key]) => capabilityLabel(key)).join(', ')}</span>
          </summary>
          {others.map(([key, r]) => (
            <div key={key} className="cap-row">
              <div className="cap-label">{capabilityLabel(key)}</div>
              <div className="cap-value">
                <span className="cap-value-text">{capabilityValueText(r.value)}</span>
                <span className="cap-source" title={r.note || undefined}>
                  {sourceText(r, vendor)}
                </span>
              </div>
            </div>
          ))}
        </details>
      ) : null}

      <div className="cap-measure">
        <span className="small" role="status">
          {view.not_measurable ? `Not measured: ${view.not_measurable}.` : probeSummary(run, run ? formatRelative(run.at) : '')}
          {manage && waiting && !inFlight ? ` You can measure it again ${formatRelative(view.measure_after)}.` : ''}
        </span>
        {!manage ? (
          <Link className="btn btn-sm" to={planCapabilitiesHref(caps.data.provider, model)}>
            Change on Subscriptions
          </Link>
        ) : view.not_measurable ? null : (
          <button
            type="button"
            className="btn btn-sm"
            disabled={inFlight || waiting || measure.isPending}
            onClick={() => setConfirm(true)}
          >
            {inFlight || measure.isPending ? 'Measuring…' : run ? 'Measure again' : 'Measure'}
          </button>
        )}
      </div>

      {manage ? (
        <ConfirmDialog
          open={confirm}
          onClose={() => setConfirm(false)}
          onConfirm={() => measure.mutate()}
          title={`Measure ${model}?`}
          consequence={`Janus sends up to ${caps.data.max_requests} small requests to ${model} through your ${vendor} plan to check pictures, sound, video, tools and thinking. They count against your plan's limits. Each model can be measured once every ${Math.round(caps.data.measure_every_seconds / 60)} minutes.`}
          confirmLabel="Measure"
          danger={false}
          busy={measure.isPending}
        />
      ) : null}
    </section>
  );
}
