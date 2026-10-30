import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { SessionContext } from '../../app/session';
import type { Me } from '../../lib/types';
import ChatPage, { groupConversations, idleLabel } from './ChatPage';
import userEvent from '@testing-library/user-event';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});

const mocked = vi.mocked(api);

function renderPage(activeTeam: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const me = {
    id: 'u1',
    active_team_id: activeTeam,
    teams: [{ id: 't1', name: 'Household', lead_user_id: '' }],
    feature_flags: {},
    chat_preview: true,
  } as unknown as Me;
  return render(
    <QueryClientProvider client={qc}>
      <SessionContext.Provider value={{ me, config: null, loading: false, error: null, refresh: () => {} }}>
        <MemoryRouter initialEntries={['/chat']}>
          <Routes>
            <Route path="/chat" element={<ChatPage />} />
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

describe('ChatPage billing context', () => {
  it('offers the models /api/v1/models returns for the Working under selection and names who pays', async () => {
    mocked.get.mockImplementation(async (path: string) => {
      if (path === '/api/v1/models') {
        return {
          models: [
            { id: 'm1', name: 'team-model', display_name: '', status: 'enabled', modalities: ['chat'], context_window: 8192 },
            { id: 'm2', name: 'embedder', display_name: '', status: 'enabled', modalities: ['embedding'], context_window: 0 },
          ],
          managed_models: [],
        } as never;
      }
      if (path.startsWith('/api/v1/chat/conversations')) return { conversations: [] } as never;
      if (path.startsWith('/api/v1/chat/search')) return { results: [] } as never;
      throw new Error(`unexpected GET ${path}`);
    });
    renderPage('t1');
    await waitFor(() => expect(screen.getByRole('option', { name: 'team-model' })).toBeTruthy());
    expect(screen.queryByRole('option', { name: 'embedder' })).toBeNull();
    expect(screen.getByLabelText('Billed to').textContent).toContain('Household');
    expect(mocked.get.mock.calls.map((c) => c[0])).toContain('/api/v1/models');
  });

  it('says plainly when nothing is available under the selection', async () => {
    mocked.get.mockImplementation(async (path: string) => {
      if (path === '/api/v1/models') return { models: [], managed_models: [] } as never;
      return { conversations: [], results: [] } as never;
    });
    renderPage('');
    await waitFor(() =>
      expect(screen.getByRole('alert').textContent).toMatch(/No chat models are available while working under Personal/),
    );
  });
});

describe('ChatPage blocked replies', () => {
  it('shows a blocked reply as a calm terminal note, not an error', async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const me = { id: 'u1', active_team_id: '', teams: [], feature_flags: {}, chat_preview: true } as unknown as Me;
    const base = {
      team_id: '',
      tokens_in: 0,
      tokens_out: 0,
      tokens_cached: 0,
      cost_usd: 0,
      tokens_out_per_second: 0,
      latency_ms: 0,
      ttfb_ms: 0,
      created_at: '2026-10-19T00:00:00Z',
    };
    mocked.get.mockImplementation(async (path: string) => {
      if (path === '/api/v1/models') return { models: [], managed_models: [] } as never;
      if (path === '/api/v1/chat/conversations/c1') {
        return {
          conversation: { id: 'c1', title: 'Hi', model: 'm', current_leaf_id: 'a1', created_at: '', updated_at: '' },
          messages: [
            { ...base, id: 'u1m', parent_id: '', role: 'user', content: 'something', status: 'complete' },
            {
              ...base,
              id: 'a1',
              parent_id: 'u1m',
              role: 'assistant',
              content: '',
              status: 'blocked',
              error_code: 'policy.security_blocked',
              error_message: "Sorry, that's not something I can help with here.",
            },
          ],
        } as never;
      }
      return { conversations: [], results: [] } as never;
    });
    render(
      <QueryClientProvider client={qc}>
        <SessionContext.Provider value={{ me, config: null, loading: false, error: null, refresh: () => {} }}>
          <MemoryRouter initialEntries={['/chat/c1']}>
            <Routes>
              <Route path="/chat/:id" element={<ChatPage />} />
            </Routes>
          </MemoryRouter>
        </SessionContext.Provider>
      </QueryClientProvider>,
    );
    const note = await screen.findByText("Sorry, that's not something I can help with here.");
    expect(note.className).toContain('chat-blocked');
    expect(note.getAttribute('role')).toBe('status');
    const article = note.closest('article');
    expect(article?.getAttribute('data-status')).toBe('blocked');
    expect(article?.querySelector('.chat-error')).toBeNull();
    expect(article?.textContent).not.toContain('policy.security_blocked');
  });
});

describe('ChatPage personal subscriptions', () => {
  it('lists the caller’s own plan models in their own group and bills them to the plan', async () => {
    mocked.get.mockImplementation(async (path: string) => {
      if (path === '/api/v1/models') {
        return {
          models: [
            { id: 'm1', name: 'grok-4.6', display_name: '', status: 'enabled', modalities: ['chat'], context_window: 8192 },
          ],
          managed_models: [],
          personal_models: [
            {
              id: 'my/xai/grok-4.7',
              owned_by: 'xAI Grok (SuperGrok / X Premium+)',
              janus: { native_name: 'grok-4.7' },
            },
          ],
        } as never;
      }
      return { conversations: [], results: [] } as never;
    });
    renderPage('t1');
    const plan = await screen.findByRole('option', { name: 'grok-4.7 · xAI Grok' });
    expect((plan as HTMLOptionElement).value).toBe('my/xai/grok-4.7');
    expect(plan.closest('optgroup')?.getAttribute('label')).toBe('Your subscriptions');
    const org = screen.getByRole('option', { name: 'grok-4.6' });
    expect(org.closest('optgroup')?.getAttribute('label')).toBe('Working under Household');
    expect(screen.getByLabelText('Billed to').textContent).toContain('Household');

    await userEvent.selectOptions(screen.getByLabelText('Model', { selector: 'select' }), 'my/xai/grok-4.7');
    expect(screen.getByLabelText('Billed to').textContent).toContain('your xAI Grok plan');
  });

  it('offers personal plans even when nothing is granted under the selection', async () => {
    mocked.get.mockImplementation(async (path: string) => {
      if (path === '/api/v1/models') {
        return {
          models: [],
          managed_models: [],
          personal_models: [{ id: 'my/openai/gpt-6.1-sol', owned_by: 'OpenAI ChatGPT', janus: { native_name: 'gpt-6.1-sol' } }],
        } as never;
      }
      return { conversations: [], results: [] } as never;
    });
    renderPage('');
    await screen.findByRole('option', { name: 'gpt-6.1-sol · OpenAI ChatGPT' });
    expect(screen.queryByRole('alert')).toBeNull();
  });
  it('never offers a plan’s video or image model as the chat model', async () => {
    mocked.get.mockImplementation(async (path: string) => {
      if (path === '/api/v1/models') {
        return {
          models: [],
          managed_models: [],
          personal_models: [
            { id: 'my/xai/grok-4.7', owned_by: 'xAI Grok', janus: { native_name: 'grok-4.7', modalities: ['chat'] } },
            {
              id: 'my/xai/grok-imagine-video',
              owned_by: 'xAI Grok',
              janus: { native_name: 'grok-imagine-video', modalities: ['video'] },
            },
            {
              id: 'my/xai/grok-imagine-image',
              owned_by: 'xAI Grok',
              janus: { native_name: 'grok-imagine-image', modalities: ['image'] },
            },
          ],
        } as never;
      }
      return { conversations: [], results: [] } as never;
    });
    renderPage('');
    await screen.findByRole('option', { name: 'grok-4.7 · xAI Grok' });
    expect(screen.queryByRole('option', { name: /grok-imagine-video/ })).toBeNull();
    expect(screen.queryByRole('option', { name: /grok-imagine-image/ })).toBeNull();
  });
});

describe('groupConversations', () => {
  const c = (id: string, updated: string) => ({
    id,
    team_id: '',
    title: id,
    model: '',
    current_leaf_id: '',
    created_at: updated,
    updated_at: updated,
  });
  it('buckets by recency, newest first', () => {
    const now = new Date('2026-10-19T15:00:00');
    const groups = groupConversations(
      [
        c('old', '2026-06-02T10:00:00'),
        c('today', '2026-10-19T09:00:00'),
        c('yesterday', '2026-10-18T22:00:00'),
        c('week', '2026-10-14T10:00:00'),
        c('month', '2026-10-01T10:00:00'),
        c('today2', '2026-10-19T14:00:00'),
      ],
      now,
    );
    expect(groups.map((g) => [g.label, g.items.map((i) => i.id)])).toEqual([
      ['Today', ['today2', 'today']],
      ['Yesterday', ['yesterday']],
      ['Previous 7 days', ['week']],
      ['Previous 30 days', ['month']],
      [new Date('2026-06-02T10:00:00').toLocaleDateString(undefined, { month: 'long', year: 'numeric' }), ['old']],
    ]);
  });
});

describe('temporary chat lifetime', () => {
  it("words the administrator's setting", () => {
    expect(idleLabel(1)).toBe('1 hour');
    expect(idleLabel(6)).toBe('6 hours');
    expect(idleLabel(24)).toBe('a day');
    expect(idleLabel(72)).toBe('3 days');
    expect(idleLabel(720)).toBe('30 days');
  });
});
