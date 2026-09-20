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
import { useLicensed } from '../../app/session';
import { DetailRow, FilterSelect } from '../shared';

const PAGE_SIZE = 50;

const ACTIONS = [
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

export function AuditPage(): ReactNode {
  const [action] = useUrlState('action', '');
  const [resource] = useUrlState('resource', '');
  const [actor, setActor] = useCollectionState('actor', '');
  const [start, setStart] = useCollectionState('start', '');
  const [end, setEnd] = useCollectionState('end', '');
  const [page, setPage] = useUrlState('page', '0');
  // Filter changes must atomically reset the page offset, otherwise a user
  // standing on page ≥2 who narrows the list lands on an out-of-range offset.
  const batchParams = useUrlStateBatch();
  const setAction = (next: string) => batchParams({ action: next, page: null });
  const setResource = (next: string) => batchParams({ resource: next, page: null });
  const [selected, setSelected] = useState<AuditEntry | null>(null);
  const offset = Number.parseInt(page, 10) || 0;

  const audit = useQuery({
    queryKey: ['admin', 'audit', action, resource, actor, start, end, offset],
    queryFn: () =>
      api.get<{ entries: AuditEntry[]; total_count: number; note: string }>(
        `/api/v1/admin/audit${qs({ action, actor, start, end, resource_type: resource, limit: PAGE_SIZE, offset })}`,
      ),
  });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminAudit.title')}</h1>
          <p className="page-subtitle">{t('adminAudit.subtitle')}</p>
        </div>
        <AuditExportButton action={action} resource={resource} actor={actor} start={start} end={end} />
      </header>

      <div className="row wrap">
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
            ...ACTIONS.map((value) => ({ value, label: titleCase(value) })),
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
            ].map((value) => ({ value, label: titleCase(value) })),
          ]}
        />
      </div>

      <section className="card card-flush">
        <AsyncSection
          query={audit}
          empty={{
            when: (data) => data.total_count === 0 && !action && !resource,
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
                        <th scope="col">{t('tables.time')}</th>
                        <th scope="col">{t('adminAudit.actor')}</th>
                        <th scope="col">{t('adminAudit.action')}</th>
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

/** Download the filtered log as CSV — Business: audit_export. */
function AuditExportButton({
  action,
  resource,
  actor,
  start,
  end,
}: {
  action: string;
  resource: string;
  actor: string;
  start: string;
  end: string;
}): ReactNode {
  const licensed = useLicensed('audit_export');
  const params = new URLSearchParams();
  if (action) params.set('action', action);
  if (actor) params.set('actor', actor);
  if (start) params.set('start', start);
  if (end) params.set('end', end);
  if (resource) params.set('resource_type', resource);
  const href = `/api/v1/admin/audit/export${params.size ? `?${params}` : ''}`;
  if (!licensed) {
    return (
      <span className="row" style={{ gap: 'var(--janus-space-2)' }} title={t('adminAudit.exportUpsell')}>
        <button type="button" className="btn btn-sm" disabled aria-describedby="audit-export-upsell">
          {t('adminAudit.export')}
        </button>
        <span id="audit-export-upsell" className="small muted">
          {t('adminAudit.exportUpsell')}
        </span>
      </span>
    );
  }
  return (
    <a className="btn btn-sm" href={href} download>
      {t('adminAudit.export')}
    </a>
  );
}
