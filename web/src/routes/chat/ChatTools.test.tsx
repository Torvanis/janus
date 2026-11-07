import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { SessionContext } from '../../app/session';
import type { Me } from '../../lib/types';
import ChatPage from './ChatPage';
import { SearchActivity, SourcesList } from './ChatTools';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

const base = {
  team_id: '',
  tokens_in: 0,
  tokens_out: 0,
  tokens_cached: 0,
  cost_usd: 0,
  tokens_out_per_second: 0,
  latency_ms: 0,
  ttfb_ms: 0,
  created_at: '2026-10-22T00:00:00Z',
};

function mockGets(conversation?: unknown) {
  mocked.get.mockImplementation(async (path: string) => {
    if (path === '/api/v1/models')
      return {
        models: [
          { id: 'm1', name: 'grok-4.6', display_name: '', status: 'enabled', modalities: ['chat'], context_window: 8192 },
          { id: 'm2', name: 'local-8b', display_name: '', status: 'enabled', modalities: ['chat'], context_window: 8192 },
        ],
        managed_models: [],
        personal_models: [],
      } as never;
    if (path === '/api/v1/chat/catalog')
      return {
        models: [{ name: 'grok-4.6', tools: ['web_search', 'x_search'] }, { name: 'local-8b' }],
        hidden: [],
        default_model: 'grok-4.6',
        starter_prompts: [],
        categories: [],
        features: { shares: true, export: true, personal_plans: true },
        tools: [
          { id: 'web_search', label: 'Web search', description: 'Searches the web.' },
          { id: 'x_search', label: 'X search', description: 'Searches X.' },
        ],
      } as never;
    if (path === '/api/v1/chat/settings') return { instructions: '', default_model: '' } as never;
    if (path === '/api/v1/chat/prompts') return { prompts: [] } as never;
    if (path === '/api/v1/dashboard/quota') return { quotas: [] } as never;
    if (path === '/api/v1/chat/conversations/c1' && conversation) return conversation as never;
    if (path.startsWith('/api/v1/chat/conversations')) return { conversations: [] } as never;
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
});

describe('chat tools', () => {
  it('shows the model’s tools (on by default) and saves a switch-off on the chat', async () => {
    const conv = { id: 'c1', title: 'News', model: 'grok-4.6', current_leaf_id: '', team_id: '', tools_off: [] };
    mockGets({ conversation: conv, messages: [] });
    mocked.patch.mockImplementation(async (_p: string, body: unknown) => ({ ...conv, ...(body as object) }) as never);
    const user = userEvent.setup();
    renderAt('/chat/c1');
    const button = await screen.findByRole('button', { name: 'Tools: Web search, X search' });
    await user.click(button);
    const menu = screen.getByRole('group', { name: 'Tools' });
    const x = within(menu).getByRole('switch', { name: 'X search' }) as HTMLInputElement;
    expect(x.checked).toBe(true);
    await user.click(x);
    await waitFor(() => expect(mocked.patch).toHaveBeenCalledWith('/api/v1/chat/conversations/c1', { tools_off: ['x_search'] }));
    expect(screen.getByRole('button', { name: 'Tools: Web search' })).toBeTruthy();
  });

  it('offers no tools for a model whose provider runs none', async () => {
    mockGets({ conversation: { id: 'c1', title: 'x', model: 'local-8b', current_leaf_id: '', team_id: '' }, messages: [] });
    renderAt('/chat/c1');
    await screen.findByRole('button', { name: /Model:/ });
    expect(screen.queryByRole('button', { name: /^Tools:/ })).toBeNull();
  });

  it('shows what a reply searched and the sources it cited', () => {
    render(
      <>
        <SearchActivity
          streaming={false}
          activity={[
            { type: 'web_search', status: 'done', query: 'news today' },
            { type: 'x_search', status: 'done', query: 'ukraine talks' },
          ]}
        />
        <SourcesList
          sources={[
            { url: 'https://apnews.com/a', title: 'Talks resume | AP', host: 'apnews.com' },
            { url: 'https://reuters.com/b', host: 'reuters.com' },
          ]}
        />
      </>,
    );
    expect(screen.getByText('Searched the web and Searched X')).toBeTruthy();
    const sources = screen.getByLabelText('Sources');
    const links = within(sources).getAllByRole('link');
    expect(links.map((a) => a.getAttribute('href'))).toEqual(['https://apnews.com/a', 'https://reuters.com/b']);
    expect(links[0]!.getAttribute('rel')).toContain('noopener');
    expect(links[0]!.textContent).toContain('Talks resume | AP');
  });

  it('names a page that was withheld from the model', () => {
    render(
      <SearchActivity
        streaming={false}
        activity={[
          { type: 'web_fetch', status: 'done', query: 'https://example.com/notes' },
          { type: 'withheld', status: 'done', query: 'https://example.com/notes' },
        ]}
      />,
    );
    expect(screen.getByText('Read a page and Withheld something it read')).toBeTruthy();
    expect(screen.getByText('Withheld')).toBeTruthy();
  });

  it('names the file a reply read, live and afterwards', () => {
    const { rerender } = render(
      <SearchActivity streaming activity={[{ type: 'read_file', status: 'searching', query: 'wg-setup.sh' }]} />,
    );
    expect(screen.getByRole('status').textContent).toContain('Reading a file wg-setup.sh');
    rerender(
      <SearchActivity
        streaming={false}
        activity={[
          { type: 'read_file', status: 'searching', query: 'wg-setup.sh' },
          { type: 'read_file', status: 'done', query: 'wg-setup.sh' },
        ]}
      />,
    );
    expect(screen.getByText('Read a file')).toBeTruthy();
  });

  it('says what it is searching while the reply is still coming', () => {
    render(<SearchActivity streaming activity={[{ type: 'web_search', status: 'searching' }]} />);
    expect(screen.getByRole('status').textContent).toContain('Searching the web');
  });

  it('shows the sources on a finished reply in the thread', async () => {
    mockGets({
      conversation: { id: 'c1', title: 'News', model: 'grok-4.6', current_leaf_id: 'a', team_id: '' },
      messages: [
        { ...base, id: 'u', parent_id: '', role: 'user', content: 'news?', status: 'complete' },
        {
          ...base,
          id: 'a',
          parent_id: 'u',
          role: 'assistant',
          content: 'Talks resume today.',
          model: 'grok-4.6',
          status: 'complete',
          activity: [{ type: 'web_search', status: 'done', query: 'news today' }],
          sources: [{ url: 'https://apnews.com/a', title: 'Talks resume | AP', host: 'apnews.com' }],
        },
      ],
    });
    renderAt('/chat/c1');
    const reply = (await screen.findByText('Talks resume today.')).closest('article')!;
    expect(within(reply as HTMLElement).getByText(/Searched the web/)).toBeTruthy();
    expect(within(reply as HTMLElement).getByRole('link', { name: /Talks resume \| AP/ })).toBeTruthy();
  });
});
