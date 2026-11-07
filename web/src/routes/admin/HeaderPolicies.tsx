import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { AsyncSection, Badge, ConfirmDialog, Drawer, Field, useToast } from '../../components/ui';
import { IconButton, RowActions } from '../../components/IconButton';

/**
 * Header policies: what the caller's request headers look like by the time
 * they reach each upstream provider. A policy is a named, ordered rule set;
 * each upstream and each personal-subscription provider either has its own
 * policy or inherits the default. The adapter's own headers (credentials,
 * provider identity, signatures) are added after the policy and always win.
 */

export type HeaderAction = 'remove' | 'set' | 'append';

export interface HeaderRule {
  action: HeaderAction;
  name: string;
  value?: string;
  secret?: boolean;
  has_value?: boolean;
  keep_value?: boolean;
}

export interface HeaderPolicy {
  id: string;
  name: string;
  description: string;
  allow_only: string[];
  rules: HeaderRule[];
  assignments: { target_type: string; target_id: string; policy_id: string }[];
  updated_at: string;
}

export interface HeaderTarget {
  target_type: 'default' | 'upstream' | 'personal_subscription';
  target_id: string;
  name: string;
  detail?: string;
  policy_id: string;
  effective_policy_id: string;
}

export type HeaderLogMode = 'values' | 'names' | 'off';

export interface HeaderPoliciesResponse {
  policies: HeaderPolicy[];
  targets: HeaderTarget[];
  settings: { mode: HeaderLogMode };
  edge_headers: string[];
}

const ACTION_LABEL: Record<HeaderAction, string> = { remove: 'Remove', set: 'Set', append: 'Append' };

const LOG_MODES: { mode: HeaderLogMode; label: string; hint: string }[] = [
  { mode: 'values', label: 'Names and values', hint: 'Credentials, cookies, tokens and secret rule values are always masked.' },
  { mode: 'names', label: 'Names only', hint: 'Which headers were present, without their values.' },
  { mode: 'off', label: 'Off', hint: 'No header record is kept with requests.' },
];

/** One-line description of a policy for tables and the assignment picker. */
export function summarizePolicy(p: Pick<HeaderPolicy, 'allow_only' | 'rules'>): string {
  const parts: string[] = [];
  if (p.allow_only.length > 0) parts.push(`allows only ${p.allow_only.length} header${p.allow_only.length === 1 ? '' : 's'}`);
  const counts = { remove: 0, set: 0, append: 0 };
  for (const r of p.rules) counts[r.action] += 1;
  if (counts.remove) parts.push(`removes ${counts.remove}`);
  if (counts.set) parts.push(`sets ${counts.set}`);
  if (counts.append) parts.push(`appends ${counts.append}`);
  return parts.length > 0 ? parts.join(', ') : 'no rules (passes headers through)';
}

const TARGET_KIND: Record<HeaderTarget['target_type'], string> = {
  default: 'Default',
  upstream: 'Upstream',
  personal_subscription: 'Personal subscription',
};

/** Header policies: the Header policies tab of Policy rules. */
export function HeaderPoliciesPanel(): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [editing, setEditing] = useState<HeaderPolicy | 'new' | null>(null);
  const [deleting, setDeleting] = useState<HeaderPolicy | null>(null);

  const data = useQuery({
    queryKey: ['admin', 'header-policies'],
    queryFn: () => api.get<HeaderPoliciesResponse>('/api/v1/admin/header-policies'),
  });
  const refresh = () => void queryClient.invalidateQueries({ queryKey: ['admin', 'header-policies'] });

  const assign = useMutation({
    mutationFn: (body: { target_type: string; target_id: string; policy_id: string }) =>
      api.put('/api/v1/admin/header-policies/assignments', body),
    onSuccess: () => {
      toast('Assignment saved');
      refresh();
    },
    onError: (e: Error) => toast(e.message, 'danger'),
  });
  const setMode = useMutation({
    mutationFn: (mode: HeaderLogMode) => api.put('/api/v1/admin/header-policies/settings', { mode }),
    onSuccess: () => {
      toast('Header logging updated');
      refresh();
    },
    onError: (e: Error) => toast(e.message, 'danger'),
  });
  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/v1/admin/header-policies/${id}`),
    onSuccess: () => {
      setDeleting(null);
      toast('Policy deleted');
      refresh();
    },
    onError: (e: Error) => toast(e.message, 'danger'),
  });

  return (
    <div className="stack">
      <div className="row" style={{ justifyContent: 'space-between', alignItems: 'flex-start', gap: 'var(--janus-space-3)' }}>
        <p className="small muted" style={{ margin: 0, maxWidth: '72ch' }}>
          Control which of the caller&apos;s request headers reach each provider: remove, set or add headers per upstream
          and per personal-subscription provider. Credentials and provider identity headers are always set by Janus and
          cannot be changed here.
        </p>
        <button type="button" className="btn btn-primary" onClick={() => setEditing('new')}>
          New policy
        </button>
      </div>

      <AsyncSection query={data}>
        {(d) => {
          const byId = new Map(d.policies.map((p) => [p.id, p]));
          return (
            <>
              <section className="card card-flush" aria-labelledby="hp-assign">
                <div className="card-header" style={{ padding: 'var(--janus-space-4)' }}>
                  <h2 id="hp-assign" style={{ margin: 0 }}>
                    Where policies apply
                  </h2>
                  <p className="small muted" style={{ margin: 0 }}>
                    An upstream or provider with its own policy uses that policy instead of the default.
                  </p>
                </div>
                <div className="table-wrap">
                  <table className="data">
                    <thead>
                      <tr>
                        <th scope="col">Target</th>
                        <th scope="col">Kind</th>
                        <th scope="col">Policy</th>
                        <th scope="col">In effect</th>
                      </tr>
                    </thead>
                    <tbody>
                      {d.targets.map((target) => {
                        const effective = byId.get(target.effective_policy_id);
                        const key = `${target.target_type}:${target.target_id}`;
                        return (
                          <tr key={key}>
                            <th scope="row">
                              <div>{target.name}</div>
                              {target.detail ? <div className="small muted truncate" style={{ maxWidth: 320 }}>{target.detail}</div> : null}
                            </th>
                            <td className="small">{TARGET_KIND[target.target_type]}</td>
                            <td>
                              <select
                                className="select"
                                aria-label={`Policy for ${target.name}`}
                                value={target.policy_id}
                                disabled={assign.isPending}
                                onChange={(e) =>
                                  assign.mutate({ target_type: target.target_type, target_id: target.target_id, policy_id: e.target.value })
                                }
                              >
                                <option value="">{target.target_type === 'default' ? 'None (pass headers through)' : 'Inherit default'}</option>
                                {d.policies.map((p) => (
                                  <option key={p.id} value={p.id}>
                                    {p.name}
                                  </option>
                                ))}
                              </select>
                            </td>
                            <td className="small">
                              {effective ? (
                                <>
                                  <Badge tone={target.policy_id ? 'info' : 'neutral'}>{effective.name}</Badge>{' '}
                                  <span className="muted">{summarizePolicy(effective)}</span>
                                </>
                              ) : (
                                <span className="muted">Caller headers forwarded unchanged</span>
                              )}
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              </section>

              <section className="card card-flush" aria-labelledby="hp-policies">
                <div className="card-header" style={{ padding: 'var(--janus-space-4)' }}>
                  <h2 id="hp-policies" style={{ margin: 0 }}>
                    Policies
                  </h2>
                </div>
                <div className="table-wrap">
                  <table className="data">
                    <thead>
                      <tr>
                        <th scope="col">Name</th>
                        <th scope="col">Rules</th>
                        <th scope="col">Used by</th>
                        <th scope="col">
                          <span className="sr-only">Actions</span>
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {d.policies.length === 0 ? (
                        <tr>
                          <td colSpan={4} className="muted small">
                            No policies yet. Caller headers are forwarded unchanged.
                          </td>
                        </tr>
                      ) : null}
                      {d.policies.map((p) => (
                        <tr key={p.id}>
                          <th scope="row">
                            <button type="button" className="btn btn-ghost btn-sm" style={{ padding: 0, minHeight: 'auto' }} onClick={() => setEditing(p)}>
                              {p.name}
                            </button>
                          </th>
                          <td className="small">{summarizePolicy(p)}</td>
                          <td className="small">
                            {p.assignments.length === 0 ? (
                              <span className="muted">Not assigned</span>
                            ) : (
                              p.assignments
                                .map((a) => d.targets.find((t) => t.target_type === a.target_type && t.target_id === a.target_id)?.name ?? a.target_id)
                                .join(', ')
                            )}
                          </td>
                          <td>
                            <RowActions>
                              <IconButton icon="edit" label={`Edit ${p.name}`} onClick={() => setEditing(p)} />
                              <IconButton icon="delete" label={`Delete ${p.name}`} danger onClick={() => setDeleting(p)} />
                            </RowActions>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </section>

              <section className="card" aria-labelledby="hp-logging">
                <h2 id="hp-logging" style={{ marginTop: 0 }}>
                  Header logging
                </h2>
                <p className="small muted">
                  Each request in the request log keeps the headers the caller sent, the headers the provider received, and what
                  the policy changed. Records follow the request log&apos;s retention.
                </p>
                <div className="segmented" role="radiogroup" aria-label="Header logging">
                  {LOG_MODES.map((m) => (
                    <button
                      key={m.mode}
                      type="button"
                      role="radio"
                      aria-checked={d.settings.mode === m.mode}
                      aria-pressed={d.settings.mode === m.mode}
                      disabled={setMode.isPending}
                      onClick={() => setMode.mutate(m.mode)}
                    >
                      {m.label}
                    </button>
                  ))}
                </div>
                <p className="small muted">{LOG_MODES.find((m) => m.mode === d.settings.mode)?.hint}</p>
              </section>

              <PolicyDrawer
                value={editing}
                edgeHeaders={d.edge_headers}
                onClose={() => setEditing(null)}
                onSaved={() => {
                  setEditing(null);
                  refresh();
                }}
              />
            </>
          );
        }}
      </AsyncSection>

      <ConfirmDialog
        open={Boolean(deleting)}
        onClose={() => setDeleting(null)}
        onConfirm={() => deleting && remove.mutate(deleting.id)}
        busy={remove.isPending}
        title={`Delete ${deleting?.name ?? 'policy'}?`}
        consequence={
          deleting && deleting.assignments.length > 0
            ? `It is assigned to ${deleting.assignments.length} target${deleting.assignments.length === 1 ? '' : 's'}; they fall back to the default policy.`
            : 'It is not assigned anywhere.'
        }
        confirmLabel="Delete policy"
      />
    </div>
  );
}

interface DraftRule extends HeaderRule {
  key: number;
}

let ruleKey = 0;
const draftOf = (r: HeaderRule): DraftRule => ({ ...r, key: ++ruleKey, keep_value: r.secret && r.has_value ? true : undefined });

function PolicyDrawer({
  value,
  edgeHeaders,
  onClose,
  onSaved,
}: {
  value: HeaderPolicy | 'new' | null;
  edgeHeaders: string[];
  onClose: () => void;
  onSaved: () => void;
}): ReactNode {
  const toast = useToast();
  const isNew = value === 'new';
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [allowOnly, setAllowOnly] = useState('');
  const [rules, setRules] = useState<DraftRule[]>([]);

  useEffect(() => {
    if (value === null) return;
    if (value === 'new') {
      setName('');
      setDescription('');
      setAllowOnly('');
      setRules([]);
      return;
    }
    setName(value.name);
    setDescription(value.description);
    setAllowOnly(value.allow_only.join('\n'));
    setRules(value.rules.map(draftOf));
  }, [value]);

  const save = useMutation({
    mutationFn: () => {
      const body = {
        name,
        description,
        allow_only: allowOnly
          .split(/[\n,]/)
          .map((s) => s.trim())
          .filter(Boolean),
        rules: rules.map(({ key: _key, has_value: _has, ...r }) => ({
          ...r,
          value: r.action === 'remove' ? undefined : r.value ?? '',
          keep_value: r.secret && !r.value ? r.keep_value : undefined,
        })),
      };
      return isNew
        ? api.post('/api/v1/admin/header-policies', body)
        : api.put(`/api/v1/admin/header-policies/${(value as HeaderPolicy).id}`, body);
    },
    onSuccess: () => {
      toast(isNew ? 'Policy created' : 'Policy saved');
      onSaved();
    },
    onError: (e: Error) => toast(e.message, 'danger'),
  });

  const update = (key: number, patch: Partial<DraftRule>) => setRules((rs) => rs.map((r) => (r.key === key ? { ...r, ...patch } : r)));
  const move = (index: number, delta: number) =>
    setRules((rs) => {
      const next = [...rs];
      const [item] = next.splice(index, 1);
      if (!item) return rs;
      next.splice(index + delta, 0, item);
      return next;
    });
  const missingEdge = useMemo(
    () => edgeHeaders.filter((h) => !rules.some((r) => r.action === 'remove' && r.name.toLowerCase() === h)),
    [edgeHeaders, rules],
  );

  return (
    <Drawer
      open={value !== null}
      onClose={onClose}
      title={isNew ? 'New header policy' : `Edit ${name || 'policy'}`}
      description="Rules run top to bottom on the caller's headers, before Janus adds the provider's credentials."
      wide
      footer={
        <div className="row" style={{ justifyContent: 'flex-end', gap: 'var(--janus-space-2)' }}>
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="button" className="btn btn-primary" disabled={save.isPending || !name.trim()} onClick={() => save.mutate()}>
            {save.isPending ? 'Saving…' : 'Save policy'}
          </button>
        </div>
      }
    >
      <div className="stack">
        <Field label="Name" required>
          <input className="input" aria-label="Name" value={name} onChange={(e) => setName(e.target.value)} maxLength={120} />
        </Field>
        <Field label="Description">
          <textarea className="textarea" aria-label="Description" rows={2} value={description} onChange={(e) => setDescription(e.target.value)} />
        </Field>
        <Field
          label="Allow only these caller headers"
          hint="Optional. One name per line; a name ending in * matches a prefix (x-trace-*). Leave empty to keep every header the rules below do not remove."
        >
          <textarea
            className="textarea mono"
            aria-label="Allow only these caller headers"
            rows={3}
            value={allowOnly}
            placeholder={'content-type\naccept\nx-request-id'}
            onChange={(e) => setAllowOnly(e.target.value)}
          />
        </Field>

        <div>
          <div className="row" style={{ justifyContent: 'space-between', alignItems: 'center' }}>
            <h3 style={{ margin: 0 }}>Rules</h3>
            <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
              {missingEdge.length > 0 ? (
                <button
                  type="button"
                  className="btn btn-sm"
                  onClick={() => setRules((rs) => [...rs, ...missingEdge.map((n) => draftOf({ action: 'remove', name: n }))])}
                >
                  Add edge &amp; proxy headers
                </button>
              ) : null}
              <button type="button" className="btn btn-sm" onClick={() => setRules((rs) => [...rs, draftOf({ action: 'set', name: '', value: '' })])}>
                Add rule
              </button>
            </div>
          </div>
          {rules.length === 0 ? <p className="small muted">No rules: headers pass through (subject to the allow-list).</p> : null}
          <ol className="stack" style={{ listStyle: 'none', padding: 0, margin: 'var(--janus-space-3) 0 0' }} aria-label="Rules">
            {rules.map((r, i) => (
              <li key={r.key} className="row wrap" style={{ gap: 'var(--janus-space-2)', alignItems: 'center' }}>
                <span className="small muted" style={{ width: '2ch', textAlign: 'right' }}>
                  {i + 1}
                </span>
                <select
                  className="select"
                  style={{ width: 'auto' }}
                  aria-label={`Rule ${i + 1} action`}
                  value={r.action}
                  onChange={(e) => update(r.key, { action: e.target.value as HeaderAction })}
                >
                  {(Object.keys(ACTION_LABEL) as HeaderAction[]).map((a) => (
                    <option key={a} value={a}>
                      {ACTION_LABEL[a]}
                    </option>
                  ))}
                </select>
                <input
                  className="input mono"
                  style={{ flex: '1 1 180px', width: 'auto' }}
                  aria-label={`Rule ${i + 1} header`}
                  placeholder={r.action === 'remove' ? 'header-name or prefix-*' : 'Header-Name'}
                  value={r.name}
                  onChange={(e) => update(r.key, { name: e.target.value })}
                />
                {r.action !== 'remove' ? (
                  <>
                    <input
                      className="input mono"
                      style={{ flex: '2 1 200px', width: 'auto' }}
                      aria-label={`Rule ${i + 1} value`}
                      type={r.secret ? 'password' : 'text'}
                      autoComplete="off"
                      placeholder={r.secret && r.keep_value ? 'Saved — leave empty to keep' : 'value'}
                      value={r.value ?? ''}
                      onChange={(e) => update(r.key, { value: e.target.value })}
                    />
                    <label className="row small" style={{ gap: 4 }}>
                      <input type="checkbox" checked={Boolean(r.secret)} onChange={(e) => update(r.key, { secret: e.target.checked, keep_value: false })} />
                      Secret
                    </label>
                  </>
                ) : null}
                <RowActions>
                  <IconButton icon="up" label={`Move rule ${i + 1} up`} disabled={i === 0} onClick={() => move(i, -1)} />
                  <IconButton icon="down" label={`Move rule ${i + 1} down`} disabled={i === rules.length - 1} onClick={() => move(i, 1)} />
                  <IconButton icon="delete" label={`Delete rule ${i + 1}`} danger onClick={() => setRules((rs) => rs.filter((x) => x.key !== r.key))} />
                </RowActions>
              </li>
            ))}
          </ol>
        </div>
      </div>
    </Drawer>
  );
}
