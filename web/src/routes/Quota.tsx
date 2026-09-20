import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import type { QuotaStatus } from '../lib/types';
import { formatDateTime, formatNumber, formatRelative, formatUSD, quotaTone } from '../lib/format';
import { AsyncSection, Badge } from '../components/ui';
import { t } from '../lib/i18n';
import { Collection } from '../components/Collection';
import { useUrlState, useUrlStateBatch } from '../lib/hooks';
import { sortCollection } from '../lib/collections';

export function QuotaPage(): ReactNode {
  const [status] = useUrlState('quota.status', '');
  const [sort] = useUrlState('quota.order', 'usage:desc');
  const setUrl = useUrlStateBatch();
  const statusOf = (quota: QuotaStatus) => quota.breached ? 'exhausted' : quota.at_risk ? 'at-risk' : 'healthy';
  const quotas = useQuery({
    queryKey: ['dashboard', 'quota', 'page'],
    queryFn: () => api.get<{ quotas: QuotaStatus[] }>('/api/v1/dashboard/quota'),
    refetchInterval: 15_000,
  });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('quota.title')}</h1>
          <p className="page-subtitle">{t('quota.subtitle')}</p>
        </div>
      </header>

      <AsyncSection
        query={quotas}
        empty={{
          when: (data) => data.quotas.length === 0,
          title: t('quota.emptyTitle'),
          body: t('quota.emptyBody'),
          action: (
            <Link className="btn" to="/docs/guides/quotas">
              {t('quota.howQuotasWork')}
            </Link>
          ),
        }}
      >
        {(data) => (
          <>
            <div className="row wrap">
              <label className="row">Quota status
                <select className="select" value={status} onChange={(e) => setUrl({ 'quota.status': e.target.value, 'personal-quotas.page': null })}>
                  <option value="">All statuses</option><option value="healthy">Healthy</option><option value="at-risk">Approaching limit</option><option value="exhausted">Exhausted</option>
                </select>
              </label>
              <label className="row">Sort quotas
                <select className="select" value={sort} onChange={(e) => setUrl({ 'quota.order': e.target.value, 'personal-quotas.page': null })}>
                  <option value="usage:desc">Usage percent (highest first)</option><option value="usage:asc">Usage percent (lowest first)</option><option value="metric:asc">Metric (A–Z)</option><option value="reset:asc">Reset (earliest first)</option>
                </select>
              </label>
              <span className="small muted">{data.quotas.length} quotas in your complete authorized inventory</span>
              {status && <button type="button" className="btn btn-sm" onClick={() => setUrl({ 'quota.status': null, 'personal-quotas.page': null })}>Clear status</button>}
            </div>
            <Collection<QuotaStatus>
              name="personal-quotas"
              rows={sortCollection(data.quotas.filter((quota) => !status || statusOf(quota) === status),
                (quota) => sort.startsWith('metric:') ? quota.metric_label : sort.startsWith('reset:') ? quota.reset_at : quota.percent,
                sort.endsWith(':asc'))}
              rowKey={(quota) => quota.id}
              resetKey={`${status}|${sort}`}
              columns={[
                { id: 'metric', label: 'Metric', value: (q) => q.metric_label, render: (q) => q.metric_label },
                { id: 'window', label: 'Window', value: (q) => q.window_label, render: (q) => q.window_label },
                { id: 'subject', label: 'Subject', value: (q) => q.subject_name, render: (q) => q.subject_name },
                { id: 'model', label: 'Model', value: (q) => q.model_name, render: (q) => q.model_name },
              ]}
            >
              {(rows) => <div className="grid grid-halves">{rows.map((quota) => <QuotaCard key={quota.id} quota={quota} />)}</div>}
            </Collection>
          </>
        )}
      </AsyncSection>
    </div>
  );
}

function QuotaCard({ quota }: { quota: QuotaStatus }): ReactNode {
  const tone = quotaTone(quota.percent);
  const format = (value: number) => (quota.unit === 'usd' ? formatUSD(value) : formatNumber(value));
  const remaining = Math.max(0, quota.limit_value - quota.current_value);

  return (
    <article className="card">
      <div className="card-header">
        <div>
          <h2>
            {quota.metric_label} <span className="muted">{quota.window_label}</span>
          </h2>
          <p className="small muted" style={{ margin: 0 }}>
            {quota.subject_type === 'team' ? t('quota.teamQuota', { name: quota.subject_name }) : t('quota.appliesToAccount')}
            {quota.model_name ? <> · {quota.model_name}</> : null}
          </p>
        </div>
        <Badge tone={tone === 'success' ? 'success' : tone === 'warning' ? 'warning' : 'danger'} dot>
          {quota.breached ? t('quota.exhausted') : quota.at_risk ? t('quota.approachingLimit') : t('quota.healthy')}
        </Badge>
      </div>

      <div className="row-between" style={{ marginBottom: 6 }}>
        <span className="num" style={{ fontFamily: 'var(--janus-font-mono)', fontSize: 'var(--janus-text-xl)' }}>
          {format(quota.current_value)}
        </span>
        <span className="small muted num">{t('quota.ofLimit', { limit: format(quota.limit_value) })}</span>
      </div>
      <div className="meter" role="progressbar" aria-valuenow={Math.round(quota.percent)} aria-valuemin={0} aria-valuemax={100}>
        <div
          className="meter-fill"
          data-level={tone === 'success' ? undefined : tone === 'warning' ? 'warning' : 'critical'}
          style={{ width: `${Math.min(100, Math.max(2, quota.percent))}%` }}
        />
      </div>

      <dl className="stack small" style={{ gap: 6, marginTop: 'var(--janus-space-4)' }}>
        <div className="row-between">
          <dt className="muted">{t('quota.remaining')}</dt>
          <dd className="num" style={{ margin: 0 }}>
            {format(remaining)}
          </dd>
        </div>
        <div className="row-between">
          <dt className="muted">{t('tables.resets')}</dt>
          <dd style={{ margin: 0 }}>
            {formatDateTime(quota.reset_at)} <span className="muted">({formatRelative(quota.reset_at)})</span>
          </dd>
        </div>
        <div className="row-between">
          <dt className="muted">{t('tables.onBreach')}</dt>
          <dd style={{ margin: 0 }}>
            {quota.breach_behavior === 'hard_kill' ? t('quota.hardKillBehavior') : t('quota.letFinishBehavior')}
          </dd>
        </div>
      </dl>

      {quota.breached ? (
        <div className="banner banner-danger" style={{ marginTop: 'var(--janus-space-4)' }}>
          <div>
            <strong>{t('quota.usedUpTitle')}</strong>
            <div className="small" style={{ marginTop: 4 }}>
              {t('quota.usedUpBody', { time: formatDateTime(quota.reset_at) })}
            </div>
          </div>
        </div>
      ) : quota.at_risk ? (
        <div className="banner banner-warning" style={{ marginTop: 'var(--janus-space-4)' }}>
          <div>{t('quota.atRiskBody', { percent: Math.round(quota.percent) })}</div>
        </div>
      ) : null}
    </article>
  );
}
