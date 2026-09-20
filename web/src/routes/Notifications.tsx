import type { ReactNode } from 'react';
import { Continuation } from '../components/Continuation';
import { useCollectionState } from '../lib/collection-state';
import { pageOffset } from '../lib/collections';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/api';
import type { Notification } from '../lib/types';
import { formatDateTime, formatRelative } from '../lib/format';
import { AsyncSection, Badge, useToast } from '../components/ui';
import { useUrlState } from '../lib/hooks';
import { t } from '../lib/i18n';

export function NotificationsPage(): ReactNode {
  const [filter, setFilter] = useCollectionState<'all' | 'unread'>('filter', 'all');
  const [page, setPage] = useUrlState('page', '0');
  const offset = pageOffset(page, 100);
  const queryClient = useQueryClient();
  const toast = useToast();

  const notifications = useQuery({
    queryKey: ['notifications', filter, offset],
    queryFn: () =>
      api.get<{ notifications: Notification[]; unread_count: number; has_more: boolean }>(
        `/api/v1/notifications?limit=100&offset=${offset}${filter === 'unread' ? '&unread=true' : ''}`,
      ),
  });

  const markRead = useMutation({
    mutationFn: (id?: string) => api.post('/api/v1/notifications/read', id ? { id } : {}),
    onSuccess: () => {
      setPage('0');
      void queryClient.invalidateQueries({ queryKey: ['notifications'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('notifications.title')}</h1>
          <p className="page-subtitle">{t('notifications.subtitle')}</p>
        </div>
        <div className="row">
          <div className="segmented" role="group" aria-label={t('notifications.filter')}>
            <button type="button" aria-pressed={filter === 'all'} onClick={() => setFilter('all')}>
              {t('notifications.all')}
            </button>
            <button type="button" aria-pressed={filter === 'unread'} onClick={() => setFilter('unread')}>
              {t('notifications.unread')} {notifications.data?.unread_count ? `(${notifications.data.unread_count})` : ''}
            </button>
          </div>
          <button
            type="button"
            className="btn"
            onClick={() => markRead.mutate(undefined)}
            disabled={(notifications.data?.unread_count ?? 0) === 0 || markRead.isPending}
          >
            {t('notifications.markAllRead')}
          </button>
        </div>
      </header>

      <AsyncSection
        query={notifications}
        empty={{
          when: (data) => data.notifications.length === 0,
          title: filter === 'unread' ? t('notifications.emptyUnreadTitle') : t('notifications.emptyTitle'),
          body: filter === 'unread' ? t('notifications.emptyUnreadBody') : t('notifications.emptyBody'),
        }}
      >
        {(data) => (
          <section className="stack">
            {data.notifications.map((item) => (
              <article
                key={item.id}
                className="card"
                style={{
                  borderLeft: `3px solid var(--janus-color-${toneFor(item.severity)}-border)`,
                  opacity: item.read_at ? 0.72 : 1,
                }}
              >
                <div className="row-between">
                  <div>
                    <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
                      <Badge tone={toneFor(item.severity)}>{item.severity}</Badge>
                      <h2 style={{ fontSize: 'var(--janus-text-md)' }}>{item.title}</h2>
                    </div>
                    <p className="secondary small" style={{ marginTop: 6, marginBottom: 0 }}>
                      {item.body}
                    </p>
                  </div>
                  <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
                    <span className="small muted" title={formatDateTime(item.created_at)}>
                      {formatRelative(item.created_at)}
                    </span>
                    {!item.read_at ? (
                      <button type="button" className="btn btn-ghost btn-sm" onClick={() => markRead.mutate(item.id)}>
                        {t('notifications.markRead')}
                      </button>
                    ) : null}
                  </div>
                </div>
              </article>
            ))}
          </section>
        )}
      </AsyncSection>
      <Continuation
        offset={offset}
        limit={100}
        count={notifications.data?.notifications.length ?? 0}
        hasMore={notifications.data?.has_more ?? false}
        busy={notifications.isFetching}
        onPage={(next) => setPage(String(next))}
      />
    </div>
  );
}

function toneFor(severity: string): 'info' | 'warning' | 'danger' {
  if (severity === 'critical') return 'danger';
  if (severity === 'warning') return 'warning';
  return 'info';
}
