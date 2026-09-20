import type { ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import { t } from '../lib/i18n';
import { useSession } from './session';

/** Names use directory membership, not detail actor_role or can_manage. */
function TeamName({ id, administration }: { id: string; administration: boolean }) {
  const { me } = useSession();
  const team = useQuery({
    queryKey: ['teams', 'breadcrumb', administration, me?.id],
    queryFn: () => api.get<{ teams: { id: string; name: string; my_role?: string }[] }>('/api/v1/teams'),
    enabled: !!me && (!administration || me.role === 'admin'),
    staleTime: 30000,
  });
  const match = team.data?.teams.find(
    (item) => item.id === id && (administration || ['member', 'moderator', 'leader'].includes(item.my_role ?? '')),
  );
  return <>{match?.name ?? 'Team'}</>;
}

export function Breadcrumb({ path }: { path: string }): ReactNode {
  let segments = path.split('/').filter(Boolean);
  const administration = segments[0] === 'admin' && segments[1] === 'teams';
  if (administration) segments = ['admin', 'people', ...segments.slice(1)];
  const teamIndex = administration ? 3 : segments[0] === 'teams' ? 1 : -1;
  return (
    <nav aria-label={t('shell.breadcrumb')} className="breadcrumb">
      <span className="muted">Janus</span>
      {segments.map((segment, index) => (
        <span key={`${segment}-${index}`}>
          <span className="muted" aria-hidden="true">
            {' / '}
          </span>
          <span>
            {index === teamIndex && segment !== 'import' ? (
              <TeamName id={segment} administration={administration} />
            ) : (
              ({
                admin: 'Administration',
                people: 'People',
                users: 'People',
                teams: administration ? 'Teams' : 'My teams',
                settings: segments[0] === 'admin' ? 'Settings' : 'Personal settings',
                general: 'General',
                'sign-in': 'Sign-in & provisioning',
                license: 'License & updates',
                troubleshooting: 'Troubleshooting',
                status: 'System status',
                import: 'Import teams',
              }[segment] ?? segment.replace(/-/g, ' '))
            )}
          </span>
        </span>
      ))}
    </nav>
  );
}
