import { useState, type ReactNode } from 'react';
import { Link, useLocation, useNavigate, useParams } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, qs } from '../../lib/api';
import type { AdminUserDetail, AdminUserRow, Group, Team } from '../../lib/types';
import { formatNumber, formatRelative, formatUSD, statusTone, titleCase } from '../../lib/format';
import { AreaChart, type MetricKey } from '../../components/charts';
import { ChargedTeam } from '../../components/ChargedTeam';
import { AsyncSection, Badge, ConfirmDialog, Drawer, EmptyState, Field, Modal, Pagination, useToast } from '../../components/ui';
import { useDebounced, useUrlState, useUrlStateBatch } from '../../lib/hooks';
import { DetailRow, FilterSelect, SearchInput, SortHeader, TokenSizeFilter, tokenSizeParams } from '../shared';
import { RetainedPanel } from '../../components/RouteTopNav';
import { PeopleNavigation } from './PeopleNavigation';
import { LegacyWorkspaceRedirect } from '../../app/LegacyWorkspaceRedirect';
import { useLocalOnly, useMe } from '../../app/session';
import { t } from '../../lib/i18n';
import { CreateLocalUserModal, ResetPasswordModal } from './LocalUserDialogs';

const PAGE_SIZE = 50;

/** sortable fields on GET /admin/v1/users. */
type UserSortField = 'email' | 'name' | 'created' | 'last_login';

/**
 * The users list sort convention shared by the URL and the API:
 * `<field>_<asc|desc>` (e.g. `email_asc`, `last_login_desc`). An absent or
 * unknown value means the backend default, email ascending.
 */
function parseUserSort(sort: string): { field: UserSortField; dir: 'asc' | 'desc' } {
  const dir = sort.endsWith('_desc') ? 'desc' : 'asc';
  const field = sort.replace(/_(asc|desc)$/, '');
  if (field === 'name' || field === 'created' || field === 'last_login') return { field, dir };
  return { field: 'email', dir };
}

/**
 * Sortable users-table header: shows ↑/↓ on the active sort column and toggles
 * direction on click (an inactive column starts ascending). Kept local because
 * the users list speaks the explicit `<field>_<dir>` convention rather than
 * the bare-key-means-descending convention of the shared <SortHeader>.
 */
function SortableColumn({
  label,
  field,
  sort,
  onSort,
}: {
  label: string;
  field: UserSortField;
  sort: string;
  onSort: (next: string) => void;
}): ReactNode {
  const current = parseUserSort(sort);
  const active = current.field === field;
  return (
    <th scope="col" aria-sort={active ? (current.dir === 'asc' ? 'ascending' : 'descending') : 'none'}>
      <button type="button" onClick={() => onSort(`${field}_${active && current.dir === 'asc' ? 'desc' : 'asc'}`)}>
        {label}
        <span aria-hidden="true" style={{ opacity: active ? 1 : 0.3 }}>
          {active && current.dir === 'desc' ? '↓' : '↑'}
        </span>
      </button>
    </th>
  );
}

export function PeoplePage(): ReactNode {
  const [tab] = useUrlState<'users' | 'groups' | 'teams'>('tab', 'users');
  if (tab === 'teams') return <LegacyWorkspaceRedirect />;
  const active = tab === 'groups' ? 'groups' : 'users';
  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminPeople.title')}</h1>
          <p className="page-subtitle">Manage accounts, identity groups, and organization teams.</p>
        </div>
      </header>
      <PeopleNavigation active={active} />
      <RetainedPanel active={active === 'users'}>
        <UsersTab />
      </RetainedPanel>
      <RetainedPanel active={active === 'groups'}>
        <GroupsTab />
      </RetainedPanel>
    </div>
  );
}

function UsersTab(): ReactNode {
  const me = useMe();
  const localOnly = useLocalOnly();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [search] = useUrlState('q', '');
  const [role] = useUrlState('role', '');
  const [active] = useUrlState('active', '');
  // group_id / team_id / sort mirror the API's query parameters one-to-one so
  // a filtered+sorted view is a shareable, refresh-safe deep link.
  const [groupId] = useUrlState('group_id', '');
  const [teamId] = useUrlState('team_id', '');
  const [sort] = useUrlState('sort', '');
  const [page, setPage] = useUrlState('page', '0');
  // Filter/search changes must atomically reset the page offset, otherwise a
  // user standing on page ≥2 who narrows the list lands on an out-of-range
  // offset and gets an empty page.
  const batchParams = useUrlStateBatch();
  const setSearch = (next: string) => batchParams({ q: next, page: null });
  const setRole = (next: string) => batchParams({ role: next, page: null });
  const setActive = (next: string) => batchParams({ active: next, page: null });
  const setGroupId = (next: string) => batchParams({ group_id: next, page: null });
  const setTeamId = (next: string) => batchParams({ team_id: next, page: null });
  // Re-sorting rearranges the whole result set, so the offset resets too.
  const setSort = (next: string) => batchParams({ sort: next, page: null });
  const { field: sortField, dir: sortDir } = parseUserSort(sort);
  // The open detail drawer is URL state (/admin/users/:id), not component
  // state: deep links are shareable and the drawer survives a refresh.
  const navigate = useNavigate();
  const location = useLocation();
  const { id: detailId } = useParams();
  const openDetail = (id: string) => navigate({ pathname: `/admin/users/${id}`, search: location.search });
  const closeDetail = () => navigate({ pathname: '/admin/users', search: location.search });
  const [pendingChange, setPendingChange] = useState<{ user: AdminUserRow; role?: string; is_active?: boolean } | null>(null);
  const [pendingDelete, setPendingDelete] = useState<AdminUserRow | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const [resetUser, setResetUser] = useState<AdminUserRow | null>(null);
  const resetTotp = useMutation({
    mutationFn: (user: AdminUserRow) => api.del<{ enabled: boolean }>(`/api/v1/admin/users/${user.id}/totp`).then(() => user),
    onSuccess: (user) => toast(t('adminPeople.resetTotpDone', { name: user.email })),
    onError: (error: Error) => toast(error.message, 'danger'),
  });
  const debounced = useDebounced(search);
  const offset = Number.parseInt(page, 10) || 0;

  // Each AdminUserRow returned by this endpoint includes tokens_out_30d
  // (30-day aggregated output tokens), rendered in the "Tokens out" column below.
  const users = useQuery({
    queryKey: ['admin', 'users', debounced, role, active, groupId, teamId, sort, offset],
    queryFn: () =>
      api.get<{ users: AdminUserRow[]; total_count: number }>(
        `/api/v1/admin/users${qs({ search: debounced, role, active, group_id: groupId, team_id: teamId, sort, limit: PAGE_SIZE, offset })}`,
      ),
  });

  // Group/team rosters feed the filter dropdowns. A failure here only leaves
  // the dropdown with its "All …" option — the users list itself still loads.
  const groups = useQuery({
    queryKey: ['admin', 'groups'],
    queryFn: () => api.get<{ groups: Group[] }>('/api/v1/admin/groups'),
  });
  const teams = useQuery({
    queryKey: ['admin', 'teams'],
    queryFn: () => api.get<{ teams: Team[] }>('/api/v1/admin/teams'),
  });

  const patch = useMutation({
    mutationFn: ({ id, body }: { id: string; body: Record<string, unknown> }) => api.patch(`/api/v1/admin/users/${id}`, body),
    onSuccess: () => {
      toast(t('adminPeople.updatedToast'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'users'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/v1/admin/users/${id}`),
    onSuccess: () => {
      toast(t('adminPeople.deletedToast'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'users'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  return (
    <>
      <div className="row wrap">
        <SearchInput
          value={search}
          onChange={setSearch}
          placeholder={t('adminPeople.searchPlaceholder')}
          label={t('adminPeople.searchLabel')}
        />
        <FilterSelect
          label={t('adminPeople.role')}
          value={role}
          onChange={setRole}
          options={[
            { value: '', label: t('adminPeople.allRoles') },
            { value: 'user', label: t('adminPeople.roleUser') },
            { value: 'team_lead', label: t('adminPeople.roleTeamLead') },
            { value: 'admin', label: t('adminPeople.roleAdmin') },
          ]}
        />
        <FilterSelect
          label={t('adminPeople.groupFilterLabel')}
          value={groupId}
          onChange={setGroupId}
          options={[
            { value: '', label: t('adminPeople.allGroups') },
            ...(groups.data?.groups ?? []).map((group) => ({ value: group.id, label: group.name })),
          ]}
        />
        <FilterSelect
          label={t('adminPeople.teamFilterLabel')}
          value={teamId}
          onChange={setTeamId}
          options={[
            { value: '', label: t('adminPeople.allTeams') },
            ...(teams.data?.teams ?? []).map((team) => ({ value: team.id, label: team.name })),
          ]}
        />
        <FilterSelect
          label={t('tables.status')}
          value={active}
          onChange={setActive}
          options={[
            { value: '', label: t('adminPeople.allAccounts') },
            { value: 'active', label: t('adminPeople.active') },
            { value: 'inactive', label: t('adminPeople.disabled') },
          ]}
        />
        <FilterSelect
          label={t('adminPeople.sortLabel')}
          value={sortField}
          onChange={(next) => setSort(`${next}_${sortDir}`)}
          options={[
            { value: 'email', label: t('adminPeople.sortByEmail') },
            { value: 'name', label: t('adminPeople.sortByName') },
            { value: 'created', label: t('adminPeople.sortByCreated') },
            { value: 'last_login', label: t('adminPeople.sortByLastLogin') },
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
        <button
          type="button"
          className="btn btn-primary btn-sm"
          style={{ marginLeft: 'auto' }}
          onClick={() => setCreateOpen(true)}
        >
          {t('adminPeople.addUser')}
        </button>
      </div>

      <section className="card card-flush">
        <AsyncSection
          query={users}
          empty={{
            when: (data) => data.total_count === 0 && !debounced && !role && !active && !groupId && !teamId,
            title: t('adminPeople.emptyTitle'),
            body: t('adminPeople.emptyBody'),
          }}
        >
          {(data) =>
            data.users.length === 0 ? (
              <EmptyState title={t('adminPeople.noMatchTitle')} body={t('adminPeople.noMatchBody')} />
            ) : (
              <>
                <div className="table-wrap">
                  <table className="data">
                    <thead>
                      <tr>
                        <SortableColumn label={t('adminPeople.colName')} field="name" sort={sort} onSort={setSort} />
                        <SortableColumn label={t('adminPeople.colEmail')} field="email" sort={sort} onSort={setSort} />
                        <th scope="col">{t('adminPeople.role')}</th>
                        <th scope="col">{t('adminPeople.colGroups')}</th>
                        <SortableColumn label={t('adminPeople.colCreated')} field="created" sort={sort} onSort={setSort} />
                        <SortableColumn label={t('adminPeople.colLastSignIn')} field="last_login" sort={sort} onSort={setSort} />
                        {localOnly ? null : <th scope="col">{t('adminPeople.colSpend30d')}</th>}
                        <th scope="col">{t('adminPeople.colTokensOut')}</th>
                        <th scope="col">{t('tables.status')}</th>
                        <th scope="col">
                          <span className="sr-only">{t('tables.actions')}</span>
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {data.users.map((user) => (
                        <tr key={user.id}>
                          <td>
                            <button
                              type="button"
                              className="btn btn-ghost btn-sm"
                              style={{ padding: 0, minHeight: 'auto', textAlign: 'left' }}
                              onClick={() => openDetail(user.id)}
                            >
                              {user.name || user.email}
                            </button>
                          </td>
                          <td className="small muted">{user.email}</td>
                          <td>
                            <select
                              className="select"
                              value={user.role}
                              style={{ minHeight: 32, padding: '4px 8px', fontSize: 'var(--janus-text-xs)' }}
                              onChange={(event) => setPendingChange({ user, role: event.target.value })}
                              disabled={user.id === me?.id}
                              aria-label={t('adminPeople.roleFor', { name: user.email })}
                            >
                              <option value="user">{t('adminPeople.roleUser')}</option>
                              <option value="team_lead">{t('adminPeople.roleTeamLead')}</option>
                              <option value="admin">{t('adminPeople.roleAdmin')}</option>
                            </select>
                          </td>
                          <td className="small muted truncate" style={{ maxWidth: 200 }}>
                            {user.groups.length ? user.groups.join(', ') : '—'}
                          </td>
                          <td className="small muted">{formatRelative(user.created_at)}</td>
                          <td className="small muted">{formatRelative(user.last_login_at)}</td>
                          {localOnly ? null : <td className="num small">{formatUSD(user.spend_30d_usd * 1_000_000_000)}</td>}
                          <td className="num small">{formatNumber(user.tokens_out_30d)}</td>
                          <td>
                            {user.is_active ? (
                              <Badge tone="success" dot>
                                {t('adminPeople.active')}
                              </Badge>
                            ) : (
                              <Badge tone="danger">{t('adminPeople.disabled')}</Badge>
                            )}
                          </td>
                          <td style={{ textAlign: 'right' }}>
                            <button
                              type="button"
                              className="btn btn-ghost btn-sm"
                              onClick={() => setPendingChange({ user, is_active: !user.is_active })}
                              disabled={user.id === me?.id}
                            >
                              {user.is_active ? t('adminPeople.disable') : t('adminPeople.enable')}
                            </button>
                            <button type="button" className="btn btn-ghost btn-sm" onClick={() => setResetUser(user)}>
                              {t('adminPeople.resetPassword')}
                            </button>
                            <button
                              type="button"
                              className="btn btn-ghost btn-sm"
                              onClick={() => resetTotp.mutate(user)}
                              disabled={user.id === me?.id}
                            >
                              {t('adminPeople.resetTotp')}
                            </button>
                            <button
                              type="button"
                              className="btn btn-ghost btn-sm"
                              onClick={() => setPendingDelete(user)}
                              disabled={user.id === me?.id}
                            >
                              {t('tables.delete')}
                            </button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                <Pagination
                  offset={offset}
                  limit={PAGE_SIZE}
                  total={data.total_count}
                  onChange={(next) => setPage(String(next))}
                />
              </>
            )
          }
        </AsyncSection>
      </section>

      <UserDetailDrawer userId={detailId ?? null} onClose={closeDetail} />

      <ConfirmDialog
        open={Boolean(pendingChange)}
        onClose={() => setPendingChange(null)}
        onConfirm={() => {
          if (pendingChange) {
            const body: Record<string, unknown> = {};
            if (pendingChange.role !== undefined) body.role = pendingChange.role;
            if (pendingChange.is_active !== undefined) body.is_active = pendingChange.is_active;
            patch.mutate({ id: pendingChange.user.id, body });
          }
          setPendingChange(null);
        }}
        danger={pendingChange?.is_active === false || pendingChange?.role === 'admin'}
        title={
          pendingChange?.role
            ? t('adminPeople.changeRoleTitle', {
                name: pendingChange.user.name || pendingChange.user.email,
                role: titleCase(pendingChange.role),
              })
            : pendingChange?.is_active
              ? t('adminPeople.reEnableTitle', { name: pendingChange.user.name || pendingChange.user.email })
              : t('adminPeople.disableTitle', { name: pendingChange?.user.name || pendingChange?.user.email || '' })
        }
        consequence={
          pendingChange?.role === 'admin'
            ? t('adminPeople.adminConsequence')
            : pendingChange?.role
              ? t('adminPeople.roleConsequence')
              : pendingChange?.is_active
                ? t('adminPeople.enableConsequence')
                : t('adminPeople.disableConsequence')
        }
        confirmLabel={t('adminPeople.applyChange')}
        busy={patch.isPending}
      />

      <ConfirmDialog
        open={Boolean(pendingDelete)}
        onClose={() => setPendingDelete(null)}
        onConfirm={() => {
          if (pendingDelete) remove.mutate(pendingDelete.id);
          setPendingDelete(null);
        }}
        danger
        title={t('adminPeople.deleteTitle', { name: pendingDelete?.name || pendingDelete?.email || '' })}
        consequence={t('adminPeople.deleteConsequence')}
        confirmLabel={t('adminPeople.deleteConfirm')}
        busy={remove.isPending}
      />
      <CreateLocalUserModal open={createOpen} onClose={() => setCreateOpen(false)} />
      <ResetPasswordModal user={resetUser} onClose={() => setResetUser(null)} />
    </>
  );
}

// AdminUserDetail in lib/types keeps the drawer and user list on one contract.

function UserDetailDrawer({ userId, onClose }: { userId: string | null; onClose: () => void }): ReactNode {
  const localOnly = useLocalOnly();
  // Local-only mode has no spend surface, so the drawer graph opens on
  // requests instead of cost there.
  const [metric, setMetric] = useState<MetricKey>(localOnly ? 'requests' : 'cost');
  // Recent-requests filters and sort are drawer-local: they narrow the 25-row
  // window the detail endpoint returns (server-side, via the same
  // tokens_in_gt / tokens_out_lt / requests_sort vocabulary the request log
  // speaks) and must not collide with the users table's own ?sort= param.
  const [tokensIn, setTokensIn] = useState('');
  const [tokensOut, setTokensOut] = useState('');
  const [requestsSort, setRequestsSort] = useState('time');
  const requestParams = {
    ...tokenSizeParams('tokens_in', tokensIn),
    ...tokenSizeParams('tokens_out', tokensOut),
    requests_sort: requestsSort === 'time' ? '' : requestsSort,
  };
  const detail = useQuery({
    queryKey: ['admin', 'user', userId, tokensIn, tokensOut, requestsSort],
    queryFn: () => api.get<AdminUserDetail>(`/api/v1/admin/users/${userId}${qs(requestParams)}`),
    enabled: Boolean(userId),
    // Refetching after a filter change must not blank the rest of the drawer.
    placeholderData: (previous) => previous,
  });
  const hasRequestFilters = Boolean(tokensIn || tokensOut);

  if (!userId) return null;

  return (
    <Drawer open onClose={onClose} title={detail.data?.user.name || detail.data?.user.email || t('adminPeople.personFallback')}>
      <AsyncSection query={detail}>
        {(data) => (
          <>
            <div className="stack">
              <DetailRow label={t('adminPeople.email')}>{data.user.email}</DetailRow>
              <DetailRow label={t('adminPeople.role')}>{titleCase(data.user.role)}</DetailRow>
              <DetailRow label={t('tables.status')}>
                {data.user.is_active ? (
                  <Badge tone="success">{t('adminPeople.active')}</Badge>
                ) : (
                  <Badge tone="danger">{t('adminPeople.disabled')}</Badge>
                )}
              </DetailRow>
              <DetailRow label={t('adminPeople.colGroups')}>{data.groups.length ? data.groups.join(', ') : '—'}</DetailRow>
              <DetailRow label={t('adminPeople.teams')}>
                {data.teams.length ? data.teams.map((team) => team.name).join(', ') : '—'}
              </DetailRow>
              <DetailRow label={t('adminPeople.colLastSignIn')}>{formatRelative(data.user.last_login_at)}</DetailRow>
            </div>

            <section>
              <div
                className="row-between"
                style={{ marginBottom: 'var(--janus-space-3)', flexWrap: 'wrap', gap: 'var(--janus-space-2)' }}
              >
                <h3 style={{ margin: 0 }}>{t('adminPeople.usageSection')}</h3>
                <div className="segmented" role="group" aria-label={t('tables.metric')}>
                  {(['cost', 'requests', 'tokens'] as const).map((key) => (
                    <button key={key} type="button" aria-pressed={metric === key} onClick={() => setMetric(key)}>
                      {key === 'cost' ? t('dashboard.spend') : key === 'tokens' ? t('tables.tokens') : t('dashboard.requests')}
                    </button>
                  ))}
                </div>
              </div>
              {data.usage_series.some(
                (point) =>
                  point.totals.request_count > 0 ||
                  point.totals.tokens_in + point.totals.tokens_out > 0 ||
                  point.totals.cost_nanousd > 0,
              ) ? (
                <AreaChart series={data.usage_series} metric={metric} height={160} />
              ) : (
                <p className="small muted">{t('adminPeople.noUsageInRange')}</p>
              )}
            </section>

            <section>
              <h3 style={{ marginBottom: 'var(--janus-space-3)' }}>{t('adminPeople.effectiveAccess')}</h3>
              {data.effective_grants.length === 0 ? (
                <p className="small muted">{t('adminPeople.noGrantsApply')}</p>
              ) : (
                <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0, gap: 6 }}>
                  {data.effective_grants.map((grant) => (
                    <li key={`${grant.model}-${grant.source}`} className="row-between small">
                      <span className="truncate">
                        {grant.model} <span className="muted">{t('adminPeople.onUpstream', { name: grant.upstream })}</span>
                      </span>
                      <Badge tone={grant.status === 'enabled' ? 'primary' : 'neutral'}>
                        {t('adminPeople.viaSource', { name: grant.source })}
                      </Badge>
                    </li>
                  ))}
                </ul>
              )}
            </section>

            <section>
              <h3 style={{ marginBottom: 'var(--janus-space-3)' }}>{t('adminPeople.tokensSection')}</h3>
              {data.tokens.length === 0 ? (
                <p className="small muted">{t('adminPeople.noTokensIssued')}</p>
              ) : (
                <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0, gap: 6 }}>
                  {data.tokens.map((token) => (
                    <li key={token.id} className="row-between small">
                      <span className="truncate">{token.description || t('adminPeople.untitled')}</span>
                      <span className="mono muted">{token.prefix}…</span>
                    </li>
                  ))}
                </ul>
              )}
            </section>

            <section>
              <h3 style={{ marginBottom: 'var(--janus-space-3)' }}>{t('adminPeople.quotasSection')}</h3>
              {data.quotas.length === 0 ? (
                <p className="small muted">{t('adminPeople.noQuotasApply')}</p>
              ) : (
                <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0, gap: 6 }}>
                  {data.quotas.map((quota) => (
                    <li key={quota.id} className="row-between small">
                      <span>
                        {quota.metric_label} {quota.window_label}
                      </span>
                      <Badge tone={quota.percent >= 100 ? 'danger' : quota.percent >= 80 ? 'warning' : 'success'}>
                        {Math.round(quota.percent)}%
                      </Badge>
                    </li>
                  ))}
                </ul>
              )}
            </section>

            <section>
              <div className="row-between" style={{ marginBottom: 'var(--janus-space-3)' }}>
                <h3 style={{ margin: 0 }}>{t('adminPeople.recentRequestsSection')}</h3>
                <Link className="small" to={`/admin/requests?user=${userId}`}>
                  {t('adminPeople.viewAllRequests')}
                </Link>
              </div>
              <div className="row wrap" style={{ marginBottom: 'var(--janus-space-3)' }}>
                <TokenSizeFilter direction="in" value={tokensIn} onChange={setTokensIn} />
                <TokenSizeFilter direction="out" value={tokensOut} onChange={setTokensOut} />
                {hasRequestFilters ? (
                  <button
                    type="button"
                    className="btn btn-ghost btn-sm"
                    onClick={() => {
                      setTokensIn('');
                      setTokensOut('');
                    }}
                  >
                    {t('tables.clearFilters')}
                  </button>
                ) : null}
              </div>
              {data.recent_requests.length === 0 ? (
                <p className="small muted">
                  {hasRequestFilters ? t('adminPeople.noRecentRequestsMatch') : t('adminPeople.noRecentRequests')}
                </p>
              ) : (
                <div className="table-wrap">
                  <table className="data" aria-label={t('adminPeople.recentRequestsSection')}>
                    <thead>
                      <tr>
                        <SortHeader label={t('tables.time')} sortKey="time" active={requestsSort} onSort={setRequestsSort} />
                        <th scope="col">{t('tables.model')}</th>
                        <th scope="col">Charged to</th>
                        <th scope="col">{t('tables.status')}</th>
                        <SortHeader
                          label={t('tables.tokensIn')}
                          sortKey="tokens_in"
                          active={requestsSort}
                          onSort={setRequestsSort}
                        />
                        <SortHeader
                          label={t('tables.tokensOut')}
                          sortKey="tokens_out"
                          active={requestsSort}
                          onSort={setRequestsSort}
                        />
                        {localOnly ? null : (
                          <SortHeader label={t('tables.cost')} sortKey="cost" active={requestsSort} onSort={setRequestsSort} />
                        )}
                      </tr>
                    </thead>
                    <tbody>
                      {data.recent_requests.map((request) => (
                        <tr key={request.id}>
                          <td className="small muted">{formatRelative(request.created_at)}</td>
                          <td className="small truncate">{request.model || '—'}</td>
                          <td className="small">
                            <ChargedTeam event={request} />
                          </td>
                          <td>
                            <Badge tone={statusTone(request.http_status)}>{request.http_status}</Badge>
                          </td>
                          <td className="num small">{formatNumber(request.tokens_in)}</td>
                          <td className="num small">{formatNumber(request.tokens_out)}</td>
                          {localOnly ? null : <td className="num small">{formatUSD(request.cost_nanousd)}</td>}
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </section>
          </>
        )}
      </AsyncSection>
    </Drawer>
  );
}

/**
 * Which identity-provider groups grant administrator access.
 *
 * Admin capability composes from three independent sources, OR'd and layered
 * so withdrawing one never collapses the others: the bootstrap email list
 * (environment, the failsafe), an explicit per-user grant, and membership of
 * a group listed here. Groups set via JANUS_ADMIN_GROUPS are shown too but
 * are not editable from the UI — they are the environment failsafe.
 */
function AdminGroupsCard(): ReactNode {
  const toast = useToast();
  const queryClient = useQueryClient();
  const [name, setName] = useState('');

  const adminGroups = useQuery({
    queryKey: ['admin', 'admin-groups'],
    queryFn: () => api.get<{ admin_groups: Array<{ name: string }>; env_admin_groups: string[] }>('/api/v1/admin/admin-groups'),
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['admin', 'admin-groups'] });
  const add = useMutation({
    mutationFn: () => api.post('/api/v1/admin/admin-groups', { name: name.trim() }),
    onSuccess: () => {
      toast(t('adminPeople.adminGroupAddedToast'));
      setName('');
      void invalidate();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });
  const remove = useMutation({
    mutationFn: (group: string) => api.del(`/api/v1/admin/admin-groups/${encodeURIComponent(group)}`),
    onSuccess: () => {
      toast(t('adminPeople.adminGroupRemovedToast'));
      void invalidate();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const managed = adminGroups.data?.admin_groups ?? [];
  const fromEnv = adminGroups.data?.env_admin_groups ?? [];

  return (
    <section className="card">
      <div className="card-header">
        <h2>{t('adminPeople.adminGroupsTitle')}</h2>
      </div>
      <p className="small muted">{t('adminPeople.adminGroupsBody')}</p>

      <div className="row wrap" style={{ gap: 'var(--janus-space-2)', alignItems: 'flex-end' }}>
        <Field label={t('adminPeople.adminGroupNameLabel')} hint={t('adminPeople.adminGroupNameHint')}>
          <input
            className="input"
            value={name}
            onChange={(event) => setName(event.target.value)}
            placeholder={t('adminPeople.adminGroupPlaceholder')}
          />
        </Field>
        <button type="button" className="btn btn-primary" disabled={!name.trim() || add.isPending} onClick={() => add.mutate()}>
          {add.isPending ? t('tables.saving') : t('adminPeople.addAdminGroup')}
        </button>
      </div>

      {managed.length === 0 && fromEnv.length === 0 ? (
        <p className="small muted">{t('adminPeople.noAdminGroups')}</p>
      ) : (
        <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0, gap: 6 }}>
          {managed.map((group) => (
            <li key={group.name} className="row-between small">
              <span className="mono">{group.name}</span>
              <button
                type="button"
                className="btn btn-ghost btn-sm"
                onClick={() => remove.mutate(group.name)}
                disabled={remove.isPending}
              >
                {t('tables.delete')}
              </button>
            </li>
          ))}
          {fromEnv.map((group) => (
            <li key={`env-${group}`} className="row-between small">
              <span className="mono">{group}</span>
              <span title={t('adminPeople.envGroupHint')}>
                <Badge tone="neutral">{t('adminPeople.fromEnvironment')}</Badge>
              </span>
            </li>
          ))}
        </ul>
      )}
      <p className="small muted">{t('adminPeople.adminGroupRemovalNote')}</p>
    </section>
  );
}

function GroupsTab(): ReactNode {
  const toast = useToast();
  const queryClient = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const [name, setName] = useState('');
  const [deleting, setDeleting] = useState<Group | null>(null);

  const groups = useQuery({
    queryKey: ['admin', 'groups'],
    queryFn: () => api.get<{ groups: Group[] }>('/api/v1/admin/groups'),
  });

  const create = useMutation({
    mutationFn: () => api.post('/api/v1/admin/groups', { name: name.trim() }),
    onSuccess: () => {
      toast(t('adminPeople.groupCreatedToast'));
      setName('');
      setCreateOpen(false);
      void queryClient.invalidateQueries({ queryKey: ['admin', 'groups'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/v1/admin/groups/${id}`),
    onSuccess: () => {
      toast(t('adminPeople.groupDeletedToast'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'groups'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  return (
    <>
      <div className="row-between">
        <p className="small muted" style={{ margin: 0 }}>
          {t('adminPeople.idpGroupsNote')}
        </p>
        <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
          {t('adminPeople.newGroup')}
        </button>
      </div>

      <AdminGroupsCard />

      <section className="card card-flush">
        <AsyncSection
          query={groups}
          empty={{
            when: (data) => data.groups.length === 0,
            title: t('adminPeople.groupsEmptyTitle'),
            body: t('adminPeople.groupsEmptyBody'),
          }}
        >
          {(data) => (
            <div className="table-wrap">
              <table className="data">
                <thead>
                  <tr>
                    <th scope="col">{t('adminPeople.colGroup')}</th>
                    <th scope="col">{t('adminPeople.colSource')}</th>
                    <th scope="col">{t('adminPeople.colMembers')}</th>
                    <th scope="col">
                      <span className="sr-only">{t('tables.actions')}</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {data.groups.map((group) => (
                    <tr key={group.id}>
                      <td>{group.name}</td>
                      <td>
                        {group.from_idp ? (
                          <Badge tone="info">{t('adminPeople.fromIdp')}</Badge>
                        ) : (
                          <Badge tone="neutral">{t('adminPeople.inApp')}</Badge>
                        )}
                      </td>
                      <td className="num small">{group.member_count}</td>
                      <td style={{ textAlign: 'right' }}>
                        {!group.from_idp ? (
                          <button type="button" className="btn btn-ghost btn-sm" onClick={() => setDeleting(group)}>
                            {t('tables.delete')}
                          </button>
                        ) : (
                          <span className="small muted">{t('adminPeople.readOnly')}</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </AsyncSection>
      </section>

      <Modal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        title={t('adminPeople.createGroupTitle')}
        description={t('adminPeople.createGroupDescription')}
        footer={
          <>
            <button type="button" className="btn" onClick={() => setCreateOpen(false)}>
              {t('common.cancel')}
            </button>
            <button
              type="button"
              className="btn btn-primary"
              onClick={() => create.mutate()}
              disabled={!name.trim() || create.isPending}
            >
              {t('adminPeople.createGroup')}
            </button>
          </>
        }
      >
        <Field label={t('adminPeople.groupName')} required>
          <input
            className="input"
            value={name}
            onChange={(event) => setName(event.target.value)}
            placeholder="ml-platform"
            autoFocus
          />
        </Field>
      </Modal>

      <ConfirmDialog
        open={Boolean(deleting)}
        onClose={() => setDeleting(null)}
        onConfirm={() => {
          if (deleting) remove.mutate(deleting.id);
          setDeleting(null);
        }}
        title={t('adminPeople.deleteGroupTitle', { name: deleting?.name ?? '' })}
        consequence={t('adminPeople.deleteGroupConsequence', { count: deleting?.member_count ?? 0 })}
        confirmLabel={t('adminPeople.deleteGroupConfirm')}
        busy={remove.isPending}
      />
    </>
  );
}
