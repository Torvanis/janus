import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
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

const base = {
  team_id: '',
  tokens_cached: 0,
  tokens_in: 12,
  tokens_out: 1056,
  cost_usd: 0.04,
  tokens_out_per_second: 0,
  latency_ms: 0,
  ttfb_ms: 0,
  created_at: '2026-10-20T00:00:00Z',
};

const catalog = {
  models: [
    { id: 'm1', name: 'alpha', display_name: '', status: 'enabled', modalities: ['chat'], context_window: 8192 },
    { id: 'm2', name: 'painter', display_name: '', status: 'enabled', modalities: ['image'], context_window: 0 },
    { id: 'm3', name: 'seer', display_name: '', status: 'enabled', modalities: ['chat', 'image'], context_window: 8192 },
    { id: 'm4', name: 'tts-1', display_name: '', status: 'enabled', modalities: ['tts'], context_window: 0 },
  ],
  managed_models: [],
};

function mockGets(conversation?: unknown) {
  mocked.get.mockImplementation(async (path: string) => {
    if (path === '/api/v1/models') return catalog as never;
    if (path === '/api/v1/chat/catalog')
      return {
        models: [],
        hidden: [],
        default_model: '',
        starter_prompts: [],
        categories: [],
        features: { shares: true, export: true, personal_plans: true },
      } as never;
    if (path === '/api/v1/chat/settings') return { instructions: '', default_model: '' } as never;
    if (path === '/api/v1/chat/prompts') return { prompts: [] } as never;
    if (path === '/api/v1/dashboard/quota') return { quotas: [] } as never;
    if (path === '/api/v1/chat/conversations/c1') {
      if (conversation) return conversation as never;
      // After the first send the page navigates to /chat/c1 and reloads the
      // chat from the server, which then holds the sent message and the
      // live reply (with its run, to resume). Answer as the server would.
      return {
        conversation: { id: 'c1', title: 'A red lighthouse', model: 'painter', current_leaf_id: 'a' },
        messages: [
          { ...base, id: 'u', parent_id: '', role: 'user', content: 'A red lighthouse', status: 'complete' },
          { ...base, id: 'a', parent_id: 'u', role: 'assistant', content: '', model: 'painter', status: 'streaming' },
        ],
        runs: { a: 'r1' },
      } as never;
    }
    if (path.startsWith('/api/v1/chat/conversations')) return { conversations: [] } as never;
    if (path.startsWith('/api/v1/chat/search')) return { results: [] } as never;
    throw new Error(`unexpected GET ${path}`);
  });
}

function renderAt(path: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const me = { id: 'u1', active_team_id: '', teams: [], feature_flags: {}, chat_preview: true } as unknown as Me;
  return render(
    <QueryClientProvider client={qc}>
      <SessionContext.Provider value={{ me, config: null, loading: false, error: null, refresh: () => {} }}>
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route path="/chat" element={<ChatPage />} />
            <Route path="/chat/:id" element={<ChatPage />} />
          </Routes>
        </MemoryRouter>
      </SessionContext.Provider>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
  sessionStorage.clear();
});

describe('image generation in chat', () => {
  it('lists image-only models in their own group, keeps vision chat models as chat', async () => {
    mockGets();
    renderAt('/chat');
    const select = (await screen.findByLabelText('Model', { selector: 'select' })) as HTMLSelectElement;
    await screen.findByRole('option', { name: 'painter' });
    const groups = within(select).getAllByRole('group');
    const images = groups.find((g) => g.getAttribute('label') === 'Make images');
    expect(images).toBeTruthy();
    expect(
      within(images!)
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['painter']);
    const chat = groups.find((g) => g.getAttribute('label')?.startsWith('Working under'));
    expect(
      within(chat!)
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['alpha', 'seer']);
    expect(screen.queryByRole('option', { name: 'tts-1' })).toBeNull();
  });

  it('switches the composer to image mode: description prompt, no attaching, sends the text', async () => {
    mockGets();
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response('', { status: 200 })),
    );
    mocked.post.mockImplementation(async (path: string) => {
      if (path === '/api/v1/chat/conversations') return { id: 'c1', title: '', model: 'painter', current_leaf_id: '' } as never;
      return {
        run_id: 'r1',
        user_message: { ...base, id: 'u', parent_id: '', role: 'user', content: 'A red lighthouse', status: 'complete' },
        assistant_message: {
          ...base,
          id: 'a',
          parent_id: 'u',
          role: 'assistant',
          content: '',
          model: 'painter',
          status: 'streaming',
        },
        conversation: { id: 'c1', title: 'A red lighthouse', model: 'painter', current_leaf_id: 'a' },
        warnings: [],
      } as never;
    });
    const user = userEvent.setup();
    renderAt('/chat');
    await screen.findByRole('option', { name: 'painter' });
    await user.selectOptions(screen.getByLabelText('Model', { selector: 'select' }), 'painter');
    const box = screen.getByLabelText('Message');
    expect(box.getAttribute('placeholder')).toBe('Describe the image you want…');
    expect(screen.queryByRole('button', { name: 'Attach files' })).toBeNull();
    expect(screen.getByText('Makes an image')).toBeTruthy();
    await user.type(box, 'A red lighthouse{Enter}');
    await waitFor(() =>
      expect(mocked.post).toHaveBeenCalledWith(
        '/api/v1/chat/conversations/c1/messages',
        expect.objectContaining({ content: 'A red lighthouse', model: 'painter' }),
      ),
    );
    expect(await screen.findByRole('status', { name: 'Making the image' })).toBeTruthy();
  });

  it('shows a generated image on the reply with its caption and a download link', async () => {
    mockGets({
      conversation: { id: 'c1', title: 'Lighthouse', model: 'painter', current_leaf_id: 'a' },
      messages: [
        { ...base, id: 'u', parent_id: '', role: 'user', content: 'A red lighthouse', status: 'complete' },
        {
          ...base,
          id: 'a',
          parent_id: 'u',
          role: 'assistant',
          content: 'A red lighthouse on rocks at dusk, watercolor',
          model: 'painter',
          served_model: 'painter',
          status: 'complete',
          attachments: [
            {
              id: 'img1',
              kind: 'image',
              filename: 'image-1.png',
              mime: 'image/png',
              size_bytes: 1000,
              est_tokens: 1000,
              width: 1024,
              height: 1024,
            },
          ],
        },
      ],
    });
    renderAt('/chat/c1');
    const img = (await screen.findByRole('img', { name: 'image-1.png' })) as HTMLImageElement;
    expect(img.getAttribute('src')).toBe('/api/v1/chat/attachments/img1/content');
    expect(screen.getByText('A red lighthouse on rocks at dusk, watercolor')).toBeTruthy();
    const dl = screen.getByRole('link', { name: 'Download' });
    expect(dl.getAttribute('download')).toBe('image-1.png');
    expect(screen.getByText('1024×1024')).toBeTruthy();
  });
});
