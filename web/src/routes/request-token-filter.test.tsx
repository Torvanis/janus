import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../lib/api';
import { RequestsPage } from './Requests';

vi.mock('../lib/api', async (original) => ({ ...(await original<typeof import('../lib/api')>()), api: { get: vi.fn() } }));
vi.mock('../app/session', () => ({ useLocalOnly: () => false }));
afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

it('consumes the selected token in server requests and CSV, preserves it on sort, and clears it explicitly', async () => {
  const user = userEvent.setup();
  vi.mocked(api.get).mockImplementation(async (path) => {
    if (path.startsWith('/api/v1/models')) return { models: [] };
    if (path.startsWith('/api/v1/requests'))
      return {
        total_count: 60,
        requests: [
          {
            id: 'one',
            model: 'visible',
            created_at: '2026-09-19T00:00:00Z',
            http_status: 200,
            modality: 'chat',
            tokens_in: 0,
            tokens_out: 0,
            latency_ms: 1,
            cost_nanousd: 0,
          },
        ],
      };
    throw new Error(`unexpected ${path}`);
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={['/requests?token_id=tok-selected&range=day&page=50']}>
        <RequestsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  await screen.findByRole('cell', { name: 'visible' });
  expect(screen.getByText('Filtered to the selected API token')).toBeTruthy();
  const exportLink = screen.getByRole('link', { name: /export/i }).getAttribute('href')!;
  expect(new URL(exportLink, 'http://local').searchParams.get('token_id')).toBe('tok-selected');
  await user.click(screen.getByRole('button', { name: /latency/i }));
  await waitFor(() => {
    const path = vi
      .mocked(api.get)
      .mock.calls.filter(([p]) => p.startsWith('/api/v1/requests'))
      .at(-1)![0];
    const q = new URL(path, 'http://local').searchParams;
    expect(q.get('token_id')).toBe('tok-selected');
    expect(q.get('offset')).toBe('0');
    expect(q.get('sort')).toBe('latency');
  });
  await user.click(screen.getByRole('button', { name: /clear filters/i }));
  await waitFor(() => {
    const path = vi
      .mocked(api.get)
      .mock.calls.filter(([p]) => p.startsWith('/api/v1/requests'))
      .at(-1)![0];
    expect(new URL(path, 'http://local').searchParams.has('token_id')).toBe(false);
  });
});
