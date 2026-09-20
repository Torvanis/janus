import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { Collection } from '../../components/Collection';
import { pageOffset } from '../../lib/collections';
import { useLocation } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import {
  isRenamedModel,
  publicModelName,
  type AdminUserRow,
  type Grant,
  type Group,
  type ManagedModelRow,
  type Model,
  type ServiceTokenRow,
} from '../../lib/types';
import { AsyncSection, Badge, Chevron, ConfirmDialog, EmptyState, Field, Modal, Pagination, useToast } from '../../components/ui';
import { useUrlState, useUrlStateBatch } from '../../lib/hooks';
import { t } from '../../lib/i18n';
import { SearchInput } from '../shared';

/** How the list is grouped: one card per model, or one card per grantee. */
type GrantsView = 'model' | 'grantee';

/** Grantee-type filter values; '' = every type. */
type GranteeTypeFilter = '' | Grant['grantee_type'];

/** Cards per page. Even a large fleet stays a short scroll. */
const GROUPS_PER_PAGE = 12;
/** Grantees/models previewed on a collapsed card before "+N more". */
const PREVIEW_COUNT = 4;
/** With this many groups or fewer, cards open by default. */
const AUTO_EXPAND_MAX_GROUPS = 3;

/** A stable id for one grantee across models ("user:u-1", "all_users:"). */
const granteeKey = (grant: Grant): string => `${grant.grantee_type}:${grant.grantee_id}`;

function granteeTypeLabel(type: Grant['grantee_type']): string {
  switch (type) {
    case 'team':
      return 'Team';
    case 'all_teams':
      return 'All teams (including future teams)';
    case 'all_users':
      return t('adminGrants.allUsers');
    case 'all_service_tokens':
      return t('adminGrants.allServiceTokens');
    case 'group':
      return t('adminGrants.group');
    case 'service_token':
      return t('adminGrants.serviceToken');
    case 'user':
      return t('adminGrants.person');
  }
}

function granteeTone(type: Grant['grantee_type']): 'warning' | 'primary' | 'info' | 'neutral' {
  switch (type) {
    case 'all_teams':
    case 'all_users':
    case 'all_service_tokens':
      return 'warning';
    case 'service_token':
      return 'info';
    case 'team':
    case 'group':
      return 'primary';
    case 'user':
      return 'neutral';
  }
}

interface GrantGroup {
  key: string;
  /** Heading of the card. */
  title: string;
  /** Secondary text next to the heading (native model name, grantee type). */
  subtitle?: string;
  badge?: { label: string; tone: 'warning' | 'primary' | 'info' | 'neutral' };
  grants: Grant[];
}

/**
 * Model grants, laid out to survive many grants: one collapsible card per
 * model (or per grantee), previewing who has access while collapsed, paged
 * twelve cards at a time, and filterable to a single grantee so "what can
 * person X call?" is one dropdown away. Grouping, filters and page live in
 * the URL so a view can be shared.
 */
export function GrantsPage(): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [search] = useUrlState('q', '');
  const [view] = useUrlState<GrantsView>('view', 'model');
  const [granteeFilter] = useUrlState('grantee', '');
  const [typeFilter] = useUrlState<GranteeTypeFilter>('type', '');
  const [pageParam] = useUrlState('page', '0');
  const [sizeParam] = useUrlState('size', String(GROUPS_PER_PAGE));
  const groupLimit = [12, 25, 50, 100].includes(Number(sizeParam)) ? Number(sizeParam) : GROUPS_PER_PAGE;
  // Several keys change in one click (a filter plus the page), so writes go
  // through the batch setter rather than one setter per key.
  const setUrl = useUrlStateBatch();
  const location = useLocation();
  const [createOpen, setCreateOpen] = useState(false);
  const [deleting, setDeleting] = useState<Grant | null>(null);
  // Per-card open/closed overrides on top of the automatic default; the
  // "expand/collapse all" buttons replace the default and reset overrides.
  const [expandMode, setExpandMode] = useState<'auto' | 'all' | 'none'>('auto');
  const [expandOverrides, setExpandOverrides] = useState<Record<string, boolean>>({});

  const grants = useQuery({
    queryKey: ['admin', 'grants'],
    queryFn: () => api.get<{ grants: Grant[] }>('/api/v1/admin/grants'),
  });

  // The grants API carries the native model name; display names come from the
  // models API. If this lookup fails the page degrades to native names rather
  // than blocking the grants list.
  const models = useQuery({
    queryKey: ['admin', 'models', 'for-grant-names'],
    queryFn: () => api.get<{ models: Model[] }>('/api/v1/admin/models'),
    staleTime: 60_000,
  });
  const modelById = useMemo(() => {
    const map = new Map<string, Model>();
    for (const model of models.data?.models ?? []) map.set(model.id, model);
    return map;
  }, [models.data]);
  const grantModelLabel = (grant: Grant): string => {
    const model = modelById.get(grant.model_id);
    return model ? publicModelName(model) : grant.model_name;
  };

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/v1/admin/grants/${id}`),
    onSuccess: () => {
      toast(t('adminGrants.removedToast'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'grants'] });
      void queryClient.invalidateQueries({ queryKey: ['models'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const allGrants = grants.data?.grants ?? [];

  // Every distinct grantee, for the "who" dropdown — grouped by kind so a
  // long list of people does not bury the handful of groups.
  const grantees = useMemo(() => {
    const seen = new Map<string, Grant>();
    for (const grant of allGrants) if (!seen.has(granteeKey(grant))) seen.set(granteeKey(grant), grant);
    const list = [...seen.values()].sort((a, b) => a.grantee_name.localeCompare(b.grantee_name));
    return {
      people: list.filter((g) => g.grantee_type === 'user'),
      teams: list.filter((g) => g.grantee_type === 'team'),
      groups: list.filter((g) => g.grantee_type === 'group'),
      services: list.filter((g) => g.grantee_type === 'service_token'),
      blanket: list.filter((g) => g.grantee_type === 'all_users' || g.grantee_type === 'all_service_tokens' || g.grantee_type === 'all_teams'),
    };
  }, [allGrants]);
  const focusedGrantee = granteeFilter ? allGrants.find((g) => granteeKey(g) === granteeFilter) : undefined;

  const term = search.trim().toLowerCase();
  const filtered = allGrants.filter((grant) => {
    if (granteeFilter && granteeKey(grant) !== granteeFilter) return false;
    if (typeFilter && grant.grantee_type !== typeFilter) return false;
    return (
      !term ||
      grant.model_name.toLowerCase().includes(term) ||
      grantModelLabel(grant).toLowerCase().includes(term) ||
      grant.grantee_name.toLowerCase().includes(term)
    );
  });

  // Group by the stable native model name (a rename must not split a model's
  // grants) or by grantee; the heading shows the display name when set.
  const groups = ((): GrantGroup[] => {
    const map = new Map<string, Grant[]>();
    for (const grant of filtered) {
      const key = view === 'model' ? grant.model_name : granteeKey(grant);
      map.set(key, [...(map.get(key) ?? []), grant]);
    }
    return [...map.entries()]
      .map(([key, list]): GrantGroup => {
        const first = list[0]!;
        const sorted = [...list].sort((a, b) =>
          view === 'model'
            ? a.grantee_type.localeCompare(b.grantee_type) || a.grantee_name.localeCompare(b.grantee_name)
            : grantModelLabel(a).localeCompare(grantModelLabel(b)),
        );
        if (view === 'model') {
          const label = grantModelLabel(first);
          return { key, title: label, subtitle: label !== key ? key : undefined, grants: sorted };
        }
        return {
          key,
          title: first.grantee_name,
          badge: { label: granteeTypeLabel(first.grantee_type), tone: granteeTone(first.grantee_type) },
          grants: sorted,
        };
      })
      .sort((a, b) => a.title.localeCompare(b.title));
  })();

  // Paging over groups (cards), not grants, so a card is never cut in half.
  const offset = pageOffset(pageParam, groupLimit, groups.length);
  const pageGroups = groups.slice(offset, offset + groupLimit);
  useEffect(() => {
    if (grants.isSuccess && pageParam !== String(offset)) setUrl({ page: offset ? String(offset) : null });
  }, [grants.isSuccess, pageParam, offset, setUrl]);

  const filtersActive = Boolean(term || granteeFilter || typeFilter);
  const defaultExpanded = groups.length <= AUTO_EXPAND_MAX_GROUPS || Boolean(granteeFilter);
  const isExpanded = (key: string): boolean =>
    expandOverrides[key] ?? (expandMode === 'all' ? true : expandMode === 'none' ? false : defaultExpanded);
  const toggle = (key: string) => setExpandOverrides((current) => ({ ...current, [key]: !isExpanded(key) }));
  const setAll = (mode: 'all' | 'none') => {
    setExpandMode(mode);
    setExpandOverrides({});
  };
  // A new grouping or filter set shows fresh cards: start from page one with
  // the automatic open/closed default.
  const changeFilters = (updates: Record<string, string | null>) => {
    const childPages = Object.fromEntries([...new URLSearchParams(location.search).keys()]
      .filter((key) => key.startsWith('grant-rows-') && key.endsWith('.page'))
      .map((key) => [key, null]));
    setUrl({ ...childPages, ...updates, page: null });
    setExpandMode('auto');
    setExpandOverrides({});
  };

  const totalLabel =
    view === 'model'
      ? t('adminGrants.groupCountModels', { shown: pageGroups.length, total: groups.length })
      : t('adminGrants.groupCountGrantees', { shown: pageGroups.length, total: groups.length });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminGrants.title')}</h1>
          <p className="page-subtitle">Access is explicit within each context. Personal, team, and service grants are separate; team access does not inherit personal or group grants.</p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
          {t('adminGrants.grantAccess')}
        </button>
      </header>

      <div className="row wrap grants-toolbar" data-collection-mode="complete-client">
        <label className="row">Groups per page
          <select className="select" value={groupLimit} onChange={(event) => setUrl({ size: event.target.value, page: null })}>
            {[12, 25, 50, 100].map((size) => <option key={size}>{size}</option>)}
          </select>
        </label>
        <span className="small muted">{filtered.length} of {allGrants.length} grants · {groups.length} matching groups · alphabetical order</span>
        <SearchInput
          value={search}
          onChange={(next) => changeFilters({ q: next || null })}
          placeholder={t('adminGrants.searchPlaceholder')}
          label={t('adminGrants.searchLabel')}
        />
        <div className="segmented" role="group" aria-label={t('adminGrants.viewLabel')}>
          <button type="button" aria-pressed={view === 'model'} onClick={() => changeFilters({ view: null })}>
            {t('adminGrants.viewByModel')}
          </button>
          <button type="button" aria-pressed={view === 'grantee'} onClick={() => changeFilters({ view: 'grantee' })}>
            {t('adminGrants.viewByGrantee')}
          </button>
        </div>
        <label className="row grants-filter">
          <span className="sr-only">{t('adminGrants.granteeFilterLabel')}</span>
          <select
            className="select"
            aria-label={t('adminGrants.granteeFilterLabel')}
            value={granteeFilter}
            onChange={(event) => changeFilters({ grantee: event.target.value || null })}
          >
            <option value="">{t('adminGrants.anyGrantee')}</option>
            {grantees.teams.length ? <optgroup label="Teams">{grantees.teams.map((g) => <option key={granteeKey(g)} value={granteeKey(g)}>{g.grantee_name}</option>)}</optgroup> : null}
            {(
              [
                ['granteeGroupPeople', grantees.people],
                ['granteeGroupGroups', grantees.groups],
                ['granteeGroupServices', grantees.services],
                ['granteeGroupBlanket', grantees.blanket],
              ] as const
            ).map(([labelKey, list]) =>
              list.length ? (
                <optgroup key={labelKey} label={t(`adminGrants.${labelKey}`)}>
                  {list.map((g) => (
                    <option key={granteeKey(g)} value={granteeKey(g)}>
                      {g.grantee_name}
                    </option>
                  ))}
                </optgroup>
              ) : null,
            )}
          </select>
        </label>
        <label className="row grants-filter">
          <span className="sr-only">{t('adminGrants.typeFilterLabel')}</span>
          <select
            className="select"
            aria-label={t('adminGrants.typeFilterLabel')}
            value={typeFilter}
            onChange={(event) => changeFilters({ type: event.target.value || null })}
          >
            <option value="">{t('adminGrants.anyType')}</option>
            <option value="team">Teams</option>
            <option value="all_teams">All teams (including future teams)</option>
            <option value="user">{t('adminGrants.typePeople')}</option>
            <option value="group">{t('adminGrants.typeGroups')}</option>
            <option value="service_token">{t('adminGrants.typeServiceTokens')}</option>
            <option value="all_users">{t('adminGrants.typeAllUsers')}</option>
            <option value="all_service_tokens">{t('adminGrants.typeAllServiceTokens')}</option>
          </select>
        </label>
        {filtersActive ? (
          <button
            type="button"
            className="btn btn-ghost btn-sm"
            onClick={() => changeFilters({ q: null, grantee: null, type: null })}
          >
            {t('adminGrants.clearFilters')}
          </button>
        ) : null}
      </div>

      <AsyncSection
        query={grants}
        empty={{
          when: (data) => data.grants.length === 0,
          title: t('adminGrants.emptyTitle'),
          body: t('adminGrants.emptyBody'),
          action: (
            <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
              {t('adminGrants.createFirst')}
            </button>
          ),
        }}
      >
        {() =>
          groups.length === 0 ? (
            <EmptyState
              title={t('adminGrants.noMatchTitle')}
              body={t('adminGrants.noMatchBody')}
              action={
                filtersActive ? (
                  <button type="button" className="btn" onClick={() => changeFilters({ q: null, grantee: null, type: null })}>
                    {t('adminGrants.clearFilters')}
                  </button>
                ) : undefined
              }
            />
          ) : (
            <div className="stack">
              {focusedGrantee ? (
                <div className="banner banner-info" role="status">
                  <div className="stack grants-focus">
                    <strong>{t('adminGrants.granteeFocusTitle', { name: focusedGrantee.grantee_name })}</strong>
                    {focusedGrantee.grantee_type === 'team' ? <span className="small muted">Only this team's explicit grants are shown. Personal, group, and all-teams grants are not included in this filter.</span> : null}
                    {focusedGrantee.grantee_type === 'user' ? (
                      <span className="small muted">{t('adminGrants.granteeFocusBody')}</span>
                    ) : null}
                  </div>
                </div>
              ) : null}
              <div className="row-between">
                <span className="small muted" data-testid="grants-group-count">
                  {totalLabel}
                </span>
                <div className="row">
                  <button type="button" className="btn btn-ghost btn-sm" onClick={() => setAll('all')}>
                    {t('adminGrants.expandAll')}
                  </button>
                  <button type="button" className="btn btn-ghost btn-sm" onClick={() => setAll('none')}>
                    {t('adminGrants.collapseAll')}
                  </button>
                </div>
              </div>

              {pageGroups.map((group) => {
                const open = isExpanded(group.key);
                const bodyId = `grants-group-${group.key.replace(/[^a-z0-9_-]/gi, '_')}`;
                const preview = group.grants.slice(0, PREVIEW_COUNT);
                return (
                  <section key={group.key} className="card grants-group" data-testid="grants-group">
                    <div className="card-header grants-group-head">
                      <button
                        type="button"
                        className="grants-group-toggle"
                        aria-expanded={open}
                        aria-controls={bodyId}
                        aria-label={
                          open
                            ? t('adminGrants.hideGrants', { name: group.title })
                            : t('adminGrants.showGrants', { name: group.title })
                        }
                        onClick={() => toggle(group.key)}
                      >
                        <Chevron open={open} className="grants-group-chevron" />
                        <h2 className="grants-group-title">
                          {group.title}
                          {group.subtitle ? (
                            <span className="small muted mono grants-group-subtitle">{group.subtitle}</span>
                          ) : null}
                        </h2>
                        {group.badge ? <Badge tone={group.badge.tone}>{group.badge.label}</Badge> : null}
                      </button>
                      <span className="small muted">
                        {view === 'model'
                          ? group.grants.length === 1
                            ? t('adminGrants.grantCountOne', { count: group.grants.length })
                            : t('adminGrants.grantCountMany', { count: group.grants.length })
                          : group.grants.length === 1
                            ? t('adminGrants.modelCountOne', { count: group.grants.length })
                            : t('adminGrants.modelCountMany', { count: group.grants.length })}
                      </span>
                    </div>
                    {open ? (
                      <Collection
                        name={`grant-rows-${view}-${encodeURIComponent(group.key)}`}
                        rows={group.grants}
                        rowKey={(grant) => grant.id}
                        resetKey={`${search}|${granteeFilter}|${typeFilter}|${view}`}
                        columns={[
                          { id: 'model', label: 'Model', value: grantModelLabel, render: grantModelLabel },
                          { id: 'native', label: 'Native model', value: (grant) => grant.model_name, render: (grant) => grant.model_name },
                          { id: 'grantee', label: 'Grantee', value: (grant) => grant.grantee_name, render: (grant) => grant.grantee_name },
                          { id: 'type', label: 'Type', value: (grant) => granteeTypeLabel(grant.grantee_type), render: (grant) => granteeTypeLabel(grant.grantee_type) },
                        ]}
                      >
                      {(rows) => <ul className="grants-list" id={bodyId}>
                        {rows.map((grant) => (
                          <li key={grant.id} className="row-between grants-row">
                            {view === 'model' ? (
                              <span className="row grants-row-main">
                                <Badge tone={granteeTone(grant.grantee_type)}>{granteeTypeLabel(grant.grantee_type)}</Badge>
                                <button
                                  type="button"
                                  className="grants-link"
                                  onClick={() => changeFilters({ grantee: granteeKey(grant) })}
                                  title={t('adminGrants.granteeFocusTitle', { name: grant.grantee_name })}
                                >
                                  {grant.grantee_name}
                                </button>
                              </span>
                            ) : (
                              <span className="row grants-row-main">
                                <span>{grantModelLabel(grant)}</span>
                                {grantModelLabel(grant) !== grant.model_name ? (
                                  <span className="small muted mono">{grant.model_name}</span>
                                ) : null}
                                {grant.model_kind === 'managed' ? <Badge tone="info">{t('adminGrants.alias')}</Badge> : null}
                              </span>
                            )}
                            <button type="button" className="btn btn-ghost btn-sm" onClick={() => setDeleting(grant)}>
                              {t('adminGrants.revoke')}
                            </button>
                          </li>
                        ))}
                      </ul>}
                      </Collection>
                    ) : (
                      <div className="row wrap grants-preview" id={bodyId}>
                        {preview.map((grant) =>
                          view === 'model' ? (
                            <Badge key={grant.id} tone={granteeTone(grant.grantee_type)}>
                              {grant.grantee_name}
                            </Badge>
                          ) : (
                            <Badge key={grant.id} tone="neutral">
                              {grantModelLabel(grant)}
                            </Badge>
                          ),
                        )}
                        {group.grants.length > PREVIEW_COUNT ? (
                          <span className="small muted">
                            {t('adminGrants.moreCount', { count: group.grants.length - PREVIEW_COUNT })}
                          </span>
                        ) : null}
                      </div>
                    )}
                  </section>
                );
              })}

              <Pagination
                offset={offset}
                limit={groupLimit}
                total={groups.length}
                onChange={(next) => setUrl({ page: next ? String(next) : null })}
              />
            </div>
          )
        }
      </AsyncSection>

      <CreateGrantModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onCreated={() => {
          setCreateOpen(false);
          void queryClient.invalidateQueries({ queryKey: ['admin', 'grants'] });
          void queryClient.invalidateQueries({ queryKey: ['models'] });
        }}
      />

      <ConfirmDialog
        open={Boolean(deleting)}
        onClose={() => setDeleting(null)}
        onConfirm={() => {
          if (deleting) remove.mutate(deleting.id);
          setDeleting(null);
        }}
        title={t('adminGrants.revokeTitle')}
        consequence={
          deleting?.grantee_type === 'all_users'
            ? t('adminGrants.revokeAllUsersConsequence', { name: grantModelLabel(deleting) })
            : t('adminGrants.revokeConsequence', {
                name: deleting?.grantee_name ?? '',
                model: deleting ? grantModelLabel(deleting) : '',
              })
        }
        confirmLabel={t('adminGrants.revokeConfirm')}
        busy={remove.isPending}
      />
    </div>
  );
}

function CreateGrantModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: () => void;
}): ReactNode {
  const toast = useToast();
  // A selection is "<kind>:<id>" so real models and managed aliases can share
  // one <select> without their ids colliding. They post to different
  // model_kind values, so the submit path splits them back apart.
  const [selection, setSelection] = useState<string[]>([]);
  const [granteeType, setGranteeType] = useState<Grant['grantee_type']>(
    'group',
  );
  const [granteeId, setGranteeId] = useState('');
  const [confirmBlanket, setConfirmBlanket] = useState(false);

  const models = useQuery({
    queryKey: ['admin', 'models', 'for-grants'],
    queryFn: () => api.get<{ models: Model[] }>('/api/v1/admin/models'),
    enabled: open,
  });
  const managed = useQuery({
    queryKey: ['admin', 'managed-models', 'for-grants'],
    queryFn: () => api.get<{ managed_models: ManagedModelRow[] }>('/api/v1/admin/managed-models'),
    enabled: open,
  });
  const groups = useQuery({
    queryKey: ['admin', 'groups'],
    queryFn: () => api.get<{ groups: Group[] }>('/api/v1/admin/groups'),
    enabled: open,
  });
  const teams = useQuery({
    queryKey: ['admin', 'teams'],
    queryFn: () => api.get<{ teams: { id: string; name: string }[] }>('/api/v1/admin/teams'),
    enabled: open,
  });
  const users = useQuery({
    queryKey: ['admin', 'users', 'for-grants'],
    queryFn: () => api.get<{ users: AdminUserRow[] }>('/api/v1/admin/users?limit=200'),
    enabled: open,
  });
  const serviceTokens = useQuery({
    queryKey: ['admin', 'service-tokens', 'for-grants'],
    queryFn: () => api.get<{ service_tokens: ServiceTokenRow[] }>('/api/v1/admin/service-tokens?status=active'),
    enabled: open,
  });

  // Only enabled models are offered. Granting a pending-approval or disabled
  // model produces a grant that cannot be exercised, and listing every
  // upstream's full catalog buries the handful an admin actually approves.
  const grantableModels = useMemo(() => (models.data?.models ?? []).filter((model) => model.status === 'enabled'), [models.data]);
  // Likewise for aliases: a disabled or broken alias cannot serve traffic, so
  // offering it would create an inert grant.
  const grantableManaged = useMemo(
    () => (managed.data?.managed_models ?? []).filter((m) => m.status === 'enabled' && !m.broken),
    [managed.data],
  );

  const create = useMutation({
    mutationFn: async () => {
      const realIds = selection.filter((s) => s.startsWith('model:')).map((s) => s.slice('model:'.length));
      const managedIds = selection.filter((s) => s.startsWith('managed:')).map((s) => s.slice('managed:'.length));
      const grantee = {
        grantee_type: granteeType,
        grantee_id: granteeType === 'all_users' || granteeType === 'all_service_tokens' || granteeType === 'all_teams' ? '' : granteeId,
      };
      // Two calls only when the admin mixed kinds in one selection; the
      // common single-kind case stays a single request.
      if (realIds.length) await api.post('/api/v1/admin/grants', { model_ids: realIds, model_kind: 'model', ...grantee });
      if (managedIds.length) await api.post('/api/v1/admin/grants', { model_ids: managedIds, model_kind: 'managed', ...grantee });
    },
    onSuccess: () => {
      toast(t('adminGrants.createdToast', { count: selection.length }));
      setSelection([]);
      setGranteeId('');
      onCreated();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const blanket = granteeType === 'all_users' || granteeType === 'all_service_tokens' || granteeType === 'all_teams';
  const valid = selection.length > 0 && (blanket || Boolean(granteeId));

  const submit = () => {
    if (!valid) return;
    // Both blanket grants are org-wide and worth a second look, but they are
    // different blast radii, so each gets its own wording.
    if (blanket) {
      setConfirmBlanket(true);
      return;
    }
    create.mutate();
  };

  return (
    <>
      <Modal
        open={open}
        onClose={onClose}
        title={t('adminGrants.createTitle')}
        description={t('adminGrants.createDescription')}
        footer={
          <>
            <button type="button" className="btn" onClick={onClose} disabled={create.isPending}>
              {t('common.cancel')}
            </button>
            <button type="button" className="btn btn-primary" onClick={submit} disabled={!valid || create.isPending}>
              {create.isPending ? t('adminGrants.granting') : t('adminGrants.grantAccess')}
            </button>
          </>
        }
      >
        <Field label={t('adminGrants.modelsLabel')} required hint={t('adminGrants.modelsHint')}>
          <select
            className="select"
            multiple
            size={9}
            value={selection}
            onChange={(event) => setSelection([...event.target.selectedOptions].map((option) => option.value))}
            style={{ minHeight: 190 }}
          >
            {grantableManaged.length ? (
              <optgroup label={t('adminGrants.managedGroupLabel')}>
                {grantableManaged.map((m) => (
                  <option key={m.id} value={`managed:${m.id}`}>
                    {m.name} · {t('adminGrants.managedResolvesTo', { name: m.target_public_name })}
                  </option>
                ))}
              </optgroup>
            ) : null}
            <optgroup label={t('adminGrants.modelsGroupLabel')}>
              {grantableModels.map((model) => (
                <option key={model.id} value={`model:${model.id}`}>
                  {publicModelName(model)}
                  {isRenamedModel(model) ? ` (${model.name})` : ''} · {model.upstream_name}
                </option>
              ))}
            </optgroup>
          </select>
        </Field>

        <Field label={t('adminGrants.grantTo')} required>
          <select
            className="select"
            value={granteeType}
            onChange={(event) => {
              setGranteeType(event.target.value as typeof granteeType);
              setGranteeId('');
            }}
          >
            <option value="team">A team</option>
            <option value="all_teams">All teams (including future teams)</option>
            <option value="group">{t('adminGrants.aGroup')}</option>
            <option value="user">{t('adminGrants.onePerson')}</option>
            <option value="all_users">{t('adminGrants.everyAuthenticatedUser')}</option>
            <option value="service_token">{t('adminGrants.oneService')}</option>
            <option value="all_service_tokens">{t('adminGrants.everyServiceToken')}</option>
          </select>
        </Field>

        {granteeType === 'team' ? (
          <Field label="Team" required>
            <select aria-label="Team" className="select" value={granteeId} onChange={(event) => setGranteeId(event.target.value)}>
              <option value="">Choose a team</option>
              {(teams.data?.teams ?? []).map((team) => <option key={team.id} value={team.id}>{team.name}</option>)}
            </select>
            {teams.isError ? <p role="alert">Teams could not be loaded. Try again before granting access.</p> : null}
          </Field>
        ) : null}

        {granteeType === 'group' ? (
          <Field label={t('adminGrants.groupLabel')} required>
            <select className="select" value={granteeId} onChange={(event) => setGranteeId(event.target.value)}>
              <option value="">{t('adminGrants.chooseGroup')}</option>
              {(groups.data?.groups ?? []).map((group) => (
                <option key={group.id} value={group.id}>
                  {group.name} (
                  {group.member_count === 1
                    ? t('adminGrants.memberCountOne', { count: group.member_count })
                    : t('adminGrants.memberCountMany', { count: group.member_count })}
                  ){group.from_idp ? ` · ${t('adminGrants.fromIdp')}` : ''}
                </option>
              ))}
            </select>
          </Field>
        ) : null}

        {granteeType === 'user' ? (
          <Field label={t('adminGrants.personLabel')} required>
            <select className="select" value={granteeId} onChange={(event) => setGranteeId(event.target.value)}>
              <option value="">{t('adminGrants.choosePerson')}</option>
              {(users.data?.users ?? []).map((user) => (
                <option key={user.id} value={user.id}>
                  {user.name || user.email} · {user.email}
                </option>
              ))}
            </select>
          </Field>
        ) : null}

        {granteeType === 'service_token' ? (
          <Field label={t('adminGrants.serviceLabel')} required hint={t('adminGrants.serviceHint')}>
            <select className="select" value={granteeId} onChange={(event) => setGranteeId(event.target.value)}>
              <option value="">{t('adminGrants.chooseService')}</option>
              {(serviceTokens.data?.service_tokens ?? []).map((token) => (
                <option key={token.id} value={token.id}>
                  {token.name}
                  {token.description ? ` · ${token.description}` : ''}
                </option>
              ))}
            </select>
          </Field>
        ) : null}
      </Modal>

      <ConfirmDialog
        open={confirmBlanket}
        onClose={() => setConfirmBlanket(false)}
        onConfirm={() => {
          setConfirmBlanket(false);
          create.mutate();
        }}
        danger={false}
        title={granteeType === 'all_teams' ? 'Grant access to all teams?' : granteeType === 'all_service_tokens' ? t('adminGrants.allServiceTokensTitle') : t('adminGrants.allUsersTitle')}
        consequence={
          granteeType === 'all_teams'
            ? 'All teams (including future teams) will have access to the selected models in team context.'
            : granteeType === 'all_service_tokens'
            ? t('adminGrants.allServiceTokensConsequence', { count: selection.length })
            : t('adminGrants.allUsersConsequence', { count: selection.length })
        }
        confirmLabel={t('adminGrants.allUsersConfirm')}
        busy={create.isPending}
      />
    </>
  );
}
