/**
 * A deleted person's content is held for 30 days: People shows “Deleted —
 * held until <date>”, the row action restores them (PATCH is_active), and
 * the detail drawer can delete the content at once.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
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

const held = {
  id: 'user-9',
  email: 'leaver@example.com',
  name: 'Leaver',
  role: 'user',
  is_active: false,
  created_at: '2024-12-01T10:00:00Z',
  last_login_at: '2025-01-02T10:00:00Z',
  groups: [],
  spend_30d_usd: 0,
  requests_30d: 0,
  tokens_out_30d: 0,
  held_until: '2026-11-28T10:00:00Z',
};

function renderAt(entry: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[entry]}>
          <Routes>
            <Route path="/admin/users" element={<PeoplePage />} />
            <Route path="/admin/users/:id" element={<PeoplePage />} />
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
    if (path.startsWith('/api/v1/admin/groups')) return Promise.resolve({ groups: [] });
    if (path.startsWith('/api/v1/admin/teams')) return Promise.resolve({ teams: [] });
    if (path.startsWith('/api/v1/admin/users/user-9')) {
      return Promise.resolve({
        user: held,
        groups: [],
        teams: [],
        effective_grants: [],
        usage_series: [],
        recent_requests: [],
        tokens: [],
        quotas: [],
        usage_range: 'month',
      });
    }
    if (path.startsWith('/api/v1/admin/users')) return Promise.resolve({ users: [held], total_count: 1 });
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
  mocked.patch.mockResolvedValue({});
  mocked.post.mockResolvedValue({});
});

describe('deleted person held for 30 days', () => {
  it('shows the hold and restores through the row action', async () => {
    const user = userEvent.setup();
    renderAt('/admin/users');
    expect(await screen.findByText(/Deleted — held until/)).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Restore' }));
    expect(await screen.findByText('Restore Leaver?')).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Apply change' }));
    await waitFor(() => expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/users/user-9', { is_active: true }));
  });

  it('deletes the held content now from the drawer', async () => {
    const user = userEvent.setup();
    renderAt('/admin/users/user-9');
    await user.click(await screen.findByRole('button', { name: 'Delete content now' }));
    const confirm = await screen.findAllByRole('button', { name: 'Delete content now' });
    await user.click(confirm.at(-1)!);
    await waitFor(() => expect(mocked.post).toHaveBeenCalledWith('/api/v1/admin/users/user-9/purge', {}));
  });
});
