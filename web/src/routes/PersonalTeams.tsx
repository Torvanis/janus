import { Link, useLocation, useParams } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import { useUrlState } from '../lib/hooks';
import { useSession } from '../app/session';
import { AsyncSection, Badge, Field } from '../components/ui';
import { TeamPage } from './Team';
import { TeamWorkspace } from './Teams';
import './teams.css';
import './team-dashboard.css';

type Membership = {
  id: string;
  name: string;
  my_role?: string;
  member_count: number;
  listed?: boolean;
  can_manage?: boolean;
  pending_request_count?: number;
};
type JoinRequest = {
  id: string;
  team_id?: string;
  team_name?: string;
  status: string;
  reason?: string;
  decision_reason?: string;
};

/** Directory my_role is membership, unlike detail my_role (which can be admin).
 * Keep this projection separate from the administration cache and never use
 * can_manage as membership. The API authorization contract remains unchanged.
 */
export function usePersonalTeams() {
  const { me } = useSession();
  return useQuery({
    queryKey: ['teams', 'personal', me?.id],
    queryFn: () => api.get<{ teams: Membership[] }>('/api/v1/teams'),
    select: (data) => ({ teams: data.teams.filter((team) => ['member', 'moderator', 'leader'].includes(team.my_role ?? '')) }),
    enabled: !!me,
    refetchInterval: 30_000,
  });
}

export default function PersonalTeams() {
  const teams = usePersonalTeams();
  const { me } = useSession();
  const { id } = useParams();
  const location = useLocation();
  const params = new URLSearchParams(location.search);
  const requested = id || params.get('team');
  const [search, setSearch] = useUrlState('q', '');
  return (
    <AsyncSection query={teams}>
      {(data) => {
        const selected = requested
          ? data.teams.find((team) => team.id === requested)
          : params.get('view') !== 'browse'
            ? (data.teams.find((team) => team.id === me?.active_team_id) ?? (data.teams.length === 1 ? data.teams[0] : undefined))
            : undefined;
        // Managing a team, or a team you are not in (to ask to join): the
        // team workspace. The API decides what each role may do.
        if (requested && (!selected || params.get('view') === 'manage')) return <TeamWorkspace id={requested} />;
        return selected ? (
          <PersonalTeamDetail key={selected.id} team={selected} />
        ) : (
          <div className="page stack team-directory">
            <header className="page-header">
              <div>
                <h1 className="page-title">My teams</h1>
                <p className="page-subtitle">Your teams, their members and usage. Find other teams to join below.</p>
              </div>
            </header>
            {data.teams.length > 6 ? (
              <Field label="Search my teams">
                <input
                  className="input"
                  value={search}
                  onChange={(event) => {
                    setSearch(event.target.value);
                  }}
                />
              </Field>
            ) : null}
            <section className="team-cards" aria-label="My team memberships">
              {!data.teams.length && <p>You are not a member of any teams.</p>}
              {data.teams
                .filter((team) => team.name.toLowerCase().includes(search.toLowerCase()))
                .map((team) => (
                  <Link className="team-card" key={team.id} to={`/teams/${encodeURIComponent(team.id)}`}>
                    <span className="team-card-mark" aria-hidden="true">
                      {team.name
                        .split(/\s+/)
                        .map((w) => w[0])
                        .join('')
                        .slice(0, 2)
                        .toUpperCase()}
                    </span>
                    <span className="team-card-body">
                      <span className="team-card-name">{team.name}</span>
                      <span className="team-card-meta">
                        {team.member_count} {team.member_count === 1 ? 'member' : 'members'}
                      </span>
                    </span>
                    <Badge>{team.my_role}</Badge>
                  </Link>
                ))}
              {data.teams
                .filter((team) => (team.pending_request_count ?? 0) > 0)
                .map((team) => (
                  <Link
                    className="btn team-request-callout"
                    key={`req-${team.id}`}
                    to={`/teams/${encodeURIComponent(team.id)}?view=manage&section=requests`}
                  >
                    {team.name}: {team.pending_request_count} {team.pending_request_count === 1 ? 'request' : 'requests'} waiting
                  </Link>
                ))}
              {!!data.teams.length && !data.teams.some((team) => team.name.toLowerCase().includes(search.toLowerCase())) && (
                <p>No memberships match your search.</p>
              )}
            </section>
            <FindTeams />
          </div>
        );
      }}
    </AsyncSection>
  );
}

/** Listed teams you are not in, with your join requests: ask to join from
 * the team's page. */
function FindTeams() {
  const [search, setSearch] = useUrlState('find', '');
  const directory = useQuery({
    queryKey: ['teams', 'find', search],
    queryFn: () => api.get<{ teams: Membership[] }>(`/api/v1/teams${search ? `?q=${encodeURIComponent(search)}` : ''}`),
  });
  const requests = useQuery({
    queryKey: ['teams', 'request-history'],
    queryFn: () => api.get<{ requests: JoinRequest[] }>('/api/v1/team-requests'),
  });
  const statusFor = (id: string) => requests.data?.requests.find((r) => r.team_id === id)?.status;
  const others = (directory.data?.teams ?? []).filter((team) => !team.my_role && team.listed !== false);
  return (
    <section className="card stack" aria-label="Find a team">
      <div>
        <h2 style={{ margin: 0 }}>Find a team</h2>
        <p className="small secondary" style={{ margin: '4px 0 0' }}>
          Ask to join a listed team. A team lead or moderator reviews your request.
        </p>
      </div>
      <Field label="Search teams">
        <input className="input" type="search" value={search} onChange={(e) => setSearch(e.target.value)} />
      </Field>
      <AsyncSection
        query={directory}
        empty={{
          when: () => !others.length,
          title: 'No other teams to join',
          body: 'Unlisted teams don’t appear here; a team lead can add you directly.',
        }}
      >
        {() => (
          <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0 }}>
            {others.map((team) => {
              const status = statusFor(team.id);
              return (
                <li className="row-between team-directory-row" key={team.id}>
                  <span>
                    {team.name}{' '}
                    <span className="small muted">
                      {team.member_count} {team.member_count === 1 ? 'member' : 'members'}
                    </span>
                  </span>
                  <span className="row">
                    {status ? <Badge>Your request: {status}</Badge> : null}
                    <Link className="btn btn-secondary btn-sm" to={`/teams/${encodeURIComponent(team.id)}`}>
                      {status === 'pending' ? 'View request' : 'Request to join'}
                    </Link>
                  </span>
                </li>
              );
            })}
          </ul>
        )}
      </AsyncSection>
      {!!requests.data?.requests.some((r) => r.status !== 'pending') && (
        <details>
          <summary>Past membership requests</summary>
          <ul>
            {requests.data.requests
              .filter((r) => r.status !== 'pending')
              .map((r) => (
                <li key={r.id}>
                  {r.team_name || 'Team'} — <Badge>{r.status}</Badge>
                  {r.decision_reason ? <span className="small muted"> {r.decision_reason}</span> : null}
                </li>
              ))}
          </ul>
        </details>
      )}
    </section>
  );
}

/**
 * A membership's workspace is the team page itself: one header (name, your
 * role, lead, range), a totals strip, the activity trend, usage by model and
 * the member roster side by side. The old Usage / Members tabs, the repeated
 * team name and the "My teams" back button are gone; the breadcrumb and the
 * team switcher cover navigation.
 */
function PersonalTeamDetail({ team }: { team: Membership }) {
  return <TeamPage teamId={team.id} />;
}
