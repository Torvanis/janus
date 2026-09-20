import { Collection } from '../../components/Collection';
import { useMemo, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, qs } from '../../lib/api';
import {
  ALL_FALLBACK_TRIGGERS,
  DEFAULT_FALLBACK_TRIGGERS,
  publicModelName,
  type FallbackTrigger,
  type ManagedModelRow,
  type Model,
} from '../../lib/types';
import { formatNumber, formatRelative, formatUSD } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, EmptyState, Field, Modal, useToast } from '../../components/ui';
import { useDebounced, useUrlState, useUrlStateBatch } from '../../lib/hooks';
import { FilterSelect, SearchInput } from '../shared';
import { useLicensed, useLocalOnly } from '../../app/session';
import { t } from '../../lib/i18n';

type ManagedSortField = 'name' | 'created' | 'spend' | 'requests' | 'tokens' | 'users' | 'grants';

const SORT_FIELDS: ManagedSortField[] = ['name', 'created', 'spend', 'requests', 'tokens', 'users', 'grants'];

function parseSort(sort: string): { field: ManagedSortField; dir: 'asc' | 'desc' } {
  const dir = sort.endsWith('_asc') ? 'asc' : 'desc';
  const field = sort.replace(/_(asc|desc)$/, '') as ManagedSortField;
  if (!SORT_FIELDS.includes(field)) return { field: 'spend', dir: 'desc' };
  return { field, dir };
}

function SortableColumn({
  label,
  field,
  sort,
  onSort,
  numeric,
}: {
  label: string;
  field: ManagedSortField;
  sort: string;
  onSort: (next: string) => void;
  numeric?: boolean;
}): ReactNode {
  const current = parseSort(sort);
  const active = current.field === field;
  return (
    <th
      scope="col"
      className={numeric ? 'num' : undefined}
      aria-sort={active ? (current.dir === 'asc' ? 'ascending' : 'descending') : 'none'}
    >
      <button type="button" onClick={() => onSort(`${field}_${active && current.dir === 'desc' ? 'asc' : 'desc'}`)}>
        {label}
        <span aria-hidden="true" style={{ opacity: active ? 1 : 0.3 }}>
          {active && current.dir === 'asc' ? '↑' : '↓'}
        </span>
      </button>
    </th>
  );
}

export function ManagedModelsPage(): ReactNode {
  const localOnly = useLocalOnly();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const [editing, setEditing] = useState<ManagedModelRow | null>(null);
  const [deleting, setDeleting] = useState<ManagedModelRow | null>(null);
  const [toggling, setToggling] = useState<ManagedModelRow | null>(null);
  const [search] = useUrlState('q', '');
  const [status] = useUrlState('status', '');
  const [sort] = useUrlState('sort', '');
  const batchParams = useUrlStateBatch();
  const setSearch = (next: string) => batchParams({ q: next });
  const setStatus = (next: string) => batchParams({ status: next });
  const setSort = (next: string) => batchParams({ sort: next });
  const { field: sortField, dir: sortDir } = parseSort(sort);
  const debounced = useDebounced(search);

  const managed = useQuery({
    queryKey: ['admin', 'managed-models', debounced, status, sort],
    queryFn: () =>
      api.get<{ managed_models: ManagedModelRow[]; total_count: number }>(
        `/api/v1/admin/managed-models${qs({ search: debounced, status, sort })}`,
      ),
  });

  const patch = useMutation({
    mutationFn: ({ id, body }: { id: string; body: Record<string, unknown> }) =>
      api.patch(`/api/v1/admin/managed-models/${id}`, body),
    onSuccess: () => {
      toast(t('adminManagedModels.updatedToast'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'managed-models'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/v1/admin/managed-models/${id}`),
    onSuccess: () => {
      toast(t('adminManagedModels.deletedToast'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'managed-models'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminManagedModels.title')}</h1>
          <p className="page-subtitle">{t('adminManagedModels.subtitle')}</p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
          {t('adminManagedModels.create')}
        </button>
      </header>

      <div className="row wrap">
        <SearchInput
          value={search}
          onChange={setSearch}
          placeholder={t('adminManagedModels.searchPlaceholder')}
          label={t('adminManagedModels.searchLabel')}
        />
        <FilterSelect
          label={t('tables.status')}
          value={status}
          onChange={setStatus}
          options={[
            { value: '', label: t('adminManagedModels.allStatuses') },
            { value: 'enabled', label: t('adminManagedModels.statusEnabled') },
            { value: 'disabled', label: t('adminManagedModels.statusDisabled') },
          ]}
        />
        <FilterSelect
          label={t('adminPeople.sortLabel')}
          value={sortField}
          onChange={(next) => setSort(`${next}_${sortDir}`)}
          options={[
            { value: 'spend', label: t('adminManagedModels.sortBySpend') },
            { value: 'name', label: t('adminManagedModels.sortByName') },
            { value: 'requests', label: t('adminManagedModels.sortByRequests') },
            { value: 'tokens', label: t('adminManagedModels.sortByTokens') },
            { value: 'users', label: t('adminManagedModels.sortByUsers') },
            { value: 'grants', label: t('adminManagedModels.sortByGrants') },
            { value: 'created', label: t('adminManagedModels.sortByCreated') },
          ]}
        />
        <button
          type="button"
          className="btn btn-ghost btn-sm"
          onClick={() => setSort(`${sortField}_${sortDir === 'asc' ? 'desc' : 'asc'}`)}
          aria-label={t('adminPeople.sortDirection')}
          aria-pressed={sortDir === 'desc'}
        >
          {sortDir === 'asc' ? `↑ ${t('adminPeople.sortAscending')}` : `↓ ${t('adminPeople.sortDescending')}`}
        </button>
      </div>

      <section className="card card-flush">
        <AsyncSection
          query={managed}
          empty={{
            when: (data) => data.managed_models.length === 0 && !debounced && !status,
            title: t('adminManagedModels.emptyTitle'),
            body: t('adminManagedModels.emptyBody'),
          }}
        >
          {(data) =>
            data.managed_models.length === 0 ? (
              <EmptyState title={t('adminManagedModels.noMatchTitle')} body={t('adminManagedModels.noMatchBody')} />
            ) : (
              <Collection
                name="Managed aliases"
                rows={data.managed_models}
                rowKey={(m) => m.id}
                resetKey={sort + debounced + status}
                hideSearch
                columns={[{ id: '0', label: '', value: (m) => m.name, render: () => null }]}
              >
                {(visible) => (
                  <div className="table-wrap">
                    <table className="data">
                      <thead>
                        <tr>
                          <SortableColumn label={t('adminManagedModels.colName')} field="name" sort={sort} onSort={setSort} />
                          <th scope="col">{t('tables.status')}</th>
                          <th scope="col">{t('adminManagedModels.colTarget')}</th>
                          <th scope="col">{t('adminManagedModels.colFallback')}</th>
                          {localOnly ? null : (
                            <SortableColumn
                              label={t('adminManagedModels.colSpend30d')}
                              field="spend"
                              sort={sort}
                              onSort={setSort}
                              numeric
                            />
                          )}
                          <SortableColumn
                            label={t('adminManagedModels.colRequests30d')}
                            field="requests"
                            sort={sort}
                            onSort={setSort}
                            numeric
                          />
                          <SortableColumn
                            label={t('adminManagedModels.colTokens30d')}
                            field="tokens"
                            sort={sort}
                            onSort={setSort}
                            numeric
                          />
                          <SortableColumn
                            label={t('adminManagedModels.colCallers30d')}
                            field="users"
                            sort={sort}
                            onSort={setSort}
                            numeric
                          />
                          <SortableColumn
                            label={t('adminManagedModels.colGrants')}
                            field="grants"
                            sort={sort}
                            onSort={setSort}
                            numeric
                          />
                          <th scope="col" className="num">
                            {t('adminManagedModels.colContext')}
                          </th>
                          <th scope="col">{t('adminManagedModels.colUpdated')}</th>
                          <th scope="col">
                            <span className="sr-only">{t('tables.actions')}</span>
                          </th>
                        </tr>
                      </thead>
                      <tbody>
                        {visible.map((m) => (
                          <tr key={m.id}>
                            <td>
                              <span className="mono">{m.name}</span>
                              {m.description ? (
                                <div className="small muted truncate" style={{ maxWidth: 240 }}>
                                  {m.description}
                                </div>
                              ) : null}
                            </td>
                            <td>
                              {m.broken && m.servable ? (
                                <span title={m.broken_reason}>
                                  <Badge tone="warning" dot>
                                    {t('adminManagedModels.coveredByFallbackBadge')}
                                  </Badge>
                                </span>
                              ) : m.broken ? (
                                <span title={m.broken_reason}>
                                  <Badge tone="danger">{t('adminManagedModels.brokenBadge')}</Badge>
                                </span>
                              ) : m.status === 'enabled' ? (
                                <Badge tone="success" dot>
                                  {t('adminManagedModels.statusEnabled')}
                                </Badge>
                              ) : (
                                <Badge tone="warning">{t('adminManagedModels.statusDisabled')}</Badge>
                              )}
                            </td>
                            <td className="small">
                              <span className="mono">{m.target_public_name}</span>
                              <div className="small muted">{m.target_upstream_name}</div>
                            </td>
                            <td className="small">
                              {m.fallback_model_id ? (
                                <>
                                  <span className="mono" style={{ whiteSpace: 'nowrap' }}>
                                    {m.fallback_public_name || m.fallback_model_id}
                                  </span>
                                  <div className="small muted" style={{ whiteSpace: 'nowrap' }}>
                                    {m.fallback_broken ? (
                                      <span title={m.fallback_broken_reason}>
                                        <Badge tone="danger">{t('adminManagedModels.fallbackBrokenBadge')}</Badge>
                                      </span>
                                    ) : (
                                      m.fallback_upstream_name
                                    )}
                                  </div>
                                </>
                              ) : (
                                <span className="muted">{t('adminManagedModels.noFallback')}</span>
                              )}
                            </td>
                            {localOnly ? null : <td className="num small">{formatUSD(m.spend_30d_usd * 1_000_000_000)}</td>}
                            <td className="num small">{formatNumber(m.requests_30d)}</td>
                            <td className="num small">{formatNumber(m.tokens_in_30d + m.tokens_out_30d)}</td>
                            <td className="num small" title={t('adminManagedModels.callersHint')}>
                              {formatNumber(m.distinct_principals_30d)}
                            </td>
                            <td className="num small">{formatNumber(m.grant_count)}</td>
                            <td className="num small muted">{m.context_window ? formatNumber(m.context_window) : '—'}</td>
                            <td className="small muted">{formatRelative(m.updated_at)}</td>
                            <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                              <button type="button" className="btn btn-ghost btn-sm" onClick={() => setEditing(m)}>
                                {t('adminManagedModels.edit')}
                              </button>
                              <button type="button" className="btn btn-ghost btn-sm" onClick={() => setToggling(m)}>
                                {m.status === 'enabled' ? t('adminManagedModels.disable') : t('adminManagedModels.enable')}
                              </button>
                              <button type="button" className="btn btn-ghost btn-sm" onClick={() => setDeleting(m)}>
                                {t('adminManagedModels.delete')}
                              </button>
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </Collection>
            )
          }
        </AsyncSection>
      </section>

      <ManagedModelDialog
        open={createOpen}
        model={null}
        onClose={() => setCreateOpen(false)}
        onSaved={() => {
          setCreateOpen(false);
          void queryClient.invalidateQueries({ queryKey: ['admin', 'managed-models'] });
        }}
      />
      <ManagedModelDialog
        open={Boolean(editing)}
        model={editing}
        onClose={() => setEditing(null)}
        onSaved={() => {
          setEditing(null);
          void queryClient.invalidateQueries({ queryKey: ['admin', 'managed-models'] });
        }}
      />

      <ConfirmDialog
        open={Boolean(toggling)}
        onClose={() => setToggling(null)}
        onConfirm={() => {
          if (toggling) {
            patch.mutate({
              id: toggling.id,
              body: { status: toggling.status === 'enabled' ? 'disabled' : 'enabled' },
            });
          }
          setToggling(null);
        }}
        danger={toggling?.status === 'enabled'}
        title={t('adminManagedModels.disableTitle', { name: toggling?.name ?? '' })}
        consequence={t('adminManagedModels.disableBody', { name: toggling?.name ?? '' })}
        confirmLabel={toggling?.status === 'enabled' ? t('adminManagedModels.disable') : t('adminManagedModels.enable')}
        busy={patch.isPending}
      />

      <ConfirmDialog
        open={Boolean(deleting)}
        onClose={() => setDeleting(null)}
        onConfirm={() => {
          if (deleting) remove.mutate(deleting.id);
          setDeleting(null);
        }}
        danger
        title={t('adminManagedModels.deleteTitle', { name: deleting?.name ?? '' })}
        consequence={t('adminManagedModels.deleteBody', { name: deleting?.name ?? '' })}
        confirmLabel={t('adminManagedModels.delete')}
        busy={remove.isPending}
      />
    </div>
  );
}

function ManagedModelDialog({
  open,
  model,
  onClose,
  onSaved,
}: {
  open: boolean;
  model: ManagedModelRow | null;
  onClose: () => void;
  onSaved: () => void;
}): ReactNode {
  const fallbacksLicensed = useLicensed('model_fallbacks');
  const toast = useToast();
  const editing = Boolean(model);
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [targetId, setTargetId] = useState('');
  const [fallbackId, setFallbackId] = useState<string | null>(null);
  const [triggers, setTriggers] = useState<FallbackTrigger[] | null>(null);
  const [confirmRepoint, setConfirmRepoint] = useState(false);

  const models = useQuery({
    queryKey: ['admin', 'models', 'for-managed'],
    queryFn: () => api.get<{ models: Model[] }>('/api/v1/admin/models'),
    enabled: open,
  });
  // Only enabled models can be a target — pointing an alias at a disabled
  // model would create a broken alias on purpose.
  const targets = useMemo(() => (models.data?.models ?? []).filter((m) => m.status === 'enabled'), [models.data]);

  const effectiveName = editing ? ((name || model?.name) ?? '') : name;
  const effectiveTarget = editing ? targetId || model?.target_model_id || '' : targetId;
  const targetChanged = editing && effectiveTarget !== model?.target_model_id;
  // Fallback state is "null until touched" so an edit that never opens the
  // section leaves the stored configuration exactly as it was.
  const effectiveFallback = fallbackId ?? model?.fallback_model_id ?? '';
  const effectiveTriggers: FallbackTrigger[] =
    triggers ?? (model?.fallback_triggers?.length ? model.fallback_triggers : DEFAULT_FALLBACK_TRIGGERS);
  // The fallback can be any enabled model except the one it would replace; a
  // fallback that IS the target is refused server-side too.
  const fallbackChoices = useMemo(() => targets.filter((m) => m.id !== effectiveTarget), [targets, effectiveTarget]);
  const toggleTrigger = (trigger: FallbackTrigger) => {
    const next = effectiveTriggers.includes(trigger)
      ? effectiveTriggers.filter((x) => x !== trigger)
      : ALL_FALLBACK_TRIGGERS.filter((x) => x === trigger || effectiveTriggers.includes(x));
    setTriggers(next);
  };
  const triggerLabel: Record<FallbackTrigger, [string, string]> = {
    target_unavailable: [t('adminManagedModels.triggerTargetUnavailable'), t('adminManagedModels.triggerTargetUnavailableHint')],
    upstream_unreachable: [
      t('adminManagedModels.triggerUpstreamUnreachable'),
      t('adminManagedModels.triggerUpstreamUnreachableHint'),
    ],
    model_down: [t('adminManagedModels.triggerModelDown'), t('adminManagedModels.triggerModelDownHint')],
    model_degraded: [t('adminManagedModels.triggerModelDegraded'), t('adminManagedModels.triggerModelDegradedHint')],
  };
  const targetLabel = (id: string): string => {
    const found = targets.find((m) => m.id === id);
    return found ? publicModelName(found) : id;
  };

  const save = useMutation({
    mutationFn: () =>
      editing
        ? api.patch(`/api/v1/admin/managed-models/${model?.id}`, {
            name: effectiveName,
            description: description || model?.description || '',
            target_model_id: effectiveTarget,
            fallback_model_id: effectiveFallback,
            fallback_triggers: effectiveFallback ? effectiveTriggers : [],
          })
        : api.post('/api/v1/admin/managed-models', {
            name,
            description,
            target_model_id: targetId,
            fallback_model_id: effectiveFallback,
            fallback_triggers: effectiveFallback ? effectiveTriggers : [],
          }),
    onSuccess: () => {
      toast(editing ? t('adminManagedModels.updatedToast') : t('adminManagedModels.createdToast'));
      setName('');
      setDescription('');
      setTargetId('');
      setFallbackId(null);
      setTriggers(null);
      onSaved();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const triggersMissing = Boolean(effectiveFallback) && effectiveTriggers.length === 0;
  const valid = Boolean(effectiveName.trim()) && Boolean(effectiveTarget) && !triggersMissing;

  const submit = () => {
    if (!valid) return;
    // Repointing silently changes what every existing caller talks to, so it
    // gets an explicit confirmation naming both models.
    if (targetChanged) {
      setConfirmRepoint(true);
      return;
    }
    save.mutate();
  };

  if (!open) return null;

  return (
    <>
      <Modal
        open={open}
        onClose={onClose}
        title={editing ? t('adminManagedModels.editTitle', { name: model?.name ?? '' }) : t('adminManagedModels.createTitle')}
        description={editing ? t('adminManagedModels.editDescription') : t('adminManagedModels.createDescription')}
        footer={
          <>
            <button type="button" className="btn" onClick={onClose} disabled={save.isPending}>
              {t('common.cancel')}
            </button>
            <button type="button" className="btn btn-primary" onClick={submit} disabled={!valid || save.isPending}>
              {save.isPending
                ? t('adminManagedModels.creating')
                : editing
                  ? t('adminManagedModels.save')
                  : t('adminManagedModels.create')}
            </button>
          </>
        }
      >
        <Field label={t('adminManagedModels.nameLabel')} required hint={t('adminManagedModels.nameHint')}>
          <input className="input" defaultValue={model?.name ?? ''} onChange={(event) => setName(event.target.value)} />
        </Field>
        <Field label={t('adminManagedModels.descriptionLabel')} hint={t('adminManagedModels.descriptionHint')}>
          <input
            className="input"
            defaultValue={model?.description ?? ''}
            onChange={(event) => setDescription(event.target.value)}
          />
        </Field>
        <Field label={t('adminManagedModels.targetLabel')} required hint={t('adminManagedModels.targetHint')}>
          <select
            className="select"
            defaultValue={model?.target_model_id ?? ''}
            onChange={(event) => setTargetId(event.target.value)}
          >
            <option value="">{t('adminManagedModels.chooseTarget')}</option>
            {targets.map((m) => (
              <option key={m.id} value={m.id}>
                {publicModelName(m)} · {m.upstream_name}
              </option>
            ))}
          </select>
        </Field>

        <section className="card stack" aria-labelledby="managed-model-fallback-heading">
          <div>
            <h3 id="managed-model-fallback-heading" className="small" style={{ margin: 0, fontWeight: 600 }}>
              {t('adminManagedModels.fallbackSection')}
            </h3>
            <p className="small muted" style={{ margin: 0 }}>
              {t('adminManagedModels.fallbackSectionHint')}
            </p>
          </div>
          <Field
            label={t('adminManagedModels.fallbackLabel')}
            hint={
              fallbacksLicensed || effectiveFallback
                ? t('adminManagedModels.fallbackHint')
                : t('adminManagedModels.fallbackUpsell')
            }
          >
            <select
              className="select"
              value={effectiveFallback}
              onChange={(event) => setFallbackId(event.target.value)}
              disabled={!fallbacksLicensed && !effectiveFallback}
              data-testid="managed-model-fallback"
            >
              <option value="">{t('adminManagedModels.noFallbackOption')}</option>
              {fallbackChoices.map((m) => (
                <option key={m.id} value={m.id}>
                  {publicModelName(m)} · {m.upstream_name}
                </option>
              ))}
            </select>
          </Field>
          {effectiveFallback ? (
            <Field
              label={t('adminManagedModels.fallbackTriggersLabel')}
              hint={t('adminManagedModels.fallbackTriggersHint')}
              error={triggersMissing ? t('adminManagedModels.fallbackTriggersRequired') : undefined}
            >
              <div className="stack" style={{ gap: 'var(--janus-space-2)' }}>
                {ALL_FALLBACK_TRIGGERS.map((trigger) => (
                  <label key={trigger} className="switch" style={{ alignItems: 'flex-start' }}>
                    <input
                      type="checkbox"
                      checked={effectiveTriggers.includes(trigger)}
                      onChange={() => toggleTrigger(trigger)}
                    />
                    <span>
                      {triggerLabel[trigger][0]}
                      <div className="small muted">{triggerLabel[trigger][1]}</div>
                    </span>
                  </label>
                ))}
              </div>
            </Field>
          ) : null}
        </section>
      </Modal>

      <ConfirmDialog
        open={confirmRepoint}
        onClose={() => setConfirmRepoint(false)}
        onConfirm={() => {
          setConfirmRepoint(false);
          save.mutate();
        }}
        danger={false}
        title={t('adminManagedModels.repointWarningTitle', { name: model?.name ?? '' })}
        consequence={t('adminManagedModels.repointWarningBody', {
          name: model?.name ?? '',
          from: model?.target_public_name ?? '',
          to: targetLabel(effectiveTarget),
        })}
        confirmLabel={t('adminManagedModels.repointConfirm')}
        busy={save.isPending}
      />
    </>
  );
}
