import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ToastProvider } from '../../components/ui';
import AdminRoutes from './AdminRoutes';
import { api } from '../../lib/api';

vi.mock('../../lib/api', async (original) => ({
  ...(await original<typeof import('../../lib/api')>()),
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn(), patch: vi.fn() },
}));
const mocked = vi.mocked(api);

afterEach(cleanup);

function at(path: string) {
  mocked.get.mockImplementation((p: string) => {
    if (p === '/api/v1/admin/rules/blocking') return Promise.resolve({ rules: [], clause_types: [], caveat: '' });
    if (p === '/api/v1/admin/header-policies')
      return Promise.resolve({ policies: [], targets: [], settings: { mode: 'off' }, edge_headers: [] });
    return Promise.resolve({});
  });
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ToastProvider>
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route path="/admin/*" element={<AdminRoutes />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

it('shows Header policies as a tab of Policy rules', async () => {
  at('/admin/rules/headers');
  expect(await screen.findByRole('heading', { level: 1, name: 'Policy rules' })).toBeTruthy();
  const tabs = screen.getByRole('navigation', { name: 'Policy rules' });
  expect(within(tabs).getByRole('link', { name: 'Header policies' }).getAttribute('aria-current')).toBe('page');
  expect(within(tabs).getByRole('link', { name: 'Blocking rules' }).getAttribute('href')).toBe('/admin/rules');
  expect(await screen.findByRole('button', { name: 'New policy' })).toBeTruthy();
});

it('opens on Blocking rules, and the old header-policies address lands on the tab', async () => {
  at('/admin/rules');
  const tabs = await screen.findByRole('navigation', { name: 'Policy rules' });
  expect(within(tabs).getByRole('link', { name: 'Blocking rules' }).getAttribute('aria-current')).toBe('page');
  cleanup();
  at('/admin/header-policies');
  const moved = await screen.findByRole('navigation', { name: 'Policy rules' });
  expect(within(moved).getByRole('link', { name: 'Header policies' }).getAttribute('aria-current')).toBe('page');
});
