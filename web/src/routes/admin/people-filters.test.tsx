/**
 * Sort/filter controls on the admin users list. The API always
 * supported search / role / group_id / team_id / active / sort query
 * parameters, but the People page exposed no controls for group, team, or
 * sort. These tests lock the contract that the controls (a) drive the API
 * query string, (b) live in the URL so the view is a shareable deep link that
 * the back button restores, and (c) reset the page offset when they change.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactNode } from 'react';
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
  created_at: '2024-12-01T10:00:00Z',
  last_login_at: '2025-01-02T10:00:00Z',
  groups: ['ml-platform'],
  spend_30d_usd: 1.25,
  requests_30d: 10,
};
const graceUser = {
  ...adaUser,
  id: 'user-2',
  email: 'grace@example.com',
  name: 'Grace Hopper',
  groups: [],
};

const groups = [{ id: 'grp-1', name: 'ml-platform', from_idp: false, member_count: 1 }];
const teams = [
  {
    id: 'team-1',
    name: 'Platform',
    lead_user_id: 'user-2',
    lead_name: 'Grace Hopper',
    lead_can_edit_quotas: false,
    member_count: 1,
  },
];

/** Exposes the router's current query string so tests can assert deep links. */
function LocationProbe(): ReactNode {
  const location = useLocation();
  return <div data-testid="location-search">{location.search}</div>;
}

function renderAt(initialEntry: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[initialEntry]}>
          <LocationProbe />
          <Routes>
            <Route path="/admin/users" element={<PeoplePage />} />
            <Route path="/admin/users/:id" element={<PeoplePage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

/** Every GET the page issued against the users list endpoint, oldest first. */
function usersCalls(): URLSearchParams[] {
  return mocked.get.mock.calls
    .map((call) => call[0] as string)
    .filter((path) => path === '/api/v1/admin/users' || path.startsWith('/api/v1/admin/users?'))
    .map((path) => new URL(`http://test${path}`).searchParams);
}

function urlSearch(): string {
  return screen.getByTestId('location-search').textContent ?? '';
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocked.get.mockImplementation((path: string) => {
    if (path.startsWith('/api/v1/admin/groups')) return Promise.resolve({ groups });
    if (path.startsWith('/api/v1/admin/teams')) return Promise.resolve({ teams });
    if (path.startsWith('/api/v1/admin/users?') || path === '/api/v1/admin/users') {
      const params = new URL(`http://test${path}`).searchParams;
      let rows = [adaUser, graceUser];
      if (params.get('group_id')) rows = rows.filter((row) => row.groups.includes('ml-platform'));
      if (params.get('team_id')) rows = rows.filter((row) => row.id === 'user-2');
      const search = (params.get('search') ?? '').toLowerCase();
      if (search) rows = rows.filter((row) => `${row.name} ${row.email}`.toLowerCase().includes(search));
      return Promise.resolve({ users: rows, total_count: rows.length });
    }
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
});

describe('documented users list filter controls', () => {
  it('group filter narrows the fetch, filters rows, and lands in the URL', async () => {
    const user = userEvent.setup();
    renderAt('/admin/users');
    expect(await screen.findByText('grace@example.com')).toBeTruthy();

    await user.selectOptions(await screen.findByRole('combobox', { name: 'Filter by group' }), 'grp-1');

    await waitFor(() => expect(usersCalls().at(-1)?.get('group_id')).toBe('grp-1'));
    await waitFor(() => expect(screen.queryByText('grace@example.com')).toBeNull());
    expect(screen.getByText('ada@example.com')).toBeTruthy();
    expect(urlSearch()).toContain('group_id=grp-1');
  });

  it('team filter drives team_id and composes with the group filter (AND)', async () => {
    const user = userEvent.setup();
    renderAt('/admin/users?group_id=grp-1');

    await user.selectOptions(await screen.findByRole('combobox', { name: 'Filter by team' }), 'team-1');

    await waitFor(() => {
      const last = usersCalls().at(-1);
      expect(last?.get('team_id')).toBe('team-1');
      expect(last?.get('group_id')).toBe('grp-1');
    });
    expect(urlSearch()).toContain('team_id=team-1');
    expect(urlSearch()).toContain('group_id=grp-1');
  });

  it('search box drives the search param and clearing it restores the full list', async () => {
    const user = userEvent.setup();
    renderAt('/admin/users');
    expect(await screen.findByText('grace@example.com')).toBeTruthy();

    await user.type(screen.getByRole('searchbox', { name: 'Search people' }), 'grace');
    await waitFor(() => expect(usersCalls().at(-1)?.get('search')).toBe('grace'));
    await waitFor(() => expect(screen.queryByText('ada@example.com')).toBeNull());

    await user.click(screen.getByRole('button', { name: 'Clear search' }));
    await waitFor(() => expect(usersCalls().at(-1)?.get('search')).toBeNull());
    expect(await screen.findByText('ada@example.com')).toBeTruthy();
  });

  it('sort dropdown and direction toggle produce <field>_<dir> in URL, API, and header indicator', async () => {
    const user = userEvent.setup();
    renderAt('/admin/users');
    await screen.findByText('ada@example.com');

    await user.selectOptions(screen.getByRole('combobox', { name: 'Sort people by' }), 'name');
    await waitFor(() => expect(usersCalls().at(-1)?.get('sort')).toBe('name_asc'));
    expect(urlSearch()).toContain('sort=name_asc');
    // The table re-renders once the sorted fetch resolves.
    const nameHeader = await screen.findByRole('columnheader', { name: 'Name' });
    expect(nameHeader.getAttribute('aria-sort')).toBe('ascending');
    expect(nameHeader.textContent).toContain('↑');

    await user.click(screen.getByRole('button', { name: 'Toggle sort direction' }));
    await waitFor(() => expect(usersCalls().at(-1)?.get('sort')).toBe('name_desc'));
    expect(urlSearch()).toContain('sort=name_desc');
    const descHeader = await screen.findByRole('columnheader', { name: 'Name' });
    expect(descHeader.getAttribute('aria-sort')).toBe('descending');
    expect(descHeader.textContent).toContain('↓');
  });

  it('clicking a column header sorts by it and toggles direction when already active', async () => {
    const user = userEvent.setup();
    renderAt('/admin/users');
    await screen.findByText('ada@example.com');

    // The backend default is email ascending, so the Email column starts
    // marked ascending and a click flips it to descending.
    expect((await screen.findByRole('columnheader', { name: 'Email' })).getAttribute('aria-sort')).toBe('ascending');
    await user.click(screen.getByRole('button', { name: 'Email' }));
    await waitFor(() => expect(usersCalls().at(-1)?.get('sort')).toBe('email_desc'));
    expect(urlSearch()).toContain('sort=email_desc');
    expect((await screen.findByRole('columnheader', { name: 'Email' })).getAttribute('aria-sort')).toBe('descending');

    // An inactive column starts ascending.
    await user.click(await screen.findByRole('button', { name: 'Last sign-in' }));
    await waitFor(() => expect(usersCalls().at(-1)?.get('sort')).toBe('last_login_asc'));
    expect((await screen.findByRole('columnheader', { name: 'Last sign-in' })).getAttribute('aria-sort')).toBe('ascending');
  });

  it('changing a filter resets the page offset but keeps the sort', async () => {
    const user = userEvent.setup();
    renderAt('/admin/users?page=50&sort=name_desc');

    await waitFor(() => expect(usersCalls().at(-1)?.get('offset')).toBe('50'));
    await user.selectOptions(await screen.findByRole('combobox', { name: 'Filter by group' }), 'grp-1');

    await waitFor(() => {
      const last = usersCalls().at(-1);
      expect(last?.get('group_id')).toBe('grp-1');
      expect(last?.get('offset')).toBe('0');
      expect(last?.get('sort')).toBe('name_desc');
    });
    expect(urlSearch()).not.toContain('page=');
    expect(urlSearch()).toContain('sort=name_desc');
  });
});
