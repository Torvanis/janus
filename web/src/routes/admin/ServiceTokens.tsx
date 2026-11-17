import { Collection } from '../../components/Collection';
import { useMemo, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, useParams, useNavigate, useLocation } from 'react-router-dom';
import { api, qs } from '../../lib/api';
import { isLongRevoked, type ServiceTokenRow } from '../../lib/types';
import { formatNumber, formatRelative, formatUSD } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, EmptyState, Field, Modal, useToast } from '../../components/ui';
import { IconButton, RowActions } from '../../components/IconButton';
import { TopModel, Truncate } from '../../components/TableText';
import { useDebounced, useUrlState, useUrlStateBatch } from '../../lib/hooks';
import { FilterSelect, SearchInput } from '../shared';
import { useLocalOnly } from '../../app/session';
import { t } from '../../lib/i18n';

/**
 * Sortable fields on GET /admin/service-tokens, sharing the users table's
 * `<field>_<asc|desc>` convention so the two pages behave identically.
 */
type TokenSortField = 'name' | 'created' | 'last_used' | 'expires' | 'spend' | 'requests' | 'tokens' | 'errors' | 'grants';

const SORT_FIELDS: TokenSortField[] = [
  'name',
  'created',
  'last_used',
  'expires',
  'spend',
  'requests',
  'tokens',
  'errors',
  'grants',
];

function parseTokenSort(sort: string): { field: TokenSortField; dir: 'asc' | 'desc' } {
  const dir = sort.endsWith('_asc') ? 'asc' : 'desc';
  const field = sort.replace(/_(asc|desc)$/, '') as TokenSortField;
  // Default: costliest integration first — the reason an admin opens this page.
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
  field: TokenSortField;
  sort: string;
  onSort: (next: string) => void;
  numeric?: boolean;
}): ReactNode {
  const current = parseTokenSort(sort);
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

export function ServiceTokensPage(): ReactNode {
  const localOnly = useLocalOnly();
  const [issueOpen, setIssueOpen] = useState(false);
  const [revoking, setRevoking] = useState<ServiceTokenRow | null>(null);
  const { id } = useParams();
  const navigate = useNavigate();
  const location = useLocation();
  const setEditing = (token: ServiceTokenRow | null) =>
    navigate(`/admin/service-tokens${token ? `/${encodeURIComponent(token.id)}` : ''}${location.search}`);
  // Resolve detail separately: search/status filters must not hide a deep link.
  const detail = useQuery({
    queryKey: ['admin', 'service-tokens', 'detail', id],
    queryFn: () => api.get<{ service_token: ServiceTokenRow }>(`/api/v1/admin/service-tokens/${encodeURIComponent(id!)}`),
    enabled: !!id,
  });
  const editing = detail.data?.service_token ?? null;
  const [search] = useUrlState('q', '');
  const [status] = useUrlState('status', '');
  const [sort] = useUrlState('sort', '');
  const batchParams = useUrlStateBatch();
  const setSearch = (next: string) => batchParams({ q: next });
  const setStatus = (next: string) => batchParams({ status: next });
  const setSort = (next: string) => batchParams({ sort: next });
  // Same rule as the Tokens page: tokens revoked more than a week ago are
  // hidden until asked for, and the choice lives in the URL (?revoked=all).
  const [revokedView] = useUrlState<'' | 'all'>('revoked', '');
  const showOldRevoked = revokedView === 'all';
  const setShowOldRevoked = (next: boolean) => batchParams({ revoked: next ? 'all' : null });
  const { field: sortField, dir: sortDir } = parseTokenSort(sort);
  const debounced = useDebounced(search);
  const toast = useToast();
  const queryClient = useQueryClient();

  const tokens = useQuery({
    queryKey: ['admin', 'service-tokens', debounced, status, sort],
    queryFn: () =>
      api.get<{ service_tokens: ServiceTokenRow[]; total_count: number }>(
        `/api/v1/admin/service-tokens${qs({ search: debounced, status, sort })}`,
      ),
  });

  const revoke = useMutation({
    mutationFn: (id: string) => api.del(`/api/v1/admin/service-tokens/${id}`),
    onSuccess: () => {
      toast(t('adminServiceTokens.revokedToast'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'service-tokens'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  // The endpoint returns every matching token (Collection pages client-side),
  // so hiding here after the server's search/status filter gives an exact
  // hidden count for the current filters.
  const { shown, hiddenOld, oldRevoked } = useMemo(() => {
    const all = tokens.data?.service_tokens ?? [];
    const now = Date.now();
    const old = all.filter((token) => isLongRevoked(token, now)).length;
    return {
      shown: showOldRevoked ? all : all.filter((token) => !isLongRevoked(token, now)),
      hiddenOld: showOldRevoked ? 0 : old,
      oldRevoked: old,
    };
  }, [tokens.data, showOldRevoked]);
  const hiddenNote = hiddenOld === 1 ? t('tokens.oldRevokedHiddenOne') : t('tokens.oldRevokedHiddenMany', { count: hiddenOld });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminServiceTokens.title')}</h1>
          <p className="page-subtitle">{t('adminServiceTokens.subtitle')}</p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setIssueOpen(true)}>
          {t('adminServiceTokens.issue')}
        </button>
      </header>

      <div className="row wrap">
        <SearchInput
          value={search}
          onChange={setSearch}
          placeholder={t('adminServiceTokens.searchPlaceholder')}
          label={t('adminServiceTokens.searchLabel')}
        />
        <FilterSelect
          label={t('tables.status')}
          value={status}
          onChange={setStatus}
          options={[
            { value: '', label: t('adminServiceTokens.allStatuses') },
            { value: 'active', label: t('adminServiceTokens.statusActive') },
            { value: 'expired', label: t('adminServiceTokens.statusExpired') },
            { value: 'revoked', label: t('adminServiceTokens.statusRevoked') },
          ]}
        />
        <FilterSelect
          label={t('adminPeople.sortLabel')}
          value={sortField}
          onChange={(next) => setSort(`${next}_${sortDir}`)}
          options={[
            { value: 'spend', label: t('adminServiceTokens.sortBySpend') },
            { value: 'name', label: t('adminServiceTokens.sortByName') },
            { value: 'requests', label: t('adminServiceTokens.sortByRequests') },
            { value: 'tokens', label: t('adminServiceTokens.sortByTokens') },
            { value: 'errors', label: t('adminServiceTokens.sortByErrors') },
            { value: 'last_used', label: t('adminServiceTokens.sortByLastUsed') },
            { value: 'created', label: t('adminServiceTokens.sortByCreated') },
            { value: 'expires', label: t('adminServiceTokens.sortByExpires') },
            { value: 'grants', label: t('adminServiceTokens.sortByGrants') },
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
          query={tokens}
          empty={{
            when: (data) => data.service_tokens.length === 0 && !debounced && !status,
            title: t('adminServiceTokens.emptyTitle'),
            body: t('adminServiceTokens.emptyBody'),
          }}
        >
          {() =>
            shown.length === 0 && hiddenOld > 0 ? (
              <EmptyState
                title={debounced || status ? t('adminServiceTokens.noMatchTitle') : t('tokens.allHiddenTitle')}
                body={hiddenNote}
                action={
                  <div className="row wrap">
                    <button type="button" className="btn" onClick={() => setShowOldRevoked(true)}>
                      {t('tokens.showAllRevoked')}
                    </button>
                    <button type="button" className="btn btn-primary" onClick={() => setIssueOpen(true)}>
                      {t('adminServiceTokens.issue')}
                    </button>
                  </div>
                }
              />
            ) : shown.length === 0 ? (
              <EmptyState title={t('adminServiceTokens.noMatchTitle')} body={t('adminServiceTokens.noMatchBody')} />
            ) : (
              <Collection
                name="Service tokens"
                rows={shown}
                rowKey={(token) => token.id}
                resetKey={sort + debounced + status + revokedView}
                hideSearch
                columns={[{ id: '0', label: '', value: (token) => token.name, render: () => null }]}
              >
                {(visible) => (
                  <>
                    <div className="table-wrap">
                      <table className="data data-compact">
                        <thead>
                          <tr>
                            <SortableColumn label={t('adminServiceTokens.colName')} field="name" sort={sort} onSort={setSort} />
                            <th scope="col">{t('tables.status')}</th>
                            <th scope="col">{t('adminServiceTokens.colTopModel')}</th>
                            {localOnly ? null : (
                              <SortableColumn
                                label={t('adminServiceTokens.colSpend30d')}
                                field="spend"
                                sort={sort}
                                onSort={setSort}
                                numeric
                              />
                            )}
                            <SortableColumn
                              label={t('adminServiceTokens.colRequests30d')}
                              field="requests"
                              sort={sort}
                              onSort={setSort}
                              numeric
                            />
                            <SortableColumn
                              label={t('adminServiceTokens.colTokens30d')}
                              field="tokens"
                              sort={sort}
                              onSort={setSort}
                              numeric
                            />
                            <SortableColumn
                              label={t('adminServiceTokens.colErrors30d')}
                              field="errors"
                              sort={sort}
                              onSort={setSort}
                              numeric
                            />
                            <SortableColumn
                              label={t('adminServiceTokens.colGrants')}
                              field="grants"
                              sort={sort}
                              onSort={setSort}
                              numeric
                            />
                            <SortableColumn
                              label={t('adminServiceTokens.colLastUsed')}
                              field="last_used"
                              sort={sort}
                              onSort={setSort}
                            />
                            <SortableColumn
                              label={t('adminServiceTokens.colExpires')}
                              field="expires"
                              sort={sort}
                              onSort={setSort}
                            />
                            <th scope="col">
                              <span className="sr-only">{t('tables.actions')}</span>
                            </th>
                          </tr>
                        </thead>
                        <tbody>
                          {visible.map((token) => (
                            <tr key={token.id}>
                              <td className="cell-person">
                                <Link to={`/admin/service-tokens/${token.id}`}>{token.name}</Link>
                                <span className="cell-sub cell-with-more">
                                  {token.description ? <Truncate>{token.description}</Truncate> : null}
                                  <span className="mono nowrap" title={t('tokens.prefixTitle', { prefix: token.prefix })}>
                                    {token.description ? '· ' : ''}
                                    {token.prefix}…
                                  </span>
                                </span>
                              </td>
                              <td>
                                {token.status === 'active' ? (
                                  <Badge tone="success" dot>
                                    {t('adminServiceTokens.statusActive')}
                                  </Badge>
                                ) : token.status === 'expired' ? (
                                  <Badge tone="warning">{t('adminServiceTokens.statusExpired')}</Badge>
                                ) : (
                                  <Badge tone="danger">{t('adminServiceTokens.statusRevoked')}</Badge>
                                )}
                              </td>
                              <td className="small muted">
                                <TopModel name={token.top_model_30d} count={token.model_count_30d} />
                              </td>
                              {localOnly ? null : <td className="num small">{formatUSD(token.spend_30d_usd * 1_000_000_000)}</td>}
                              <td className="num small">{formatNumber(token.requests_30d)}</td>
                              <td className="num small">{formatNumber(token.tokens_in_30d + token.tokens_out_30d)}</td>
                              <td className="num small">
                                {token.errors_30d > 0 ? (
                                  <span className="danger">{formatNumber(token.errors_30d)}</span>
                                ) : (
                                  formatNumber(0)
                                )}
                              </td>
                              <td className="num small">{formatNumber(token.grant_count)}</td>
                              <td className="small muted">
                                {token.last_used_at ? formatRelative(token.last_used_at) : t('adminServiceTokens.neverUsed')}
                              </td>
                              <td className="small muted">
                                {token.expires_at ? formatRelative(token.expires_at) : t('adminServiceTokens.noExpiry')}
                              </td>
                              <td>
                                <RowActions>
                                  <IconButton
                                    icon="edit"
                                    label={t('adminServiceTokens.edit')}
                                    onClick={() => setEditing(token)}
                                  />
                                  <IconButton
                                    icon="revoke"
                                    label={t('adminServiceTokens.revoke')}
                                    danger
                                    onClick={() => setRevoking(token)}
                                    disabled={token.status === 'revoked'}
                                  />
                                </RowActions>
                              </td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                    {oldRevoked > 0 ? (
                      <div className="row wrap small muted" style={{ padding: 'var(--janus-space-3) var(--janus-card-padding)' }}>
                        {hiddenOld > 0 ? (
                          <>
                            <span>{hiddenNote}</span>
                            <button type="button" className="btn btn-ghost btn-sm" onClick={() => setShowOldRevoked(true)}>
                              {t('tokens.showAllRevoked')}
                            </button>
                          </>
                        ) : (
                          <button type="button" className="btn btn-ghost btn-sm" onClick={() => setShowOldRevoked(false)}>
                            {t('tokens.hideOldRevoked')}
                          </button>
                        )}
                      </div>
                    ) : null}
                  </>
                )}
              </Collection>
            )
          }
        </AsyncSection>
      </section>

      <IssueTokenModal open={issueOpen} onClose={() => setIssueOpen(false)} />
      {id && detail.isPending ? <p role="status">Loading service token…</p> : null}
      {id && detail.isError ? (
        <div role="alert">
          {detail.error.message}{' '}
          <button className="btn" onClick={() => setEditing(null)}>
            Back to tokens
          </button>
        </div>
      ) : null}
      {editing ? <EditTokenModal key={id} token={editing} onClose={() => setEditing(null)} /> : null}

      <ConfirmDialog
        open={Boolean(revoking)}
        onClose={() => setRevoking(null)}
        onConfirm={() => {
          if (revoking) revoke.mutate(revoking.id);
          setRevoking(null);
        }}
        danger
        title={t('adminServiceTokens.revokeTitle')}
        consequence={t('adminServiceTokens.revokeBody', { name: revoking?.name ?? '' })}
        confirmLabel={t('adminServiceTokens.revokeConfirm')}
        busy={revoke.isPending}
      />
    </div>
  );
}

function IssueTokenModal({ open, onClose }: { open: boolean; onClose: () => void }): ReactNode {
  const toast = useToast();
  const queryClient = useQueryClient();
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [expiresAt, setExpiresAt] = useState('');
  // The plaintext is returned exactly once. It lives in component state only
  // until the admin dismisses the panel — never refetched, never persisted.
  const [issued, setIssued] = useState<{ value: string; name: string } | null>(null);

  const create = useMutation({
    mutationFn: () =>
      api.post<{ value: string; service_token: ServiceTokenRow }>('/api/v1/admin/service-tokens', {
        name,
        description,
        expires_at: expiresAt ? new Date(expiresAt).toISOString() : '',
      }),
    onSuccess: (data) => {
      setIssued({ value: data.value, name: data.service_token.name });
      setName('');
      setDescription('');
      setExpiresAt('');
      void queryClient.invalidateQueries({ queryKey: ['admin', 'service-tokens'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const close = () => {
    setIssued(null);
    onClose();
  };

  if (issued) {
    return (
      <Modal
        open={open}
        onClose={close}
        title={t('adminServiceTokens.issuedTitle', { name: issued.name })}
        description={t('adminServiceTokens.issuedDescription')}
        footer={
          <button type="button" className="btn btn-primary" onClick={close}>
            {t('adminServiceTokens.issuedDone')}
          </button>
        }
      >
        <pre
          className="mono"
          style={{
            userSelect: 'all',
            whiteSpace: 'pre-wrap',
            wordBreak: 'break-all',
            padding: 'var(--janus-space-3)',
            background: 'var(--janus-color-bg-surface-raised)',
            borderRadius: 'var(--janus-radius-md)',
          }}
        >
          {issued.value}
        </pre>
        <p className="small muted">{t('adminServiceTokens.issuedHint')}</p>
        <button
          type="button"
          className="btn btn-sm"
          onClick={() => {
            void navigator.clipboard?.writeText(issued.value);
            toast(t('adminServiceTokens.copied'));
          }}
        >
          {t('adminServiceTokens.copy')}
        </button>
      </Modal>
    );
  }

  return (
    <Modal
      open={open}
      onClose={close}
      title={t('adminServiceTokens.issueTitle')}
      description={t('adminServiceTokens.issueDescription')}
      footer={
        <>
          <button type="button" className="btn" onClick={close} disabled={create.isPending}>
            {t('common.cancel')}
          </button>
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => create.mutate()}
            disabled={!name.trim() || create.isPending}
          >
            {create.isPending ? t('adminServiceTokens.issuing') : t('adminServiceTokens.issue')}
          </button>
        </>
      }
    >
      <Field label={t('adminServiceTokens.nameLabel')} required hint={t('adminServiceTokens.nameHint')}>
        <input className="input" value={name} onChange={(event) => setName(event.target.value)} />
      </Field>
      <Field label={t('adminServiceTokens.descriptionLabel')} hint={t('adminServiceTokens.descriptionHint')}>
        <input className="input" value={description} onChange={(event) => setDescription(event.target.value)} />
      </Field>
      <Field label={t('adminServiceTokens.expiresLabel')} hint={t('adminServiceTokens.expiresHint')}>
        <input className="input" type="date" value={expiresAt} onChange={(event) => setExpiresAt(event.target.value)} />
      </Field>
      <p className="small muted">{t('adminServiceTokens.grantReminder')}</p>
    </Modal>
  );
}

function EditTokenModal({ token, onClose }: { token: ServiceTokenRow | null; onClose: () => void }): ReactNode {
  const toast = useToast();
  const queryClient = useQueryClient();
  const [name, setName] = useState(token?.name ?? '');
  const [description, setDescription] = useState(token?.description ?? '');

  const save = useMutation({
    mutationFn: () => api.patch(`/api/v1/admin/service-tokens/${token?.id}`, { name, description }),
    onSuccess: () => {
      toast(t('adminServiceTokens.updatedToast'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'service-tokens'] });
      onClose();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  if (!token) return null;

  return (
    <Modal
      open
      onClose={onClose}
      title={t('adminServiceTokens.editTitle', { name: token.name })}
      description={t('adminServiceTokens.editDescription')}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={save.isPending}>
            {t('common.cancel')}
          </button>
          <button type="button" className="btn btn-primary" onClick={() => save.mutate()} disabled={save.isPending}>
            {save.isPending ? t('tables.saving') : t('adminServiceTokens.save')}
          </button>
        </>
      }
    >
      <Field label={t('adminServiceTokens.nameLabel')} required>
        <input className="input" defaultValue={token.name} onChange={(event) => setName(event.target.value)} />
      </Field>
      <Field label={t('adminServiceTokens.descriptionLabel')}>
        <input className="input" defaultValue={token.description} onChange={(event) => setDescription(event.target.value)} />
      </Field>
    </Modal>
  );
}
