import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import { SessionContext } from '../../app/session';
import type { Me } from '../../lib/types';
import ChatPage from './ChatPage';
import SharedChatPage from './SharedChatPage';
import { fillPrompt, matchPrompts, promptVariables, slashQuery } from './ShareAndPrompts';
import { costUSD, estimateInputTokens, pressingQuota } from './Estimate';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

const base = {
  team_id: '',
  tokens_cached: 0,
  cost_usd: 0,
  tokens_out_per_second: 0,
  latency_ms: 0,
  ttfb_ms: 0,
  created_at: '2026-10-19T00:00:00Z',
};

const prompts = [
  {
    id: 'p1',
    name: 'summarise',
    title: 'Summarise for a reader',
    body: 'Summarise for {{audience}}:\n\n{{text}}',
    updated_at: '',
  },
  { id: 'p2', name: 'polite', title: 'Make it polite', body: 'Rewrite this politely.', updated_at: '' },
];

function mockGets(opts: { conversation?: unknown; quotas?: unknown[]; inRate?: number; shares?: () => unknown[] } = {}) {
  mocked.get.mockImplementation(async (path: string) => {
    if (path === '/api/v1/models') {
      return {
        models: [
          {
            id: 'm1',
            name: 'alpha',
            display_name: '',
            status: 'enabled',
            modalities: ['chat'],
            context_window: 100000,
            rate_in_nanousd: opts.inRate ?? 2_000_000_000,
            rate_out_nanousd: 8_000_000_000,
          },
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
    if (path === '/api/v1/chat/settings') return { instructions: '', default_model: '' } as never;
    if (path === '/api/v1/chat/prompts') return { prompts } as never;
    if (path === '/api/v1/dashboard/quota') return { quotas: opts.quotas ?? [] } as never;
    if (path === '/api/v1/chat/conversations/c1/shares') return { shares: opts.shares?.() ?? [] } as never;
    if (path === '/api/v1/chat/conversations/c1' && opts.conversation) return opts.conversation as never;
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
            <Route path="/chat/shared/:share" element={<SharedChatPage />} />
            <Route path="/chat/:id" element={<ChatPage />} />
          </Routes>
        </MemoryRouter>
      </SessionContext.Provider>
    </QueryClientProvider>,
  );
}

const oneTurn = {
  conversation: { id: 'c1', title: 'Moon', model: 'alpha', current_leaf_id: 'a1' },
  messages: [
    {
      ...base,
      id: 'u1m',
      parent_id: '',
      role: 'user',
      content: 'How far is the moon?',
      status: 'complete',
      tokens_in: 0,
      tokens_out: 0,
    },
    {
      ...base,
      id: 'a1',
      parent_id: 'u1m',
      role: 'assistant',
      content: 'About 384,400 km.',
      status: 'complete',
      tokens_in: 1200,
      tokens_out: 300,
    },
  ],
};

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
  sessionStorage.clear();
});

describe('prompt helpers', () => {
  it('finds variables, fills them, and reads the slash query', () => {
    expect(promptVariables('Hi {{ name }}, about {{topic}} and {{name}}')).toEqual(['name', 'topic']);
    expect(fillPrompt('Hi {{ name }} / {{other}}', { name: 'Sam' })).toBe('Hi Sam / {{other}}');
    expect(slashQuery('/sum')).toBe('sum');
    expect(slashQuery('/')).toBe('');
    expect(slashQuery('not /sum')).toBeNull();
    expect(slashQuery('/sum more')).toBeNull();
    expect(matchPrompts(prompts, 'po').map((p) => p.name)).toEqual(['polite']);
    expect(matchPrompts(prompts, '').map((p) => p.name)).toEqual(['polite', 'summarise']);
  });
});

describe('estimate helpers', () => {
  it('adds the draft and files to the last measured request', () => {
    expect(
      estimateInputTokens({
        lastIn: 1000,
        lastOut: 200,
        draft: 'x'.repeat(400),
        fileTokens: 50,
        instructionChars: 0,
        conversationStarted: true,
      }),
    ).toBe(1000 + 200 + 100 + 4 + 50);
    expect(costUSD(1_000_000, 2_000_000_000)).toBeCloseTo(2);
    const q = (over: Partial<{ breached: boolean; at_risk: boolean; percent: number }>) =>
      ({ id: String(Math.random()), breached: false, at_risk: false, percent: 0, ...over }) as never;
    expect(pressingQuota([q({ at_risk: true, percent: 85 }), q({ breached: true, percent: 100 })])?.breached).toBe(true);
    expect(pressingQuota([q({ percent: 30 })])).toBeNull();
  });
});

describe('composer estimate and quota', () => {
  it('shows tokens and input cost before sending, and the pressing quota', async () => {
    mockGets({
      conversation: oneTurn,
      quotas: [
        {
          id: 'q1',
          metric: 'cost_usd',
          metric_label: 'Spend',
          window: 'monthly',
          window_label: 'this month',
          subject_name: 'Personal',
          breached: false,
          at_risk: true,
          percent: 86.4,
          reset_at: '',
          breach_behavior: 'let_finish',
        },
      ],
    });
    const user = userEvent.setup();
    renderAt('/chat/c1');
    await screen.findByText('About 384,400 km.');
    expect(await screen.findByRole('status', { name: '' })).toBeTruthy();
    expect(screen.getByText(/Spend for Personal: 86% used \(this month\)/)).toBeTruthy();
    await user.type(screen.getByLabelText('Message'), 'x'.repeat(400));
    // 1200 + 300 + 100 + 4 = 1604 tokens × $2/M = $0.0032
    expect(screen.getByText('~1.6k tokens in · ~$0.0032')).toBeTruthy();
  });
});

describe('saved prompts in the composer', () => {
  it('inserts a prompt from the "/" menu with the keyboard', async () => {
    mockGets();
    const user = userEvent.setup();
    renderAt('/chat');
    await screen.findByRole('option', { name: 'alpha' });
    const box = screen.getByLabelText('Message') as HTMLTextAreaElement;
    await user.type(box, '/po');
    const menu = await screen.findByRole('listbox', { name: 'Saved prompts' });
    expect(
      within(menu)
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['/politeMake it polite']);
    await user.keyboard('{Enter}');
    expect(box.value).toBe('Rewrite this politely.');
    expect(screen.queryByRole('listbox', { name: 'Saved prompts' })).toBeNull();
    expect(mocked.post).not.toHaveBeenCalled(); // Enter picked the prompt, it did not send
  });

  it('asks for {{variables}} before inserting', async () => {
    mockGets();
    const user = userEvent.setup();
    renderAt('/chat');
    await screen.findByRole('option', { name: 'alpha' });
    const box = screen.getByLabelText('Message') as HTMLTextAreaElement;
    await user.type(box, '/sum');
    await user.click(await screen.findByRole('option', { name: /summarise/ }));
    const dialog = await screen.findByRole('dialog', { name: '/summarise' });
    await user.type(within(dialog).getByLabelText('audience'), 'a ten-year-old');
    await user.click(within(dialog).getByRole('button', { name: 'Insert' }));
    expect(box.value).toBe('Summarise for a ten-year-old:\n\n{{text}}');
  });

  it('saves a sent message as a prompt', async () => {
    mockGets({ conversation: oneTurn });
    mocked.post.mockResolvedValue({ id: 'p9', name: 'moon', title: '', body: 'How far is the moon?', updated_at: '' } as never);
    const user = userEvent.setup();
    renderAt('/chat/c1');
    await screen.findByText('About 384,400 km.');
    await user.click(screen.getByRole('button', { name: 'Save as prompt' }));
    const dialog = await screen.findByRole('dialog', { name: 'Saved prompts' });
    expect((within(dialog).getByLabelText('Prompt') as HTMLTextAreaElement).value).toBe('How far is the moon?');
    await user.type(within(dialog).getByLabelText('Command'), 'Moon Far');
    expect((within(dialog).getByLabelText('Command') as HTMLInputElement).value).toBe('moon-far');
    await user.click(within(dialog).getByRole('button', { name: 'Save prompt' }));
    await waitFor(() =>
      expect(mocked.post).toHaveBeenCalledWith('/api/v1/chat/prompts', {
        name: 'moon-far',
        title: '',
        body: 'How far is the moon?',
      }),
    );
  });
});

describe('share and export', () => {
  it('makes and copies a link, and offers exports', async () => {
    const link = {
      id: 'tok',
      url: '/chat/shared/tok',
      title: 'Moon',
      message_count: 2,
      views: 0,
      created_at: '2026-10-20T00:00:00Z',
    };
    let made = false;
    mockGets({ conversation: oneTurn, shares: () => (made ? [link] : []) });
    mocked.post.mockImplementation(async () => {
      made = true;
      return link as never;
    });
    const user = userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, 'writeText');
    renderAt('/chat/c1');
    await screen.findByText('About 384,400 km.');
    await user.click(screen.getByRole('button', { name: 'Share and export' }));
    const dialog = await screen.findByRole('dialog', { name: 'Share & export' });
    expect(within(dialog).getByRole('link', { name: 'Markdown' }).getAttribute('href')).toBe(
      '/api/v1/chat/conversations/c1/export?format=markdown',
    );
    expect(within(dialog).getByRole('link', { name: 'JSON' }).getAttribute('href')).toBe(
      '/api/v1/chat/conversations/c1/export?format=json',
    );
    await user.click(within(dialog).getByRole('button', { name: 'Copy a new link' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(`${window.location.origin}/chat/shared/tok`));
    expect(await within(dialog).findByRole('button', { name: 'Copied' })).toBeTruthy();
    expect(mocked.post).toHaveBeenCalledWith('/api/v1/chat/conversations/c1/shares', {});
  });

  it('renders a shared chat read-only, and names a revoked link', async () => {
    mocked.get.mockImplementation(async (path: string) => {
      if (path === '/api/v1/chat/shared/ok')
        return {
          title: 'Moon',
          shared_by: 'Ada',
          shared_at: '2026-10-20T00:00:00Z',
          own: false,
          messages: [
            { role: 'user', content: 'How far?' },
            { role: 'assistant', content: 'About **384,400 km**.', model: 'alpha' },
          ],
        } as never;
      throw new ApiError(404, { error: { message: 'not found', type: 'not_found_error', code: 'not_found' } } as never);
    });
    const first = renderAt('/chat/shared/ok');
    expect(await screen.findByRole('heading', { name: 'Moon', level: 1 })).toBeTruthy();
    expect(screen.getByText(/Shared by Ada/)).toBeTruthy();
    expect(screen.getByText('384,400 km').tagName).toBe('STRONG');
    expect(screen.queryByLabelText('Message')).toBeNull();
    first.unmount();
    renderAt('/chat/shared/gone');
    expect(await screen.findByRole('heading', { name: 'This link no longer works' })).toBeTruthy();
  });
});
