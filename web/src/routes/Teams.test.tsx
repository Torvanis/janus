import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { Link, MemoryRouter, Route, Routes, useLocation, useNavigate } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api, ApiError } from '../lib/api';
import { TeamManagementWorkspace as Teams } from './Teams';
vi.mock('./Team', () => ({
  TeamPage: ({ teamId }: { teamId: string }) => (
    <>
      <h1>Usage dashboard for {teamId}</h1>
      <Link to="/admin/teams?view=browse">Browse teams</Link>
    </>
  ),
  TeamQuotaManagement: ({ teamId }: { teamId: string }) => <h3>Quota administration for {teamId}</h3>,
}));
it('places delegated quota administration only in manager settings', async () => {
  mount('/admin/teams/t1?view=manage&section=settings');
  await screen.findByRole('heading', { name: 'Quota administration for t1' });
  fireEvent.click(screen.getByRole('link', { name: 'Members' }));
  expect(screen.queryByRole('heading', { name: 'Quota administration for t1' })).toBeNull();
});
it('opens an explicit administration team selection at usage', async () => {
  mount('/admin/teams?team=t1');
  await screen.findByRole('heading', { name: 'Usage dashboard for t1' });
  expect(screen.queryByLabelText('Team name')).toBeNull();
  expect(screen.queryByRole('region', { name: 'Team directory' })).toBeNull();
  expect(screen.getByRole('link', { name: 'Browse teams' })).toBeTruthy();
});
vi.mock('../lib/api', async (original) => ({
  ...(await original<typeof import('../lib/api')>()),
  api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
}));
const session = vi.hoisted(() => ({ role: 'user', active_team_id: '' }));
vi.mock('../app/session', () => ({
  useSession: () => ({ me: { id: 'me', role: session.role, active_team_id: session.active_team_id }, refresh: vi.fn() }),
}));
afterEach(cleanup);
let role = 'leader';
let listed = true;
beforeEach(() => {
  vi.resetAllMocks();
  role = 'leader';
  listed = true;
  session.role = 'user';
  session.active_team_id = '';
  vi.mocked(api.get).mockImplementation((async (path: string) => {
    const team = { id: 't1', name: 'Engineering', listed, my_role: role, member_count: 2 };
    if (path.startsWith('/api/v1/teams/t1/candidates'))
      return { users: [{ id: 'u2', email: 'new@example.com', name: 'New User' }] };
    if (path === '/api/v1/teams/t1')
      return {
        team,
        my_role: role,
        members: [
          { user_id: 'me', email: 'me@example.com', role: 'leader', sources: [{ source_type: 'manual' }] },
          { user_id: 'u1', email: 'member@example.com', role: 'member', sources: [{ source_type: 'group', source_id: 'g1' }] },
        ],
        requests: [
          {
            id: 'r1',
            user_id: 'u3',
            user_email: 'request@example.com',
            user_name: 'Requesting Person',
            status: 'pending',
            reason: 'Join please',
          },
        ],
        group_ids: [],
      };
    if (path === '/api/v1/team-requests') return { requests: [] };
    return { teams: [team] };
  }) as typeof api.get);
});
it('requires an explicit named save before applying a drafted role', async () => {
  mount('/admin/teams/t1?view=manage');
  fireEvent.change(await screen.findByLabelText('Role for member@example.com'), { target: { value: 'moderator' } });
  expect(api.patch).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole('button', { name: 'Save role for member@example.com as moderator' }));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('/api/v1/teams/t1/members/u1', { role: 'moderator' }));
});
it('limits moderators to ordinary member management', async () => {
  role = 'moderator';
  mount();
  await screen.findByLabelText('Find people by email');
  expect(screen.queryByLabelText('New member role')).toBeNull();
  expect(screen.queryByLabelText('Role for member@example.com')).toBeNull();
  expect(screen.queryByRole('button', { name: 'Save team' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Remove member@example.com' })).toBeTruthy();
});
it('keeps an ordinary member roster read-only', async () => {
  role = 'member';
  mount();
  await screen.findByText('member@example.com');
  expect(screen.queryByLabelText('Find people by email')).toBeNull();
  expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Leave team' })).toBeTruthy();
});
it('keeps existing requests visible and rejectable after a team becomes unlisted', async () => {
  listed = false;
  mount('/admin/teams/t1?view=manage&section=requests');
  expect(await screen.findByRole('link', { name: 'Requests (1)' })).toBeTruthy();
  expect(screen.getByText('Requesting Person')).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Deny' }));
  await waitFor(() =>
    expect(api.post).toHaveBeenCalledWith('/api/v1/teams/t1/requests/r1/decision', { approve: false, reason: '' }),
  );
});

it('disables all requests and approvals for unlisted teams', async () => {
  listed = false;
  mount();
  await screen.findByText(/Unlisted teams are direct-add only/);
  expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'Request to join' })).toBeNull();
});
it('sends a join reason for a listed team as a nonmember', async () => {
  role = '';
  mount();
  fireEvent.change(await screen.findByLabelText('Why would you like to join?'), { target: { value: 'Working together' } });
  fireEvent.click(screen.getByRole('button', { name: 'Request to join' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/teams/t1/join-requests', { reason: 'Working together' }));
  expect(screen.queryByText('member@example.com')).toBeNull();
});
it('approves a request with an explicit reason', async () => {
  mount('/admin/teams/t1?view=manage&section=requests');
  fireEvent.change(await screen.findByLabelText('Decision reason for request@example.com'), { target: { value: 'Welcome' } });
  fireEvent.click(screen.getByRole('button', { name: 'Approve' }));
  await waitFor(() =>
    expect(api.post).toHaveBeenCalledWith('/api/v1/teams/t1/requests/r1/decision', { approve: true, reason: 'Welcome' }),
  );
});
it('confirms removal and explains retained group access', async () => {
  mount();
  fireEvent.click(await screen.findByRole('button', { name: 'Remove member@example.com' }));
  expect(screen.getByText(/Only direct membership will be removed/)).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Remove direct membership' }));
  await waitFor(() => expect(api.del).toHaveBeenCalledWith('/api/v1/teams/t1/members/u1'));
  expect(await screen.findByText('Direct membership removed. Group-derived membership remains.')).toBeTruthy();
});
it('surfaces server last-leader errors instead of claiming success', async () => {
  vi.mocked(api.del).mockRejectedValue(new Error('The last leader cannot leave'));
  mount();
  fireEvent.click(await screen.findByRole('button', { name: 'Leave team' }));
  fireEvent.click(screen.getByRole('button', { name: 'Remove direct membership' }));
  expect(await screen.findByText(/The last leader cannot leave/)).toBeTruthy();
  expect(screen.queryByText('Membership removed.')).toBeNull();
});
it('supports administrator group mappings by readable group names', async () => {
  session.role = 'admin';
  const original = vi.mocked(api.get).getMockImplementation()!;
  vi.mocked(api.get).mockImplementation((async (path: string) =>
    path === '/api/v1/admin/groups'
      ? { groups: [{ id: 'g1', name: 'Identity engineering' }] }
      : original(path)) as typeof api.get);
  mount('/admin/teams/t1?view=manage&section=settings');
  fireEvent.click(await screen.findByLabelText('Identity engineering'));
  fireEvent.click(screen.getByRole('button', { name: 'Save group mappings' }));
  await waitFor(() => expect(api.put).toHaveBeenCalledWith('/api/v1/teams/t1/groups', { group_ids: ['g1'] }));
});
it('cancels an existing pending request', async () => {
  role = '';
  const original = vi.mocked(api.get).getMockImplementation()!;
  vi.mocked(api.get).mockImplementation((async (path: string) =>
    path === '/api/v1/team-requests'
      ? { requests: [{ id: 'mine', team_id: 't1', team_name: 'Engineering', status: 'pending' }] }
      : original(path)) as typeof api.get);
  mount();
  fireEvent.click(await screen.findByRole('button', { name: 'Cancel request' }));
  await waitFor(() => expect(api.del).toHaveBeenCalledWith('/api/v1/teams/t1/join-requests'));
});
it('saves a renamed and unlisted team', async () => {
  mount('/admin/teams/t1?view=manage&section=settings');
  fireEvent.change(await screen.findByLabelText('Team name'), { target: { value: 'Platform' } });
  fireEvent.click(screen.getByLabelText('Listed in the directory'));
  fireEvent.click(screen.getByRole('button', { name: 'Save team' }));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('/api/v1/teams/t1', { name: 'Platform', listed: false }));
});
it('opens a directory selection at usage and closes back to browse', async () => {
  mount('/admin/teams?view=browse');
  fireEvent.click(await screen.findByRole('button', { name: 'Engineering' }));
  await screen.findByRole('heading', { name: 'Usage dashboard for t1' });
  fireEvent.click(screen.getByRole('button', { name: 'Close team' }));
  await screen.findByRole('region', { name: 'Team directory' });
  expect(screen.queryByRole('heading', { name: 'Usage dashboard for t1' })).toBeNull();
});
it('allows retry after directory loading fails', async () => {
  vi.mocked(api.get).mockRejectedValue(new Error('Directory unavailable'));
  mount('/admin/teams?team=t1');
  expect((await screen.findAllByText('Directory unavailable')).length).toBeGreaterThan(0);
  expect(screen.getAllByRole('button', { name: 'Try again' }).length).toBeGreaterThan(0);
});
it('opens a direct link, closes to directory and restores detail on Back', async () => {
  mount('/admin/teams/t1?q=eng&view=manage&section=settings');
  await screen.findByLabelText('Team name');
  fireEvent.click(screen.getByRole('button', { name: 'Close team' }));
  expect(screen.queryByLabelText('Team name')).toBeNull();
  expect(screen.getByTestId('location').textContent).toBe('/admin/teams?q=eng&view=browse');
  fireEvent.click(screen.getByRole('button', { name: 'Back' }));
  await screen.findByLabelText('Team name');
});
it.each(['member', 'moderator', 'leader', 'admin'])('exposes usage and quotas to %s', async (access) => {
  role = access === 'admin' ? '' : access;
  session.role = access === 'admin' ? 'admin' : 'user';
  mount();
  expect((await screen.findByRole('link', { name: 'Usage and quotas' })).getAttribute('href')).toBe(
    '/admin/teams/t1?view=manage&section=usage',
  );
});
it('hides usage and administration links from nonmembers', async () => {
  role = '';
  mount();
  await screen.findByRole('button', { name: 'Request to join' });
  expect(screen.queryByRole('link', { name: 'Usage and quotas' })).toBeNull();
  expect(screen.queryByRole('link', { name: 'Create and manage teams' })).toBeNull();
  expect(screen.queryByRole('link', { name: 'Import teams' })).toBeNull();
});
it('links administrators to existing creation and import tools', async () => {
  session.role = 'admin';
  mount('/admin/teams?view=browse');
  expect(await screen.findByRole('button', { name: 'Create team' })).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Import teams' }).getAttribute('href')).toBe('/admin/teams/import');
});
it('resets saved group mappings to normalized server values and syncs clean refreshes', async () => {
  session.role = 'admin';
  const original = vi.mocked(api.get).getMockImplementation()!;
  vi.mocked(api.get).mockImplementation((async (path: string) =>
    path === '/api/v1/admin/groups'
      ? { groups: [{ id: 'g1', name: 'Engineering identity' }] }
      : original(path)) as typeof api.get);
  const client = mount('/admin/teams/t1?view=manage&section=settings');
  const checkbox = (await screen.findByLabelText('Engineering identity')) as HTMLInputElement;
  fireEvent.click(checkbox);
  expect(checkbox.checked).toBe(true);
  fireEvent.click(screen.getByRole('button', { name: 'Save group mappings' }));
  await waitFor(() => expect(api.put).toHaveBeenCalledWith('/api/v1/teams/t1/groups', { group_ids: ['g1'] }));
  await waitFor(() => expect(checkbox.checked).toBe(false));
  await act(async () => {
    client.setQueryData(['teams', 'detail', 't1'], (old: any) => ({ ...old, group_ids: ['g1'] }));
  });
  await waitFor(() => expect(checkbox.checked).toBe(true));
});
it('preserves dirty group mappings on unrelated refresh and failed save', async () => {
  session.role = 'admin';
  const original = vi.mocked(api.get).getMockImplementation()!;
  vi.mocked(api.get).mockImplementation((async (path: string) =>
    path === '/api/v1/admin/groups'
      ? {
          groups: [
            { id: 'g1', name: 'Engineering identity' },
            { id: 'g2', name: 'Other identity' },
          ],
        }
      : original(path)) as typeof api.get);
  vi.mocked(api.put).mockRejectedValue(new Error('Mapping denied'));
  const client = mount('/admin/teams/t1?view=manage&section=settings');
  const checkbox = (await screen.findByLabelText('Engineering identity')) as HTMLInputElement;
  fireEvent.click(checkbox);
  await act(async () => {
    client.setQueryData(['teams', 'detail', 't1'], (old: any) => ({ ...old, group_ids: ['g2'] }));
  });
  expect(checkbox.checked).toBe(true);
  expect((screen.getByLabelText('Other identity') as HTMLInputElement).checked).toBe(false);
  fireEvent.click(screen.getByRole('button', { name: 'Save group mappings' }));
  await screen.findByText(/Mapping denied/);
  expect(checkbox.checked).toBe(true);
});
it('shows readable applicant identity without request identifiers', async () => {
  mount('/admin/teams/t1?view=manage&section=requests');
  await screen.findByText('Requesting Person');
  expect(screen.getByText('request@example.com')).toBeTruthy();
  expect(screen.getByLabelText('Decision reason for request@example.com')).toBeTruthy();
  expect(screen.queryByText(/\br1\b|\bu3\b/)).toBeNull();
});
it('shows the API action error reason as well as its message', async () => {
  vi.mocked(api.patch).mockRejectedValue(
    new ApiError(400, {
      message: 'Cannot update team',
      reason: 'Choose a different team name',
      code: 'invalid_request',
      type: 'invalid_request_error',
    }),
  );
  mount('/admin/teams/t1?view=manage&section=settings');
  fireEvent.click(await screen.findByRole('button', { name: 'Save team' }));
  await screen.findByText(/Cannot update team/);
  expect(screen.getByText(/Choose a different team name/)).toBeTruthy();
});
it('keeps query selections and directory Back navigation usable', async () => {
  mount('/admin/teams?q=eng&view=browse');
  fireEvent.click(await screen.findByRole('button', { name: 'Engineering' }));
  await screen.findByRole('heading', { name: 'Usage dashboard for t1' });
  expect(screen.getByTestId('location').textContent).toBe('/admin/teams?q=eng&team=t1');
  fireEvent.click(screen.getByRole('button', { name: 'Back' }));
  expect(screen.queryByRole('heading', { name: 'Usage dashboard for t1' })).toBeNull();
  expect(screen.getByTestId('location').textContent).toBe('/admin/teams?q=eng&view=browse');
});
it('prioritizes the route id over query selection and can select another team', async () => {
  const original = vi.mocked(api.get).getMockImplementation()!;
  vi.mocked(api.get).mockImplementation((async (path: string) => {
    if (path === '/api/v1/teams/t2') {
      const data = (await original('/api/v1/teams/t1')) as any;
      return { ...data, team: { ...data.team, id: 't2', name: 'Design' } };
    }
    if (path === '/api/v1/teams')
      return {
        teams: [
          { id: 't1', name: 'Engineering', listed: true, member_count: 2 },
          { id: 't2', name: 'Design', listed: true, member_count: 2 },
        ],
      };
    return original(path);
  }) as typeof api.get);
  mount('/admin/teams/t1?team=t2');
  await screen.findByRole('heading', { name: 'Usage dashboard for t1' });
  expect(api.get).not.toHaveBeenCalledWith('/api/v1/teams/t2');
  fireEvent.click(screen.getByRole('link', { name: 'Browse teams' }));
  fireEvent.click(await screen.findByRole('button', { name: 'Design' }));
  await screen.findByRole('heading', { name: 'Usage dashboard for t2' });
  expect(screen.getByTestId('location').textContent).toBe('/admin/teams?team=t2');
  fireEvent.click(screen.getByRole('button', { name: 'Close team' }));
  expect(screen.getByTestId('location').textContent).toBe('/admin/teams?view=browse');
});
it('opens the authorized directory request count directly into a focused management queue', async () => {
  const original = vi.mocked(api.get).getMockImplementation()!;
  vi.mocked(api.get).mockImplementation((async (path: string) => {
    const data: any = await original(path);
    if (data.teams) data.teams = data.teams.map((team: any) => ({ ...team, can_manage: true, pending_request_count: 1 }));
    return data;
  }) as typeof api.get);
  mount('/admin/teams?view=browse');
  const queue = await screen.findByRole('link', { name: '1 request waiting' });
  expect(queue.getAttribute('href')).toBe('/admin/teams/t1?view=manage&section=requests');
  fireEvent.click(queue);
  await screen.findByRole('button', { name: 'Approve' });
  expect(screen.queryByLabelText('Find people by email')).toBeNull();
  expect(screen.queryByLabelText('Team name')).toBeNull();
  expect(screen.getByRole('link', { name: 'Requests (1)' }).getAttribute('aria-current')).toBe('page');
  fireEvent.click(screen.getByRole('link', { name: 'Members' }));
  await screen.findByLabelText('Find people by email');
  expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull();
  fireEvent.click(screen.getByRole('link', { name: 'Settings' }));
  await screen.findByLabelText('Team name');
  expect(screen.queryByLabelText('Find people by email')).toBeNull();
});
it('honors explicit membership and management scope over legacy role on a forged manage URL', async () => {
  role = 'moderator';
  const original = vi.mocked(api.get).getMockImplementation()!;
  vi.mocked(api.get).mockImplementation((async (path: string) => {
    const data: any = await original(path);
    if (path === '/api/v1/teams/t1')
      return { ...data, membership_role: '', actor_role: 'user', can_manage: false, pending_request_count: 7 };
    return data;
  }) as typeof api.get);
  mount('/admin/teams/t1?view=manage&section=requests');
  await screen.findByRole('button', { name: 'Request to join' });
  expect(screen.queryByRole('navigation', { name: 'Team administration' })).toBeNull();
  expect(screen.queryByText(/requests waiting/)).toBeNull();
  expect(screen.queryByRole('button', { name: 'Remove member@example.com' })).toBeNull();
  expect(screen.queryByRole('heading', { name: 'Usage dashboard for t1' })).toBeNull();
});
it('keeps explicitly denied moderator management read-only', async () => {
  role = 'moderator';
  const original = vi.mocked(api.get).getMockImplementation()!;
  vi.mocked(api.get).mockImplementation((async (path: string) => {
    const data: any = await original(path);
    return path === '/api/v1/teams/t1' ? { ...data, can_manage: false } : data;
  }) as typeof api.get);
  mount('/admin/teams/t1?view=manage');
  await screen.findByRole('button', { name: 'Leave team' });
  expect(screen.queryByRole('button', { name: 'Remove member@example.com' })).toBeNull();
});
it('switches explicit administration team selections without changing working context', async () => {
  session.active_team_id = 't2';
  const original = vi.mocked(api.get).getMockImplementation()!;
  vi.mocked(api.get).mockImplementation((async (path: string) => {
    if (path === '/api/v1/teams')
      return {
        teams: [
          { id: 't1', name: 'Engineering', my_role: 'member', listed: true, member_count: 2 },
          { id: 't2', name: 'Design', my_role: 'member', listed: true, member_count: 2 },
        ],
      };
    const data: any = await original(path === '/api/v1/teams/t2' ? '/api/v1/teams/t1' : path);
    return path === '/api/v1/teams/t2' ? { ...data, team: { ...data.team, id: 't2', name: 'Design' } } : data;
  }) as typeof api.get);
  mount('/admin/teams?team=t2');
  await screen.findByRole('heading', { name: 'Usage dashboard for t2' });
  fireEvent.change(screen.getByLabelText('Viewing team'), { target: { value: 't1' } });
  await screen.findByRole('heading', { name: 'Usage dashboard for t1' });
  expect(api.post).not.toHaveBeenCalled();
  expect(api.patch).not.toHaveBeenCalled();
  expect(session.active_team_id).toBe('t2');
});
it('refreshes directory and selected request queues every 30 seconds', async () => {
  vi.useFakeTimers();
  try {
    mount('/admin/teams/t1?view=manage&section=requests');
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10);
    });
    expect(screen.getByRole('button', { name: 'Approve' })).toBeTruthy();
    vi.mocked(api.get).mockClear();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000);
    });
    expect(api.get).toHaveBeenCalledWith('/api/v1/teams');
    expect(api.get).toHaveBeenCalledWith('/api/v1/teams/t1');
    expect(vi.mocked(api.get).mock.calls.every(([path]) => !String(path).includes('/requests'))).toBe(true);
  } finally {
    cleanup();
    vi.useRealTimers();
  }
});
it('uses explicit membership scope for the usage landing instead of legacy actor role', async () => {
  const original = vi.mocked(api.get).getMockImplementation()!;
  vi.mocked(api.get).mockImplementation((async (path: string) => {
    const data: any = await original(path);
    return path === '/api/v1/teams/t1' ? { ...data, membership_role: '', actor_role: 'user', can_manage: false } : data;
  }) as typeof api.get);
  mount('/admin/teams/t1');
  await screen.findByRole('button', { name: 'Request to join' });
  expect(screen.queryByRole('heading', { name: 'Usage dashboard for t1' })).toBeNull();
});
it('shows only the requester status in discovery and keeps past requests secondary', async () => {
  role = '';
  const original = vi.mocked(api.get).getMockImplementation()!;
  vi.mocked(api.get).mockImplementation((async (path: string) => {
    if (path === '/api/v1/team-requests')
      return {
        requests: [
          { id: 'mine', team_id: 't1', team_name: 'Engineering', status: 'pending' },
          { id: 'past', team_id: 'old', team_name: 'Previous team', status: 'approved' },
        ],
      };
    const data: any = await original(path);
    if (data.teams) data.teams = data.teams.map((team: any) => ({ ...team, can_manage: false, pending_request_count: 9 }));
    return data;
  }) as typeof api.get);
  mount('/admin/teams?view=browse');
  await screen.findByText('Your request: pending');
  expect(screen.queryByText('9 requests waiting')).toBeNull();
  const disclosure = screen.getByText('My membership requests').closest('details');
  expect(disclosure).toBeTruthy();
  expect(disclosure?.open).toBe(false);
  fireEvent.click(screen.getByRole('button', { name: 'Engineering' }));
  await screen.findByRole('button', { name: 'Cancel request' });
});
it('preserves role-aware component leave semantics without widening authorization', async () => {
  role = 'member';
  mount('/admin/teams/t1');
  fireEvent.click(await screen.findByRole('link', { name: 'Members' }));
  expect(screen.queryByRole('navigation', { name: 'Team administration' })).toBeNull();
  fireEvent.click(await screen.findByRole('button', { name: 'Leave team' }));
  fireEvent.click(screen.getByRole('button', { name: 'Remove direct membership' }));
  await waitFor(() => expect(api.del).toHaveBeenCalledWith('/api/v1/teams/t1/members/me'));
});
it('offers admin creation and import on an explicit People team detail', async () => {
  session.role = 'admin';
  mount('/admin/teams?team=t1');
  await screen.findByRole('heading', { name: 'Usage dashboard for t1' });
  expect(screen.getByRole('button', { name: 'Create team' })).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Import teams' }).getAttribute('href')).toBe('/admin/teams/import');
});
it('retains team drafts and directory query state while visiting usage and returning', async () => {
  mount('/admin/teams/t1?view=manage&section=settings&q=keep');
  fireEvent.change(await screen.findByLabelText('Team name'), { target: { value: 'Unsaved platform' } });
  fireEvent.click(screen.getByRole('link', { name: 'Usage and quotas' }));
  expect(await screen.findByRole('heading', { name: 'Usage dashboard for t1' })).toBeTruthy();
  expect(screen.getByTestId('location').textContent).toContain('q=keep');
  fireEvent.click(screen.getByRole('button', { name: 'Back' }));
  expect(((await screen.findByLabelText('Team name')) as HTMLInputElement).value).toBe('Unsaved platform');
  expect(api.patch).not.toHaveBeenCalled();
});
it('creates through the existing admin contract and opens the returned team', async () => {
  session.role = 'admin';
  vi.mocked(api.post).mockResolvedValue({ team: { id: 't1' } });
  mount('/admin/teams?view=browse');
  fireEvent.click(screen.getByRole('button', { name: 'Create team' }));
  fireEvent.change(screen.getByLabelText('Team name'), { target: { value: ' Platform ' } });
  fireEvent.submit(screen.getByLabelText('Team name').closest('form')!);
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/admin/teams', { name: 'Platform', lead_user_id: '' }));
  await waitFor(() => expect(screen.getByTestId('location').textContent).toBe('/admin/teams/t1?view=manage&section=members'));
});
it('preserves admin quota delegation and confirms team deletion', async () => {
  session.role = 'admin';
  mount('/admin/teams/t1?view=manage&section=settings');
  fireEvent.click(await screen.findByLabelText('Team leads may edit quotas'));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('/api/v1/admin/teams/t1', { lead_can_edit_quotas: true }));
  fireEvent.click(screen.getByRole('button', { name: 'Delete team' }));
  expect(api.del).not.toHaveBeenCalled();
  const buttons = screen.getAllByRole('button', { name: 'Delete team' });
  fireEvent.click(buttons[buttons.length - 1]!);
  await waitFor(() => expect(api.del).toHaveBeenCalledWith('/api/v1/admin/teams/t1'));
  await waitFor(() => expect(screen.getByTestId('location').textContent).toBe('/admin/teams?view=browse'));
});
function RouterState() {
  const location = useLocation();
  const navigate = useNavigate();
  return (
    <>
      <output data-testid="location">{location.pathname + location.search}</output>
      <button onClick={() => navigate(-1)}>Back</button>
    </>
  );
}
function mount(url = '/admin/teams?team=t1&view=manage') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[url]}>
        <RouterState />
        <Routes>
          <Route path="/admin/teams" element={<Teams />} />
          <Route path="/admin/teams/:id" element={<Teams />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return client;
}
it('lets leaders select users by email and add a moderator without entering identifiers', async () => {
  mount();
  fireEvent.change(await screen.findByLabelText('Find people by email'), { target: { value: 'new@' } });
  fireEvent.click(await screen.findByRole('button', { name: 'Select new@example.com' }));
  fireEvent.change(screen.getByLabelText('New member role'), { target: { value: 'moderator' } });
  fireEvent.click(screen.getByRole('button', { name: 'Add member' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/teams/t1/members', { user_id: 'u2', role: 'moderator' }));
});
