import { Collection } from '../../components/Collection';
import { useMemo, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { hasRateCard, listAdminModels, patchModel, type ModelRatePatch } from '../../api/client';
import { isRenamedModel, publicModelName, type Model } from '../../lib/types';
import { formatDate, formatModelRate, formatTokenCount, titleCase } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, EmptyState, useToast } from '../../components/ui';
import { useDebounced, useUrlState } from '../../lib/hooks';
import { t } from '../../lib/i18n';
import { useLocalOnly } from '../../app/session';
import { FilterSelect, SearchInput } from '../shared';
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
  const [sort, setSort] = useUrlState('sort', 'name');
  const [editing, setEditing] = useState<Model | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [bulkAction, setBulkAction] = useState<'enabled' | 'disabled' | null>(null);
  const debounced = useDebounced(search);

  const models = useQuery({
    queryKey: ['admin', 'models', status, upstream, sort],
    queryFn: () => listAdminModels({ status, upstream_id: upstream, sort: localOnly && sort === 'cost' ? 'name' : sort }),
  });

  const patch = useMutation({
    mutationFn: ({ id, body }: { id: string; body: ModelRatePatch }) => patchModel(id, body),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['admin', 'models'] });
      void queryClient.invalidateQueries({ queryKey: ['models'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const upstreams = useMemo(() => {
    const map = new Map<string, string>();
    for (const model of models.data?.models ?? []) {
      map.set(model.upstream_id, model.upstream_name);
    }
    return [...map.entries()];
  }, [models.data]);

  const filtered = (models.data?.models ?? []).filter((model) => {
    const term = debounced.trim().toLowerCase();
    // Either name matches: admins search by the alias users quote at them or
    // by the native name from the provider's docs.
    return !term || model.name.toLowerCase().includes(term) || model.display_name.toLowerCase().includes(term);
  });

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

      <FilterSelect
        label="Sort models"
        value={sort}
        onChange={setSort}
        options={[
          { value: 'name', label: 'Name (A–Z)' },
          { value: 'discovered', label: 'Newest discovered' },
          ...(localOnly ? [] : [{ value: 'cost', label: 'Input rate (highest first)' }]),
        ]}
      />
      <div className="row-between wrap">
        <div className="row wrap">
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
              { value: 'enabled', label: t('tables.enabled') },
              { value: 'disabled', label: t('tables.disabled') },
              { value: 'pending_approval', label: t('adminModels.pendingApproval') },
              { value: 'stale', label: t('adminModels.stale') },
            ]}
          />
          <FilterSelect
            label={t('adminModels.colUpstream')}
            value={upstream}
            onChange={setUpstream}
            options={[
              { value: '', label: t('adminModels.allUpstreams') },
              ...upstreams.map(([id, name]) => ({ value: id, label: name })),
            ]}
          />
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
            when: (data) => data.models.length === 0 && !status && !upstream,
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
                  <button
                    type="button"
                    className="btn"
                    onClick={() => {
                      setSearch('');
                      setStatus('');
                      setUpstream('');
                    }}
                  >
                    {t('tables.clearFilters')}
                  </button>
                }
              />
            ) : (
              <Collection
                name="Discovered models"
                rows={filtered}
                rowKey={(model) => model.id}
                resetKey={sort + debounced + status + upstream}
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
                          <th scope="col">{t('tables.model')}</th>
                          <th scope="col">{t('adminModels.colUpstream')}</th>
                          <th scope="col">{t('tables.status')}</th>
                          {localOnly ? null : (
                            <>
                              <th scope="col">{t('adminModels.colInputRate')}</th>
                              <th scope="col">{t('adminModels.colOutputRate')}</th>
                            </>
                          )}
                          <th scope="col">{t('adminModels.colContextWindow')}</th>
                          <th scope="col">{t('adminModels.colGrants')}</th>
                          <th scope="col">{t('adminModels.colDiscovered')}</th>
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
                                <div className="small muted">{model.modalities.map(titleCase).join(', ')}</div>
                              </>
                            </td>
                            <td className="small muted">{model.upstream_name}</td>
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
                            <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                              <button
                                type="button"
                                className="btn btn-ghost btn-sm"
                                aria-label={t('adminModels.editFor', { name: model.name })}
                                onClick={() => setEditing(model)}
                              >
                                {t('adminModels.edit')}
                              </button>
                              {model.status === 'enabled' ? (
                                <button
                                  type="button"
                                  className="btn btn-ghost btn-sm"
                                  onClick={() => patch.mutate({ id: model.id, body: { status: 'disabled' } })}
                                >
                                  {t('tables.disable')}
                                </button>
                              ) : (
                                <button
                                  type="button"
                                  className="btn btn-ghost btn-sm"
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
                                >
                                  {t('tables.enable')}
                                </button>
                              )}
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
