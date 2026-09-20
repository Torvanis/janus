import { Collection } from '../../components/Collection';
import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, useParams, useNavigate, useLocation } from 'react-router-dom';
import { api, qs } from '../../lib/api';
import type { ServiceTokenRow } from '../../lib/types';
import { formatNumber, formatRelative, formatUSD } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, EmptyState, Field, Modal, useToast } from '../../components/ui';
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
          {(data) =>
            data.service_tokens.length === 0 ? (
              <EmptyState title={t('adminServiceTokens.noMatchTitle')} body={t('adminServiceTokens.noMatchBody')} />
            ) : (
              <Collection
                name="Service tokens"
                rows={data.service_tokens}
                rowKey={(token) => token.id}
                resetKey={sort + debounced + status}
                hideSearch
                columns={[{ id: '0', label: '', value: (token) => token.name, render: () => null }]}
              >
                {(visible) => (
                  <div className="table-wrap">
                    <table className="data">
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
                            <td>
                              <Link to={`/admin/service-tokens/${token.id}`}>{token.name}</Link>
                              {token.description ? (
                                <div className="small muted truncate" style={{ maxWidth: 260 }}>
                                  {token.description}
                                </div>
                              ) : null}
                              <div className="small muted mono">{token.prefix}…</div>
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
                            <td className="small muted truncate" style={{ maxWidth: 200 }}>
                              {token.top_model_30d || '—'}
                              {token.model_count_30d > 1 ? (
                                <span className="muted">
                                  {' '}
                                  {t('adminServiceTokens.plusMore', { count: token.model_count_30d - 1 })}
                                </span>
                              ) : null}
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
                            <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                              <button type="button" className="btn btn-ghost btn-sm" onClick={() => setEditing(token)}>
                                {t('adminServiceTokens.edit')}
                              </button>
                              <button
                                type="button"
                                className="btn btn-ghost btn-sm"
                                onClick={() => setRevoking(token)}
                                disabled={token.status === 'revoked'}
                              >
                                {t('adminServiceTokens.revoke')}
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
            background: 'var(--janus-surface-2)',
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
