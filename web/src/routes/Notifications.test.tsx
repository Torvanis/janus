import { afterEach, it, expect, vi } from 'vitest';
import { render, screen, cleanup, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router-dom';
import { ToastProvider } from '../components/ui';
import { NotificationsPage } from './Notifications';
const mock = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }));
vi.mock('../lib/api', () => ({ api: mock }));
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});
it('follows server continuation and resets the server offset on unread filtering', async () => {
  mock.get.mockImplementation(async (path: string) => {
    const offset = new URL(path, 'http://test').searchParams.get('offset');
    return {
      notifications: [
        {
          id: offset,
          title: offset === '100' ? 'Older notification' : 'Newest notification',
          body: 'body',
          severity: 'info',
          created_at: '2026-09-01T00:00:00Z',
          read_at: '',
        },
      ],
      unread_count: 101,
      has_more: offset === '0',
    };
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ToastProvider>
        <MemoryRouter>
          <NotificationsPage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
  await screen.findByText('Newest notification');
  await userEvent.click(screen.getByRole('button', { name: 'Next' }));
  await screen.findByText('Older notification');
  expect((screen.getByRole('button', { name: 'Next' }) as HTMLButtonElement).disabled).toBe(true);
  await userEvent.click(screen.getByRole('button', { name: /Unread/ }));
  await waitFor(() => expect(mock.get.mock.calls.at(-1)?.[0]).toContain('offset=0&unread=true'));
});
