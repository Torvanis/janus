import { sortCollection } from '../../lib/collections';
import { Collection } from '../../components/Collection';
import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { AdminUserRow, Model, QuotaStatus, RateLimitRule, Team } from '../../lib/types';
import { formatDateTime, formatNumber, formatUSD } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, Field, Modal, useToast } from '../../components/ui';
import { useUrlState } from '../../lib/hooks';
import { FilterSelect, SortHeader } from '../shared';
import { t } from '../../lib/i18n';
import { useLocalOnly } from '../../app/session';

interface QuotasResponse {
  quotas: QuotaStatus[];
  metrics: Array<{ value: string; label: string }>;
  windows: Array<{ value: string; label: string }>;
}

export function AdminQuotasPage(): ReactNode {
  // Local-only mode (JANUS_LOCAL_ONLY): the server already withholds the
  // Spend (USD) metric option, and this filter guarantees the create dialog
  // never offers a USD-based limit even against a stale/cached response.
  const localOnly = useLocalOnly();
  const queryClient = useQueryClient();
  const toast = useToast();
  const [subjectFilter, setSubjectFilter] = useUrlState('subject', '');
  const [sort, setSort] = useUrlState('sort', 'utilisation');
  const [createOpen, setCreateOpen] = useState(false);
  const [deleting, setDeleting] = useState<QuotaStatus | null>(null);

  const quotas = useQuery({
    queryKey: ['admin', 'quotas'],
    queryFn: () => api.get<QuotasResponse>('/api/v1/admin/quotas'),
    refetchInterval: 30_000,
  });

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/v1/admin/quotas/${id}`),
    onSuccess: () => {
      toast(t('adminQuotas.deletedToast'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'quotas'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const matched = (quotas.data?.quotas ?? []).filter((quota) => !subjectFilter || quota.subject_type === subjectFilter);
  // Utilisation descending is the default: the quota about to bite someone
  // should be the first row, not buried alphabetically.
  const filtered = sortCollection(
    matched,
    (quota) => {
      const key = sort.replace(/_asc$/, '');
      if (key === 'subject') return quota.subject_name;
      if (key === 'metric') return quota.metric;
      if (key === 'limit') return quota.limit_value;
      return quota.percent;
    },
    sort.endsWith('_asc'),
  );

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminQuotas.title')}</h1>
          <p className="page-subtitle">{t('adminQuotas.subtitle')}</p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
          {t('adminQuotas.newQuota')}
        </button>
      </header>

      <div className="banner banner-info">
        <div>
          <strong>{t('adminQuotas.headroomTitle')}</strong>
          <div className="small" style={{ marginTop: 4 }}>
            {t('adminQuotas.headroomBody')}
          </div>
        </div>
      </div>

      <div className="row wrap">
        <FilterSelect
          label={t('adminQuotas.subject')}
          value={subjectFilter}
          onChange={setSubjectFilter}
          options={[
            { value: '', label: t('adminQuotas.allSubjects') },
            { value: 'user', label: t('adminQuotas.people') },
            { value: 'team', label: t('adminQuotas.teams') },
          ]}
        />
      </div>

      <section className="card card-flush">
        <AsyncSection
          query={quotas}
          empty={{
            when: (data) => data.quotas.length === 0,
            title: t('adminQuotas.emptyTitle'),
            body: t('adminQuotas.emptyBody'),
            action: (
              <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
                {t('adminQuotas.createFirst')}
              </button>
            ),
          }}
        >
          {() => (
            <Collection
              name="Quotas"
              rows={filtered}
              rowKey={(quota) => quota.id}
              resetKey={sort + subjectFilter}
              columns={[
                { id: '0', label: '', value: (quota) => quota.subject_name, render: () => null },
                { id: '1', label: '', value: (quota) => quota.model_name, render: () => null },
                { id: '2', label: '', value: (quota) => quota.metric_label, render: () => null },
              ]}
            >
              {(visible) => (
                <div className="table-wrap">
                  <table className="data">
                    <thead>
                      <tr>
                        <SortHeader label={t('adminQuotas.subject')} sortKey="subject" active={sort} onSort={setSort} />
                        <th scope="col">{t('adminQuotas.colScope')}</th>
                        <SortHeader label={t('tables.metric')} sortKey="metric" active={sort} onSort={setSort} />
                        <SortHeader label={t('tables.limit')} sortKey="limit" active={sort} onSort={setSort} />
                        <SortHeader label={t('tables.utilisation')} sortKey="utilisation" active={sort} onSort={setSort} />
                        <th scope="col">{t('tables.resets')}</th>
                        <th scope="col">{t('tables.onBreach')}</th>
                        <th scope="col">
                          <span className="sr-only">{t('tables.actions')}</span>
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {visible.map((quota) => (
                        <tr key={quota.id}>
                          <td>
                            <div>{quota.subject_name || '—'}</div>
                            <div className="small muted">
                              {quota.subject_type === 'team' ? t('adminQuotas.team') : t('adminQuotas.person')}
                            </div>
                          </td>
                          <td className="small muted">{quota.model_name}</td>
                          <td className="small">
                            {quota.metric_label} <span className="muted">{quota.window_label}</span>
                            {quota.alert_thresholds?.length ? (
                              <div className="small muted">
                                {t('adminQuotas.alertsAt', { list: quota.alert_thresholds.map((v) => `${v}%`).join(' / ') })}
                              </div>
                            ) : null}
                          </td>
                          <td className="num small">
                            {quota.unit === 'usd' ? formatUSD(quota.limit_value) : formatNumber(quota.limit_value)}
                          </td>
                          <td style={{ minWidth: 140 }}>
                            <div className="row" style={{ gap: 8 }}>
                              <div className="meter" style={{ flex: 1 }}>
                                <div
                                  className="meter-fill"
                                  data-level={quota.percent >= 100 ? 'critical' : quota.percent >= 80 ? 'warning' : undefined}
                                  style={{ width: `${Math.min(100, Math.max(2, quota.percent))}%` }}
                                />
                              </div>
                              <span className="small num muted">{Math.round(quota.percent)}%</span>
                            </div>
                          </td>
                          <td className="small muted">{formatDateTime(quota.reset_at)}</td>
                          <td>
                            {quota.breach_behavior === 'hard_kill' ? (
                              <Badge tone="danger">{t('team.cutStreams')}</Badge>
                            ) : (
                              <Badge tone="neutral">{t('team.letFinish')}</Badge>
                            )}
                          </td>
                          <td style={{ textAlign: 'right' }}>
                            <button type="button" className="btn btn-ghost btn-sm" onClick={() => setDeleting(quota)}>
                              {t('tables.delete')}
                            </button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </Collection>
          )}
        </AsyncSection>
      </section>

      <RateLimitsSection />

      <CreateQuotaModal
        open={createOpen}
        metrics={(quotas.data?.metrics ?? []).filter((option) => !localOnly || option.value !== 'cost_usd')}
        windows={quotas.data?.windows ?? []}
        onClose={() => setCreateOpen(false)}
        onCreated={() => {
          setCreateOpen(false);
          void queryClient.invalidateQueries({ queryKey: ['admin', 'quotas'] });
        }}
      />

      <ConfirmDialog
        open={Boolean(deleting)}
        onClose={() => setDeleting(null)}
        onConfirm={() => {
          if (deleting) remove.mutate(deleting.id);
          setDeleting(null);
        }}
        title={t('adminQuotas.deleteTitle')}
        consequence={t('adminQuotas.deleteConsequence', {
          name: deleting?.subject_name || t('adminQuotas.deleteFallbackSubject'),
          metric: deleting?.metric_label ?? '',
        })}
        confirmLabel={t('adminQuotas.deleteConfirm')}
        busy={remove.isPending}
      />
    </div>
  );
}

interface RateLimitsResponse {
  rules: RateLimitRule[];
  enforcement_enabled: boolean;
}

function RateLimitsSection(): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [createOpen, setCreateOpen] = useState(false);
  const [deleting, setDeleting] = useState<RateLimitRule | null>(null);

  const limits = useQuery({
    queryKey: ['admin', 'rate-limits'],
    queryFn: () => api.get<RateLimitsResponse>('/api/v1/admin/rate-limits'),
    refetchInterval: 30_000,
  });

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/v1/admin/rate-limits/${id}`),
    onSuccess: () => {
      toast(t('adminQuotas.rlDeletedToast'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'rate-limits'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const [editing, setEditing] = useState<RateLimitRule | null>(null);
  const [editValue, setEditValue] = useState('');

  const update = useMutation({
    mutationFn: (input: { id: string; requests_per_minute: number }) =>
      api.put(`/api/v1/admin/rate-limits/${input.id}`, { requests_per_minute: input.requests_per_minute }),
    onSuccess: () => {
      toast(t('adminQuotas.rlUpdatedToast'));
      setEditing(null);
      void queryClient.invalidateQueries({ queryKey: ['admin', 'rate-limits'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const saveEdit = (): void => {
    const value = Number(editValue);
    if (!editing) return;
    if (!Number.isInteger(value) || value < 1 || value > 1_000_000) {
      toast(t('adminQuotas.rlEditRangeError'), 'danger');
      return;
    }
    update.mutate({ id: editing.id, requests_per_minute: value });
  };

  return (
    <>
      <header className="page-header" style={{ marginTop: 'var(--janus-space-6, 24px)' }}>
        <div>
          <h2 className="page-title" style={{ fontSize: '1.25rem' }}>
            {t('adminQuotas.rateLimits')}
          </h2>
          <p className="page-subtitle">{t('adminQuotas.rateLimitsSubtitle')}</p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
          {t('adminQuotas.newRateLimit')}
        </button>
      </header>

      {limits.data && !limits.data.enforcement_enabled && limits.data.rules.length > 0 ? (
        <div className="banner banner-warning">
          <div className="small">
            <strong>{t('adminQuotas.enforcementOffTitle')}</strong> {t('adminQuotas.enforcementOffBody')}
          </div>
        </div>
      ) : null}

      <section className="card card-flush">
        <AsyncSection
          query={limits}
          empty={{
            when: (data) => data.rules.length === 0,
            title: t('adminQuotas.rlEmptyTitle'),
            body: t('adminQuotas.rlEmptyBody'),
            action: (
              <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
                {t('adminQuotas.rlCreateFirst')}
              </button>
            ),
          }}
        >
          {(data) => (
            <Collection
              name="Rate limits"
              rows={data.rules}
              rowKey={(rule) => rule.id}
              columns={[
                {
                  id: 'subject',
                  label: t('adminQuotas.appliesTo'),
                  value: (rule) => rule.subject_name || rule.subject_id,
                  render: (rule) => <>{rule.subject_id ? rule.subject_name || rule.subject_id : t('adminQuotas.everyPerson')}</>,
                },
                {
                  id: 'endpoint',
                  label: t('adminQuotas.colEndpoint'),
                  value: (rule) => rule.endpoint,
                  render: (rule) => <>{rule.endpoint}</>,
                },
                {
                  id: 'rpm',
                  label: t('adminQuotas.colRpm'),
                  value: (rule) => rule.requests_per_minute,
                  render: (rule) => (
                    <>
                      {editing?.id === rule.id ? (
                        <input
                          type="number"
                          min={1}
                          max={1_000_000}
                          value={editValue}
                          aria-label={t('adminQuotas.rpmAriaLabel')}
                          style={{ width: 110 }}
                          onChange={(event) => setEditValue(event.target.value)}
                          onKeyDown={(event) => {
                            if (event.key === 'Enter') saveEdit();
                            if (event.key === 'Escape') setEditing(null);
                          }}
                        />
                      ) : (
                        formatNumber(rule.requests_per_minute)
                      )}
                    </>
                  ),
                },
                {
                  id: 'actions',
                  label: t('tables.actions'),
                  render: (rule) => (
                    <>
                      {editing?.id === rule.id ? (
                        <>
                          <button type="button" className="btn btn-primary btn-sm" disabled={update.isPending} onClick={saveEdit}>
                            {t('adminQuotas.save')}
                          </button>{' '}
                          <button type="button" className="btn btn-ghost btn-sm" onClick={() => setEditing(null)}>
                            {t('common.cancel')}
                          </button>
                        </>
                      ) : (
                        <>
                          <button
                            type="button"
                            className="btn btn-ghost btn-sm"
                            onClick={() => {
                              setEditing(rule);
                              setEditValue(String(rule.requests_per_minute));
                            }}
                          >
                            {t('adminQuotas.edit')}
                          </button>{' '}
                          <button type="button" className="btn btn-ghost btn-sm" onClick={() => setDeleting(rule)}>
                            {t('tables.delete')}
                          </button>
                        </>
                      )}
                    </>
                  ),
                },
              ]}
            />
          )}
        </AsyncSection>
      </section>

      <CreateRateLimitModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onCreated={() => {
          setCreateOpen(false);
          void queryClient.invalidateQueries({ queryKey: ['admin', 'rate-limits'] });
        }}
      />

      <ConfirmDialog
        open={Boolean(deleting)}
        onClose={() => setDeleting(null)}
        onConfirm={() => {
          if (deleting) remove.mutate(deleting.id);
          setDeleting(null);
        }}
        title={t('adminQuotas.rlDeleteTitle')}
        consequence={t('adminQuotas.rlDeleteConsequence', {
          name: deleting?.endpoint ?? t('adminQuotas.rlDeleteFallbackEndpoint'),
        })}
        confirmLabel={t('adminQuotas.rlDeleteConfirm')}
        busy={remove.isPending}
      />
    </>
  );
}

function CreateRateLimitModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: () => void;
}): ReactNode {
  const toast = useToast();
  const [subjectId, setSubjectId] = useState('');
  const [endpoint, setEndpoint] = useState('*');
  const [rpm, setRpm] = useState('60');

  const users = useQuery({
    queryKey: ['admin', 'users', 'for-rate-limits'],
    queryFn: () => api.get<{ users: AdminUserRow[] }>('/api/v1/admin/users?limit=200'),
    enabled: open,
  });

  const create = useMutation({
    mutationFn: () =>
      api.post('/api/v1/admin/rate-limits', {
        subject_id: subjectId,
        endpoint,
        requests_per_minute: Number.parseInt(rpm, 10),
      }),
    onSuccess: () => {
      toast(t('adminQuotas.rlCreatedToast'));
      onCreated();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const numericRpm = Number.parseInt(rpm, 10);
  const rpmError = rpm !== '' && (Number.isNaN(numericRpm) || numericRpm < 1) ? t('adminQuotas.rlRpmError') : undefined;
  const endpointError = endpoint !== '*' && !endpoint.startsWith('/v1/') ? t('adminQuotas.rlEndpointError') : undefined;
  const valid = rpm !== '' && !rpmError && !endpointError;

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={t('adminQuotas.rlCreateTitle')}
      description={t('adminQuotas.rlCreateDescription')}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={create.isPending}>
            {t('common.cancel')}
          </button>
          <button type="button" className="btn btn-primary" onClick={() => create.mutate()} disabled={!valid || create.isPending}>
            {create.isPending ? t('adminQuotas.creating') : t('adminQuotas.rlCreateConfirm')}
          </button>
        </>
      }
    >
      <Field label={t('adminQuotas.appliesTo')} hint={t('adminQuotas.rlAppliesToHint')}>
        <select className="select" value={subjectId} onChange={(event) => setSubjectId(event.target.value)}>
          <option value="">{t('adminQuotas.everyPerson')}</option>
          {(users.data?.users ?? []).map((user) => (
            <option key={user.id} value={user.id}>
              {user.name || user.email}
            </option>
          ))}
        </select>
      </Field>

      <Field label={t('adminQuotas.colEndpoint')} required error={endpointError} hint={t('adminQuotas.rlEndpointHint')}>
        <input
          className="input mono"
          value={endpoint}
          onChange={(event) => setEndpoint(event.target.value.trim())}
          aria-invalid={Boolean(endpointError)}
        />
      </Field>

      <Field label={t('adminQuotas.rlRpmLabel')} required error={rpmError} hint={t('adminQuotas.rlRpmHint')}>
        <input
          className="input"
          type="number"
          min="1"
          step="1"
          value={rpm}
          onChange={(event) => setRpm(event.target.value)}
          aria-invalid={Boolean(rpmError)}
        />
      </Field>
    </Modal>
  );
}

function CreateQuotaModal({
  open,
  metrics,
  windows,
  onClose,
  onCreated,
}: {
  open: boolean;
  metrics: Array<{ value: string; label: string }>;
  windows: Array<{ value: string; label: string }>;
  onClose: () => void;
  onCreated: () => void;
}): ReactNode {
  const toast = useToast();
  const [subjectType, setSubjectType] = useState<'user' | 'team'>('user');
  const [subjectId, setSubjectId] = useState('');
  const [modelId, setModelId] = useState('');
  const [metric, setMetric] = useState('tokens_in');
  const [limit, setLimit] = useState('1000000');
  const [window, setWindow] = useState('daily');
  const [behavior, setBehavior] = useState<'let_finish' | 'hard_kill'>('let_finish');
  const [thresholds, setThresholds] = useState('80, 95');

  const users = useQuery({
    queryKey: ['admin', 'users', 'for-quotas'],
    queryFn: () => api.get<{ users: AdminUserRow[] }>('/api/v1/admin/users?limit=200'),
    enabled: open,
  });
  const teams = useQuery({
    queryKey: ['admin', 'teams'],
    queryFn: () => api.get<{ teams: Team[] }>('/api/v1/admin/teams'),
    enabled: open,
  });
  const models = useQuery({
    queryKey: ['admin', 'models', 'for-quotas'],
    queryFn: () => api.get<{ models: Model[] }>('/api/v1/admin/models?status=enabled'),
    enabled: open,
  });

  // parse "50, 90" into sorted unique whole percentages (1–100).
  // Returns null when invalid; [] when the input matches the 80/95 defaults
  // (sent as an empty list so the server keeps the rule on the defaults).
  const parsedThresholds = ((): number[] | null => {
    const trimmed = thresholds.trim();
    if (!trimmed) return [];
    const values = trimmed
      .split(',')
      .map((part) => part.trim())
      .filter(Boolean)
      .map(Number);
    if (values.length === 0 || values.some((v) => !Number.isInteger(v) || v < 1 || v > 100)) return null;
    const unique = [...new Set(values)].sort((a, b) => a - b);
    if (unique.length === 2 && unique[0] === 80 && unique[1] === 95) return [];
    return unique;
  })();

  const create = useMutation({
    mutationFn: () =>
      api.post('/api/v1/admin/quotas', {
        subject_type: subjectType,
        subject_id: subjectId,
        model_id: modelId,
        metric,
        limit_value: Number.parseFloat(limit),
        window,
        breach_behavior: behavior,
        alert_thresholds: parsedThresholds ?? [],
      }),
    onSuccess: () => {
      toast(t('adminQuotas.createdToast'));
      onCreated();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const numericLimit = Number.parseFloat(limit);
  const limitError = limit !== '' && (Number.isNaN(numericLimit) || numericLimit <= 0) ? t('adminQuotas.limitError') : undefined;
  const thresholdsError = parsedThresholds === null ? t('adminQuotas.alertThresholdsError') : undefined;
  const valid = Boolean(subjectId) && !limitError && limit !== '' && !thresholdsError;

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={t('adminQuotas.createTitle')}
      description={t('adminQuotas.createDescription')}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={create.isPending}>
            {t('common.cancel')}
          </button>
          <button type="button" className="btn btn-primary" onClick={() => create.mutate()} disabled={!valid || create.isPending}>
            {create.isPending ? t('adminQuotas.creating') : t('adminQuotas.createQuota')}
          </button>
        </>
      }
    >
      <Field label={t('adminQuotas.appliesTo')} required>
        <select
          className="select"
          value={subjectType}
          onChange={(event) => {
            setSubjectType(event.target.value as typeof subjectType);
            setSubjectId('');
          }}
        >
          <option value="user">{t('adminQuotas.aPerson')}</option>
          <option value="team">{t('adminQuotas.aTeam')}</option>
        </select>
      </Field>

      <Field label={subjectType === 'user' ? t('adminQuotas.person') : t('adminQuotas.team')} required>
        <select className="select" value={subjectId} onChange={(event) => setSubjectId(event.target.value)}>
          <option value="">{t('adminQuotas.choose')}</option>
          {subjectType === 'user'
            ? (users.data?.users ?? []).map((user) => (
                <option key={user.id} value={user.id}>
                  {user.name || user.email}
                </option>
              ))
            : (teams.data?.teams ?? []).map((team) => (
                <option key={team.id} value={team.id}>
                  {team.name}
                </option>
              ))}
        </select>
      </Field>

      <Field label={t('adminQuotas.modelScope')} hint={t('adminQuotas.modelScopeHint')}>
        <select className="select" value={modelId} onChange={(event) => setModelId(event.target.value)}>
          <option value="">{t('adminQuotas.allModels')}</option>
          {(models.data?.models ?? []).map((model) => (
            <option key={model.id} value={model.id}>
              {model.name} · {model.upstream_name}
            </option>
          ))}
        </select>
      </Field>

      <Field label={t('tables.metric')} required>
        <select className="select" value={metric} onChange={(event) => setMetric(event.target.value)}>
          {metrics.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </select>
      </Field>

      <Field
        label={metric === 'cost_usd' ? t('adminQuotas.limitUsd') : t('tables.limit')}
        required
        error={limitError}
        hint={metric === 'cost_usd' ? t('adminQuotas.limitUsdHint') : t('adminQuotas.limitHint')}
      >
        <input
          className="input"
          type="number"
          min="0"
          step={metric === 'cost_usd' ? '0.01' : '1'}
          value={limit}
          onChange={(event) => setLimit(event.target.value)}
          aria-invalid={Boolean(limitError)}
        />
      </Field>

      <Field label={t('tables.window')} required>
        <select className="select" value={window} onChange={(event) => setWindow(event.target.value)}>
          {windows.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </select>
      </Field>

      <Field label={t('adminQuotas.alertThresholds')} error={thresholdsError} hint={t('adminQuotas.alertThresholdsHint')}>
        <input
          className="input"
          value={thresholds}
          onChange={(event) => setThresholds(event.target.value)}
          aria-invalid={Boolean(thresholdsError)}
        />
      </Field>

      <Field label={t('adminQuotas.whenCrossed')}>
        <select className="select" value={behavior} onChange={(event) => setBehavior(event.target.value as typeof behavior)}>
          <option value="let_finish">{t('adminQuotas.letFinishOption')}</option>
          <option value="hard_kill">{t('adminQuotas.hardKillOption')}</option>
        </select>
      </Field>

      {behavior === 'hard_kill' ? (
        <div className="banner banner-warning">
          <div className="small">{t('adminQuotas.hardKillWarning')}</div>
        </div>
      ) : null}
    </Modal>
  );
}
