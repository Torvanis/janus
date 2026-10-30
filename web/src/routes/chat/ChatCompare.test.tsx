import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { SessionContext } from '../../app/session';
import type { Me } from '../../lib/types';
import ChatPage from './ChatPage';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

const base = {
  team_id: '',
  tokens_in: 1,
  tokens_out: 1,
  tokens_cached: 0,
  cost_usd: 0,
  tokens_out_per_second: 0,
  latency_ms: 0,
  ttfb_ms: 0,
  created_at: '',
};

/** One SSE stream per run id, each finished by the test. */
function runs() {
  const push: Record<string, (s: string) => void> = {};
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) => {
      const rid = /runs\/([^/]+)\/events/.exec(String(url))?.[1] ?? '';
      const body = new ReadableStream<Uint8Array>({
        start(c) {
          const enc = new TextEncoder();
          push[rid] = (s) => c.enqueue(enc.encode(s));
        },
      });
      return new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
    }),
  );
  return {
    ready: () => Object.keys(push).length,
    // The done message as an older server sent it: no compare_group.
    done: (rid: string, msg: Record<string, unknown>) => push[rid]!(`event: done\ndata: ${JSON.stringify(msg)}\n\n`),
  };
}

it('keeps every compared reply side by side once they finish, and Prefer this answer switches the chat', async () => {
  const r = runs();
  const reply = (id: string, model: string) => ({
    ...base,
    id,
    parent_id: 'u1',
    role: 'assistant',
    model,
    content: '',
    status: 'streaming',
    compare_group: 'g1',
  });
  mocked.get.mockImplementation(async (path: string) => {
    if (path === '/api/v1/models') return { models: [], managed_models: [] } as never;
    if (path === '/api/v1/chat/conversations/c1') {
      return {
        conversation: { id: 'c1', title: 'Penguins', model: 'm1', current_leaf_id: 'a1', created_at: '', updated_at: '' },
        messages: [
          { ...base, id: 'u1', parent_id: '', role: 'user', content: 'What is a penguin?', status: 'complete' },
          reply('a1', 'm1'),
          reply('a2', 'm2'),
          reply('a3', 'm3'),
        ],
        runs: { a1: 'r1', a2: 'r2', a3: 'r3' },
      } as never;
    }
    return { conversations: [], results: [] } as never;
  });
  mocked.patch.mockResolvedValue({
    id: 'c1',
    title: 'Penguins',
    model: 'm2',
    current_leaf_id: 'a2',
    created_at: '',
    updated_at: '',
  } as never);
  const me = { id: 'u1', active_team_id: '', teams: [], feature_flags: {}, chat_preview: true } as unknown as Me;
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <SessionContext.Provider value={{ me, config: null, loading: false, error: null, refresh: () => {} }}>
        <MemoryRouter initialEntries={['/chat/c1']}>
          <Routes>
            <Route path="/chat/:id" element={<ChatPage />} />
          </Routes>
        </MemoryRouter>
      </SessionContext.Provider>
    </QueryClientProvider>,
  );
  await waitFor(() => expect(r.ready()).toBe(3));
  const row = await screen.findByRole('region', { name: '3 models compared' });
  expect(within(row).getByLabelText('m1 (preferred)')).toBeTruthy();

  await act(async () => {
    for (const [rid, id, model] of [
      ['r1', 'a1', 'm1'],
      ['r2', 'a2', 'm2'],
      ['r3', 'a3', 'm3'],
    ] as const) {
      r.done(rid, {
        ...base,
        id,
        parent_id: 'u1',
        role: 'assistant',
        model,
        content: `Answer from ${model}`,
        status: 'complete',
      });
    }
  });
  await waitFor(() => expect(screen.getByText('Answer from m3')).toBeTruthy());
  // Still three columns, the bar model's reply included; no pager.
  const after = screen.getByRole('region', { name: '3 models compared' });
  expect(within(after).getByText('Answer from m1')).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Next version' })).toBeNull();

  const m2 = within(after).getByLabelText('m2');
  fireEvent.click(within(m2).getByRole('button', { name: 'Prefer this answer' }));
  await waitFor(() =>
    expect(mocked.patch).toHaveBeenCalledWith(
      '/api/v1/chat/conversations/c1',
      expect.objectContaining({ current_leaf_id: 'a2' }),
    ),
  );
  // The new preferred answer is marked, and all three stay in view.
  await waitFor(() => expect(within(after).getByLabelText('m2 (preferred)')).toBeTruthy());
  expect(within(after).getByText('Answer from m1')).toBeTruthy();
});
