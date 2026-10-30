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
  tokens_in: 1,
  tokens_out: 1,
  tokens_cached: 0,
  cost_usd: 0,
  tokens_out_per_second: 0,
  latency_ms: 0,
  ttfb_ms: 0,
  created_at: '2026-10-19T00:00:00Z',
};

function mockGets(opts: { conversations?: unknown[]; conversation?: unknown; settings?: unknown } = {}) {
  mocked.get.mockImplementation(async (path: string) => {
    if (path === '/api/v1/models') {
      return {
        models: [
          { id: 'm1', name: 'alpha', display_name: '', status: 'enabled', modalities: ['chat'], context_window: 8192 },
          { id: 'm2', name: 'beta', display_name: '', status: 'enabled', modalities: ['chat'], context_window: 8192 },
        ],
        managed_models: [],
      } as never;
    }
    if (path === '/api/v1/chat/catalog')
      return {
        models: [],
        hidden: [],
        default_model: '',
        starter_prompts: [],
        categories: [],
        features: { shares: true, export: true, personal_plans: true },
      } as never;
    if (path === '/api/v1/chat/settings') return (opts.settings ?? { instructions: '', default_model: '' }) as never;
    if (path === '/api/v1/chat/conversations/c1' && opts.conversation) return opts.conversation as never;
    if (path.startsWith('/api/v1/chat/conversations')) return { conversations: opts.conversations ?? [] } as never;
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

function sendResponse(conv: Record<string, unknown>) {
  return {
    run_id: 'r1',
    user_message: { ...base, id: 'u1m', parent_id: '', role: 'user', content: 'hi', status: 'complete' },
    assistant_message: { ...base, id: 'a1', parent_id: 'u1m', role: 'assistant', content: '', status: 'streaming' },
    conversation: { id: 'c1', title: 'hi', model: 'beta', current_leaf_id: 'a1', ...conv },
    warnings: [],
  };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
  sessionStorage.clear();
});

describe('Chat settings', () => {
  it('starts a new chat on the saved default model', async () => {
    mockGets({ settings: { instructions: '', default_model: 'beta' } });
    renderAt('/chat');
    await waitFor(() => expect((screen.getByLabelText('Model', { selector: 'select' }) as HTMLSelectElement).value).toBe('beta'));
  });

  it('applies instructions, thinking and temporary chosen before the first message', async () => {
    mockGets();
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response('', { status: 200 })),
    );
    mocked.post.mockImplementation(async (path: string) => {
      if (path === '/api/v1/chat/conversations') return { id: 'c1', title: '', model: 'alpha', current_leaf_id: '' } as never;
      return sendResponse({ temporary: true }) as never;
    });
    mocked.put.mockResolvedValue({ instructions: '', default_model: 'alpha' } as never);
    const user = userEvent.setup();
    renderAt('/chat');
    await screen.findByRole('option', { name: 'alpha' });

    await user.click(screen.getByRole('button', { name: /Temporary/ }));
    expect(screen.getByRole('heading', { name: 'Temporary chat', level: 2 })).toBeTruthy();

    await user.click(screen.getByRole('button', { name: 'Chat settings' }));
    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByLabelText('Instructions for this chat'), 'Be brief.');
    await user.click(within(dialog).getByLabelText(/^High/));
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(screen.getByRole('button', { name: /Thinking: High/ })).toBeTruthy();

    await user.type(screen.getByLabelText('Message'), 'hi{Enter}');
    await waitFor(() =>
      expect(mocked.post).toHaveBeenCalledWith('/api/v1/chat/conversations', {
        model: 'alpha',
        temporary: true,
        instructions: 'Be brief.',
        reasoning_effort: 'high',
      }),
    );
    // A temporary chat never changes the default model.
    expect(mocked.put).not.toHaveBeenCalledWith('/api/v1/chat/settings', expect.anything());
  });

  it('saves the user’s own instructions and the conversation’s settings from the panel', async () => {
    mockGets({
      settings: { instructions: 'Call me Sam.', default_model: '' },
      conversation: {
        conversation: { id: 'c1', title: 'Maths', model: 'alpha', current_leaf_id: '', instructions: '', reasoning_effort: '' },
        messages: [],
      },
    });
    mocked.put.mockResolvedValue({ instructions: 'Call me Sam. Use metric.', default_model: '' } as never);
    mocked.patch.mockResolvedValue({
      id: 'c1',
      title: 'Maths',
      model: 'alpha',
      current_leaf_id: '',
      instructions: 'Tutor mode.',
      reasoning_effort: 'low',
    } as never);
    const user = userEvent.setup();
    renderAt('/chat/c1');
    await screen.findByRole('heading', { name: 'Maths', level: 1 });
    await user.click(screen.getByRole('button', { name: 'Chat settings' }));
    const dialog = await screen.findByRole('dialog');
    const mine = within(dialog).getByLabelText('Your instructions for every chat') as HTMLTextAreaElement;
    await waitFor(() => expect(mine.value).toBe('Call me Sam.'));
    await user.type(mine, ' Use metric.');
    await user.type(within(dialog).getByLabelText('Instructions for this chat'), 'Tutor mode.');
    await user.click(within(dialog).getByLabelText(/^Low/));
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(mocked.patch).toHaveBeenCalled());
    expect(mocked.put).toHaveBeenCalledWith('/api/v1/chat/settings', { instructions: 'Call me Sam. Use metric.' });
    expect(mocked.patch).toHaveBeenCalledWith('/api/v1/chat/conversations/c1', {
      instructions: 'Tutor mode.',
      reasoning_effort: 'low',
    });
  });
});

describe('Chat history', () => {
  const convs = [
    {
      id: 'c1',
      team_id: '',
      title: 'Groceries',
      model: 'alpha',
      current_leaf_id: '',
      created_at: '',
      updated_at: '2026-10-19T10:00:00Z',
    },
    {
      id: 'c2',
      team_id: '',
      title: 'Trip plan',
      model: 'alpha',
      current_leaf_id: '',
      created_at: '',
      updated_at: '2026-10-18T10:00:00Z',
      pinned: true,
    },
  ];

  it('lists pinned chats in their own group first', async () => {
    mockGets({ conversations: convs });
    renderAt('/chat');
    const nav = await screen.findByRole('navigation', { name: 'Chat history' });
    await within(nav).findByText('Trip plan');
    const headings = within(nav)
      .getAllByRole('heading')
      .map((h) => h.textContent);
    expect(headings[0]).toBe('Pinned');
    expect(
      within(nav)
        .getAllByRole('link')
        .map((a) => a.textContent)[0],
    ).toBe('Trip plan');
  });

  it('renames, pins and deletes from the row menu', async () => {
    mockGets({ conversations: convs });
    mocked.patch.mockResolvedValue({ ...convs[0] } as never);
    mocked.del.mockResolvedValue(undefined as never);
    const confirm = vi.fn(() => true);
    vi.stubGlobal('confirm', confirm);
    const user = userEvent.setup();
    renderAt('/chat');
    await screen.findByText('Groceries');

    await user.click(screen.getByRole('button', { name: 'Options for Groceries' }));
    await user.click(screen.getByRole('menuitem', { name: 'Rename' }));
    const field = screen.getByLabelText('Chat title');
    await user.clear(field);
    await user.type(field, 'Weekly shop{Enter}');
    await waitFor(() => expect(mocked.patch).toHaveBeenCalledWith('/api/v1/chat/conversations/c1', { title: 'Weekly shop' }));

    await user.click(screen.getByRole('button', { name: 'Options for Groceries' }));
    await user.click(screen.getByRole('menuitem', { name: 'Pin' }));
    await waitFor(() => expect(mocked.patch).toHaveBeenCalledWith('/api/v1/chat/conversations/c1', { pinned: true }));

    await user.click(screen.getByRole('button', { name: 'Options for Trip plan' }));
    await user.click(screen.getByRole('menuitem', { name: 'Unpin' }));
    await waitFor(() => expect(mocked.patch).toHaveBeenCalledWith('/api/v1/chat/conversations/c2', { pinned: false }));

    await user.click(screen.getByRole('button', { name: 'Options for Groceries' }));
    await user.click(screen.getByRole('menuitem', { name: 'Delete' }));
    expect(confirm).toHaveBeenCalled();
    await waitFor(() => expect(mocked.del).toHaveBeenCalledWith('/api/v1/chat/conversations/c1'));
  });

  it('keeps a draft per chat across a reload', async () => {
    mockGets();
    const user = userEvent.setup();
    const first = renderAt('/chat');
    await screen.findByRole('option', { name: 'alpha' });
    await user.type(screen.getByLabelText('Message'), 'half a thought');
    first.unmount();
    renderAt('/chat');
    await screen.findByRole('option', { name: 'alpha' });
    expect((screen.getByLabelText('Message') as HTMLTextAreaElement).value).toBe('half a thought');
  });
});

describe('Reply notes', () => {
  it('shows routing, fallback and security outcomes from the request log', async () => {
    mockGets({
      conversation: {
        conversation: { id: 'c1', title: 'Notes', model: 'auto', current_leaf_id: 'a1' },
        messages: [
          { ...base, id: 'u1m', parent_id: '', role: 'user', content: 'q', status: 'complete' },
          {
            ...base,
            id: 'a1',
            parent_id: 'u1m',
            role: 'assistant',
            content: 'answer',
            status: 'complete',
            model: 'auto',
            served_model: 'beta',
            notes: { requested_model: 'auto', route_reason: 'matched:coding', secgw_action: 'observed', secgw_violations: 2 },
          },
        ],
      },
    });
    renderAt('/chat/c1');
    const notes = await screen.findByRole('list', { name: 'About this reply' });
    expect(notes.textContent).toContain('auto → beta (matched coding)');
    expect(notes.textContent).toContain('Flagged by a security policy (2 findings)');
  });
});
