import { useState, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api, qs } from '../../lib/api';
import type { AuditEntry } from '../../lib/types';
import { formatDateTime, titleCase } from '../../lib/format';
import { AsyncSection, Badge, Drawer, EmptyState, Pagination } from '../../components/ui';
import { useCollectionState } from '../../lib/collection-state';
import { LookupInput } from '../../components/LookupInput';
import { userLookup } from '../../lib/lookups';
import { useUrlState, useUrlStateBatch } from '../../lib/hooks';
import { t } from '../../lib/i18n';
import { DetailRow, FilterSelect } from '../shared';

const PAGE_SIZE = 50;

const AUTH_ACTIONS = [
  'sign_in',
  'sign_in_failed',
  'sign_out',
  'session_timeout',
  'session_ended_cap',
  'account_locked',
  'account_unlocked',
  'banner_acknowledged',
  'password_expired',
  'user_password_changed',
];

const ACTIONS = [
  ...AUTH_ACTIONS,
  'access_denied',
  'security_policy_changed',
  'user_smart_card_set',
  'user_created',
  'user_updated',
  'user_role_changed',
  'upstream_created',
  'upstream_updated',
  'upstream_deleted',
  'model_updated',
  'grant_created',
  'grant_deleted',
  'quota_created',
  'quota_updated',
  'quota_deleted',
  'rule_created',
  'rule_deleted',
  'alert_created',
  'alert_deleted',
  'group_created',
  'group_updated',
  'group_deleted',
  'team_created',
  'team_updated',
  'team_deleted',
  'token_revoked',
  'feature_flag_changed',
];

const CATEGORIES = [
  { id: '', label: 'Everything' },
  { id: 'authentication', label: 'Sign-in and sessions' },
  { id: 'access', label: 'Access denied' },
  { id: 'administration', label: 'Administrative changes' },
];

type SortKey = 'created_at' | 'actor' | 'action' | 'outcome' | 'ip';

export function AuditPage(): ReactNode {
  const [action] = useUrlState('action', '');
  const [resource] = useUrlState('resource', '');
  const [category] = useUrlState('category', '');
  const [outcome] = useUrlState('outcome', '');
  const [ip, setIp] = useCollectionState('ip', '');
  const [q, setQ] = useCollectionState('q', '');
  const [sort] = useUrlState('sort', 'created_at');
  const [dir] = useUrlState('dir', 'desc');
  const [qDraft, setQDraft] = useState(q);
  const [ipDraft, setIpDraft] = useState(ip);
  const [actor, setActor] = useCollectionState('actor', '');
  const [start, setStart] = useCollectionState('start', '');
  const [end, setEnd] = useCollectionState('end', '');
  const [page, setPage] = useUrlState('page', '0');
  // Filter changes must atomically reset the page offset, otherwise a user
  // standing on page ≥2 who narrows the list lands on an out-of-range offset.
  const batchParams = useUrlStateBatch();
  const setAction = (next: string) => batchParams({ action: next, page: null });
  const setResource = (next: string) => batchParams({ resource: next, page: null });
  const setCategory = (next: string) => batchParams({ category: next || null, action: null, page: null });
  const setOutcome = (next: string) => batchParams({ outcome: next || null, page: null });
  const sortBy = (key: SortKey) =>
    batchParams({
      sort: key === 'created_at' ? null : key,
      dir: sort === key ? (dir === 'asc' ? null : 'asc') : key === 'created_at' ? null : 'asc',
      page: null,
    });
  const filters = { action, actor, start, end, resource_type: resource, category, outcome, ip, q, sort, dir };
  const [selected, setSelected] = useState<AuditEntry | null>(null);
  const offset = Number.parseInt(page, 10) || 0;

  const audit = useQuery({
    queryKey: ['admin', 'audit', filters, offset],
    queryFn: () =>
      api.get<{ entries: AuditEntry[]; total_count: number; note: string }>(
        `/api/v1/admin/audit${qs({ ...filters, limit: PAGE_SIZE, offset })}`,
      ),
    placeholderData: (prev) => prev,
  });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminAudit.title')}</h1>
          <p className="page-subtitle">{t('adminAudit.subtitle')}</p>
        </div>
        <AuditExportButton filters={filters} />
      </header>

      <div
        className="chip-row"
        role="tablist"
        aria-label="Kind of event"
        style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--janus-space-2)' }}
      >
        {CATEGORIES.map((c) => (
          <button
            key={c.id || 'all'}
            type="button"
            role="tab"
            aria-selected={category === c.id}
            className={`btn btn-sm ${category === c.id ? 'btn-primary' : ''}`}
            onClick={() => setCategory(c.id)}
          >
            {c.label}
          </button>
        ))}
      </div>

      <div className="row wrap">
        <form
          className="row"
          role="search"
          style={{ gap: 'var(--janus-space-2)' }}
          onSubmit={(e) => {
            e.preventDefault();
            setQ(qDraft.trim());
            setIp(ipDraft.trim());
          }}
        >
          <label>
            Search
            <input
              className="input"
              type="search"
              placeholder="Name, action, resource, detail"
              aria-label="Search the audit log"
              value={qDraft}
              onChange={(e) => setQDraft(e.target.value)}
              onBlur={() => setQ(qDraft.trim())}
            />
          </label>
          <label>
            Address
            <input
              className="input mono"
              placeholder="10.0.0.5"
              aria-label="Source address"
              value={ipDraft}
              onChange={(e) => setIpDraft(e.target.value)}
              onBlur={() => setIp(ipDraft.trim())}
              style={{ width: '11em' }}
            />
          </label>
          <button type="submit" className="sr-only">
            Search
          </button>
        </form>
        <FilterSelect
          label="Outcome"
          value={outcome}
          onChange={setOutcome}
          options={[
            { value: '', label: 'Any outcome' },
            { value: 'success', label: 'Succeeded' },
            { value: 'failure', label: 'Failed' },
          ]}
        />
        <LookupInput label="Actor" value={actor ? [actor] : []} onChange={(ids) => setActor(ids.at(-1) ?? '')} {...userLookup} />
        <label>
          From (UTC)
          <input
            className="input"
            type="datetime-local"
            aria-label="From (UTC)"
            value={start.replace(/Z$/, '')}
            onChange={(e) => setStart(e.target.value ? new Date(`${e.target.value}Z`).toISOString() : '')}
          />
        </label>
        <label>
          Until (UTC)
          <input
            className="input"
            type="datetime-local"
            aria-label="Until (UTC)"
            value={end.replace(/Z$/, '')}
            onChange={(e) => setEnd(e.target.value ? new Date(`${e.target.value}Z`).toISOString() : '')}
          />
        </label>
        <FilterSelect
          label={t('adminAudit.action')}
          value={action}
          onChange={setAction}
          options={[
            { value: '', label: t('adminAudit.allActions') },
            ...(category === 'authentication' ? AUTH_ACTIONS : category === 'access' ? ['access_denied'] : ACTIONS).map(
              (value) => ({
                value,
                label: titleCase(value),
              }),
            ),
          ]}
        />
        <FilterSelect
          label={t('adminAudit.resource')}
          value={resource}
          onChange={setResource}
          options={[
            { value: '', label: t('adminAudit.allResources') },
            ...[
              'upstream',
              'model',
              'grant',
              'quota',
              'blocking_rule',
              'alert_rule',
              'user',
              'group',
              'team',
              'token',
              'feature_flag',
              'session',
              'security_policy',
              'sign_in',
              'audit',
            ].map((value) => ({ value, label: titleCase(value) })),
          ]}
        />
      </div>

      <section className="card card-flush">
        <AsyncSection
          query={audit}
          empty={{
            when: (data) => data.total_count === 0 && !action && !resource && !category && !outcome && !ip && !q,
            title: t('adminAudit.emptyTitle'),
            body: t('adminAudit.emptyBody'),
          }}
        >
          {(data) =>
            data.entries.length === 0 ? (
              <EmptyState title={t('adminAudit.noMatchTitle')} body={t('adminAudit.noMatchBody')} />
            ) : (
              <>
                <div className="table-wrap">
                  <table className="data">
                    <thead>
                      <tr>
                        <SortHeader label={t('tables.time')} k="created_at" sort={sort} dir={dir} onSort={sortBy} />
                        <SortHeader label={t('adminAudit.actor')} k="actor" sort={sort} dir={dir} onSort={sortBy} />
                        <SortHeader label={t('adminAudit.action')} k="action" sort={sort} dir={dir} onSort={sortBy} />
                        <SortHeader label="Outcome" k="outcome" sort={sort} dir={dir} onSort={sortBy} />
                        <SortHeader label="Address" k="ip" sort={sort} dir={dir} onSort={sortBy} />
                        <th scope="col">{t('adminAudit.resource')}</th>
                        <th scope="col">
                          <span className="sr-only">{t('adminAudit.detail')}</span>
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {data.entries.map((entry) => (
                        <tr
                          key={entry.id}
                          data-clickable="true"
                          onClick={() => setSelected(entry)}
                          tabIndex={0}
                          onKeyDown={(keyEvent) => {
                            if (keyEvent.key === 'Enter') setSelected(entry);
                          }}
                        >
                          <td className="small muted">{formatDateTime(entry.created_at)}</td>
                          <td className="small truncate">{entry.actor_label || t('adminAudit.system')}</td>
                          <td>
                            <Badge tone={entry.action.includes('deleted') ? 'danger' : 'neutral'}>
                              {titleCase(entry.action)}
                            </Badge>
                          </td>
                          <td>
                            {entry.outcome === 'failure' ? (
                              <Badge tone="danger">Failed</Badge>
                            ) : (
                              <span className="small muted">OK</span>
                            )}
                          </td>
                          <td className="small mono muted">{entry.source_ip || '—'}</td>
                          <td className="small muted">{titleCase(entry.resource_type)}</td>
                          <td style={{ textAlign: 'right' }}>
                            <span className="small muted">{t('adminAudit.viewDiff')}</span>
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

      <Drawer open={Boolean(selected)} onClose={() => setSelected(null)} title={t('adminAudit.drawerTitle')}>
        {selected ? (
          <>
            <div className="stack">
              <DetailRow label={t('tables.time')}>{formatDateTime(selected.created_at)}</DetailRow>
              <DetailRow label={t('adminAudit.actor')}>{selected.actor_label || t('adminAudit.system')}</DetailRow>
              <DetailRow label={t('adminAudit.action')}>{titleCase(selected.action)}</DetailRow>
              <DetailRow label="Outcome">{selected.outcome === 'failure' ? 'Failed' : 'Succeeded'}</DetailRow>
              {selected.source_ip ? <DetailRow label="Address">{selected.source_ip}</DetailRow> : null}
              <DetailRow label={t('adminAudit.resource')}>
                {titleCase(selected.resource_type)}
                {selected.resource_id ? <span className="mono small muted"> {selected.resource_id}</span> : null}
              </DetailRow>
            </div>

            <section>
              <h3 style={{ marginBottom: 'var(--janus-space-3)' }}>{t('adminAudit.before')}</h3>
              <JsonBlock value={selected.old_value} emptyLabel={t('adminAudit.nothingBefore')} />
            </section>
            <section>
              <h3 style={{ marginBottom: 'var(--janus-space-3)' }}>{t('adminAudit.after')}</h3>
              <JsonBlock value={selected.new_value} emptyLabel={t('adminAudit.resourceRemoved')} />
            </section>
          </>
        ) : null}
      </Drawer>
    </div>
  );
}

function JsonBlock({ value, emptyLabel }: { value: string; emptyLabel: string }): ReactNode {
  if (!value) {
    return <p className="small muted">{emptyLabel}</p>;
  }
  let formatted = value;
  try {
    formatted = JSON.stringify(JSON.parse(value), null, 2);
  } catch {
    // A value that is not JSON is shown as-is rather than dropped.
  }
  return (
    <div className="code-block">
      <pre>
        <code>{formatted}</code>
      </pre>
    </div>
  );
}

/**
 * Download the filtered log as CSV. Never license-gated: an administrator can
 * always take the audit trail out, whatever the license state.
 */
function AuditExportButton({ filters }: { filters: Record<string, string> }): ReactNode {
  const params = new URLSearchParams();
  for (const [k, v] of Object.entries(filters)) {
    if (v && !(k === 'sort' && v === 'created_at') && !(k === 'dir' && v === 'desc')) params.set(k, v);
  }
  const href = `/api/v1/admin/audit/export${params.size ? `?${params}` : ''}`;
  return (
    <a className="btn btn-sm" href={href} download>
      {t('adminAudit.export')}
    </a>
  );
}

function SortHeader({
  label,
  k,
  sort,
  dir,
  onSort,
}: {
  label: string;
  k: SortKey;
  sort: string;
  dir: string;
  onSort: (k: SortKey) => void;
}): ReactNode {
  const active = sort === k;
  return (
    <th scope="col" aria-sort={active ? (dir === 'asc' ? 'ascending' : 'descending') : 'none'}>
      <button type="button" className="btn btn-ghost btn-sm" style={{ padding: 0, font: 'inherit' }} onClick={() => onSort(k)}>
        {label}
        <span aria-hidden="true" style={{ marginLeft: 4, opacity: active ? 1 : 0.35 }}>
          {active && dir === 'asc' ? '▲' : '▼'}
        </span>
      </button>
    </th>
  );
}
