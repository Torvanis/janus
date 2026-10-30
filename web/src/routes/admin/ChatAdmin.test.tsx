import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { SessionContext } from '../../app/session';
import { ToastProvider } from '../../components/ui';
import { ChatAdminPage, type ChatAdminDoc } from './ChatAdmin';
import { GrantsPage } from './Grants';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

const baseDoc: ChatAdminDoc = {
  enabled: true,
  access: { mode: 'everyone', groups: [], teams: [], users: [], message: '' },
  default_model: '',
  temporary_ttl_hours: 24,
  shares_enabled: true,
  export_enabled: true,
  personal_plans: true,
  org_instructions: '',
  starter_prompts: [],
  models: {},
  access_licensed: false,
  preview_on: true,
};

const grant = {
  id: 'g1',
  model_id: 'm1',
  model_kind: 'model',
  model_name: 'qwen-27b',
  upstream_name: 'local',
  grantee_type: 'all_users',
  grantee_id: '',
  grantee_name: 'All users',
  created_at: '2026-10-20T00:00:00Z',
  chat_name: '',
  chat_description: '',
};

function mockGets(doc: ChatAdminDoc) {
  mocked.get.mockImplementation(async (path: string) => {
    if (path === '/api/v1/admin/chat') return doc as never;
    if (path === '/api/v1/admin/models')
      return {
        models: [
          { id: 'm1', name: 'qwen-27b', display_name: '', status: 'enabled', modalities: ['chat'], upstream_name: 'local' },
        ],
      } as never;
    if (path === '/api/v1/admin/managed-models') return { managed_models: [] } as never;
    if (path.startsWith('/api/v1/admin/grants')) return { grants: [grant] } as never;
    if (path.startsWith('/api/v1/admin/groups')) return { groups: [] } as never;
    throw new Error(`unexpected GET ${path}`);
  });
}

function renderAt(path: string, edition = 'community') {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const me = { id: 'a', role: 'admin', teams: [], license: { edition, features: [], restricted: false } } as never;
  return render(
    <QueryClientProvider client={qc}>
      <SessionContext.Provider value={{ me, config: null, loading: false, error: null, refresh: () => {} }}>
        <ToastProvider>
          <MemoryRouter initialEntries={[path]}>
            <Routes>
              <Route path="/admin/chat" element={<ChatAdminPage />} />
              <Route path="/admin/chat/:tab" element={<ChatAdminPage />} />
              <Route path="/admin/grants" element={<GrantsPage />} />
            </Routes>
          </MemoryRouter>
        </ToastProvider>
      </SessionContext.Provider>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('Administration → Chat', () => {
  it('saves general settings: off switch, instructions and suggestions', async () => {
    mockGets(baseDoc);
    mocked.put.mockImplementation(async (_p: string, body: unknown) => ({ ...(body as object) }) as never);
    const user = userEvent.setup();
    renderAt('/admin/chat');
    const save = await screen.findByRole('button', { name: 'Save' });
    expect((save as HTMLButtonElement).disabled).toBe(true);
    await user.click(screen.getByLabelText('Janus Chat is on'));
    await user.type(screen.getByLabelText('Instructions for every chat'), 'Be brief.');
    await user.type(screen.getByLabelText('Suggestion 1'), 'Summarize a document');
    await user.click(screen.getByLabelText('Export'));
    await user.click(save);
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    const body = mocked.put.mock.calls[0]?.[1] as ChatAdminDoc;
    expect(body.enabled).toBe(false);
    expect(body.org_instructions).toBe('Be brief.');
    expect(body.starter_prompts).toEqual(['Summarize a document']);
    expect(body.export_enabled).toBe(false);
  });

  it('tools are on by default; turning one off saves it in tools_off', async () => {
    mockGets(baseDoc);
    mocked.put.mockImplementation(async (_p: string, body: unknown) => body as never);
    const user = userEvent.setup();
    renderAt('/admin/chat/tools');
    const web = (await screen.findByLabelText('Web search')) as HTMLInputElement;
    expect(web.checked).toBe(true);
    expect((screen.getByLabelText('Let chat models make images') as HTMLInputElement).checked).toBe(true);
    await user.click(screen.getByLabelText('X search'));
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    expect((mocked.put.mock.calls[0]?.[1] as ChatAdminDoc).tools_off).toEqual(['x_search']);
  });

  it('web search for other models: off by default, key links, key never shown, test first', async () => {
    mockGets({
      ...baseDoc,
      search: { enabled: false, provider: '', url: '', max_results: 5 },
      search_keys_set: { brave: false },
    });
    mocked.put.mockImplementation(async (_p: string, body: unknown) => body as never);
    mocked.post.mockResolvedValue({
      ok: true,
      ms: 312,
      results: [{ title: 'Talks resume', url: 'https://apnews.com/a', snippet: 's' }],
    } as never);
    const user = userEvent.setup();
    renderAt('/admin/chat/tools');
    expect(await screen.findByText('Choose a service to see how to set it up.')).toBeTruthy();
    await user.click(screen.getByLabelText(/Brave Search/));
    const useIt = screen.getByLabelText('Use this search for models without their own') as HTMLInputElement;
    expect(useIt.checked).toBe(false);
    const link = screen.getByRole('link', { name: /Get a Brave Search key/ });
    expect(link.getAttribute('href')).toBe('https://api-dashboard.search.brave.com/app/keys');
    expect(link.getAttribute('rel')).toContain('noopener');
    expect((screen.getByRole('button', { name: 'Test search' }) as HTMLButtonElement).disabled).toBe(true);
    const keyInput = screen.getByLabelText('API key') as HTMLInputElement;
    expect(keyInput.type).toBe('password');
    await user.type(keyInput, 'BSA-123');
    await user.click(screen.getByRole('button', { name: 'Test search' }));
    expect(await screen.findByText(/Works — 1 results in 312 ms/)).toBeTruthy();
    expect(mocked.post).toHaveBeenCalledWith(
      '/api/v1/admin/chat/search-test',
      expect.objectContaining({ provider: 'brave', key: 'BSA-123' }),
    );
    await user.click(useIt);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    const sent = mocked.put.mock.calls[0]?.[1] as ChatAdminDoc;
    expect(sent.search).toMatchObject({ enabled: true, provider: 'brave' });
    expect(sent.search_key).toBe('BSA-123');
  });

  it('curates models: hide, category, recommended', async () => {
    mockGets(baseDoc);
    mocked.put.mockImplementation(async (_p: string, body: unknown) => body as never);
    const user = userEvent.setup();
    renderAt('/admin/chat/models');
    const table = await screen.findByRole('table', { name: 'Chat models' });
    await within(table).findByText('qwen-27b');
    await user.selectOptions(within(table).getByLabelText('Category for qwen-27b'), 'local');
    await user.click(within(table).getByLabelText('Recommend qwen-27b'));
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    expect((mocked.put.mock.calls[0]?.[1] as ChatAdminDoc).models).toEqual({
      'qwen-27b': { category: 'local', recommended: true },
    });
  });

  it('shows "who can use Chat" as Business on Community, disabled, with no save', async () => {
    mockGets(baseDoc);
    renderAt('/admin/chat/access');
    expect(await screen.findByText(/part of Janus Business/)).toBeTruthy();
    const restricted = screen.getByRole('radio', { name: /Only these groups/ });
    expect(restricted.closest('fieldset')?.disabled).toBe(true);
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
  });

  it('after a downgrade, a saved restriction is shown as still applying and can be lifted', async () => {
    mockGets({ ...baseDoc, access: { mode: 'restricted', groups: ['g1'], teams: [], users: [], message: '' } });
    mocked.put.mockResolvedValue(baseDoc as never);
    const user = userEvent.setup();
    renderAt('/admin/chat/access');
    expect(await screen.findByText(/still applies/)).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Let everyone use Chat' }));
    await waitFor(() =>
      expect(mocked.put).toHaveBeenCalledWith(
        '/api/v1/admin/chat',
        expect.objectContaining({ access: expect.objectContaining({ mode: 'everyone' }) }),
      ),
    );
  });

  it('on Business, a restriction needs someone before it can be saved', async () => {
    mockGets({ ...baseDoc, access_licensed: true });
    const user = userEvent.setup();
    renderAt('/admin/chat/access', 'business');
    await user.click(await screen.findByRole('radio', { name: /Only these groups/ }));
    expect(screen.getByText('Choose at least one group, team or person.')).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement).disabled).toBe(true);
  });
});

describe('grant chat label', () => {
  it('edits a grant’s chat name and description from Grants', async () => {
    mockGets(baseDoc);
    mocked.patch.mockResolvedValue({ grant: { ...grant, chat_name: 'Fast & private' } } as never);
    const user = userEvent.setup();
    renderAt('/admin/grants');
    const group = await screen.findByRole('button', { name: /qwen-27b/ });
    if (group.getAttribute('aria-expanded') === 'false') await user.click(group);
    await user.click((await screen.findAllByRole('button', { name: 'Chat name and description' }))[0]!);
    await user.type(screen.getByLabelText('Chat name'), 'Fast & private');
    await user.type(screen.getByLabelText('Description'), 'Runs on our own servers.');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() =>
      expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/grants/g1/chat', {
        chat_name: 'Fast & private',
        chat_description: 'Runs on our own servers.',
      }),
    );
  });
});
