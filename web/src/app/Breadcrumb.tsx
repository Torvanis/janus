import type { ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import { t } from '../lib/i18n';
import { useSession } from './session';

/** Names use directory membership, not detail actor_role or can_manage. */
function useTeamName(id: string | undefined, administration: boolean): string | undefined {
  const { me } = useSession();
  const team = useQuery({
    queryKey: ['teams', 'breadcrumb', administration, me?.id],
    queryFn: () => api.get<{ teams: { id: string; name: string; my_role?: string }[] }>('/api/v1/teams'),
    enabled: !!id && !!me && (!administration || me.role === 'admin'),
    staleTime: 30000,
  });
  if (!id) return undefined;
  const match = team.data?.teams.find(
    (item) => item.id === id && (administration || ['member', 'moderator', 'leader'].includes(item.my_role ?? '')),
  );
  return match?.name ?? 'Team';
}

/** A subscription provider's name from the page's own (already cached) list. */
function useSubscriptionProviderName(id: string | undefined): string | undefined {
  const list = useQuery({
    queryKey: ['subscriptions'],
    queryFn: () => api.get<{ providers: { id: string; name: string }[] }>('/api/v1/me/subscriptions'),
    enabled: !!id,
    staleTime: 30000,
  });
  if (!id) return undefined;
  return list.data?.providers.find((p) => p.id === id)?.name ?? slugName(id);
}

/** An auto-routing router's name from the (already cached) routers list. */
function useRouterName(id: string | undefined): string | undefined {
  const list = useQuery({
    queryKey: ['admin', 'routing', 'routers'],
    queryFn: () => api.get<{ routers: { id: string; name: string }[] }>('/api/v1/admin/routing/routers'),
    enabled: !!id && id !== 'new',
    staleTime: 30000,
  });
  if (!id) return undefined;
  if (id === 'new') return 'New router';
  return list.data?.routers.find((r) => r.id === id)?.name ?? 'Router';
}

/** A chat's title from the (already cached) conversation list, never its id. */
function useChatTitle(id: string | undefined): string | undefined {
  const list = useQuery({
    queryKey: ['chat', 'conversations'],
    queryFn: () => api.get<{ conversations: { id: string; title: string }[] }>('/api/v1/chat/conversations'),
    enabled: !!id,
    staleTime: 30000,
  });
  if (!id) return undefined;
  const title = list.data?.conversations.find((c) => c.id === id)?.title;
  return title || (list.data ? 'New chat' : '…');
}

/** A route segment with no known name: "pricing-accuracy" → "Pricing accuracy". */
function slugName(segment: string): string {
  const words = segment.replace(/-/g, ' ');
  return words.charAt(0).toUpperCase() + words.slice(1);
}

const NAMES: Record<string, string> = {
  admin: 'Administration',
  people: 'People',
  users: 'People',
  general: 'General',
  'sign-in': 'Sign-in & provisioning',
  license: 'License & updates',
  troubleshooting: 'Troubleshooting',
  status: 'System status',
  import: 'Import teams',
  routing: 'Auto-routing',
  chat: 'Chat',
};

/** The page's place in the app, on ONE line in the top bar. When it does not
 *  fit, the middle crumbs give way first (down to "…") and the page's own
 *  name last (ellipsis); every crumb carries its full name as a tooltip, so
 *  the top bar never grows or covers the controls beside it. */
export function Breadcrumb({ path }: { path: string }): ReactNode {
  let segments = path.split('/').filter(Boolean);
  const administration = segments[0] === 'admin' && segments[1] === 'teams';
  if (administration) segments = ['admin', 'people', ...segments.slice(1)];
  const teamIndex = administration ? 3 : segments[0] === 'teams' ? 1 : -1;
  const teamSegment = teamIndex >= 0 && segments[teamIndex] !== 'import' ? segments[teamIndex] : undefined;
  const sharedChat = segments[0] === 'chat' && segments[1] === 'shared';
  const teamName = useTeamName(teamSegment, administration);
  const chatTitle = useChatTitle(segments[0] === 'chat' && !sharedChat ? segments[1] : undefined);
  const providerName = useSubscriptionProviderName(segments[0] === 'subscriptions' ? segments[1] : undefined);
  const routerName = useRouterName(segments[1] === 'routing' && segments[2] === 'routers' ? segments[3] : undefined);
  const label = (segment: string, index: number): string => {
    if (index === teamIndex && teamName !== undefined) return teamName;
    if (sharedChat && index === 1) return 'Shared';
    if (sharedChat && index === 2) return 'Shared chat';
    if (index === 1 && chatTitle !== undefined) return chatTitle;
    if (index === 1 && providerName !== undefined) return providerName;
    if (index === 3 && routerName !== undefined) return routerName;
    if (segment === 'teams') return administration ? 'Teams' : 'My teams';
    if (segment === 'settings') return segments[0] === 'admin' ? 'Settings' : 'Personal settings';
    return NAMES[segment] ?? slugName(segment);
  };
  return (
    <nav aria-label={t('shell.breadcrumb')} className="breadcrumb">
      <span className="breadcrumb-root muted">Janus</span>
      {segments.map((segment, index) => {
        const name = label(segment, index);
        const last = index === segments.length - 1;
        return (
          <span key={`${segment}-${index}`} className={last ? 'breadcrumb-crumb breadcrumb-last' : 'breadcrumb-crumb'}>
            <span className="breadcrumb-sep muted" aria-hidden="true">
              {' / '}
            </span>
            <span className="breadcrumb-name" title={name} aria-current={last ? 'page' : undefined}>
              {name}
            </span>
          </span>
        );
      })}
    </nav>
  );
}
