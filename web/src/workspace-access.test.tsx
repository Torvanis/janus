import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { App } from './App';
import type { ReactNode } from 'react';
import { api } from './lib/api';

const session = vi.hoisted(() => ({ role: 'user' }));
vi.mock('./app/session', () => ({
  SessionProvider: ({ children }: { children: ReactNode }) => children,
  useSession: () => ({ me: { role: session.role, groups: [], email: 'viewer@example.com' }, loading: false }),
}));
vi.mock('./app/Shell', () => ({ Shell: ({ children }: { children: ReactNode }) => children }));
vi.mock('./lib/api', async (original) => ({
  ...(await original<typeof import('./lib/api')>()),
  api: { get: vi.fn(), post: vi.fn() },
}));
vi.mock('./routes/admin/TeamImportPage', () => ({ default: () => <h1>Authorized import workspace</h1> }));
afterEach(cleanup);
beforeEach(() => {
  session.role = 'user';
  vi.clearAllMocks();
});
function mount(path: string) {
  render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter initialEntries={[path]}>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}
it.each([
  '/admin/settings/general',
  '/admin/settings/sign-in',
  '/admin/settings/license',
  '/admin/settings/troubleshooting',
  '/admin/settings/status',
  '/admin/system',
  '/admin/provisioning',
  '/admin/team-import',
  '/teams/import',
  '/admin/teams',
  '/admin/teams/t1',
  '/admin/teams/import',
])('blocks nonadmin direct entry %s before fetching admin data', (path) => {
  mount(path);
  expect(screen.queryByRole('heading', { name: 'Authorized import workspace' })).toBeNull();
  expect(screen.getByText(/administrator/i)).toBeTruthy();
  expect(api.get).not.toHaveBeenCalled();
  expect(api.post).not.toHaveBeenCalled();
});
it('routes an administrator to static import, not a team called import', async () => {
  session.role = 'admin';
  mount('/teams/import');
  expect(await screen.findByRole('heading', { name: 'Authorized import workspace' })).toBeTruthy();
  expect(api.get).not.toHaveBeenCalled();
});
