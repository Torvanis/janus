import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Shell } from './Shell';
import { SessionContext } from './session';
vi.mock('./ActiveTeamSelector', () => ({ ActiveTeamSelector: () => null }));
vi.mock('./LicenseBanner', () => ({ LicenseBanner: () => null }));
vi.mock('../components/AmbientCanvas', () => ({ AmbientCanvas: () => null }));
vi.mock('../lib/api', () => ({ api: { get: vi.fn(async (url: string) => (url === '/api/v1/teams' ? { teams: [] } : {})) } }));
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});
it.each(['/admin/teams', '/admin/teams/t1', '/admin/teams/import'])(
  'marks Administration People current for %s, not personal Team',
  (path) => {
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => ({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() })),
    );
    render(
      <QueryClientProvider client={new QueryClient()}>
        <SessionContext.Provider
          value={
            {
              me: { id: 'me', name: 'Admin', email: 'admin@example.com', role: 'admin', groups: [], teams: [] },
              config: null,
              refresh: vi.fn(),
              loading: false,
              error: null,
            } as any
          }
        >
          <MemoryRouter initialEntries={[path]}>
            <Shell>
              <p>Workspace</p>
            </Shell>
          </MemoryRouter>
        </SessionContext.Provider>
      </QueryClientProvider>,
    );
    const people = screen.getAllByRole('link', { name: 'People' });
    for (const link of people) expect(link.getAttribute('aria-current')).toBe('page');
    for (const link of screen.getAllByRole('link', { name: 'Team' })) expect(link.getAttribute('aria-current')).toBeNull();
  },
);
