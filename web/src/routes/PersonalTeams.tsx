import { Link, useLocation, useParams } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import { useUrlState } from '../lib/hooks';
import { useSession } from '../app/session';
import { AsyncSection, Badge, Field } from '../components/ui';
import { TeamPage } from './Team';
import './teams.css';
import './team-dashboard.css';

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
    <AsyncSection query={teams}>
        {(data) => {
          const selected = requested
            ? data.teams.find((team) => team.id === requested)
            : params.get('view') !== 'browse'
              ? (data.teams.find((team) => team.id === me?.active_team_id) ??
                (data.teams.length === 1 ? data.teams[0] : undefined))
              : undefined;
          if (requested && !selected)
            return (
              <div className="page">
                <p role="alert">This team is not one of your memberships.</p>
              </div>
            );
          return selected ? (
            <PersonalTeamDetail key={selected.id} team={selected} />
          ) : (
            <div className="page stack team-directory">
              <header className="page-header">
                <div>
                  <h1 className="page-title">My teams</h1>
                  <p className="page-subtitle">Your team memberships, members, usage, and quotas. This workspace is informational.</p>
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
                {!!data.teams.length && !data.teams.some((team) => team.name.toLowerCase().includes(search.toLowerCase())) && (
                  <p>No memberships match your search.</p>
                )}
              </section>
            </div>
          );
        }}
    </AsyncSection>
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
