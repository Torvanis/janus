import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../lib/api';
import { TeamQuotaManagement } from './Team';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

const quota = {
  id: 'q1',
  metric: 'requests',
  metric_label: 'Requests',
  window: 'daily',
  window_label: 'per day',
  limit_value: 100,
  unit: 'count',
  percent: 10,
  reset_at: '2026-10-28T00:00:00Z',
  breach_behavior: 'let_finish',
};

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocked.get.mockResolvedValue({
    teams: [{ id: 't1', name: 'Data Science', can_edit_quotas: true, quotas: [quota], member_count: 2 }],
  } as never);
  mocked.put.mockResolvedValue({ id: 'q1', updated: true } as never);
});

describe('team lead quota editing', () => {
  it('changes a quota’s limit and breach behaviour in place', async () => {
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <MemoryRouter>
          <TeamQuotaManagement teamId="t1" />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Edit' }));
    const input = screen.getByRole('spinbutton', { name: 'New limit' });
    await user.clear(input);
    await user.type(input, '250');
    await user.click(screen.getByRole('checkbox', { name: /cut/i }));
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() =>
      expect(mocked.put).toHaveBeenCalledWith('/api/v1/lead/teams/t1/quotas/q1', {
        limit_value: 250,
        breach_behavior: 'hard_kill',
      }),
    );
  });
});
