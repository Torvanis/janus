/**
 * model display names in the UI.
 *
 * Admin side (A04): the Models table carries a per-row inline rename editor —
 * save, cancel, clear-to-reset, keyboard access, and the server's field-level
 * validation error (duplicate alias) surfaced inline without closing the form.
 *
 * User side: the catalog presents the display name as the primary label with
 * the native upstream name as provenance, and search matches either name.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import type { Model } from '../../lib/types';
import { ToastProvider } from '../../components/ui';
import { AdminModelsPage } from './Models';
import { ModelsPage } from '../Models';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

function model(overrides: Partial<Model>): Model {
  return {
    id: 'model-1',
    upstream_id: 'up-1',
    upstream_name: 'OpenAI production',
    adapter_type: 'openai_compatible',
    name: 'gpt-4-turbo-2026-01-01',
    display_name: '',
    status: 'enabled',
    modalities: ['chat'],
    rate_in_nanousd: 10_000,
    rate_out_nanousd: 30_000,
    rate_cached_nanousd: 5_000,
    rate_cache_write_5m_nanousd: 0,
    rate_cache_write_1h_nanousd: 0,
    context_window: 200_000,
    rate_effective_from: '2026-01-02T10:00:00Z',
    discovered_at: '2026-01-02T10:00:00Z',
    grant_count: 2,
    ...overrides,
  };
}

const plainModel = model({});
const renamedModel = model({
  id: 'model-2',
  name: 'anthropic/claude-sonnet-4-20260514',
  display_name: 'Claude Sonnet',
  upstream_id: 'up-2',
  upstream_name: 'Anthropic',
  adapter_type: 'anthropic',
});

function renderWithProviders(ui: React.ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter>{ui}</MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('AdminModelsPage drawer alias', () => {
  beforeEach(() => {
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/admin/models')) {
        return Promise.resolve({
          models: [plainModel, renamedModel],
          counts: { pending_approval: 0 },
          modalities: [{ value: 'chat', label: 'Chat' }],
        });
      }
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
  });

  it('shows the display name as primary with the native name as provenance', async () => {
    renderWithProviders(<AdminModelsPage />);

    expect(await screen.findByText('Claude Sonnet')).toBeTruthy();
    // The renamed row keeps the upstream name visible as secondary text.
    expect(screen.getByText('anthropic/claude-sonnet-4-20260514')).toBeTruthy();
    // A never-renamed model shows only its native name.
    expect(screen.getByText('gpt-4-turbo-2026-01-01')).toBeTruthy();
  });

  it('renames a model in the drawer: Edit → type → Save PATCHes display_name', async () => {
    const user = userEvent.setup();
    mocked.patch.mockResolvedValue(model({ display_name: 'GPT-4 Turbo' }));
    renderWithProviders(<AdminModelsPage />);

    await user.click(await screen.findByRole('button', { name: `Edit ${plainModel.name}` }));
    const input = screen.getByRole('textbox', { name: `Display name for ${plainModel.name}` });
    await user.type(input, 'GPT-4 Turbo');
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() =>
      expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/models/model-1', { display_name: 'GPT-4 Turbo' }),
    );
    // The editor closes after a successful save.
    await waitFor(() => expect(screen.queryByRole('textbox', { name: `Display name for ${plainModel.name}` })).toBeNull());
  });

  it('saves on Enter and trims surrounding whitespace client-side', async () => {
    const user = userEvent.setup();
    mocked.patch.mockResolvedValue(model({ display_name: 'Fable 5' }));
    renderWithProviders(<AdminModelsPage />);

    await user.click(await screen.findByRole('button', { name: `Edit ${plainModel.name}` }));
    await user.type(screen.getByRole('textbox', { name: `Display name for ${plainModel.name}` }), '  Fable 5  {Enter}');

    await waitFor(() => expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/models/model-1', { display_name: 'Fable 5' }));
  });

  it('surfaces a duplicate-name server error inline and keeps the form open', async () => {
    const user = userEvent.setup();
    const message = 'Display name "Claude Sonnet" is already in use by another enabled model.';
    mocked.patch.mockRejectedValue(
      new ApiError(400, { message, code: 'invalid_request_error', type: 'invalid_request_error', param: 'display_name' }),
    );
    renderWithProviders(<AdminModelsPage />);

    await user.click(await screen.findByRole('button', { name: `Edit ${plainModel.name}` }));
    const input = screen.getByRole('textbox', { name: `Display name for ${plainModel.name}` });
    await user.type(input, 'Claude Sonnet{Enter}');

    // The server's field error appears inline (role=alert via the Field error slot)…
    expect(await screen.findByText(message)).toBeTruthy();
    // …and the form stays open for correction; editing clears the error.
    await user.type(input, ' v2');
    expect(screen.queryByText(message)).toBeNull();
  });

  it('Clear empties the draft so saving restores the native name', async () => {
    const user = userEvent.setup();
    mocked.patch.mockResolvedValue(model({ id: 'model-2', display_name: '' }));
    renderWithProviders(<AdminModelsPage />);

    await user.click(await screen.findByRole('button', { name: `Edit ${renamedModel.name}` }));
    const input = screen.getByRole('textbox', { name: `Display name for ${renamedModel.name}` });
    expect((input as HTMLInputElement).value).toBe('Claude Sonnet');

    await user.click(screen.getByRole('button', { name: 'Clear' }));
    expect((input as HTMLInputElement).value).toBe('');

    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/models/model-2', { display_name: '' }));
  });

  it('Escape cancels the editor without saving; unchanged drafts cannot be saved', async () => {
    const user = userEvent.setup();
    renderWithProviders(<AdminModelsPage />);

    await user.click(await screen.findByRole('button', { name: `Edit ${renamedModel.name}` }));
    const input = screen.getByRole('textbox', { name: `Display name for ${renamedModel.name}` });

    // The draft equals the stored alias, so Save is disabled.
    expect((screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement).disabled).toBe(true);

    await user.type(input, '{Escape}');
    await waitFor(() => expect(screen.queryByRole('textbox', { name: `Display name for ${renamedModel.name}` })).toBeNull());
    expect(mocked.patch).not.toHaveBeenCalled();
  });

  it('search matches the display name and the native name alike', async () => {
    const user = userEvent.setup();
    renderWithProviders(<AdminModelsPage />);
    await screen.findByText('Claude Sonnet');

    const search = screen.getByRole('searchbox', { name: 'Search models' });
    await user.type(search, 'sonnet');
    await waitFor(() => expect(screen.queryByText('gpt-4-turbo-2026-01-01')).toBeNull());
    expect(screen.getByText('Claude Sonnet')).toBeTruthy();

    await user.clear(search);
    await user.type(search, 'claude-sonnet-4');
    await waitFor(() => expect(screen.getByText('Claude Sonnet')).toBeTruthy());
  });
});

describe('ModelsPage user catalog', () => {
  beforeEach(() => {
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/models')) {
        return Promise.resolve({ models: [plainModel, renamedModel] });
      }
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
  });

  it('presents the display name as primary with the native name as subtext', async () => {
    renderWithProviders(<ModelsPage />);

    const card = await screen.findByRole('button', { name: 'Claude Sonnet' });
    expect(within(card).getByText('Claude Sonnet')).toBeTruthy();
    expect(within(card).getByText('anthropic/claude-sonnet-4-20260514')).toBeTruthy();
    // A never-renamed model shows no duplicate subtext.
    const plain = screen.getByRole('button', { name: 'gpt-4-turbo-2026-01-01' });
    expect(within(plain).getAllByText('gpt-4-turbo-2026-01-01')).toHaveLength(1);
  });

  it('search matches either the display name or the native name', async () => {
    const user = userEvent.setup();
    renderWithProviders(<ModelsPage />);
    await screen.findByRole('button', { name: 'Claude Sonnet' });

    const search = screen.getByRole('searchbox', { name: 'Search models' });
    await user.type(search, 'Claude Son');
    await waitFor(() => expect(screen.queryByRole('button', { name: 'gpt-4-turbo-2026-01-01' })).toBeNull());
    expect(screen.getByRole('button', { name: 'Claude Sonnet' })).toBeTruthy();

    await user.clear(search);
    await user.type(search, 'claude-sonnet-4-2026');
    await waitFor(() => expect(screen.getByRole('button', { name: 'Claude Sonnet' })).toBeTruthy());
  });
});
