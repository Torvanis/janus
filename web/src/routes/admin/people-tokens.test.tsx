/**
 * Tokens-out column tests: the admin users table must surface each user's
 * 30-day output-token total (tokens_out_30d from GET /api/v1/admin/users) as
 * its own column between "30-day spend" and "Status", formatted with
 * thousands separators and right-aligned like the other numeric columns, so
 * admins can gauge usage volume without opening every detail drawer.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import { PeoplePage } from './People';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

const adaUser = {
  id: 'user-1',
  email: 'ada@example.com',
  name: 'Ada Lovelace',
  role: 'user',
  is_active: true,
  groups: ['ml-platform'],
  last_login_at: '2025-01-02T10:00:00Z',
  spend_30d_usd: 1.25,
  requests_30d: 42,
  tokens_out_30d: 1234567,
};

function renderUsers() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={['/admin/users']}>
          <Routes>
            <Route path="/admin/users" element={<PeoplePage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocked.get.mockImplementation((path: string) => {
    if (path.startsWith('/api/v1/admin/users')) {
      return Promise.resolve({ users: [adaUser], total_count: 1 });
    }
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
});

describe('admin users tokens-out column', () => {
  it('renders the column header between spend and status', async () => {
    renderUsers();

    await screen.findByRole('button', { name: /Ada Lovelace/ });
    const headers = screen.getAllByRole('columnheader').map((th) => th.textContent);
    const spend = headers.indexOf('30-day spend');
    const tokens = headers.indexOf('Tokens out (30d)');
    const status = headers.indexOf('Status');
    expect(spend).toBeGreaterThanOrEqual(0);
    expect(tokens).toBe(spend + 1);
    expect(status).toBe(tokens + 1);
  });

  it('formats the 30-day output-token total as a grouped number in a numeric cell', async () => {
    renderUsers();

    await screen.findByRole('button', { name: /Ada Lovelace/ });
    // Match the runtime's default-locale grouping (en-US CI renders '1,234,567').
    const cell = screen.getByText(new Intl.NumberFormat().format(1234567));
    expect(cell.tagName).toBe('TD');
    expect(cell.className).toContain('num');
  });
});
