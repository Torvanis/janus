import { useState, type FormEvent, type ReactNode } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, qs } from '../lib/api';
import type { Breakdown, QuotaStatus, Team, TimePoint, Totals } from '../lib/types';
import { formatDateTime, formatNumber, formatUSD } from '../lib/format';
import { AreaChart, BarList, breakdownValue, formatMetric, type MetricKey } from '../components/charts';
import { AsyncSection, Badge, EmptyState, Field, useToast } from '../components/ui';
import { useUrlState } from '../lib/hooks';
import { t } from '../lib/i18n';
import { useDefaultMetric, useLocalOnly } from '../app/session';
import { RangePicker, type RangeKey } from './shared';
import './team-dashboard.css';

interface TeamDashboard {
  team: Team | null;
  teams: Team[];
  range: string;
  totals: Totals;
  per_model: Breakdown[];
  per_member: Breakdown[];
  series?: TimePoint[];
  can_see_member_detail: boolean;
  member_count: number;
}

export function TeamPage({
  teamId: selectedTeamId,
  workspace = 'personal',
}: { teamId?: string; workspace?: 'personal' | 'administration' } = {}): ReactNode {
  const administration = workspace === 'administration';
  const basePath = administration ? '/admin/teams' : '/teams';
  const { teamId: routeTeamId } = useParams();
  const teamId = selectedTeamId ?? routeTeamId;
  const localOnly = useLocalOnly();
  const [range, setRange] = useUrlState<RangeKey>('range', 'week');
  const defaultMetric = useDefaultMetric();
  const [chosenMetric, setChosenMetric] = useState<MetricKey>();
  const chartMetric = localOnly && chosenMetric === 'cost' ? 'tokens' : (chosenMetric ?? (localOnly ? 'tokens' : defaultMetric));
  const metricLabel = chartMetric === 'cost' ? 'Recorded spend (USD)' : chartMetric === 'requests' ? 'Requests' : 'Tokens';

  const dashboard = useQuery({
    queryKey: ['dashboard', 'team', teamId, range],
    queryFn: () => api.get<TeamDashboard>(`/api/v1/dashboard/team${qs({ team_id: teamId, range })}`),
  });

  const resolvedTeamId = teamId ?? dashboard.data?.team?.id;
  const detail = useQuery({
    queryKey: ['teams', 'detail', resolvedTeamId],
    queryFn: () => api.get<TeamDetail>(`/api/v1/teams/${resolvedTeamId}`),
    enabled: Boolean(resolvedTeamId),
    refetchInterval: 30000,
  });
  const manageHref = `${basePath}/${resolvedTeamId}?view=manage`;
  const membership = detail.data?.membership_role;
  const team = dashboard.data?.team;

  return (
    <div className="page team-dashboard">
      {administration ? (
        <Link className="small" to={`${basePath}?view=browse`}>
          All teams
        </Link>
      ) : null}
      <header className="team-hero">
        <div className="team-hero-id">
          <h1 className="page-title">{team?.name ?? t('team.fallbackTitle')}</h1>
          <div className="team-dashboard-context">
            {membership ? (
              <span className="team-role-pill">Team membership: {membership.charAt(0).toUpperCase() + membership.slice(1)}</span>
            ) : null}
            {detail.data?.actor_role === 'admin' ? <Badge tone="neutral">Organization administrator</Badge> : null}
            {team?.lead_name ? <span className="muted">Led by {team.lead_name}</span> : null}
          </div>
        </div>
        <div className="team-dashboard-actions">
          <RangePicker value={range} onChange={setRange} />
          {administration && detail.data?.can_manage ? (
            <Link className="btn btn-secondary btn-sm" to={manageHref}>
              Manage team
            </Link>
          ) : null}
          {administration && detail.data?.can_manage && (detail.data.pending_request_count ?? 0) > 0 ? (
            <Link
              className="team-dashboard-pending"
              aria-label={`${detail.data.pending_request_count} ${detail.data.pending_request_count === 1 ? 'request' : 'requests'} waiting`}
              to={`${manageHref}&section=requests`}
            >
              {detail.data.pending_request_count} {detail.data.pending_request_count === 1 ? 'request' : 'requests'} waiting
            </Link>
          ) : null}
        </div>
      </header>

      <AsyncSection
        query={dashboard}
        empty={{
          when: (data) => data.team === null,
          title: t('team.emptyTitle'),
          body: t('team.emptyBody'),
        }}
      >
        {(data) => (
          <>
            {data.teams.length > 1 ? (
              <nav className="team-switcher" aria-label={t('team.chooseTeam')}>
                {data.teams.map((option) => (
                  <Link
                    key={option.id}
                    to={`${basePath}/${option.id}${qs({ range })}`}
                    aria-current={option.id === data.team?.id ? 'page' : undefined}
                  >
                    {option.name}
                  </Link>
                ))}
              </nav>
            ) : null}

            <dl className="team-stats" aria-label="Team totals">
              <Stat label={t('team.members')} value={formatNumber(data.member_count)} />
              <Stat label={t('dashboard.requests')} value={formatNumber(data.totals.request_count)} />
              <Stat
                label={t('tables.tokens')}
                value={formatNumber(data.totals.tokens_in + data.totals.tokens_out, { compact: true })}
                sub={`${formatNumber(data.totals.tokens_in, { compact: true })} in · ${formatNumber(data.totals.tokens_out, { compact: true })} out`}
              />
              {localOnly ? null : <Stat label={t('dashboard.spend')} value={formatUSD(data.totals.cost_nanousd)} />}
              <Stat
                label="Errors"
                value={formatNumber(data.totals.error_count)}
                sub={
                  data.totals.request_count
                    ? `${((data.totals.error_count / data.totals.request_count) * 100).toFixed(1)}% of requests`
                    : undefined
                }
              />
            </dl>

            <div className="team-layout">
              <div className="team-main">
                <section className="card">
                  <div className="card-header">
                    <h2>{metricLabel} over time</h2>
                    <label className="team-metric">
                      <span>Chart metric</span>
                      <select
                        className="select"
                        aria-label="Chart metric"
                        value={chartMetric}
                        onChange={(event) => setChosenMetric(event.target.value as MetricKey)}
                      >
                        <option value="requests">Requests</option>
                        <option value="tokens">Tokens (input + output)</option>
                        {!localOnly ? <option value="cost">Recorded spend (USD)</option> : null}
                      </select>
                    </label>
                  </div>
                  <AreaChart series={data.series ?? []} metric={chartMetric} height={170} label={`Team ${metricLabel.toLowerCase()}`} />
                </section>
                <section className="card">
                  <div className="card-header">
                    <h2>{metricLabel} by model</h2>
                  </div>
                  <BarList items={data.per_model} metric={chartMetric} emptyLabel={t('team.noTeamUsage')} />
                </section>
              </div>

              <aside className="card team-roster" aria-label="Team members">
                <div className="card-header">
                  <h2>
                    {metricLabel} by member
                    <span className="team-roster-count">{formatNumber(data.member_count)}</span>
                  </h2>
                  {data.can_see_member_detail ? (
                    <Badge tone="primary">Member detail</Badge>
                  ) : (
                    <Badge tone="neutral">{t('team.anonymised')}</Badge>
                  )}
                </div>
                <MemberRoster
                  members={detail.data?.members ?? []}
                  usage={data.per_member}
                  metric={chartMetric}
                  detailed={data.can_see_member_detail}
                />
                {!data.can_see_member_detail && data.per_member.length > 0 ? (
                  <p className="small muted">{t('team.anonymisedNote')}</p>
                ) : null}
              </aside>
            </div>

            <TeamQuotaManagement teamId={data.team?.id} readOnly />
          </>
        )}
      </AsyncSection>
    </div>
  );
}

interface TeamDetail {
  membership_role: string;
  actor_role: string;
  can_manage: boolean;
  pending_request_count?: number;
  members?: Array<{ user_id: string; email: string; name?: string; role: string }>;
}

function initials(name: string): string {
  const parts = name.replace(/@.*/, '').split(/[\s._-]+/).filter(Boolean);
  return ((parts[0]?.[0] ?? '?') + (parts[1]?.[0] ?? '')).toUpperCase();
}

/**
 * One compact row per person: avatar, name, role, and — when the viewer may
 * see who is who — that person's share of the team's usage in the selected
 * metric. Members with no activity in the range still appear (at zero), so
 * the roster doubles as the member list. Without member detail, usage rows
 * are anonymous and the roster shows only names and roles.
 */
function MemberRoster({
  members,
  usage,
  metric,
  detailed,
}: {
  members: NonNullable<TeamDetail['members']>;
  usage: Breakdown[];
  metric: MetricKey;
  detailed: boolean;
}): ReactNode {
  const byUser = new Map(usage.map((row) => [row.key, row]));
  const rows = detailed
    ? [
        ...members.map((m) => ({
          key: m.user_id,
          name: m.name || m.email,
          email: m.name ? m.email : '',
          role: m.role,
          value: byUser.has(m.user_id) ? breakdownValue(byUser.get(m.user_id)!, metric) : 0,
        })),
        // Former members still carry attributed usage in the range.
        ...usage
          .filter((row) => row.key && !members.some((m) => m.user_id === row.key))
          .map((row) => ({ key: row.key, name: row.label, email: '', role: 'former', value: breakdownValue(row, metric) })),
      ]
    : members.map((m) => ({ key: m.user_id, name: m.name || m.email, email: m.name ? m.email : '', role: m.role, value: -1 }));
  rows.sort((a, b) => b.value - a.value || a.name.localeCompare(b.name));
  const max = Math.max(1, ...rows.map((r) => r.value));
  if (!rows.length) return <p className="small muted">{t('team.noMemberActivity')}</p>;
  return (
    <ul className="roster">
      {rows.map((row) => (
        <li key={row.key} className="roster-row">
          <span className="roster-avatar" aria-hidden="true">
            {initials(row.name)}
          </span>
          <span className="roster-id">
            <span className="roster-name truncate" title={row.email || row.name}>
              {row.name}
            </span>
            {row.role !== 'member' ? <span className="roster-role">{row.role}</span> : null}
          </span>
          {row.value > 0 ? (
            <span className="roster-usage">
              <span className="roster-bar" aria-hidden="true">
                <span style={{ width: `${Math.max(3, (row.value / max) * 100)}%` }} />
              </span>
              <span className="num small muted">{formatMetric(row.value, metric)}</span>
            </span>
          ) : row.value === 0 ? (
            <span className="roster-idle">No activity</span>
          ) : null}
        </li>
      ))}
    </ul>
  );
}

interface LeadTeamsResponse {
  teams: Array<{
    id: string;
    name: string;
    member_count: number;
    can_edit_quotas: boolean;
    quotas: QuotaStatus[];
  }>;
  metrics: Array<{ value: string; label: string }>;
  windows: Array<{ value: string; label: string }>;
}

/**
 * Delegated quota management: visible only when the viewer leads the
 * team AND an administrator has switched on lead_can_edit_quotas for it.
 */
export function TeamQuotaManagement({ teamId, readOnly = false }: { teamId?: string; readOnly?: boolean }): ReactNode {
  // Local-only mode: the server already withholds the Spend (USD) metric
  // option; the client filter below is defense against stale caches.
  const localOnly = useLocalOnly();
  const queryClient = useQueryClient();
  const toast = useToast();
  const lead = useQuery({
    queryKey: ['lead', 'teams'],
    queryFn: () => api.get<LeadTeamsResponse>('/api/v1/lead/teams'),
  });

  const [metric, setMetric] = useState('requests');
  const [window, setWindow] = useState('daily');
  const [limit, setLimit] = useState('');
  const [hardKill, setHardKill] = useState(false);

  const team = (lead.data?.teams ?? []).find((candidate) => (teamId ? candidate.id === teamId : true));

  const refresh = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['lead', 'teams'] });
  };

  const create = useMutation({
    mutationFn: (body: Record<string, unknown>) => api.post(`/api/v1/lead/teams/${team?.id}/quotas`, body),
    onSuccess: () => {
      toast(t('team.quotaCreated'));
      setLimit('');
      refresh();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/v1/lead/teams/${team?.id}/quotas/${id}`),
    onSuccess: () => {
      toast(t('team.quotaDeleted'));
      refresh();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  if (!team?.can_edit_quotas) {
    return null;
  }

  const submit = (event: FormEvent): void => {
    event.preventDefault();
    const value = Number(limit);
    if (!Number.isFinite(value) || value <= 0) {
      toast(t('team.limitValidation'), 'danger');
      return;
    }
    create.mutate({
      metric,
      window,
      limit_value: value,
      breach_behavior: hardKill ? 'hard_kill' : 'let_finish',
    });
  };

  return (
    <section className="card">
      <div className="card-header">
        <h2>{t('team.quotasTitle')}</h2>
        <Badge tone="primary">{t('team.delegatedBadge')}</Badge>
      </div>
      <p className="small muted">{t('team.delegatedNote', { name: team.name })}</p>

      {team.quotas.length === 0 ? (
        <EmptyState title={t('team.quotasEmptyTitle')} body={t('team.quotasEmptyBody')} />
      ) : (
        <div className="table-wrap">
          <table className="data">
            <thead>
              <tr>
                <th scope="col">{t('tables.metric')}</th>
                <th scope="col">{t('tables.limit')}</th>
                <th scope="col">{t('tables.utilisation')}</th>
                <th scope="col">{t('tables.resets')}</th>
                <th scope="col">{t('tables.onBreach')}</th>
                {!readOnly ? (
                  <th scope="col">
                    <span className="sr-only">{t('tables.actions')}</span>
                  </th>
                ) : null}
              </tr>
            </thead>
            <tbody>
              {team.quotas.map((quota) => (
                <tr key={quota.id}>
                  <td className="small">
                    {quota.metric_label} <span className="muted">{quota.window_label}</span>
                  </td>
                  <td className="num small">
                    {quota.unit === 'usd' ? formatUSD(quota.limit_value) : formatNumber(quota.limit_value)}
                  </td>
                  <td style={{ minWidth: 140 }}>
                    <div className="row" style={{ gap: 8 }}>
                      <div className="meter" style={{ flex: 1 }}>
                        <div
                          className="meter-fill"
                          data-level={quota.percent >= 100 ? 'critical' : quota.percent >= 80 ? 'warning' : undefined}
                          style={{ width: `${Math.min(100, Math.max(2, quota.percent))}%` }}
                        />
                      </div>
                      <span className="small num muted">{Math.round(quota.percent)}%</span>
                    </div>
                  </td>
                  <td className="small muted">{formatDateTime(quota.reset_at)}</td>
                  <td>
                    {quota.breach_behavior === 'hard_kill' ? (
                      <Badge tone="danger">{t('team.cutStreams')}</Badge>
                    ) : (
                      <Badge tone="neutral">{t('team.letFinish')}</Badge>
                    )}
                  </td>
                  {!readOnly ? (
                    <td style={{ textAlign: 'right' }}>
                      <button
                        type="button"
                        className="btn btn-ghost btn-sm"
                        disabled={remove.isPending}
                        onClick={() => remove.mutate(quota.id)}
                      >
                        {t('tables.delete')}
                      </button>
                    </td>
                  ) : null}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {!readOnly ? (
        <form onSubmit={submit} className="row wrap" style={{ alignItems: 'flex-end', gap: 12, marginTop: 12 }}>
          <Field label={t('tables.metric')}>
            <select className="select" value={metric} onChange={(event) => setMetric(event.target.value)}>
              {(lead.data?.metrics ?? [])
                .filter((option) => !localOnly || option.value !== 'cost_usd')
                .map((option) => (
                  <option key={option.value} value={option.value}>
                    {option.label}
                  </option>
                ))}
            </select>
          </Field>
          <Field label={t('tables.window')}>
            <select className="select" value={window} onChange={(event) => setWindow(event.target.value)}>
              {(lead.data?.windows ?? []).map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label}
                </option>
              ))}
            </select>
          </Field>
          <Field label={t('tables.limit')}>
            <input
              className="input"
              type="number"
              min="0"
              step="any"
              value={limit}
              onChange={(event) => setLimit(event.target.value)}
              placeholder={t('team.limitPlaceholder')}
              required
            />
          </Field>
          <Field label={t('tables.onBreach')}>
            <select
              className="select"
              value={hardKill ? 'hard_kill' : 'let_finish'}
              onChange={(event) => setHardKill(event.target.value === 'hard_kill')}
            >
              <option value="let_finish">{t('team.letRequestsFinish')}</option>
              <option value="hard_kill">{t('team.cutStreamsImmediately')}</option>
            </select>
          </Field>
          <button type="submit" className="btn btn-primary" disabled={create.isPending}>
            {t('team.addQuota')}
          </button>
        </form>
      ) : null}
    </section>
  );
}

function Stat({ label, value, sub }: { label: string; value: string; sub?: string }): ReactNode {
  return (
    <div className="team-stat">
      <dt className="overline">{label}</dt>
      <dd>
        <span className="team-stat-value">{value}</span>
        {sub ? <span className="team-stat-sub">{sub}</span> : null}
      </dd>
    </div>
  );
}
