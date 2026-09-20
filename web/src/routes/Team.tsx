import { useState, type FormEvent, type ReactNode } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, qs } from '../lib/api';
import type { Breakdown, QuotaStatus, Team, Totals } from '../lib/types';
import { formatDateTime, formatNumber, formatUSD } from '../lib/format';
import { BarList, type MetricKey } from '../components/charts';
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
    queryFn: () =>
      api.get<{ membership_role: string; actor_role: string; can_manage: boolean; pending_request_count?: number }>(
        `/api/v1/teams/${resolvedTeamId}`,
      ),
    enabled: Boolean(resolvedTeamId),
    refetchInterval: 30000,
  });
  const manageHref = `${basePath}/${resolvedTeamId}?view=manage`;
  const membership = detail.data?.membership_role;

  return (
    <div className="page team-dashboard">
      <Link className="small" to={`${basePath}?view=browse`}>
        {administration ? 'All teams' : 'My teams'}
      </Link>
      <header className="page-header">
        <div>
          <h1 className="page-title">{dashboard.data?.team?.name ?? t('team.fallbackTitle')}</h1>
          <p className="page-subtitle">Team usage · activity attributed to this team, including former members.</p>
          <div className="team-dashboard-context">
            {membership ? <span>Team membership: {membership.charAt(0).toUpperCase() + membership.slice(1)}</span> : null}
            {detail.data?.actor_role === 'admin' ? <Badge tone="neutral">Organization administrator</Badge> : null}
          </div>
        </div>
        <div className="team-dashboard-actions">
          <RangePicker value={range} onChange={setRange} />
          {administration && detail.data?.can_manage ? (
            <Link className="btn btn-secondary" to={manageHref}>
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
              <div className="segmented" role="group" aria-label={t('team.chooseTeam')}>
                {data.teams.map((team) => (
                  <Link
                    key={team.id}
                    to={`${basePath}/${team.id}${qs({ range })}`}
                    aria-current={team.id === data.team?.id ? 'page' : undefined}
                  >
                    {team.name}
                  </Link>
                ))}
              </div>
            ) : null}

            <div className="grid grid-tiles">
              <Tile label={t('team.members')} value={formatNumber(data.member_count)} />
              <Tile label={t('dashboard.requests')} value={formatNumber(data.totals.request_count)} />
              <Tile
                label={t('tables.tokens')}
                value={formatNumber(data.totals.tokens_in + data.totals.tokens_out, { compact: true })}
              />
              {localOnly ? null : <Tile label={t('dashboard.spend')} value={formatUSD(data.totals.cost_nanousd)} />}
            </div>

            <div className="team-dashboard-chart-toolbar">
              <h2>Usage breakdown</h2>
              <label className="small">
                Chart metric
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
            <div className="grid grid-halves">
              <section className="card">
                <div className="card-header">
                  <h2>{metricLabel} by model</h2>
                </div>
                <BarList items={data.per_model} metric={chartMetric} emptyLabel={t('team.noTeamUsage')} />
              </section>

              <section className="card">
                <div className="card-header">
                  <h2>{metricLabel} by member</h2>
                  {data.can_see_member_detail ? (
                    <Badge tone="primary">Member detail</Badge>
                  ) : (
                    <Badge tone="neutral">{t('team.anonymised')}</Badge>
                  )}
                </div>
                {data.per_member.length === 0 ? (
                  <EmptyState title={t('team.noActivityTitle')} body={t('team.noActivityBody')} />
                ) : (
                  <>
                    <BarList items={data.per_member} metric={chartMetric} emptyLabel={t('team.noMemberActivity')} />
                    {!data.can_see_member_detail ? (
                      <p className="small muted" style={{ marginTop: 'var(--janus-space-3)' }}>
                        {t('team.anonymisedNote')}
                      </p>
                    ) : null}
                  </>
                )}
              </section>
            </div>

            <TeamQuotaManagement teamId={data.team?.id} readOnly />
          </>
        )}
      </AsyncSection>
    </div>
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

function Tile({ label, value }: { label: string; value: string }): ReactNode {
  return (
    <article className="tile">
      <div className="overline">{label}</div>
      <div className="tile-value">{value}</div>
    </article>
  );
}
