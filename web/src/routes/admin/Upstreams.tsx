import { Collection } from '../../components/Collection';
import { useState, type ReactNode } from 'react';
import { Link, useLocation, useNavigate, useParams } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, qs } from '../../lib/api';
import { listAdminModels } from '../../api/client';
import type { Model, Upstream } from '../../lib/types';
import { formatDateTime, formatModelRate, formatRelative, titleCase } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, Drawer, Field, useToast } from '../../components/ui';
import { RateCardDialog } from './RateCardDialog';
import { useDebounced, useUnsavedGuard, useUrlState } from '../../lib/hooks';
import { t, type MessageKey } from '../../lib/i18n';
import { useLocalOnly } from '../../app/session';
import { SearchInput, SortHeader, useTableSort } from '../shared';

interface UpstreamsResponse {
  upstreams: Upstream[];
  adapter_types: string[];
}

const ADAPTER_HELP_KEYS: Record<string, MessageKey> = {
  openai_compatible: 'adminUpstreams.adapterHelp.openaiCompatible',
  anthropic: 'adminUpstreams.adapterHelp.anthropic',
  bedrock: 'adminUpstreams.adapterHelp.bedrock',
  vertex: 'adminUpstreams.adapterHelp.vertex',
  ollama: 'adminUpstreams.adapterHelp.ollama',
  vlm: 'adminUpstreams.adapterHelp.vlm',
  llama_cpp: 'adminUpstreams.adapterHelp.llamaCpp',
  tei: 'adminUpstreams.adapterHelp.tei',
};

/** Sort accessors for the upstreams table; see useTableSort. */
const UPSTREAM_SORTS = {
  name: (u: Upstream) => u.name.toLowerCase(),
  provider: (u: Upstream) => u.adapter_type,
  health: (u: Upstream) => (u.last_error ? 0 : 1),
  models: (u: Upstream) => u.model_count ?? 0,
};

export function UpstreamsPage(): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [search, setSearch] = useUrlState('q', '');
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<Upstream | null>(null);
  const [refreshing, setRefreshing] = useState<string | null>(null);
  const [pricing, setPricing] = useState<Model | null>(null);
  const debounced = useDebounced(search);

  const upstreams = useQuery({
    queryKey: ['admin', 'upstreams'],
    queryFn: () => api.get<UpstreamsResponse>('/api/v1/admin/upstreams'),
  });

  // The open edit drawer is URL state (/admin/upstreams/:id), not component
  // state: deep links are shareable and the drawer survives a refresh. The
  // create drawer stays local — a new upstream has no id to address.
  const navigate = useNavigate();
  const location = useLocation();
  const { id: editId } = useParams();
  const editing: Upstream | 'new' | null = creating
    ? 'new'
    : editId
      ? ((upstreams.data?.upstreams ?? []).find((upstream) => upstream.id === editId) ?? null)
      : null;
  const openEdit = (upstream: Upstream) => navigate({ pathname: `/admin/upstreams/${upstream.id}`, search: location.search });
  const closeDrawer = () => {
    // While the rate-card dialog is stacked on top, Escape/scrim must close
    // only the dialog (its own handler), never the drawer underneath it.
    if (pricing) return;
    setCreating(false);
    if (editId) navigate({ pathname: '/admin/upstreams', search: location.search });
  };

  const remove = useMutation({
    mutationFn: ({ id, force, purgeGrants }: { id: string; force: boolean; purgeGrants: boolean }) =>
      api.del<{ models_disabled: number; managed_models_broken: string[]; grants_removed: number }>(
        `/api/v1/admin/upstreams/${id}${qs({ force: force ? 'true' : '', purge_grants: purgeGrants ? 'true' : '' })}`,
      ),
    onSuccess: (data) => {
      setDeleting(null);
      if (data.managed_models_broken.length > 0) {
        toast(t('adminUpstreams.deletedBrokeAliasesToast', { count: data.managed_models_broken.length }), 'danger');
      } else {
        toast(t('adminUpstreams.disabledToast'));
      }
      void queryClient.invalidateQueries({ queryKey: ['admin'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const refresh = useMutation({
    mutationFn: (id: string) =>
      api.post<{ result: { models_found: number; models_new: number; error?: string } }>(`/api/v1/admin/upstreams/${id}/refresh`),
    onSuccess: (data) => {
      setRefreshing(null);
      if (data.result.error) {
        toast(t('adminUpstreams.discoveryFailed', { error: data.result.error }), 'danger');
      } else {
        toast(t('adminUpstreams.discoveryResult', { found: data.result.models_found, fresh: data.result.models_new }));
      }
      void queryClient.invalidateQueries({ queryKey: ['admin'] });
    },
    onError: (error: Error) => {
      setRefreshing(null);
      toast(error.message, 'danger');
    },
  });

  const matched = (upstreams.data?.upstreams ?? []).filter((upstream) => {
    const term = debounced.trim().toLowerCase();
    return !term || upstream.name.toLowerCase().includes(term) || upstream.base_url.toLowerCase().includes(term);
  });
  // The upstream list is returned whole, so sorting is client-side. Health
  // sorts by whether the last probe succeeded — "show me what is broken" is
  // the reason to sort this table at all.
  const { sorted: filtered, sort, setSort } = useTableSort(matched, UPSTREAM_SORTS, 'name_asc');

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminUpstreams.title')}</h1>
          <p className="page-subtitle">{t('adminUpstreams.subtitle')}</p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
          {t('adminUpstreams.add')}
        </button>
      </header>

      <div className="row wrap">
        <SearchInput
          value={search}
          onChange={setSearch}
          placeholder={t('adminUpstreams.searchLabel')}
          label={t('adminUpstreams.searchLabel')}
        />
      </div>

      <section className="card card-flush">
        <AsyncSection
          query={upstreams}
          empty={{
            when: (data) => data.upstreams.length === 0,
            title: t('adminUpstreams.emptyTitle'),
            body: t('adminUpstreams.emptyBody'),
            action: (
              <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
                {t('adminUpstreams.addFirst')}
              </button>
            ),
          }}
        >
          {() => (
            <Collection
              name="Upstreams"
              rows={filtered}
              rowKey={(upstream) => upstream.id}
              resetKey={sort + search}
              hideSearch
              columns={[
                { id: '0', label: '', value: (upstream) => upstream.name, render: () => null },
                { id: '1', label: '', value: (upstream) => upstream.base_url, render: () => null },
                { id: '2', label: '', value: (upstream) => upstream.adapter_type, render: () => null },
              ]}
            >
              {(visible) => (
                <div className="table-wrap">
                  <table className="data">
                    <thead>
                      <tr>
                        <SortHeader label={t('adminUpstreams.colName')} sortKey="name" active={sort} onSort={setSort} />
                        <SortHeader label={t('adminUpstreams.colProvider')} sortKey="provider" active={sort} onSort={setSort} />
                        <th scope="col">{t('adminUpstreams.colBaseUrl')}</th>
                        <th scope="col">{t('adminUpstreams.colCredential')}</th>
                        <SortHeader label={t('adminUpstreams.colHealth')} sortKey="health" active={sort} onSort={setSort} />
                        <SortHeader label={t('adminUpstreams.colModels')} sortKey="models" active={sort} onSort={setSort} />
                        <th scope="col">
                          <span className="sr-only">{t('tables.actions')}</span>
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {visible.map((upstream) => (
                        <tr key={upstream.id}>
                          <td>
                            <button
                              type="button"
                              className="btn btn-ghost btn-sm"
                              style={{ padding: 0, minHeight: 'auto' }}
                              onClick={() => openEdit(upstream)}
                            >
                              {upstream.name}
                            </button>
                          </td>
                          <td className="small">{upstream.adapter_type}</td>
                          <td className="small muted truncate" style={{ maxWidth: 260 }}>
                            {upstream.base_url}
                          </td>
                          <td className="mono small muted">{upstream.api_key_mask || t('adminUpstreams.credentialNone')}</td>
                          <td>
                            {!upstream.enabled ? (
                              <Badge tone="neutral">{t('tables.disabled')}</Badge>
                            ) : upstream.last_error ? (
                              <Badge tone="danger" dot>
                                {t('adminUpstreams.unreachable')}
                              </Badge>
                            ) : upstream.last_check_at ? (
                              <Badge tone="success" dot>
                                {upstream.last_latency_ms} ms
                              </Badge>
                            ) : (
                              <Badge tone="neutral">{t('adminUpstreams.notChecked')}</Badge>
                            )}
                          </td>
                          <td className="num small">{upstream.model_count}</td>
                          <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                            <button
                              type="button"
                              className="btn btn-ghost btn-sm"
                              onClick={() => {
                                setRefreshing(upstream.id);
                                refresh.mutate(upstream.id);
                              }}
                              disabled={refreshing === upstream.id}
                            >
                              {refreshing === upstream.id ? t('adminUpstreams.discovering') : t('adminUpstreams.refreshModels')}
                            </button>
                            <button type="button" className="btn btn-ghost btn-sm" onClick={() => setDeleting(upstream)}>
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

      {(upstreams.data?.upstreams ?? []).some((upstream) => upstream.last_error) ? (
        <div className="banner banner-danger">
          <div>
            <strong>{t('adminUpstreams.lastCheckErrors')}</strong>
            <p className="small">All upstreams with discovery problems, independent of the upstream table search.</p>
            <Collection
              name="Discovery problems"
              rows={(upstreams.data?.upstreams ?? []).filter((upstream) => upstream.last_error)}
              rowKey={(upstream) => upstream.id}
              columns={[
                { id: 'name', label: 'Upstream', value: (u) => u.name, render: (u) => u.name },
                { id: 'error', label: 'Problem', value: (u) => u.last_error, render: (u) => u.last_error },
                {
                  id: 'time',
                  label: 'Last check',
                  value: (u) => u.last_check_at,
                  render: (u) => formatRelative(u.last_check_at),
                },
              ]}
            />
          </div>
        </div>
      ) : null}

      <UpstreamDrawer
        value={editing}
        adapterTypes={upstreams.data?.adapter_types ?? []}
        onClose={closeDrawer}
        onSaved={() => {
          closeDrawer();
          void queryClient.invalidateQueries({ queryKey: ['admin'] });
        }}
        onEditRates={setPricing}
      />

      <RateCardDialog
        model={pricing}
        onClose={() => setPricing(null)}
        onSaved={() => {
          setPricing(null);
          void queryClient.invalidateQueries({ queryKey: ['admin', 'models'] });
          void queryClient.invalidateQueries({ queryKey: ['models'] });
        }}
      />

      <DeleteUpstreamDialog
        key={deleting?.id ?? 'none'}
        upstream={deleting}
        onClose={() => setDeleting(null)}
        busy={remove.isPending}
        onConfirm={(options) => {
          if (deleting) remove.mutate({ id: deleting.id, ...options });
        }}
      />
    </div>
  );
}

/** Blast radius of deleting one upstream, from GET /admin/upstreams/{id}/dependents. */
interface UpstreamDependents {
  models: Array<{ id: string; name: string; display_name: string; status: string }>;
  managed_models: Array<{ id: string; name: string; status: string; target_model_name: string }>;
  grant_count: number;
  blocking_managed_models: number;
}

/**
 * Delete confirmation that shows what the delete will break before the admin
 * commits: the hosted models (all disabled), the managed models targeting
 * them (broken until repointed — enabled ones block the delete unless the
 * admin explicitly forces it), and the direct grants that would dangle (with
 * an opt-in to remove them as part of the delete).
 */
function DeleteUpstreamDialog({
  upstream,
  onClose,
  onConfirm,
  busy,
}: {
  upstream: Upstream | null;
  onClose: () => void;
  onConfirm: (options: { force: boolean; purgeGrants: boolean }) => void;
  busy: boolean;
}): ReactNode {
  // The caller keys this component by upstream id, so the acknowledgements
  // below start unticked for every upstream — a "force" ticked for one never
  // carries over to the next.
  const [force, setForce] = useState(false);
  const [purgeGrants, setPurgeGrants] = useState(false);
  const dependents = useQuery({
    queryKey: ['admin', 'upstreams', upstream?.id, 'dependents'],
    queryFn: () => api.get<UpstreamDependents>(`/api/v1/admin/upstreams/${upstream?.id}/dependents`),
    enabled: Boolean(upstream),
  });
  const deps = dependents.data;
  const blocking = deps?.blocking_managed_models ?? 0;
  const enabledAliases = deps?.managed_models.filter((mm) => mm.status === 'enabled') ?? [];
  const disabledAliases = deps?.managed_models.filter((mm) => mm.status !== 'enabled') ?? [];

  return (
    <ConfirmDialog
      open={Boolean(upstream)}
      onClose={onClose}
      onConfirm={() => onConfirm({ force, purgeGrants })}
      title={t('adminUpstreams.deleteTitle', { name: upstream?.name ?? '' })}
      consequence={t('adminUpstreams.deleteConsequence', { count: deps?.models.length ?? upstream?.model_count ?? 0 })}
      confirmLabel={t('adminUpstreams.deleteConfirm')}
      requireTyped={upstream?.name}
      busy={busy}
      // Never let the delete through while the blast radius is unknown or an
      // enabled alias depends on this upstream without an explicit force.
      confirmDisabled={!deps || dependents.isFetching || dependents.isError || (blocking > 0 && !force)}
    >
      {dependents.isPending ? (
        <p className="small muted">{t('adminUpstreams.dependentsLoading')}</p>
      ) : dependents.isError ? (
        <p className="small" style={{ color: 'var(--janus-color-danger-fg)' }}>
          {t('adminUpstreams.dependentsFailed')}
        </p>
      ) : deps ? (
        <div className="stack" data-testid="upstream-dependents">
          {deps.models.length > 0 ? (
            <details>
              <summary className="small">{t('adminUpstreams.dependentsModels', { count: deps.models.length })}</summary>
              <Collection
                name="Hosted dependents"
                rows={deps.models}
                rowKey={(m) => m.id}
                columns={[
                  {
                    id: 'name',
                    label: 'Model',
                    value: (m) => `${m.display_name} ${m.name}`,
                    render: (m) => m.display_name || m.name,
                  },
                  { id: 'status', label: 'Status', value: (m) => m.status, render: (m) => m.status },
                ]}
              />
            </details>
          ) : null}
          {deps.managed_models.length > 0 ? (
            <div className="banner banner-warning" role="alert">
              <p className="small" style={{ margin: 0 }}>
                <strong>{t('adminUpstreams.dependentsAliasesTitle', { count: deps.managed_models.length })}</strong>{' '}
                {t('adminUpstreams.dependentsAliasesBody')}
              </p>
              <Collection
                name="Managed alias dependents"
                rows={[...enabledAliases, ...disabledAliases]}
                rowKey={(mm) => mm.id}
                columns={[
                  { id: 'name', label: 'Alias', value: (mm) => mm.name, render: (mm) => mm.name },
                  { id: 'target', label: 'Target', value: (mm) => mm.target_model_name, render: (mm) => mm.target_model_name },
                  { id: 'status', label: 'Status', value: (mm) => mm.status, render: (mm) => mm.status },
                ]}
              >
                {(visible) => (
                  <ul className="small" style={{ margin: 'var(--janus-space-2) 0 0', paddingLeft: 'var(--janus-space-5)' }}>
                    {visible.map((mm) => (
                      <li key={mm.id}>
                        <span className="mono">{mm.name}</span>
                        <span className="muted">
                          {' → '}
                          {mm.target_model_name}
                          {mm.status !== 'enabled' ? ` (${t('adminUpstreams.dependentsAliasDisabled')})` : ''}
                        </span>
                      </li>
                    ))}
                  </ul>
                )}
              </Collection>
              <p className="small">Deletion affects all dependents, not just the matching or visible rows.</p>
              <p className="small" style={{ margin: 'var(--janus-space-2) 0 0' }}>
                <Link to="/admin/managed-models" onClick={onClose}>
                  {t('adminUpstreams.dependentsRepointLink')}
                </Link>
              </p>
              {blocking > 0 ? (
                <label className="row small" style={{ marginTop: 'var(--janus-space-2)', gap: 'var(--janus-space-2)' }}>
                  <input type="checkbox" checked={force} onChange={(e) => setForce(e.target.checked)} />
                  <span>{t('adminUpstreams.dependentsForce', { count: blocking })}</span>
                </label>
              ) : null}
            </div>
          ) : null}
          {deps.grant_count > 0 ? (
            <label className="row small" style={{ gap: 'var(--janus-space-2)' }}>
              <input type="checkbox" checked={purgeGrants} onChange={(e) => setPurgeGrants(e.target.checked)} />
              <span>{t('adminUpstreams.dependentsPurgeGrants', { count: deps.grant_count })}</span>
            </label>
          ) : null}
        </div>
      ) : null}
    </ConfirmDialog>
  );
}

function UpstreamDrawer({
  value,
  adapterTypes,
  onClose,
  onSaved,
  onEditRates,
}: {
  value: Upstream | 'new' | null;
  adapterTypes: string[];
  onClose: () => void;
  onSaved: () => void;
  onEditRates: (model: Model) => void;
}): ReactNode {
  const isNew = value === 'new';
  const existing = value && value !== 'new' ? value : null;
  // Local-only mode (JANUS_LOCAL_ONLY): the per-upstream rate-card section is
  // a pricing surface and stays hidden.
  const localOnly = useLocalOnly();
  const toast = useToast();

  // Rate cards for the models discovered on this upstream. Fetched only while
  // the drawer shows an existing upstream — a new one has no models yet.
  const models = useQuery({
    queryKey: ['admin', 'models', 'by-upstream', existing?.id ?? ''],
    queryFn: () => listAdminModels({ upstream_id: existing?.id ?? '' }),
    enabled: Boolean(existing),
  });

  const [name, setName] = useState('');
  const [adapterType, setAdapterType] = useState('openai_compatible');
  const [baseURL, setBaseURL] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [enabled, setEnabled] = useState(true);
  const [touched, setTouched] = useState(false);
  const [initialised, setInitialised] = useState<string | null>(null);

  // Reset the form whenever a different upstream is opened.
  const key = existing?.id ?? (isNew ? 'new' : '');
  if (value && initialised !== key) {
    setInitialised(key);
    setName(existing?.name ?? '');
    setAdapterType(existing?.adapter_type ?? adapterTypes[0] ?? 'openai_compatible');
    setBaseURL(existing?.base_url ?? '');
    setApiKey('');
    setEnabled(existing?.enabled ?? true);
    setTouched(false);
  }

  const dirty = Boolean(value) && (name !== (existing?.name ?? '') || baseURL !== (existing?.base_url ?? '') || apiKey !== '');
  useUnsavedGuard(dirty);

  const nameError = touched && !name.trim() ? t('adminUpstreams.nameError') : undefined;
  const urlError = touched && !/^https?:\/\/.+/.test(baseURL.trim()) ? t('adminUpstreams.baseUrlError') : undefined;
  const valid = Boolean(name.trim()) && /^https?:\/\/.+/.test(baseURL.trim());

  const save = useMutation({
    mutationFn: () =>
      isNew
        ? api.post('/api/v1/admin/upstreams', {
            name: name.trim(),
            adapter_type: adapterType,
            base_url: baseURL.trim(),
            api_key: apiKey,
          })
        : api.put(`/api/v1/admin/upstreams/${existing?.id}`, {
            name: name.trim(),
            base_url: baseURL.trim(),
            api_key: apiKey,
            enabled,
          }),
    onSuccess: () => {
      toast(isNew ? t('adminUpstreams.createdToast') : t('adminUpstreams.updatedToast'));
      onSaved();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  if (!value) return null;

  return (
    <Drawer
      open
      onClose={onClose}
      title={isNew ? t('adminUpstreams.add') : t('adminUpstreams.drawerEditTitle', { name: existing?.name ?? '' })}
    >
      <Field label={t('adminUpstreams.colName')} required error={nameError} hint={t('adminUpstreams.nameHint')}>
        <input
          className="input"
          value={name}
          onChange={(event) => setName(event.target.value)}
          onBlur={() => setTouched(true)}
          aria-invalid={Boolean(nameError)}
          placeholder={t('adminUpstreams.namePlaceholder')}
        />
      </Field>

      <Field
        label={t('adminUpstreams.providerLabel')}
        hint={
          isNew
            ? ADAPTER_HELP_KEYS[adapterType]
              ? t(ADAPTER_HELP_KEYS[adapterType])
              : t('adminUpstreams.providerHintDefault')
            : t('adminUpstreams.providerHintLocked')
        }
      >
        <select className="select" value={adapterType} onChange={(event) => setAdapterType(event.target.value)} disabled={!isNew}>
          {adapterTypes.map((type) => (
            <option key={type} value={type}>
              {titleCase(type)}
            </option>
          ))}
        </select>
      </Field>

      <Field label={t('adminUpstreams.colBaseUrl')} required error={urlError} hint={t('adminUpstreams.baseUrlHint')}>
        <input
          className="input"
          value={baseURL}
          onChange={(event) => setBaseURL(event.target.value)}
          onBlur={() => setTouched(true)}
          aria-invalid={Boolean(urlError)}
          placeholder="https://api.openai.com"
          inputMode="url"
        />
      </Field>

      <Field
        label={isNew ? t('adminUpstreams.credentialLabel') : t('adminUpstreams.replaceCredentialLabel')}
        hint={
          isNew
            ? t('adminUpstreams.credentialHintNew')
            : t('adminUpstreams.credentialHintExisting', {
                mask: existing?.api_key_mask || t('adminUpstreams.credentialUnset'),
              })
        }
      >
        <input
          className="input"
          type="password"
          value={apiKey}
          onChange={(event) => setApiKey(event.target.value)}
          placeholder={isNew ? 'sk-…' : t('adminUpstreams.credentialPlaceholderKeep')}
          autoComplete="new-password"
        />
      </Field>

      {!isNew ? (
        <label className="switch">
          <input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
          <span>{t('adminUpstreams.enabledSwitch')}</span>
        </label>
      ) : null}

      <div className="row" style={{ marginTop: 'var(--janus-space-4)' }}>
        <button
          type="button"
          className="btn btn-primary"
          onClick={() => {
            setTouched(true);
            if (valid) save.mutate();
          }}
          disabled={save.isPending}
        >
          {save.isPending ? t('tables.saving') : isNew ? t('adminUpstreams.createAndDiscover') : t('adminUpstreams.saveChanges')}
        </button>
        <button type="button" className="btn" onClick={onClose} disabled={save.isPending}>
          {t('common.cancel')}
        </button>
      </div>

      {existing ? (
        <p className="small muted">
          {t('adminUpstreams.lastChecked', {
            time: existing.last_check_at ? formatDateTime(existing.last_check_at) : t('adminUpstreams.never'),
          })}
          {existing.last_error ? ` — ${existing.last_error}` : ''}
        </p>
      ) : null}

      {existing && !localOnly ? (
        <section aria-label={t('adminUpstreams.rateCardsTitle')} style={{ marginTop: 'var(--janus-space-4)' }}>
          <h3>{t('adminUpstreams.rateCardsTitle')}</h3>
          <p className="small muted">{t('adminUpstreams.rateCardsIntro')}</p>
          <AsyncSection
            query={models}
            empty={{
              when: (data) => data.models.length === 0,
              title: t('adminUpstreams.rateCardsEmptyTitle'),
              body: t('adminUpstreams.rateCardsEmptyBody'),
            }}
          >
            {(data) => (
              <Collection
                name="Upstream models"
                rows={data.models}
                rowKey={(model) => model.id}
                columns={[
                  { id: 'name', label: t('tables.model'), value: (model) => model.name, render: (model) => <>{model.name}</> },
                  {
                    id: 'input',
                    label: t('adminModels.colInputRate'),
                    value: (model) => model.rate_in_nanousd,
                    render: (model) => <>{formatModelRate(model, 'rate_in_nanousd')}</>,
                  },
                  {
                    id: 'output',
                    label: t('adminModels.colOutputRate'),
                    value: (model) => model.rate_out_nanousd,
                    render: (model) => <>{formatModelRate(model, 'rate_out_nanousd')}</>,
                  },
                  {
                    id: 'actions',
                    label: t('tables.actions'),
                    render: (model) => (
                      <>
                        <button type="button" className="btn btn-ghost btn-sm" onClick={() => onEditRates(model)}>
                          {t('adminUpstreams.editRates')}
                        </button>
                      </>
                    ),
                  },
                ]}
              />
            )}
          </AsyncSection>
        </section>
      ) : null}
    </Drawer>
  );
}
