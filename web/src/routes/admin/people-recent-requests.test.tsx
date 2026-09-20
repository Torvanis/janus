/**
 * Recent-requests table in the admin user detail drawer: tokens in and
 * tokens out are separate sortable columns, and per-direction token-size
 * filters narrow the server fetch with the same tokens_in_gt / tokens_out_lt
 * vocabulary the request log speaks.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
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
  groups: [],
  last_login_at: '2025-01-02T10:00:00Z',
  spend_30d_usd: 1.25,
  requests_30d: 2,
  tokens_out_30d: 900,
};

const detail = {
  user: adaUser,
  groups: [],
  teams: [],
  effective_grants: [],
  tokens: [],
  quotas: [],
  recent_requests: [
    {
      id: 'evt-1',
      team_ids: 'former-team',
      team_names: ['Historical Research'],
      model: 'gpt-large',
      http_status: 200,
      created_at: '2025-01-02T10:00:00Z',
      cost_nanousd: 1000,
      latency_ms: 20,
      tokens_in: 12_345,
      tokens_out: 678,
    },
  ],
  audit: [],
  usage_range: 'month',
  usage_series: [],
  usage_totals: {
    tokens_in: 0,
    tokens_out: 0,
    tokens_cached: 0,
    tokens_cache_write_5m: 0,
    tokens_cache_write_1h: 0,
    cost_nanousd: 0,
    request_count: 0,
    error_count: 0,
  },
};

function renderDrawer() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={['/admin/users/user-1']}>
          <Routes>
            <Route path="/admin/users" element={<PeoplePage />} />
            <Route path="/admin/users/:id" element={<PeoplePage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

/** Every GET the drawer issued against the user detail endpoint, oldest first. */
function detailCalls(): URLSearchParams[] {
  return mocked.get.mock.calls
    .map((call) => call[0] as string)
    .filter((path) => path.startsWith('/api/v1/admin/users/user-1'))
    .map((path) => new URL(`http://test${path}`).searchParams);
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocked.get.mockImplementation((path: string) => {
    if (path.startsWith('/api/v1/admin/users/user-1')) return Promise.resolve(detail);
    if (path.startsWith('/api/v1/admin/users')) return Promise.resolve({ users: [adaUser], total_count: 1 });
    if (path.startsWith('/api/v1/admin/groups')) return Promise.resolve({ groups: [] });
    if (path.startsWith('/api/v1/admin/teams')) return Promise.resolve({ teams: [] });
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
});

describe('user drawer recent requests — split token columns', () => {
  it('shows recorded team attribution even when the user has no current teams', async () => {
    renderDrawer();
    const table = await screen.findByRole('table', { name: 'Recent requests' });
    expect(detail.teams).toEqual([]);
    expect(within(table).getByRole('columnheader', { name: 'Charged to' })).toBeTruthy();
    expect(within(table).getByRole('cell', { name: 'Historical Research' })).toBeTruthy();
  });

  it('renders tokens in and tokens out as separate columns with their counts', async () => {
    renderDrawer();
    const table = await screen.findByRole('table', { name: 'Recent requests' });
    const headers = Array.from(table.querySelectorAll('th')).map((th) => th.textContent ?? '');
    expect(headers.some((h) => h.includes('Tokens in'))).toBe(true);
    expect(headers.some((h) => h.includes('Tokens out'))).toBe(true);
    expect(screen.getByRole('cell', { name: '12,345' })).toBeTruthy();
    expect(screen.getByRole('cell', { name: '678' })).toBeTruthy();
  });

  it('sorting by a token column and filtering by size re-fetch the detail with request-log params', async () => {
    const user = userEvent.setup();
    renderDrawer();
    const table = await screen.findByRole('table', { name: 'Recent requests' });

    await user.click(screen.getByRole('button', { name: /Tokens out/ }));
    await vi.waitFor(() => {
      expect(detailCalls().at(-1)?.get('requests_sort')).toBe('tokens_out');
    });

    // The drawer's filters are scoped to the section; select them by label
    // within the section that owns the table.
    const section = table.closest('section');
    if (!section) throw new Error('recent requests table is not inside its section');
    const selects = Array.from(section.querySelectorAll('select'));
    expect(selects).toHaveLength(2);
    await user.selectOptions(selects[0]!, 'gt:100000');
    await vi.waitFor(() => {
      const last = detailCalls().at(-1);
      expect(last?.get('tokens_in_gt')).toBe('100000');
      expect(last?.get('requests_sort')).toBe('tokens_out');
    });
    await user.selectOptions(selects[1]!, 'lt:1000');
    await vi.waitFor(() => {
      const last = detailCalls().at(-1);
      expect(last?.get('tokens_in_gt')).toBe('100000');
      expect(last?.get('tokens_out_lt')).toBe('1000');
    });

    // Clearing drops both thresholds while leaving the sort alone.
    await user.click(screen.getByRole('button', { name: 'Clear filters' }));
    await vi.waitFor(() => {
      const last = detailCalls().at(-1);
      expect(last?.has('tokens_in_gt')).toBe(false);
      expect(last?.has('tokens_out_lt')).toBe(false);
      expect(last?.get('requests_sort')).toBe('tokens_out');
    });
  });
});
