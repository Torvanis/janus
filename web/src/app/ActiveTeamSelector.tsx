import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/api';

/** Browser-only selection: API-key affinity is edited separately on Tokens. */
export function ActiveTeamSelector({
  teams,
  activeTeam,
  refresh,
}: {
  teams: { id: string; name: string }[];
  activeTeam: string;
  refresh: () => void;
}) {
  const queryClient = useQueryClient();
  const change = useMutation({
    mutationFn: (teamID: string) => api.put('/api/v1/me/active-team', { team_id: teamID }),
    onSuccess: async () => {
      await queryClient.invalidateQueries();
      refresh();
    },
  });
  const unavailable = activeTeam && !teams.some((team) => team.id === activeTeam);
  return (
    <div className="active-team-context" style={{ minWidth: 0 }}>
      <label style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
        <span>Working under</span>
        <select
          aria-label="Working under"
          value={activeTeam}
          disabled={change.isPending}
          onChange={(event) => change.mutate(event.target.value)}
          title="Browser context for model views. This selection does not change API keys."
        >
          <option value="">Personal</option>
          {unavailable ? <option value={activeTeam}>Team unavailable — select another context</option> : null}
          {teams.map((team) => (
            <option key={team.id} value={team.id}>
              {team.name}
            </option>
          ))}
        </select>
      </label>
      <a className="small" href="/tokens" title="Browser selection does not change API keys. Manage each key’s team on Tokens.">
        Manage token teams
      </a>
      {change.isError ? (
        <span role="alert">{change.error instanceof Error ? change.error.message : 'Unable to change team.'}</span>
      ) : null}
    </div>
  );
}
