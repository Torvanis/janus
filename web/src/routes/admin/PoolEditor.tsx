import { useQuery } from '@tanstack/react-query';
import { useMemo, useState, type ReactNode } from 'react';
import { api } from '../../lib/api';
import { formatNumber, formatRelative } from '../../lib/format';
import { t } from '../../lib/i18n';
import {
  POOL_AFFINITIES,
  POOL_POLICIES,
  publicModelName,
  type ManagedModelPool,
  type Model,
  type PoolAffinity,
  type PoolMember,
  type PoolMemberHealth,
  type PoolMemberState,
  type PoolPolicy,
} from '../../lib/types';
import { Badge, Field } from '../../components/ui';
import { IconButton } from '../../components/IconButton';

/** The editable shape of a pool; members carry only what the API accepts. */
export interface PoolDraft {
  policy: PoolPolicy;
  affinity: PoolAffinity;
  spill_pct: number;
  members: Pick<PoolMember, 'model_id' | 'weight' | 'priority' | 'enabled' | 'context_capacity'>[];
}

export function poolDraftFrom(pool: ManagedModelPool | undefined, targetId: string): PoolDraft {
  if (pool && pool.members.length > 0) {
    return {
      policy: pool.policy,
      affinity: pool.affinity,
      spill_pct: pool.spill_pct,
      members: pool.members.map((m) => ({
        model_id: m.model_id,
        weight: m.weight,
        priority: m.priority,
        enabled: m.enabled,
        context_capacity: m.context_capacity,
      })),
    };
  }
  return {
    policy: 'failover',
    affinity: 'bounded',
    spill_pct: 25,
    members: targetId ? [{ model_id: targetId, weight: 1, priority: 0, enabled: true, context_capacity: 0 }] : [],
  };
}

/** Mirrors ManagedModelPool.IsLoadBalanced on the server (the licensed shape). */
export function isLoadBalanced(draft: PoolDraft): boolean {
  return draft.members.filter((m) => m.enabled).length > 1 || draft.policy !== 'failover';
}

const policyLabel = (p: PoolPolicy): string =>
  ({
    failover: t('adminManagedModels.poolPolicyFailover'),
    round_robin: t('adminManagedModels.poolPolicyRoundRobin'),
    least_loaded: t('adminManagedModels.poolPolicyLeastLoaded'),
    context: t('adminManagedModels.poolPolicyContext'),
  })[p];

const affinityLabel = (a: PoolAffinity): string =>
  ({
    bounded: t('adminManagedModels.poolAffinityBounded'),
    strict: t('adminManagedModels.poolAffinityStrict'),
    off: t('adminManagedModels.poolAffinityOff'),
  })[a];

/**
 * Pool editor: members (model, weight, priority, capacity, in use) plus the
 * balancing policy and conversation affinity. The first member is the
 * alias's target; a pool of one behaves exactly like a classic alias.
 */
export function PoolEditor({
  draft,
  onChange,
  models,
  licensed,
  wasBalanced,
  fallbackId,
}: {
  draft: PoolDraft;
  onChange: (next: PoolDraft) => void;
  models: Model[];
  licensed: boolean;
  /** The stored pool already balances, so edits stay allowed when unlicensed. */
  wasBalanced: boolean;
  fallbackId: string;
}): ReactNode {
  const [adding, setAdding] = useState('');
  const byId = useMemo(() => new Map(models.map((m) => [m.id, m])), [models]);
  const inPool = new Set(draft.members.map((m) => m.model_id));
  const addable = models.filter((m) => !inPool.has(m.id) && m.id !== fallbackId);
  const locked = !licensed && !wasBalanced;
  const balanced = draft.members.length > 1;
  const update = (i: number, patch: Partial<PoolDraft['members'][number]>) =>
    onChange({ ...draft, members: draft.members.map((m, j) => (j === i ? { ...m, ...patch } : m)) });

  const windows = new Set(draft.members.map((m) => byId.get(m.model_id)?.context_window ?? 0).filter((w) => w > 0));

  return (
    <section className="card stack" aria-labelledby="managed-model-pool-heading" data-testid="pool-editor">
      <div>
        <h3 id="managed-model-pool-heading" className="small" style={{ margin: 0, fontWeight: 600 }}>
          {t('adminManagedModels.poolSection')}
        </h3>
        <p className="small muted" style={{ margin: 0 }}>
          {locked ? t('adminManagedModels.poolUpsell') : t('adminManagedModels.poolSectionHint')}
        </p>
      </div>

      {balanced ? (
        <div className="table-wrap">
          <table className="data">
            <thead>
              <tr>
                <th scope="col">{t('adminManagedModels.poolColModel')}</th>
                <th scope="col" className="num" title={t('adminManagedModels.poolWeightHint')}>
                  {t('adminManagedModels.poolColWeight')}
                </th>
                <th scope="col" className="num" title={t('adminManagedModels.poolPriorityHint')}>
                  {t('adminManagedModels.poolColPriority')}
                </th>
                {draft.policy === 'context' ? (
                  <th scope="col" className="num" title={t('adminManagedModels.poolCapacityHint')}>
                    {t('adminManagedModels.poolColCapacity')}
                  </th>
                ) : null}
                <th scope="col">{t('adminManagedModels.poolColEnabled')}</th>
                <th scope="col">
                  <span className="sr-only">{t('tables.actions')}</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {draft.members.map((m, i) => {
                const model = byId.get(m.model_id);
                const name = model ? publicModelName(model) : m.model_id;
                return (
                  <tr key={m.model_id}>
                    <td className="small">
                      <span className="mono">{name}</span>
                      <div className="small muted">{model?.upstream_name ?? ''}</div>
                    </td>
                    <td className="num">
                      <input
                        className="input"
                        type="number"
                        min={1}
                        max={100}
                        style={{ width: 72 }}
                        aria-label={`${t('adminManagedModels.poolColWeight')} ${name}`}
                        value={m.weight}
                        onChange={(e) => update(i, { weight: Math.max(1, Math.min(100, Number(e.target.value) || 1)) })}
                      />
                    </td>
                    <td className="num">
                      <input
                        className="input"
                        type="number"
                        min={0}
                        max={100}
                        style={{ width: 72 }}
                        aria-label={`${t('adminManagedModels.poolColPriority')} ${name}`}
                        value={m.priority}
                        onChange={(e) => update(i, { priority: Math.max(0, Math.min(100, Number(e.target.value) || 0)) })}
                      />
                    </td>
                    {draft.policy === 'context' ? (
                      <td className="num">
                        <input
                          className="input"
                          type="number"
                          min={0}
                          step={1024}
                          style={{ width: 110 }}
                          aria-label={`${t('adminManagedModels.poolColCapacity')} ${name}`}
                          value={m.context_capacity}
                          onChange={(e) => update(i, { context_capacity: Math.max(0, Number(e.target.value) || 0) })}
                        />
                      </td>
                    ) : null}
                    <td>
                      <input
                        type="checkbox"
                        aria-label={`${t('adminManagedModels.poolColEnabled')} ${name}`}
                        checked={m.enabled}
                        onChange={(e) => update(i, { enabled: e.target.checked })}
                      />
                    </td>
                    <td>
                      <IconButton
                        icon="delete"
                        danger
                        label={t('adminManagedModels.poolRemoveMember', { name })}
                        onClick={() => onChange({ ...draft, members: draft.members.filter((_, j) => j !== i) })}
                      />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}

      <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
        <select
          className="select"
          aria-label={t('adminManagedModels.poolChooseMember')}
          value={adding}
          disabled={locked}
          onChange={(e) => setAdding(e.target.value)}
          data-testid="pool-add-select"
        >
          <option value="">{t('adminManagedModels.poolChooseMember')}</option>
          {addable.map((m) => (
            <option key={m.id} value={m.id}>
              {publicModelName(m)} · {m.upstream_name}
            </option>
          ))}
        </select>
        <button
          type="button"
          className="btn"
          disabled={!adding || locked}
          onClick={() => {
            onChange({
              ...draft,
              members: [...draft.members, { model_id: adding, weight: 1, priority: 0, enabled: true, context_capacity: 0 }],
            });
            setAdding('');
          }}
        >
          {t('adminManagedModels.poolAddMember')}
        </button>
      </div>

      {balanced ? (
        <>
          {windows.size > 1 ? <p className="small muted">{t('adminManagedModels.poolContextMismatch')}</p> : null}
          <RadioGroup
            label={t('adminManagedModels.poolPolicyLabel')}
            hint={draft.policy === 'context' ? t('adminManagedModels.poolPolicyContextHint') : undefined}
          >
            {POOL_POLICIES.map((p) => (
              <label key={p} className="switch">
                <input
                  type="radio"
                  name="pool-policy"
                  checked={draft.policy === p}
                  disabled={locked}
                  onChange={() => onChange({ ...draft, policy: p })}
                />
                <span>{policyLabel(p)}</span>
              </label>
            ))}
          </RadioGroup>
          <RadioGroup label={t('adminManagedModels.poolAffinityLabel')} hint={t('adminManagedModels.poolAffinityHint')}>
            {POOL_AFFINITIES.map((a) => (
              <label key={a} className="switch">
                <input
                  type="radio"
                  name="pool-affinity"
                  checked={draft.affinity === a}
                  onChange={() => onChange({ ...draft, affinity: a })}
                />
                <span>{affinityLabel(a)}</span>
              </label>
            ))}
          </RadioGroup>
          {draft.affinity === 'bounded' ? (
            <Field label={t('adminManagedModels.poolSpillLabel')} hint={t('adminManagedModels.poolSpillHint')}>
              <input
                className="input"
                type="number"
                min={0}
                max={400}
                style={{ width: 96 }}
                aria-label={t('adminManagedModels.poolSpillLabel')}
                value={draft.spill_pct}
                onChange={(e) => onChange({ ...draft, spill_pct: Math.max(0, Math.min(400, Number(e.target.value) || 0)) })}
              />
            </Field>
          ) : null}
        </>
      ) : null}
    </section>
  );
}

/** A labelled radio group (Field is a <label>, which must wrap one control). */
function RadioGroup({ label, hint, children }: { label: string; hint?: string; children: ReactNode }): ReactNode {
  return (
    <fieldset className="field" style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }}>
      <legend className="field-label">{label}</legend>
      <div className="stack" style={{ gap: 'var(--janus-space-1)' }}>
        {children}
      </div>
      {hint ? <span className="field-hint">{hint}</span> : null}
    </fieldset>
  );
}

const stateTone: Record<PoolMemberState, 'success' | 'warning' | 'danger' | 'neutral'> = {
  serving: 'success',
  ejected: 'danger',
  probe_failing: 'danger',
  disabled: 'neutral',
  unwatched: 'warning',
};

const stateLabel = (s: PoolMemberState): string =>
  ({
    serving: t('adminManagedModels.poolStateServing'),
    ejected: t('adminManagedModels.poolStateEjected'),
    probe_failing: t('adminManagedModels.poolStateProbeFailing'),
    disabled: t('adminManagedModels.poolStateDisabled'),
    unwatched: t('adminManagedModels.poolStateUnwatched'),
  })[s];

const pct = (v: number): string => `${Math.round(v * 100)}%`;

/** Live per-server state of a saved pool, polled while the dialog is open. */
export function PoolHealthPanel({ managedModelId }: { managedModelId: string }): ReactNode {
  const health = useQuery({
    queryKey: ['admin', 'managed-models', managedModelId, 'pool-health'],
    queryFn: () =>
      api.get<{ members: PoolMemberHealth[]; checked_at: string }>(`/api/v1/admin/managed-models/${managedModelId}/pool-health`),
    refetchInterval: 3_000,
  });
  const members = health.data?.members ?? [];
  if (members.length < 2) return null;
  const mixed = new Set(members.map((m) => m.load)).size > 1;
  return (
    <section className="card stack" aria-labelledby="pool-health-heading" data-testid="pool-health">
      <div>
        <h3 id="pool-health-heading" className="small" style={{ margin: 0, fontWeight: 600 }}>
          {t('adminManagedModels.poolHealthTitle')}
        </h3>
        <p className="small muted" style={{ margin: 0 }}>
          {t('adminManagedModels.poolHealthRefresh')}
        </p>
      </div>
      {mixed ? <p className="small muted">{t('adminManagedModels.poolMixedEngines')}</p> : null}
      <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0, gap: 'var(--janus-space-2)' }}>
        {members.map((m) => (
          <li key={m.model_id} className="stack" style={{ gap: 2 }}>
            <div className="row" style={{ gap: 'var(--janus-space-2)', flexWrap: 'wrap', alignItems: 'center' }}>
              <Badge tone={stateTone[m.state]} dot>
                {stateLabel(m.state)}
              </Badge>
              <span className="mono small">{m.public_name}</span>
              <span className="small muted">{m.upstream_name}</span>
              {m.engine ? <span className="small muted">· {m.engine}</span> : null}
            </div>
            <div className="small muted" title={m.probe_error || m.ejected_reason || undefined}>
              {t('adminManagedModels.poolHealthQueue', {
                count: formatNumber(Math.round(m.running + m.in_flight)),
                value: formatNumber(Math.round(m.waiting)),
              })}{' '}
              ({m.load === 'live' ? t('adminManagedModels.poolLoadLive') : t('adminManagedModels.poolLoadEstimated')})
              {m.context_capacity > 0
                ? ` · ${t('adminManagedModels.poolHealthContext', { percent: pct(m.context_used / m.context_capacity) })}`
                : ''}
              {m.prefix_hit_rate !== undefined
                ? ` · ${t('adminManagedModels.poolHealthCache', { percent: pct(m.prefix_hit_rate) })}`
                : ''}
              {m.ejected_until
                ? ` · ${t('adminManagedModels.poolHealthEjectedUntil', { when: formatRelative(m.ejected_until) })}`
                : ''}
              {m.probe_error && m.state !== 'ejected' ? ` · ${m.probe_error}` : ''}
            </div>
          </li>
        ))}
      </ul>
    </section>
  );
}
