import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import AdminRoutes from './AdminRoutes';
import { Breadcrumb } from '../../app/Breadcrumb';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';

const session = vi.hoisted(() => ({ role: 'admin' }));
vi.mock('../../app/session', () => ({
  useSession: () => ({ me: { id: 'me', role: session.role }, refresh: vi.fn() }),
  useMe: () => ({ id: 'me', role: session.role }),
  useLocalOnly: () => false,
  useLicensed: () => true,
  useDefaultMetric: () => 'tokens',
}));
vi.mock('../../lib/api', async (original) => ({
  ...(await original<typeof import('../../lib/api')>()),
  api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
}));
vi.mock('../Team', () => ({ TeamPage: () => <h2>Usage dashboard</h2>, TeamQuotaManagement: () => null }));
const teams = [
  { id: 'mine', name: 'My team', my_role: 'member', member_count: 1, listed: true, can_manage: true },
  { id: 'other', name: 'Global nonmember team', member_count: 1, listed: true, can_manage: true },
];
beforeEach(() => {
  session.role = 'admin';
  vi.mocked(api.get).mockImplementation(async (url) => {
    if (url === '/api/v1/teams' || url === '/api/v1/admin/teams') return { teams } as any;
    if (url === '/api/v1/team-requests') return { requests: [] } as any;
    if (url === '/api/v1/admin/groups') return { groups: [] } as any;
    if (url === '/api/v1/admin/admin-groups') return { admin_groups: [], env_admin_groups: [] } as any;
    if (url.startsWith('/api/v1/admin/users')) return { users: [], has_more: false } as any;
    if (url.startsWith('/api/v1/teams/'))
      return {
        team: teams.find((team) => url.endsWith(team.id)),
        membership_role: '',
        my_role: 'admin',
        actor_role: 'admin',
        can_manage: true,
        members: [],
        requests: [],
        group_ids: [],
      } as any;
    throw new Error(`Unexpected GET ${url}`);
  });
});
afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});
function Shell() {
  const location = useLocation();
  return (
    <>
      <Breadcrumb path={location.pathname} />
      <output data-testid="url">{location.pathname + location.search + location.hash}</output>
      <Routes>
        <Route path="/admin/*" element={<AdminRoutes />} />
      </Routes>
    </>
  );
}
function mount(path: string) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ToastProvider>
        <MemoryRouter initialEntries={[path]}>
          <Shell />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}
it('keeps Users, Groups, and the complete team directory together in People', async () => {
  mount('/admin/users');
  const people = screen.getByRole('navigation', { name: 'People sections' });
  expect(within(people).getByRole('link', { name: 'Users' }).getAttribute('aria-current')).toBe('page');
  fireEvent.click(within(people).getByRole('link', { name: 'Groups' }));
  await screen.findByRole('button', { name: 'New in-app group' });
  fireEvent.click(within(people).getByRole('link', { name: 'Teams' }));
  await screen.findByRole('button', { name: 'Global nonmember team' });
  expect(screen.getByRole('button', { name: 'My team' })).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Create team' })).toBeTruthy();
  expect(screen.getByTestId('url').textContent).toBe('/admin/teams');
  expect(screen.getByRole('navigation', { name: 'Breadcrumb' }).textContent).toContain('Administration / People / Teams');
  fireEvent.click(screen.getByRole('link', { name: 'Import teams' }));
  await screen.findByRole('heading', { name: 'Import teams from CSV' });
  expect(screen.getByTestId('url').textContent).toBe('/admin/teams/import');
  expect(screen.getByRole('link', { name: 'Download CSV template' })).toBeTruthy();
  expect(screen.getByRole('navigation', { name: 'Breadcrumb' }).textContent).toContain('People / Teams / Import teams');
  fireEvent.click(screen.getByRole('link', { name: 'Team directory' }));
  await screen.findByRole('button', { name: 'Global nonmember team' });
});
it('resolves admin team detail names under People', async () => {
  mount('/admin/teams/other?view=manage&section=members');
  await screen.findByRole('heading', { name: 'Global nonmember team' });
  expect(screen.getByRole('navigation', { name: 'Breadcrumb' }).textContent).toContain('People / Teams / Global nonmember team');
  expect(screen.getByRole('button', { name: 'Add member' })).toBeTruthy();
});
it.each(['/admin/team-import', '/admin/teams/import'])(
  'keeps import under People for %s without resolving import as an ID',
  async (path) => {
    mount(`${path}?opaque=keep#preview`);
    await screen.findByRole('heading', { name: 'Import teams from CSV' });
    expect(screen.getByTestId('url').textContent).toBe('/admin/teams/import?opaque=keep#preview');
    expect(api.get).not.toHaveBeenCalledWith('/api/v1/teams/import');
  },
);
it('does not resolve nonmember names in the personal breadcrumb even for admins', async () => {
  mount('/teams/other');
  expect(screen.getByRole('navigation', { name: 'Breadcrumb' }).textContent).not.toContain('Global nonmember team');
  expect(api.get).not.toHaveBeenCalledWith('/api/v1/teams/other');
});
