import { Link, useLocation, useParams } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import { useUrlState } from '../lib/hooks';
import { useSession } from '../app/session';
import { AsyncSection, Badge, Field } from '../components/ui';
import { RouteTopNav } from '../components/RouteTopNav';
import { TeamPage } from './Team';
import './teams.css';

type Membership = { id: string; name: string; my_role?: string; member_count: number };

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
    <div className="page stack team-directory">
      <header className="page-header">
        <div>
          <h1 className="page-title">My teams</h1>
          <p className="page-subtitle">Your team memberships, members, usage, and quotas. This workspace is informational.</p>
        </div>
      </header>
      <AsyncSection query={teams}>
        {(data) => {
          const selected = requested
            ? data.teams.find((team) => team.id === requested)
            : params.get('view') !== 'browse'
              ? (data.teams.find((team) => team.id === me?.active_team_id) ??
                (data.teams.length === 1 ? data.teams[0] : undefined))
              : undefined;
          if (requested && !selected) return <p role="alert">This team is not one of your memberships.</p>;
          return selected ? (
            <>
              <Link className="btn btn-ghost" to="/teams?view=browse">
                My teams
              </Link>
              <PersonalTeamDetail key={selected.id} team={selected} />
            </>
          ) : (
            <>
              <Field label="Search my teams">
                <input
                  className="input"
                  value={search}
                  onChange={(event) => {
                    setSearch(event.target.value);
                  }}
                />
              </Field>
              <section className="card stack" aria-label="My team memberships">
                {!data.teams.length && <p>You are not a member of any teams.</p>}
                {data.teams
                  .filter((team) => team.name.toLowerCase().includes(search.toLowerCase()))
                  .map((team) => (
                    <div className="row-between" key={team.id}>
                      <Link to={`/teams/${encodeURIComponent(team.id)}`}>{team.name}</Link>
                      <span>
                        {team.member_count} members · <Badge>{team.my_role}</Badge>
                      </span>
                    </div>
                  ))}
                {!!data.teams.length && !data.teams.some((team) => team.name.toLowerCase().includes(search.toLowerCase())) && (
                  <p>No memberships match your search.</p>
                )}
              </section>
            </>
          );
        }}
      </AsyncSection>
    </div>
  );
}

function PersonalTeamDetail({ team }: { team: Membership }) {
  const location = useLocation();
  const section = new URLSearchParams(location.search).get('section') === 'members' ? 'members' : 'usage';
  const detail = useQuery({
    queryKey: ['teams', 'personal-detail', team.id],
    queryFn: () =>
      api.get<{ members: { user_id: string; email: string; name?: string; role: string }[] }>(
        `/api/v1/teams/${encodeURIComponent(team.id)}`,
      ),
    enabled: section === 'members',
  });
  return (
    <section className="stack">
      <h2>{team.name}</h2>
      <p>Your role: {team.my_role}</p>
      <RouteTopNav
        label="Team information"
        active={section}
        items={[
          { id: 'usage', label: 'Usage and quotas' },
          { id: 'members', label: 'Members' },
        ].map((item) => ({ ...item, to: `/teams/${encodeURIComponent(team.id)}?section=${item.id}` }))}
      />
      {section === 'usage' ? (
        <TeamPage teamId={team.id} />
      ) : (
        <AsyncSection query={detail}>
          {(data) => (
            <div className="table-wrap">
              <table className="data" aria-label="Team members">
                <thead>
                  <tr>
                    <th>Person</th>
                    <th>Role</th>
                  </tr>
                </thead>
                <tbody>
                  {data.members.map((member) => (
                    <tr key={member.user_id}>
                      <td>
                        {member.name ? `${member.name} · ` : ''}
                        {member.email}
                      </td>
                      <td>{member.role}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </AsyncSection>
      )}
    </section>
  );
}
