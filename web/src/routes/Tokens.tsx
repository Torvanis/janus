import { sortCollection } from '../lib/collections';
import { useCollectionState, useRepairCollectionPage } from '../lib/collection-state';
import { useMemo, useState, type ReactNode } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, qs } from '../lib/api';
import { isTokenRevoked, type ApiToken, type Breakdown, type Totals, type UsageEvent } from '../lib/types';
import { formatDateTime, formatNumber, formatRelative, formatUSD } from '../lib/format';
import {
  AsyncSection,
  Badge,
  CodeBlock,
  ConfirmDialog,
  CopyButton,
  Drawer,
  EmptyState,
  Field,
  Modal,
  Pagination,
  useToast,
} from '../components/ui';
import { BarList } from '../components/charts';
import { ChargedTeam } from '../components/ChargedTeam';
import { useDebounced, useUrlState, useUrlStateBatch } from '../lib/hooks';
import { useLocalOnly, useMe } from '../app/session';
import { t } from '../lib/i18n';
import { DetailRow, FilterSelect, SearchInput, SortHeader } from './shared';

const PAGE_SIZE = 25;

function TokenContext({ token }: { token: ApiToken }): ReactNode {
  const me = useMe();
  return (
    <span>{token.team_id ? (me?.teams?.find((team) => team.id === token.team_id)?.name ?? token.team_id) : 'Personal'}</span>
  );
}

function TokenStatus({ token }: { token: ApiToken }): ReactNode {
  const me = useMe();
  if (isTokenRevoked(token)) return <Badge tone="neutral">{t('tokens.revoked')}</Badge>;
  if (token.team_id && me && !me.teams?.some((team) => team.id === token.team_id)) {
    return (
      <span className="stack">
        <Badge tone="warning">Membership removed</Badge>
        <span className="small muted">
          This key cannot be used. Change this key to Personal or a current team with Change team, or ask a team owner to restore
          membership.
        </span>
      </span>
    );
  }
  return (
    <Badge tone="success" dot>
      {t('tokens.active')}
    </Badge>
  );
}

export function TokensPage(): ReactNode {
  const me = useMe();
  // Local-only deployments have no spend surface, so the cost column is
  // dropped entirely rather than rendered as zeros.
  const localOnly = useLocalOnly();
  const toast = useToast();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const { id: routeTokenId } = useParams();

  const [search] = useUrlState('q', '');
  const [status] = useUrlState<'all' | 'active' | 'revoked' | 'used' | 'unused'>('status', 'all');
  const [sort, setSort] = useCollectionState<
    | 'created'
    | 'created_asc'
    | 'used'
    | 'used_asc'
    | 'description'
    | 'description_asc'
    | 'requests'
    | 'requests_asc'
    | 'tokens'
    | 'tokens_asc'
    | 'spend'
    | 'spend_asc'
  >('sort', 'created');
  const [page, setPage] = useUrlState('page', '0');
  // Filter/search changes must atomically reset the page offset, otherwise a
  // user standing on page ≥2 who narrows the list lands on an out-of-range
  // offset: an empty table body with the pagination control hidden.
  const batchParams = useUrlStateBatch();
  const setSearch = (next: string) => batchParams({ q: next, page: null });
  const setStatus = (next: typeof status) => batchParams({ status: next === 'all' ? null : next, page: null });
  const [createOpen, setCreateOpen] = useState(false);
  const [revealed, setRevealed] = useState<{ token: ApiToken; value: string } | null>(null);
  const [confirming, setConfirming] = useState<ApiToken | null>(null);
  const [changingTeam, setChangingTeam] = useState<ApiToken | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [bulkConfirm, setBulkConfirm] = useState(false);

  const debouncedSearch = useDebounced(search);
  const offset = Number.parseInt(page, 10) || 0;

  const tokens = useQuery({
    queryKey: ['tokens'],
    queryFn: () => api.get<{ tokens: ApiToken[] }>('/api/v1/tokens'),
  });

  const revoke = useMutation({
    mutationFn: (id: string) => api.del(`/api/v1/tokens/${id}`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['tokens'] });
      toast(t('tokens.revokedToast'));
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const filtered = useMemo(() => {
    const all = tokens.data?.tokens ?? [];
    const term = debouncedSearch.trim().toLowerCase();
    const matched = all.filter((token) => {
      // isTokenRevoked treats '' (and the legacy Go zero time) as active, so
      // the status groups split cleanly: active = unrevoked, revoked = rest.
      const revoked = isTokenRevoked(token);
      if (status === 'active' && revoked) return false;
      if (status === 'revoked' && !revoked) return false;
      // "Used" / "unused" answers the question the page is really for:
      // which of these credentials can I revoke without breaking anything?
      const used = (token.requests_30d ?? 0) > 0;
      if (status === 'used' && !used) return false;
      if (status === 'unused' && used) return false;
      if (term && !token.description.toLowerCase().includes(term) && !token.prefix.toLowerCase().includes(term)) {
        return false;
      }
      return true;
    });
    // SortHeader toggles between "<key>" (descending) and "<key>_asc".
    const descending = !sort.endsWith('_asc');
    const key = sort.replace(/_asc$/, '');
    const sorted = sortCollection(
      matched,
      (token) => {
        if (key === 'description') return token.description;
        if (key === 'used') return token.last_used_at;
        if (key === 'requests') return token.requests_30d ?? 0;
        if (key === 'tokens') return (token.tokens_in_30d ?? 0) + (token.tokens_out_30d ?? 0);
        if (key === 'spend') return token.spend_30d_usd ?? 0;
        return token.created_at;
      },
      !descending,
    );
    return sorted;
  }, [tokens.data, debouncedSearch, status, sort]);

  useRepairCollectionPage(tokens.data ? filtered.length : undefined, PAGE_SIZE);
  const pageItems = filtered.slice(offset, offset + PAGE_SIZE);
  const activeSelected = [...selected].filter((id) => filtered.some((token) => token.id === id && !isTokenRevoked(token)));

  const toggleSelected = (id: string) => {
    setSelected((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('tokens.title')}</h1>
          <p className="page-subtitle">{t('tokens.subtitle')}</p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
          {t('tokens.create')}
        </button>
      </header>

      <section className="card card-flush">
        <div className="card-header" style={{ padding: 'var(--janus-card-padding)', marginBottom: 0 }}>
          <div className="row wrap">
            <SearchInput
              value={search}
              onChange={setSearch}
              placeholder={t('tokens.searchPlaceholder')}
              label={t('tokens.searchLabel')}
            />
            <FilterSelect
              label={t('tables.status')}
              value={status}
              onChange={(next) => setStatus(next as typeof status)}
              options={[
                { value: 'all', label: t('tokens.allTokens') },
                { value: 'active', label: t('tokens.activeOnly') },
                { value: 'revoked', label: t('tokens.revokedOnly') },
                { value: 'used', label: t('tokens.usedOnly') },
                { value: 'unused', label: t('tokens.unusedOnly') },
              ]}
            />
          </div>
          {activeSelected.length > 0 ? (
            <button type="button" className="btn btn-danger btn-sm" onClick={() => setBulkConfirm(true)}>
              {t('tokens.revokeSelected', { count: activeSelected.length })}
            </button>
          ) : null}
        </div>

        <AsyncSection
          query={tokens}
          empty={{
            when: () => filtered.length === 0 && !debouncedSearch && status === 'all',
            title: t('tokens.emptyTitle'),
            body: t('tokens.emptyBody'),
            action: (
              <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
                {t('tokens.createFirst')}
              </button>
            ),
          }}
        >
          {() =>
            filtered.length === 0 ? (
              <EmptyState
                title={t('tokens.noMatchTitle')}
                body={t('tokens.noMatchBody')}
                action={
                  <button type="button" className="btn" onClick={() => batchParams({ q: null, status: null, page: null })}>
                    {t('tables.clearFilters')}
                  </button>
                }
              />
            ) : (
              <>
                <div className="table-wrap">
                  <table className="data">
                    <thead>
                      <tr>
                        <th scope="col" style={{ width: 36 }}>
                          <span className="sr-only">{t('tokens.select')}</span>
                        </th>
                        <SortHeader
                          label={t('tokens.colDescription')}
                          sortKey="description"
                          active={sort}
                          onSort={(key) => setSort(key as typeof sort)}
                        />
                        <th scope="col">Context</th>
                        <th scope="col">{t('tokens.colPrefix')}</th>
                        <SortHeader
                          label={t('tokens.colCreated')}
                          sortKey="created"
                          active={sort}
                          onSort={(key) => setSort(key as typeof sort)}
                        />
                        <SortHeader
                          label={t('tokens.colLastUsed')}
                          sortKey="used"
                          active={sort}
                          onSort={(key) => setSort(key as typeof sort)}
                        />
                        <SortHeader
                          label={t('tokens.colRequests30d')}
                          sortKey="requests"
                          active={sort}
                          onSort={(key) => setSort(key as typeof sort)}
                        />
                        <SortHeader
                          label={t('tokens.colTokens30d')}
                          sortKey="tokens"
                          active={sort}
                          onSort={(key) => setSort(key as typeof sort)}
                        />
                        {localOnly ? null : (
                          <SortHeader
                            label={t('tokens.colSpend30d')}
                            sortKey="spend"
                            active={sort}
                            onSort={(key) => setSort(key as typeof sort)}
                          />
                        )}
                        <th scope="col">{t('tokens.colTopModel')}</th>
                        <th scope="col">{t('tables.status')}</th>
                        <th scope="col">
                          <span className="sr-only">{t('tables.actions')}</span>
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {pageItems.map((token) => (
                        <tr key={token.id} data-clickable="true" data-selected={selected.has(token.id)}>
                          <td>
                            <input
                              type="checkbox"
                              checked={selected.has(token.id)}
                              onChange={() => toggleSelected(token.id)}
                              disabled={isTokenRevoked(token)}
                              aria-label={t('tokens.selectToken', { name: token.description })}
                            />
                          </td>
                          <td>
                            <button
                              type="button"
                              className="btn btn-ghost btn-sm"
                              onClick={() => navigate(`/tokens/${token.id}`)}
                              style={{ padding: 0, minHeight: 'auto' }}
                            >
                              {token.description || t('tokens.untitled')}
                            </button>
                          </td>
                          <td className="small">
                            <TokenContext token={token} />
                          </td>
                          <td className="mono small muted">{token.prefix}…</td>
                          <td className="small muted">{formatDateTime(token.created_at)}</td>
                          <td className="small muted">{formatRelative(token.last_used_at)}</td>
                          <td className="num small">{formatNumber(token.requests_30d ?? 0)}</td>
                          <td className="num small">{formatNumber((token.tokens_in_30d ?? 0) + (token.tokens_out_30d ?? 0))}</td>
                          {localOnly ? null : (
                            <td className="num small">{formatUSD((token.spend_30d_usd ?? 0) * 1_000_000_000)}</td>
                          )}
                          <td className="small muted truncate" style={{ maxWidth: 180 }}>
                            {token.top_model_30d || '—'}
                            {(token.model_count_30d ?? 0) > 1 ? (
                              <span className="muted"> {t('tokens.plusMore', { count: (token.model_count_30d ?? 1) - 1 })}</span>
                            ) : null}
                          </td>
                          <td>
                            <TokenStatus token={token} />
                          </td>
                          <td style={{ textAlign: 'right' }}>
                            {!isTokenRevoked(token) ? (
                              <button type="button" className="btn btn-ghost btn-sm" onClick={() => setChangingTeam(token)}>
                                Change team
                              </button>
                            ) : null}
                            {!isTokenRevoked(token) ? (
                              <button type="button" className="btn btn-ghost btn-sm" onClick={() => setConfirming(token)}>
                                {t('tokens.revoke')}
                              </button>
                            ) : null}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                <Pagination
                  offset={offset}
                  limit={PAGE_SIZE}
                  total={filtered.length}
                  onChange={(next) => setPage(String(next))}
                />
              </>
            )
          }
        </AsyncSection>
      </section>

      {createOpen ? (
        <CreateTokenModal
          open={createOpen}
          onClose={() => setCreateOpen(false)}
          onCreated={(token, value) => {
            setCreateOpen(false);
            setRevealed({ token, value });
            void queryClient.invalidateQueries({ queryKey: ['tokens'] });
          }}
        />
      ) : null}

      <RevealTokenModal reveal={revealed} endpoint={me?.endpoint ?? ''} onClose={() => setRevealed(null)} />
      {changingTeam ? <ChangeTokenTeamModal token={changingTeam} onClose={() => setChangingTeam(null)} /> : null}

      <ConfirmDialog
        open={Boolean(confirming)}
        onClose={() => setConfirming(null)}
        onConfirm={() => {
          if (confirming) revoke.mutate(confirming.id);
          setConfirming(null);
        }}
        title={t('tokens.revokeTitle', { name: confirming?.description ?? '' })}
        consequence={t('tokens.revokeConsequence')}
        confirmLabel={t('tokens.revokeConfirm')}
        busy={revoke.isPending}
      />

      <ConfirmDialog
        open={bulkConfirm}
        onClose={() => setBulkConfirm(false)}
        onConfirm={async () => {
          const failed = new Set<string>();
          for (const id of activeSelected) {
            await revoke.mutateAsync(id).catch(() => failed.add(id));
          }
          // Keep failed tokens selected so a retry is one click away; only
          // successfully revoked tokens leave the selection.
          setSelected(failed);
          setBulkConfirm(false);
          if (failed.size > 0) {
            toast(t('tokens.bulkRevokeSummary', { revoked: activeSelected.length - failed.size, failed: failed.size }), 'danger');
          }
        }}
        title={t('tokens.bulkRevokeTitle', { count: activeSelected.length })}
        consequence={t('tokens.bulkRevokeConsequence')}
        confirmLabel={t('tokens.bulkRevokeConfirm')}
        busy={revoke.isPending}
      />

      <TokenDetailDrawer tokenId={routeTokenId} onClose={() => navigate('/tokens')} />
    </div>
  );
}

function ChangeTokenTeamModal({ token, onClose }: { token: ApiToken; onClose: () => void }): ReactNode {
  const me = useMe();
  const queryClient = useQueryClient();
  const toast = useToast();
  const [teamId, setTeamId] = useState(token.team_id ?? '');
  const [moveHistory, setMoveHistory] = useState(false);
  const missingTeam = Boolean(teamId) && !(me?.teams ?? []).some((team) => team.id === teamId);
  const change = useMutation({
    mutationFn: () =>
      api.put<{ token: ApiToken; reattributed_requests: number }>(`/api/v1/tokens/${token.id}/team`, {
        team_id: teamId,
        move_history: moveHistory,
      }),
    onSuccess: async (result) => {
      // Attribution affects token detail, usage reports, dashboards and quotas.
      await queryClient.invalidateQueries();
      toast(`Token team updated. ${formatNumber(result.reattributed_requests)} existing requests reattributed.`);
      onClose();
    },
  });

  return (
    <Modal
      open
      onClose={() => {
        if (!change.isPending) onClose();
      }}
      title={`Change team — ${token.description || token.prefix}`}
      description="Choose the context this API key uses, independently of your browser selection."
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={change.isPending}>
            {t('common.cancel')}
          </button>
          <button
            type="button"
            className="btn btn-primary"
            disabled={missingTeam || change.isPending}
            onClick={() => change.mutate()}
          >
            {change.isPending ? 'Saving…' : 'Save team'}
          </button>
        </>
      }
    >
      <Field label="Token context">
        <select
          aria-label="Token context"
          className="select"
          value={teamId}
          disabled={change.isPending}
          onChange={(event) => setTeamId(event.target.value)}
        >
          <option value="">Personal</option>
          {missingTeam ? (
            <option value={teamId} disabled>
              {teamId} — membership removed
            </option>
          ) : null}
          {(me?.teams ?? []).map((team) => (
            <option key={team.id} value={team.id}>
              {team.name}
            </option>
          ))}
        </select>
      </Field>
      {missingTeam ? <p role="alert">Membership removed. Choose Personal or a current team to use this key again.</p> : null}
      <fieldset disabled={change.isPending} className="stack">
        <legend>Usage attribution</legend>
        <label>
          <input type="radio" name="token-history" checked={!moveHistory} onChange={() => setMoveHistory(false)} /> Future
          requests only
        </label>
        <label>
          <input type="radio" name="token-history" checked={moveHistory} onChange={() => setMoveHistory(true)} /> Reattribute
          existing usage for this token
        </label>
      </fieldset>
      <p className="small muted">
        The plaintext key stays unchanged; no client configuration changes are needed. In-flight requests retain their original
        context.
      </p>
      <p className="small muted">
        Future requests use the selected context and its model access and quotas. Future requests only leaves historical usage
        unchanged.
      </p>
      {moveHistory ? (
        <p role="note">
          Reattributing existing usage moves this token's recorded requests to the selected context. Historical totals change for
          the source and destination contexts, including usage counted toward quotas; this may affect remaining quota and whether
          new requests are allowed. Other tokens are not moved.
        </p>
      ) : null}
      {change.isError ? (
        <p role="alert">{change.error instanceof Error ? change.error.message : 'Unable to change token team.'}</p>
      ) : null}
    </Modal>
  );
}

function CreateTokenModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: (token: ApiToken, value: string) => void;
}): ReactNode {
  const me = useMe();
  const [teamId, setTeamId] = useState(me?.active_team_id ?? '');
  const [description, setDescription] = useState('');
  const [touched, setTouched] = useState(false);

  const create = useMutation({
    mutationFn: () =>
      api.post<{ token: ApiToken; value: string }>('/api/v1/tokens', {
        description: description.trim(),
        ...(teamId ? { team_id: teamId } : {}),
      }),
    onSuccess: (result) => {
      setDescription('');
      setTouched(false);
      onCreated(result.token, result.value);
    },
  });

  // Client-side validation mirrors the server rule; the server remains authoritative.
  const trimmed = description.trim();
  const error =
    touched && trimmed.length === 0
      ? t('tokens.descriptionRequired')
      : trimmed.length > 100
        ? t('tokens.descriptionTooLong')
        : undefined;
  const missingTeam = Boolean(teamId) && !(me?.teams ?? []).some((team) => team.id === teamId);
  const valid = trimmed.length > 0 && trimmed.length <= 100 && !missingTeam;

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={t('tokens.createTitle')}
      description={t('tokens.createDescription')}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={create.isPending}>
            {t('common.cancel')}
          </button>
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => {
              setTouched(true);
              if (valid) create.mutate();
            }}
            disabled={create.isPending}
          >
            {create.isPending ? t('tokens.creating') : t('tokens.create')}
          </button>
        </>
      }
    >
      <Field label="Token context">
        <select aria-label="Token context" className="select" value={teamId} onChange={(event) => setTeamId(event.target.value)}>
          <option value="">Personal</option>
          {missingTeam ? (
            <option value={teamId} disabled>
              {teamId} — membership removed
            </option>
          ) : null}
          {(me?.teams ?? []).map((team) => (
            <option key={team.id} value={team.id}>
              {team.name}
            </option>
          ))}
        </select>
      </Field>
      {missingTeam ? <p role="alert">Membership removed. Choose Personal or a current team before creating a key.</p> : null}
      <p className="small muted">
        This key uses the selected context, independent of browser team switching. Use Change team on an existing token to update
        its context without replacing the key. If your team membership is removed, you cannot use this key until you change its
        context or membership is restored.
      </p>
      <Field
        label={t('tokens.descriptionLabel')}
        required
        hint={t('tokens.descriptionHint', { count: trimmed.length })}
        error={error ?? (create.error instanceof Error ? create.error.message : undefined)}
      >
        <input
          className="input"
          value={description}
          onChange={(event) => setDescription(event.target.value)}
          onBlur={() => setTouched(true)}
          aria-invalid={Boolean(error)}
          maxLength={140}
          placeholder={t('tokens.descriptionPlaceholder')}
          autoFocus
        />
      </Field>
    </Modal>
  );
}

function RevealTokenModal({
  reveal,
  endpoint,
  onClose,
}: {
  reveal: { token: ApiToken; value: string } | null;
  endpoint: string;
  onClose: () => void;
}): ReactNode {
  if (!reveal) return null;
  const snippet = `curl ${endpoint}/chat/completions \\
  -H "Authorization: Bearer ${reveal.value}" \\
  -H "Content-Type: application/json" \\
  -d '{"model": "MODEL", "messages": [{"role": "user", "content": "Hello"}]}'`;

  return (
    <Modal
      open
      onClose={onClose}
      title={t('tokens.revealTitle')}
      footer={
        <button type="button" className="btn btn-primary" onClick={onClose}>
          {t('tokens.revealSaved')}
        </button>
      }
    >
      <div className="banner banner-warning">
        <div>
          <strong>{t('tokens.revealWarningTitle')}</strong>
          <div className="small" style={{ marginTop: 4 }}>
            {t('tokens.revealWarningBody')}
          </div>
        </div>
      </div>
      <div className="code-block">
        <div className="code-block-bar">
          <span className="overline">{reveal.token.description}</span>
          <CopyButton value={reveal.value} label={t('tokens.copyToken')} />
        </div>
        <pre>
          <code>{reveal.value}</code>
        </pre>
      </div>
      <p className="small muted" style={{ marginBottom: 0 }}>
        {t('tokens.tryIt')}
      </p>
      <CodeBlock code={snippet} language="bash" />
    </Modal>
  );
}

function TokenDetailDrawer({ tokenId, onClose }: { tokenId: string | undefined; onClose: () => void }): ReactNode {
  const localOnly = useLocalOnly();
  const detail = useQuery({
    queryKey: ['token-usage', tokenId],
    queryFn: () =>
      api.get<{ token: ApiToken; range?: string; totals: Totals; per_model: Breakdown[]; recent_requests: UsageEvent[] }>(
        `/api/v1/dashboard/tokens/${tokenId}`,
      ),
    enabled: Boolean(tokenId),
  });

  if (!tokenId) return null;

  return (
    <Drawer open onClose={onClose} title={detail.data?.token.description ?? t('tokens.drawerFallbackTitle')}>
      <AsyncSection query={detail}>
        {(data) => (
          <>
            <div className="stack">
              <DetailRow label="Context">
                <TokenContext token={data.token} />
              </DetailRow>
              <DetailRow label={t('tokens.colPrefix')}>
                <span className="mono">{data.token.prefix}…</span>
              </DetailRow>
              <DetailRow label={t('tokens.colCreated')}>{formatDateTime(data.token.created_at)}</DetailRow>
              <DetailRow label={t('tokens.colLastUsed')}>{formatRelative(data.token.last_used_at)}</DetailRow>
              <DetailRow label={t('tables.status')}>
                <TokenStatus token={data.token} />
              </DetailRow>
            </div>

            <div className="grid grid-tiles">
              <div className="tile">
                <div className="overline">{t('dashboard.requests')}</div>
                <div className="tile-value" style={{ fontSize: 'var(--janus-text-xl)' }}>
                  {formatNumber(data.totals.request_count)}
                </div>
              </div>
              {localOnly ? null : (
                <div className="tile">
                  <div className="overline">{t('dashboard.spend')}</div>
                  <div className="tile-value" style={{ fontSize: 'var(--janus-text-xl)' }}>
                    {formatUSD(data.totals.cost_nanousd)}
                  </div>
                </div>
              )}
            </div>

            <section>
              <h3 style={{ marginBottom: 'var(--janus-space-3)' }}>{t('tokens.byModel')}</h3>
              <BarList items={data.per_model} metric="requests" emptyLabel={t('tokens.notUsedYet')} />
            </section>

            <section>
              <h3 style={{ marginBottom: 'var(--janus-space-3)' }}>{t('tokens.recentCalls')}</h3>
              <p className="small muted">
                Latest {Math.min(10, data.recent_requests.length)} calls in this token's {data.range || 'day'} snapshot (up to
                10), newest first.
              </p>
              <Link className="small" to={`/requests${qs({ token_id: tokenId, range: data.range || 'day' })}`}>
                Open this token's request log
              </Link>
              {data.recent_requests.length === 0 ? (
                <p className="small muted">{t('tokens.noCalls')}</p>
              ) : (
                <div className="stack" style={{ gap: 6 }}>
                  {data.recent_requests.slice(0, 10).map((event) => (
                    <div key={event.id} className="row-between small">
                      <span className="truncate">{event.model || event.endpoint_path}</span>
                      <span className="muted">
                        Charged team: <ChargedTeam event={event} />
                      </span>
                      <span className="muted num">{formatDateTime(event.created_at)}</span>
                    </div>
                  ))}
                </div>
              )}
            </section>
          </>
        )}
      </AsyncSection>
    </Drawer>
  );
}
