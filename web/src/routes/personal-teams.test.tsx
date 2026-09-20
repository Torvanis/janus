import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import Teams from './Teams';
import { api } from '../lib/api';

vi.mock('../lib/api', async (original) => ({ ...(await original<typeof import('../lib/api')>()), api: { get: vi.fn() } }));
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
function mount(path: string, role = 'admin', membershipRole = 'member') {
  session.role = role;
  vi.mocked(api.get).mockImplementation(async (url) => {
    // GET /teams uses my_role for membership; detail my_role is the actor role.
    if (url === '/api/v1/teams')
      return {
        teams: [
          {
            id: 'mine',
            name: 'My engineering',
            my_role: membershipRole,
            listed: true,
            member_count: 1,
            can_manage: role === 'admin',
          },
          { id: 'other', name: 'Other organization team', listed: true, member_count: 2, can_manage: role === 'admin' },
        ],
      } as any;
    if (url === '/api/v1/teams/mine')
      return {
        team: { id: 'mine', name: 'My engineering' },
        membership_role: membershipRole,
        my_role: role === 'admin' ? 'admin' : 'member',
        can_manage: role === 'admin',
        members: [{ user_id: 'me', email: 'me@example.com', role: 'member' }],
      } as any;
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
it.each(['admin', 'user'])('lists only actual memberships for %s, never global management', async (role) => {
  mount('/teams?view=browse', role);
  await screen.findByText('My engineering');
  expect(screen.queryByText('Other organization team')).toBeNull();
  expect(screen.queryByRole('button', { name: 'Create team' })).toBeNull();
  expect(screen.queryByRole('link', { name: 'Import teams' })).toBeNull();
  expect(screen.queryByRole('link', { name: 'Manage team' })).toBeNull();
});
it.each(['admin', 'user'])('rejects nonmember deep links before detail or usage fetch for %s', async (role) => {
  mount('/teams/other?view=manage&section=settings', role);
  await screen.findByText('This team is not one of your memberships.');
  expect(api.get).not.toHaveBeenCalledWith('/api/v1/teams/other');
  expect(screen.queryByText('Other organization team')).toBeNull();
});
it.each(['admin', 'user'])('keeps member usage and roster informational even with old manage URLs for %s', async (role) => {
  mount('/teams/mine?view=manage&section=settings', role);
  await screen.findByRole('heading', { name: 'Usage for mine' });
  fireEvent.click(screen.getByRole('link', { name: 'Members' }));
  await screen.findByText('me@example.com');
  expect(screen.queryByRole('button', { name: 'Leave team' })).toBeNull();
  expect(screen.queryByRole('link', { name: 'Settings' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'Create team' })).toBeNull();
  expect(api.get).not.toHaveBeenCalledWith('/api/v1/admin/groups');
});
