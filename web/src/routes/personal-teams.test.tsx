import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import Teams from './Teams';
import { api } from '../lib/api';

vi.mock('../lib/api', async (original) => ({
  ...(await original<typeof import('../lib/api')>()),
  api: { get: vi.fn(), post: vi.fn(), del: vi.fn(), patch: vi.fn() },
}));
const session = vi.hoisted(() => ({ role: 'admin' }));
vi.mock('../app/session', () => ({ useSession: () => ({ me: { id: 'me', role: session.role } }) }));
vi.mock('./Team', () => ({
  TeamPage: ({ teamId }: { teamId: string }) => <h2>Usage for {teamId}</h2>,
  TeamQuotaManagement: () => null,
}));
afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});
function mount(path: string, role = 'admin', membershipRole = 'member', otherListed = true) {
  session.role = role;
  const manager = role === 'admin' || membershipRole === 'leader' || membershipRole === 'moderator';
  vi.mocked(api.get).mockImplementation(async (url) => {
    // GET /teams uses my_role for membership; detail my_role is the actor role.
    if (url === '/api/v1/teams' || url.startsWith('/api/v1/teams?q='))
      return {
        teams: [
          {
            id: 'mine',
            name: 'My engineering',
            my_role: membershipRole,
            listed: true,
            member_count: 1,
            can_manage: manager,
            pending_request_count: manager ? 1 : undefined,
          },
          { id: 'other', name: 'Other organization team', listed: otherListed, member_count: 2, can_manage: role === 'admin' },
        ],
      } as any;
    if (url === '/api/v1/team-requests') return { requests: [] } as any;
    if (url === '/api/v1/teams/mine')
      return {
        team: { id: 'mine', name: 'My engineering', listed: true },
        membership_role: membershipRole,
        my_role: role === 'admin' ? 'admin' : membershipRole,
        can_manage: manager,
        members: [{ user_id: 'me', email: 'me@example.com', role: membershipRole }],
        requests: manager
          ? [{ id: 'r1', user_id: 'u2', user_email: 'new@example.com', status: 'pending', reason: 'Please' }]
          : undefined,
        pending_request_count: manager ? 1 : undefined,
      } as any;
    if (url === '/api/v1/teams/other') {
      if (!otherListed) throw new Error('not found');
      return { team: { id: 'other', name: 'Other organization team', listed: true }, my_role: '', can_manage: false } as any;
    }
    if (url.startsWith('/api/v1/teams/mine/candidates')) return { users: [{ id: 'u3', email: 'pat@example.com' }] } as any;
    throw new Error(`Unexpected API ${url}`);
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/teams" element={<Teams />} />
          <Route path="/teams/:id" element={<Teams />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}
it.each(['admin', 'user'])('lists actual memberships, and other listed teams only to join, for %s', async (role) => {
  mount('/teams?view=browse', role);
  const mine = await screen.findByRole('region', { name: 'My team memberships' });
  expect(within(mine).getByText('My engineering')).toBeTruthy();
  expect(within(mine).queryByText('Other organization team')).toBeNull();
  const find = await screen.findByRole('region', { name: 'Find a team' });
  expect(await within(find).findByText('Other organization team')).toBeTruthy();
  expect(within(find).getByRole('link', { name: 'Request to join' }).getAttribute('href')).toBe('/teams/other');
  expect(within(find).queryByText('My engineering')).toBeNull();
  // Organization administration never appears in the personal workspace.
  expect(screen.queryByRole('button', { name: 'Create team' })).toBeNull();
  expect(screen.queryByRole('link', { name: 'Import teams' })).toBeNull();
});

it('a person can ask to join a listed team they are not in', async () => {
  mount('/teams/other', 'user');
  await screen.findByRole('heading', { name: 'Join this team' });
  vi.mocked(api.post).mockResolvedValue({} as never);
  await userEvent.setup().click(screen.getByRole('button', { name: 'Request to join' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/teams/other/join-requests', { reason: '' }));
  // Not a member: no roster, no management.
  expect(screen.queryByRole('heading', { name: 'Add a member' })).toBeNull();
  expect(api.get).not.toHaveBeenCalledWith('/api/v1/admin/groups');
});

it('an unlisted team someone is not in is refused, not shown', async () => {
  mount('/teams/other', 'user', 'member', false);
  expect(await screen.findByText(/not found/i)).toBeTruthy();
  expect(screen.queryByRole('heading', { name: 'Join this team' })).toBeNull();
});

it.each(['leader', 'moderator'])('a team %s manages members and requests outside Administration', async (membershipRole) => {
  mount('/teams?view=browse', 'user', membershipRole);
  const callout = await screen.findByRole('link', { name: /My engineering: 1 request waiting/ });
  expect(callout.getAttribute('href')).toBe('/teams/mine?view=manage&section=requests');
  cleanup();
  mount('/teams/mine?view=manage&section=members', 'user', membershipRole);
  await screen.findByRole('heading', { name: 'Add a member' });
  const user = userEvent.setup();
  await user.type(screen.getByLabelText('Find people by email'), 'pat');
  await user.click(await screen.findByRole('button', { name: 'Select pat@example.com' }));
  vi.mocked(api.post).mockResolvedValue({} as never);
  await user.click(screen.getByRole('button', { name: 'Add member' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/teams/mine/members', { user_id: 'u3', role: 'member' }));
  // Section links stay in the personal workspace.
  const nav = screen.getByRole('navigation', { name: 'Team administration' });
  for (const link of within(nav).getAllByRole('link')) expect(link.getAttribute('href')).toMatch(/^\/teams\/mine\?/);
  // Settings (rename, listing) are a lead's only.
  expect(!!within(nav).queryByRole('link', { name: 'Settings' })).toBe(membershipRole === 'leader');
  expect(api.get).not.toHaveBeenCalledWith('/api/v1/admin/groups');
});

it('a team moderator decides a join request', async () => {
  mount('/teams/mine?view=manage&section=requests', 'user', 'moderator');
  await screen.findByText('new@example.com');
  vi.mocked(api.post).mockResolvedValue({} as never);
  await userEvent.setup().click(screen.getByRole('button', { name: 'Approve' }));
  await waitFor(() =>
    expect(api.post).toHaveBeenCalledWith('/api/v1/teams/mine/requests/r1/decision', { approve: true, reason: '' }),
  );
});

it('a plain member sees the roster and can leave, but cannot add people', async () => {
  mount('/teams/mine?view=manage&section=members', 'user', 'member');
  await screen.findByRole('button', { name: 'Leave team' });
  expect(screen.getByText('me@example.com')).toBeTruthy();
  expect(screen.queryByRole('heading', { name: 'Add a member' })).toBeNull();
  expect(screen.queryByRole('link', { name: /Requests/ })).toBeNull();
});

it('a member opens their team page (usage) by default, with a way to membership', async () => {
  mount('/teams/mine', 'user', 'member');
  await screen.findByRole('heading', { name: 'Usage for mine' });
});
