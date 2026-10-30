import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import { AdminQuotasPage } from './Quotas';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocked.get.mockImplementation((path: string) => {
    if (path.startsWith('/api/v1/admin/quotas')) {
      return Promise.resolve({ quotas: [], metrics: [], windows: [] });
    }
    if (path.startsWith('/api/v1/admin/users')) return Promise.resolve({ users: [] });
    if (path.startsWith('/api/v1/admin/service-tokens')) {
      return Promise.resolve({ service_tokens: [{ id: 'st-1', name: 'looping-agent' }] });
    }
    if (path.startsWith('/api/v1/admin/rate-limits')) {
      return Promise.resolve({
        rules: [
          {
            id: 'r-1',
            subject_type: 'service_token',
            subject_id: 'st-9',
            subject_name: 'nightly-bot',
            endpoint: '*',
            requests_per_minute: 5,
            created_at: '',
          },
        ],
        enforcement_enabled: true,
      });
    }
    return Promise.resolve({});
  });
  mocked.post.mockResolvedValue({});
});

describe('Rate limits — service tokens', () => {
  it('lists a service-token rule and creates one for a chosen service token', async () => {
    const user = userEvent.setup();
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <ToastProvider>
          <MemoryRouter initialEntries={['/admin/quotas']}>
            <AdminQuotasPage />
          </MemoryRouter>
        </ToastProvider>
      </QueryClientProvider>,
    );
    expect(await screen.findByText('nightly-bot')).toBeTruthy();

    await user.click(await screen.findByRole('button', { name: 'New rate limit' }));
    const dialog = await screen.findByRole('dialog');
    const create = within(dialog).getByRole('button', { name: 'Create rate limit' });
    await user.selectOptions(within(dialog).getAllByRole('combobox')[0] as HTMLElement, 'service_token');
    // A service token must be chosen; there is no "every" option for it.
    expect((create as HTMLButtonElement).disabled).toBe(true);
    await within(dialog).findByRole('option', { name: 'looping-agent' });
    await user.selectOptions(within(dialog).getAllByRole('combobox')[1] as HTMLElement, 'st-1');
    await user.click(create);
    expect(mocked.post).toHaveBeenCalledWith(
      '/api/v1/admin/rate-limits',
      expect.objectContaining({ subject_type: 'service_token', subject_id: 'st-1' }),
    );
  });
});
