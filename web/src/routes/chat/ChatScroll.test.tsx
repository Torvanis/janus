import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
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

/** A reply streaming over SSE that the test pushes chunks into. */
function streamingRun() {
  let push: (s: string) => void = () => {};
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      const enc = new TextEncoder();
      push = (s) => c.enqueue(enc.encode(s));
    },
  });
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })),
  );
  return { delta: (text: string) => push(`event: delta\ndata: ${JSON.stringify({ content: text })}\n\n`) };
}

/** jsdom has no layout: give the thread a fixed viewport over growing content. */
function fakeLayout(el: HTMLElement) {
  let top = 0;
  Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => 400 });
  Object.defineProperty(el, 'scrollHeight', { configurable: true, get: () => 400 + (el.textContent?.length ?? 0) * 10 });
  Object.defineProperty(el, 'scrollTop', {
    configurable: true,
    get: () => top,
    set: (v: number) => {
      top = Math.max(0, Math.min(v, el.scrollHeight - el.clientHeight));
    },
  });
}

it('stops following the reply when the reader scrolls up, and resumes at the bottom', async () => {
  const run = streamingRun();
  const base = {
    team_id: '',
    tokens_in: 0,
    tokens_out: 0,
    tokens_cached: 0,
    cost_usd: 0,
    tokens_out_per_second: 0,
    latency_ms: 0,
    ttfb_ms: 0,
    created_at: '',
  };
  mocked.get.mockImplementation(async (path: string) => {
    if (path === '/api/v1/models') return { models: [], managed_models: [] } as never;
    if (path === '/api/v1/chat/conversations/c1') {
      return {
        conversation: { id: 'c1', title: 'Hi', model: 'm', current_leaf_id: 'a1', created_at: '', updated_at: '' },
        messages: [
          { ...base, id: 'u1m', parent_id: '', role: 'user', content: 'tell me a long story', status: 'complete' },
          { ...base, id: 'a1', parent_id: 'u1m', role: 'assistant', content: '', status: 'streaming' },
        ],
        runs: { a1: 'r1' },
      } as never;
    }
    return { conversations: [], results: [] } as never;
  });
  const me = { id: 'u1', active_team_id: '', teams: [], feature_flags: {}, chat_preview: true } as unknown as Me;
  const { container } = render(
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
  const thread = container.querySelector('.chat-thread') as HTMLElement;
  fakeLayout(thread);
  const atEnd = () => thread.scrollTop + thread.clientHeight >= thread.scrollHeight - 1;

  // Following: new text keeps the end in view.
  await act(async () => run.delta('Once upon a time. '.repeat(20)));
  await waitFor(() => expect(thread.textContent).toContain('Once upon a time.'));
  await waitFor(() => expect(atEnd()).toBe(true));
  expect(screen.queryByRole('button', { name: /Jump to latest/ })).toBeNull();

  // The reader scrolls up to read: more text must not move them.
  act(() => {
    thread.scrollTop = 100;
    fireEvent.scroll(thread);
  });
  await act(async () => run.delta('The dragon woke. '.repeat(20)));
  await waitFor(() => expect(thread.textContent).toContain('The dragon woke.'));
  expect(thread.scrollTop).toBe(100);
  const jump = await screen.findByRole('button', { name: /Jump to latest/ });

  // Back at the bottom (by hand): following resumes.
  act(() => {
    thread.scrollTop = thread.scrollHeight;
    fireEvent.scroll(thread);
  });
  await act(async () => run.delta('The end. '.repeat(20)));
  await waitFor(() => expect(thread.textContent).toContain('The end.'));
  await waitFor(() => expect(atEnd()).toBe(true));
  expect(screen.queryByRole('button', { name: /Jump to latest/ })).toBeNull();

  // Scroll up again, then "Jump to latest" resumes too.
  act(() => {
    thread.scrollTop = 50;
    fireEvent.scroll(thread);
  });
  await act(async () => run.delta('Epilogue. '.repeat(20)));
  await waitFor(() => expect(thread.textContent).toContain('Epilogue.'));
  expect(thread.scrollTop).toBe(50);
  void jump;
  fireEvent.click(await screen.findByRole('button', { name: /Jump to latest/ }));
  expect(atEnd()).toBe(true);
  await act(async () => run.delta('More. '.repeat(20)));
  await waitFor(() => expect(thread.textContent).toContain('More.'));
  await waitFor(() => expect(atEnd()).toBe(true));
});
