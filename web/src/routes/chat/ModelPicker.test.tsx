import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { SessionContext } from '../../app/session';
import type { Me } from '../../lib/types';
import ChatPage from './ChatPage';
import { arrangeOptions, type ChatCatalog } from './ModelPicker';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

const models = {
  models: [
    { id: 'm1', name: 'qwen-27b', display_name: '', status: 'enabled', modalities: ['chat'], context_window: 8192 },
    { id: 'm2', name: 'gpt-6', display_name: '', status: 'enabled', modalities: ['chat'], context_window: 8192 },
    { id: 'm3', name: 'legacy-7b', display_name: '', status: 'enabled', modalities: ['chat'], context_window: 8192 },
  ],
  managed_models: [],
  personal_models: [{ id: 'my/xai/grok-4.7', owned_by: 'xAI Grok (SuperGrok)', janus: { native_name: 'grok-4.7' } }],
};

const curated: ChatCatalog = {
  models: [
    {
      name: 'qwen-27b',
      chat_name: 'Fast & private',
      description: 'Runs on our own servers.',
      category: 'local',
      recommended: true,
    },
    { name: 'gpt-6', category: 'reasoning', order: 1 },
  ],
  hidden: ['legacy-7b'],
  default_model: 'gpt-6',
  starter_prompts: ['Summarize a document', 'Draft an email'],
  categories: ['general', 'fast', 'reasoning', 'coding', 'vision', 'local', 'creative'],
  features: { shares: true, export: true, personal_plans: false },
};

function mockGets(catalog: ChatCatalog) {
  mocked.get.mockImplementation(async (path: string) => {
    if (path === '/api/v1/models') return models as never;
    if (path === '/api/v1/chat/catalog') return catalog as never;
    if (path === '/api/v1/chat/settings') return { instructions: '', default_model: '' } as never;
    if (path === '/api/v1/chat/prompts') return { prompts: [] } as never;
    if (path === '/api/v1/dashboard/quota') return { quotas: [] } as never;
    if (path.startsWith('/api/v1/chat/conversations')) return { conversations: [] } as never;
    throw new Error(`unexpected GET ${path}`);
  });
}

function renderChat() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const me = { id: 'u1', active_team_id: '', teams: [], feature_flags: {}, chat_preview: true } as unknown as Me;
  return render(
    <QueryClientProvider client={qc}>
      <SessionContext.Provider value={{ me, config: null, loading: false, error: null, refresh: () => {} }}>
        <MemoryRouter initialEntries={['/chat']}>
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
  sessionStorage.clear();
});

describe('chat picker follows Administration → Chat', () => {
  it('groups by category, shows grant chat names and descriptions, hides hidden models and plans when off', async () => {
    mockGets(curated);
    const user = userEvent.setup();
    renderChat();
    // The organization default is preselected.
    const button = await screen.findByRole('button', { name: 'Model: gpt-6' });
    await user.click(button);
    const list = screen.getByRole('listbox', { name: 'Models' });
    const groups = within(list).getAllByRole('group');
    expect(groups.map((g) => g.getAttribute('aria-label'))).toEqual(['Reasoning', 'Local & private']);
    const qwen = within(list).getByRole('option', { name: /Fast & private/ });
    expect(qwen.textContent).toContain('Runs on our own servers.');
    expect(qwen.textContent).toContain('Recommended');
    expect(qwen.textContent).toContain('qwen-27b'); // the real model under the chat name
    expect(within(list).queryByRole('option', { name: /legacy-7b/ })).toBeNull();
    expect(within(list).queryByRole('option', { name: /grok-4.7/ })).toBeNull();
    await user.click(qwen);
    expect(screen.getByRole('button', { name: 'Model: Fast & private' })).toBeTruthy();
    expect((screen.getByLabelText('Model', { selector: 'select' }) as HTMLSelectElement).value).toBe('qwen-27b');
  });

  it('offers the admin suggestions on the empty screen; clicking one fills the box', async () => {
    mockGets(curated);
    const user = userEvent.setup();
    renderChat();
    const suggestions = await screen.findByRole('group', { name: 'Suggestions' });
    await user.click(within(suggestions).getByRole('button', { name: 'Draft an email' }));
    await waitFor(() => expect((screen.getByLabelText('Message') as HTMLTextAreaElement).value).toBe('Draft an email'));
  });

  it('falls back to the plain grouping when nothing is curated', () => {
    const groups = arrangeOptions(
      [
        { value: 'a', label: 'a' },
        { value: 'my/xai/x', label: 'x · xAI', personal: true },
      ],
      undefined,
      'Working under Personal',
    );
    expect(groups.map((g) => g.title)).toEqual(['Working under Personal', 'Your subscriptions']);
  });
});
