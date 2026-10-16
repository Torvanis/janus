import { Collection } from '../../components/Collection';
import { useMemo, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { hasRateCard, listAdminModels, patchModel, type ModelRatePatch } from '../../api/client';
import { isRenamedModel, publicModelName, type Model } from '../../lib/types';
import { formatDate, formatModelRate, formatTokenCount, modalityLabel } from '../../lib/format';
import { sortCollection } from '../../lib/collections';
import { AsyncSection, Badge, ConfirmDialog, EmptyState, useToast } from '../../components/ui';
import { IconButton, RowActions } from '../../components/IconButton';
import { useDebounced, useUrlState, useUrlStateBatch } from '../../lib/hooks';
import { t, type MessageKey } from '../../lib/i18n';
import { useLocalOnly } from '../../app/session';
import { Link } from 'react-router-dom';
import { FilterSelect, SearchInput, SortHeader } from '../shared';
import { ModelDrawer } from './ModelDrawer';

export function AdminModelsPage(): ReactNode {
  // Local-only mode (JANUS_LOCAL_ONLY): pricing is meaningless, so the rate
  // columns, the Rates action, and the rate-card gate on enabling all vanish.
  const localOnly = useLocalOnly();
  const queryClient = useQueryClient();
  const toast = useToast();
  const [search, setSearch] = useUrlState('q', '');
  const [status, setStatus] = useUrlState('status', '');
  const [upstream, setUpstream] = useUrlState('upstream', '');
  const [kind, setKind] = useUrlState('kind', '');
  const [role, setRole] = useUrlState('role', '');
  const [priced, setPriced] = useUrlState('priced', '');
  const [granted, setGranted] = useUrlState('granted', '');
  const [sortParam, setSort] = useUrlState('sort', 'model_asc');
  const sort = LEGACY_SORTS[sortParam] ?? sortParam;
  const [editing, setEditing] = useState<Model | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [bulkAction, setBulkAction] = useState<'enabled' | 'disabled' | null>(null);
  const debounced = useDebounced(search);

  // The whole catalog is loaded once (a few hundred rows) and filtered,
  // counted and sorted here, so every filter can say how many models each
  // choice would show.
  const models = useQuery({
    queryKey: ['admin', 'models'],
    queryFn: () => listAdminModels({}),
  });

  const patch = useMutation({
    mutationFn: ({ id, body }: { id: string; body: ModelRatePatch }) => patchModel(id, body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['admin', 'models'] });
      void queryClient.invalidateQueries({ queryKey: ['models'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const all = useMemo(() => models.data?.models ?? [], [models.data]);

  // Each filter is a predicate; a filter's option counts apply every OTHER
  // filter, so the numbers say what picking that option would show.
  const predicates = useMemo(() => {
    const term = debounced.trim().toLowerCase();
    return {
      // Either name matches: admins search by the alias users quote at them
      // or by the native name from the provider's docs.
      search: (m: Model) =>
        !term ||
        m.name.toLowerCase().includes(term) ||
        m.display_name.toLowerCase().includes(term) ||
        m.upstream_name.toLowerCase().includes(term),
      status: (m: Model) => !status || m.status === status,
      upstream: (m: Model) => !upstream || m.upstream_id === upstream,
      kind: (m: Model) => !kind || m.modalities.includes(kind),
      role: (m: Model) => !role || modelRole(m) === role,
      priced: (m: Model) => !priced || (priced === 'yes') === hasRateCard(m),
      granted: (m: Model) => !granted || (granted === 'yes') === m.grant_count > 0,
    } as const;
  }, [debounced, status, upstream, kind, role, priced, granted]);
  type FilterKey = keyof typeof predicates;
  const passesExcept = (m: Model, skip: FilterKey | null) =>
    (Object.keys(predicates) as FilterKey[]).every((k) => k === skip || predicates[k](m));
  const countBy = (skip: FilterKey, keyOf: (m: Model) => string[]) => {
    const counts = new Map<string, number>();
    for (const m of all) {
      if (!passesExcept(m, skip)) continue;
      for (const k of keyOf(m)) counts.set(k, (counts.get(k) ?? 0) + 1);
    }
    return counts;
  };
  // Choices that would show nothing are hidden (unless selected), so
  // narrowing to decision models doesn't leave a list of zeros.
  const withCount = (label: string, n: number | undefined) => `${label} (${(n ?? 0).toLocaleString()})`;

  const statusCounts = countBy('status', (m) => [m.status]);
  const upstreamCounts = countBy('upstream', (m) => [m.upstream_id]);
  const kindCounts = countBy('kind', (m) => m.modalities);
  const roleCounts = countBy('role', (m) => [modelRole(m)]);
  const pricedCounts = countBy('priced', (m) => [hasRateCard(m) ? 'yes' : 'no']);
  const grantedCounts = countBy('granted', (m) => [m.grant_count > 0 ? 'yes' : 'no']);

  const upstreams = useMemo(() => {
    const map = new Map<string, string>();
    for (const model of all) map.set(model.upstream_id, model.upstream_name);
    return [...map.entries()].sort((a, b) => a[1].localeCompare(b[1]));
  }, [all]);
  const kinds = useMemo(() => [...new Set(all.flatMap((m) => m.modalities))].sort(), [all]);

  const sortKey = sort.replace(/_asc$/, '');
  const accessor = SORTS[sortKey] ?? SORTS.model!;
  const filtered = sortCollection(
    all.filter((m) => passesExcept(m, null)),
    (m) => (localOnly && (sortKey === 'rate_in' || sortKey === 'rate_out') ? publicModelName(m).toLowerCase() : accessor(m)),
    sort.endsWith('_asc'),
  );
  const filterCount = [search, status, upstream, kind, role, priced, granted].filter(Boolean).length;
  // One URL update: separate setters would each overwrite the last.
  const batchParams = useUrlStateBatch();
  const clearFilters = () =>
    batchParams({ q: null, status: null, upstream: null, kind: null, role: null, priced: null, granted: null });
  const sortable = (label: string, key: string) => <SortHeader label={label} sortKey={key} active={sort} onSort={setSort} />;

  // Names served by more than one upstream (the same vLLM model on two nodes,
  // or an old and a new endpoint for one deployment). Each copy is its own
  // catalog entry, so the table marks them and points at managed models as
  // the stable handle.
  const sharedNameCounts = useMemo(() => {
    const counts = new Map<string, number>();
    for (const model of all) {
      if (model.status === 'stale') continue;
      const key = model.name.toLowerCase();
      counts.set(key, (counts.get(key) ?? 0) + 1);
    }
    return counts;
  }, [all]);
  const sharedNameTotal = [...sharedNameCounts.values()].filter((n) => n > 1).length;

  const selectedModels = filtered.filter((model) => selected.has(model.id));
  const unpricedSelected = bulkAction === 'enabled' && !localOnly ? selectedModels.filter((model) => !hasRateCard(model)) : [];

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminModels.title')}</h1>
          <p className="page-subtitle">{t(localOnly ? 'adminModels.subtitleLocalOnly' : 'adminModels.subtitle')}</p>
        </div>
      </header>

      {sharedNameTotal > 0 ? (
        <div className="banner banner-info" role="status" data-testid="shared-name-notice">
          <div className="row-between" style={{ width: '100%' }}>
            <span>{t('adminModels.sharedNameNotice', { count: sharedNameTotal })}</span>
            <Link className="btn btn-sm" to="/admin/managed-models">
              {t('adminModels.sharedNameAction')}
            </Link>
          </div>
        </div>
      ) : null}

      {(models.data?.counts.pending_approval ?? 0) > 0 && status !== 'pending_approval' ? (
        <div className="banner banner-info">
          <div className="row-between" style={{ width: '100%' }}>
            <span>
              <strong>{t('adminModels.awaitingReview', { count: models.data?.counts.pending_approval ?? 0 })}</strong>
            </span>
            <button type="button" className="btn btn-sm" onClick={() => setStatus('pending_approval')}>
              {t('adminModels.showThem')}
            </button>
          </div>
        </div>
      ) : null}

      <div className="row wrap" role="group" aria-label="Filter models">
        <SearchInput
          value={search}
          onChange={setSearch}
          placeholder={t('adminModels.searchLabel')}
          label={t('adminModels.searchLabel')}
        />
        <FilterSelect
          label={t('tables.status')}
          value={status}
          onChange={setStatus}
          options={[
            { value: '', label: t('adminModels.allStatuses') },
            ...STATUSES.map(([value, key]) => ({ value, label: withCount(t(key), statusCounts.get(value)) })),
          ]}
        />
        <FilterSelect
          label={t('adminModels.colUpstream')}
          value={upstream}
          onChange={setUpstream}
          options={[
            { value: '', label: t('adminModels.allUpstreams') },
            ...upstreams
              .filter(([id]) => id === upstream || upstreamCounts.get(id))
              .map(([id, name]) => ({ value: id, label: withCount(name, upstreamCounts.get(id)) })),
          ]}
        />
        <FilterSelect
          label="Kind"
          value={kind}
          onChange={setKind}
          options={[
            { value: '', label: 'All kinds' },
            ...kinds
              .filter((value) => value === kind || kindCounts.get(value))
              .map((value) => ({ value, label: withCount(modalityLabel(value), kindCounts.get(value)) })),
          ]}
        />
        <FilterSelect
          label="Used as"
          value={role}
          onChange={setRole}
          options={[
            { value: '', label: 'Any use' },
            ...ROLES.map(([value, label]) => ({ value, label: withCount(label, roleCounts.get(value)) })),
          ]}
        />
        {localOnly ? null : (
          <FilterSelect
            label="Price"
            value={priced}
            onChange={setPriced}
            options={[
              { value: '', label: 'Priced or not' },
              { value: 'yes', label: withCount('Priced', pricedCounts.get('yes')) },
              { value: 'no', label: withCount('No price yet', pricedCounts.get('no')) },
            ]}
          />
        )}
        <FilterSelect
          label="Access"
          value={granted}
          onChange={setGranted}
          options={[
            { value: '', label: 'Granted or not' },
            { value: 'yes', label: withCount('Granted to someone', grantedCounts.get('yes')) },
            { value: 'no', label: withCount('Not granted', grantedCounts.get('no')) },
          ]}
        />
      </div>
      <div className="row-between wrap">
        <div className="row wrap" role="status">
          <span className="small muted">
            {filterCount > 0
              ? `${filtered.length.toLocaleString()} of ${all.length.toLocaleString()} models match`
              : `${all.length.toLocaleString()} models`}
          </span>
          {filterCount > 0 ? (
            <button type="button" className="btn btn-ghost btn-sm" onClick={clearFilters}>
              {t('tables.clearFilters')}
            </button>
          ) : null}
        </div>
        {selected.size > 0 ? (
          <div className="row">
            <span className="small muted">{t('adminModels.selectedCount', { count: selected.size })}</span>
            <button type="button" className="btn btn-sm" onClick={() => setBulkAction('enabled')}>
              {t('tables.enable')}
            </button>
            <button type="button" className="btn btn-sm" onClick={() => setBulkAction('disabled')}>
              {t('tables.disable')}
            </button>
          </div>
        ) : null}
      </div>

      <section className="card card-flush">
        <AsyncSection
          query={models}
          empty={{
            when: (data) => data.models.length === 0,
            title: t('adminModels.emptyTitle'),
            body: t('adminModels.emptyBody'),
          }}
        >
          {() =>
            filtered.length === 0 ? (
              <EmptyState
                title={t('adminModels.noMatchTitle')}
                body={t('adminModels.noMatchBody')}
                action={
                  <button type="button" className="btn" onClick={clearFilters}>
                    {t('tables.clearFilters')}
                  </button>
                }
              />
            ) : (
              <Collection
                name="Discovered models"
                rows={filtered}
                rowKey={(model) => model.id}
                resetKey={[sort, debounced, status, upstream, kind, role, priced, granted].join('|')}
                hideSearch
                columns={[
                  { id: '0', label: '', value: (model) => model.name, render: () => null },
                  { id: '1', label: '', value: (model) => model.display_name, render: () => null },
                ]}
              >
                {(visible) => (
                  <div className="table-wrap">
                    <table className="data">
                      <thead>
                        <tr>
                          <th scope="col" style={{ width: 36 }}>
                            <span className="sr-only">{t('adminModels.colSelect')}</span>
                          </th>
                          {sortable(t('tables.model'), 'model')}
                          {sortable(t('adminModels.colUpstream'), 'upstream')}
                          {sortable(t('tables.status'), 'status')}
                          {localOnly ? null : (
                            <>
                              {sortable(t('adminModels.colInputRate'), 'rate_in')}
                              {sortable(t('adminModels.colOutputRate'), 'rate_out')}
                            </>
                          )}
                          {sortable(t('adminModels.colContextWindow'), 'context')}
                          {sortable(t('adminModels.colGrants'), 'grants')}
                          {sortable(t('adminModels.colDiscovered'), 'discovered')}
                          <th scope="col">
                            <span className="sr-only">{t('tables.actions')}</span>
                          </th>
                        </tr>
                      </thead>
                      <tbody>
                        {visible.map((model) => (
                          <tr key={model.id}>
                            <td>
                              <input
                                type="checkbox"
                                checked={selected.has(model.id)}
                                onChange={() =>
                                  setSelected((current) => {
                                    const next = new Set(current);
                                    if (next.has(model.id)) next.delete(model.id);
                                    else next.add(model.id);
                                    return next;
                                  })
                                }
                                aria-label={t('adminModels.selectModel', { name: publicModelName(model) })}
                              />
                            </td>
                            <td>
                              <>
                                <div
                                  title={
                                    isRenamedModel(model) ? t('adminModels.upstreamNameTitle', { name: model.name }) : undefined
                                  }
                                >
                                  {publicModelName(model)}
                                </div>
                                {isRenamedModel(model) ? <div className="small muted mono">{model.name}</div> : null}
                                <div className="small muted">
                                  {model.modalities.map(modalityLabel).join(', ')}
                                  {modelRole(model) !== 'chat' ? (
                                    <>
                                      {' '}
                                      <Badge tone="info">{ROLE_LABEL[modelRole(model)]}</Badge>
                                    </>
                                  ) : null}
                                </div>
                              </>
                            </td>
                            <td className="small muted">
                              {model.upstream_name}
                              {(sharedNameCounts.get(model.name.toLowerCase()) ?? 0) > 1 && model.status !== 'stale' ? (
                                <div>
                                  <span title={t('adminModels.sharedNameTitle', { name: model.name })}>
                                    <Badge tone="info">
                                      {t('adminModels.sharedNameBadge', {
                                        count: (sharedNameCounts.get(model.name.toLowerCase()) ?? 1) - 1,
                                      })}
                                    </Badge>
                                  </span>
                                </div>
                              ) : null}
                            </td>
                            <td>
                              <StatusBadge status={model.status} />
                            </td>
                            {localOnly ? null : (
                              <>
                                <td className="num small">{formatModelRate(model, 'rate_in_nanousd')}</td>
                                <td className="num small">{formatModelRate(model, 'rate_out_nanousd')}</td>
                              </>
                            )}
                            <td className="num small">{formatTokenCount(model.context_window)}</td>
                            <td className="num small">{model.grant_count}</td>
                            <td className="small muted">{formatDate(model.discovered_at)}</td>
                            <td>
                              <RowActions>
                                <IconButton
                                  icon="edit"
                                  label={t('adminModels.editFor', { name: model.name })}
                                  title={t('adminModels.edit')}
                                  onClick={() => setEditing(model)}
                                />
                                {model.status === 'enabled' ? (
                                  <IconButton
                                    icon="disable"
                                    label={t('tables.disable')}
                                    onClick={() => patch.mutate({ id: model.id, body: { status: 'disabled' } })}
                                  />
                                ) : (
                                  <IconButton
                                    icon="enable"
                                    label={t('tables.enable')}
                                    onClick={() => {
                                      // Without cost tracking there is nothing to
                                      // price: enable directly in local-only mode.
                                      if (!localOnly && !hasRateCard(model)) {
                                        setEditing(model);
                                        toast(t('adminModels.setRateFirst'), 'warning');
                                        return;
                                      }
                                      patch.mutate({ id: model.id, body: { status: 'enabled' } });
                                    }}
                                  />
                                )}
                              </RowActions>
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

      {editing ? (
        <ModelDrawer
          key={editing.id}
          model={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            void queryClient.invalidateQueries({ queryKey: ['admin'] });
            void queryClient.invalidateQueries({ queryKey: ['models'] });
          }}
        />
      ) : null}

      <ConfirmDialog
        open={Boolean(bulkAction)}
        onClose={() => setBulkAction(null)}
        onConfirm={async () => {
          if (!bulkAction) return;
          const targets = bulkAction === 'enabled' && !localOnly ? selectedModels.filter((m) => hasRateCard(m)) : selectedModels;
          for (const model of targets) {
            if (!bulkAction) break;
            await patch.mutateAsync({ id: model.id, body: { status: bulkAction } }).catch(() => undefined);
          }
          setSelected(new Set());
          setBulkAction(null);
          toast(
            bulkAction === 'enabled'
              ? t('adminModels.bulkEnabled', { count: targets.length })
              : t('adminModels.bulkDisabled', { count: targets.length }),
          );
        }}
        danger={bulkAction === 'disabled'}
        title={
          bulkAction === 'enabled'
            ? t('adminModels.bulkEnableTitle', { count: selectedModels.length })
            : t('adminModels.bulkDisableTitle', { count: selectedModels.length })
        }
        consequence={
          bulkAction === 'enabled'
            ? unpricedSelected.length > 0
              ? t('adminModels.bulkEnableSkip', { count: unpricedSelected.length })
              : t('adminModels.bulkEnableConsequence')
            : t('adminModels.bulkDisableConsequence')
        }
        confirmLabel={bulkAction === 'enabled' ? t('adminModels.enableModels') : t('adminModels.disableModels')}
        busy={patch.isPending}
      />
    </div>
  );
}

/** What a catalog model is for: callers' chat, typed decisions, or a guard. */
function modelRole(m: Model): 'chat' | 'decision' | 'guard' {
  if (m.classifier_role === 'decision') return 'decision';
  if (m.classifier_role) return 'guard';
  return 'chat';
}

const ROLES: Array<[ReturnType<typeof modelRole>, string]> = [
  ['chat', 'Models people call'],
  ['decision', 'Decision models'],
  ['guard', 'Guard classifiers'],
];
const ROLE_LABEL: Record<string, string> = { decision: 'Decision model', guard: 'Guard classifier' };

const STATUSES: Array<[Model['status'], MessageKey]> = [
  ['enabled', 'tables.enabled'],
  ['disabled', 'tables.disabled'],
  ['pending_approval', 'adminModels.pendingApproval'],
  ['stale', 'adminModels.stale'],
];

const STATUS_ORDER: Record<string, number> = { enabled: 0, pending_approval: 1, disabled: 2, stale: 3 };

/** Sort keys (bare = descending, `_asc` = ascending, as SortHeader speaks). */
const SORTS: Record<string, (m: Model) => string | number | null> = {
  model: (m) => publicModelName(m).toLowerCase(),
  upstream: (m) => `${m.upstream_name.toLowerCase()}\u0000${publicModelName(m).toLowerCase()}`,
  status: (m) => STATUS_ORDER[m.status] ?? 9,
  // Unpriced models sort last either way, never as free.
  rate_in: (m) => (hasRateCard(m) ? m.rate_in_nanousd : null),
  rate_out: (m) => (hasRateCard(m) ? m.rate_out_nanousd : null),
  context: (m) => m.context_window || null,
  grants: (m) => m.grant_count,
  discovered: (m) => m.discovered_at,
};

/**
 * Sort values from links made before the sortable headers. The Model column
 * sorts as `model` / `model_asc`: the old `name` meant A-Z, so it cannot also
 * be the header's Z-A key.
 */
const LEGACY_SORTS: Record<string, string> = {
  name: 'model_asc',
  name_asc: 'model_asc',
  cost: 'rate_in',
  discovered: 'discovered',
};

function StatusBadge({ status }: { status: Model['status'] }): ReactNode {
  switch (status) {
    case 'enabled':
      return (
        <Badge tone="success" dot>
          {t('tables.enabled')}
        </Badge>
      );
    case 'pending_approval':
      return <Badge tone="info">{t('adminModels.pendingApproval')}</Badge>;
    case 'stale':
      return <Badge tone="warning">{t('adminModels.stale')}</Badge>;
    default:
      return <Badge tone="neutral">{t('tables.disabled')}</Badge>;
  }
}
