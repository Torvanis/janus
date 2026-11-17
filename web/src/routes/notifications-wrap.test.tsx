import { afterEach, it, expect, vi } from 'vitest';
import { render, screen, cleanup } from '@testing-library/react';
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

// A discovery failure quotes the upstream URL; on a phone the unbroken URL
// pushed the whole page 125px sideways. It must offer a break after each "/".
it('lets a long upstream URL in a notification wrap after its slashes', async () => {
  const body = 'Last error: reach upstream: Get "http://vllm-a.inference.svc:8000/v1/models": dial tcp: i/o timeout';
  mock.get.mockResolvedValue({
    notifications: [
      {
        id: 'n1',
        title: 'Upstream vllm-a is unreachable',
        body,
        severity: 'critical',
        created_at: '2026-09-01T00:00:00Z',
        read_at: '',
      },
    ],
    unread_count: 1,
    has_more: false,
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
  const para = await screen.findByText((_, el) => el?.tagName === 'P' && el.textContent === body);
  expect(para.querySelectorAll('wbr').length).toBe(body.split('/').length - 1);
  expect(para.closest('.break-slash')).not.toBeNull();
});
