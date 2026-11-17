import { StrictMode } from 'react';
import { beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import { AdminModelsPage } from './Models';
import type { CatalogResetPlan } from '../../api/client';

vi.mock('../../lib/api', async (original) => ({
  ...(await original<typeof import('../../lib/api')>()),
  api: { get: vi.fn(), patch: vi.fn(), post: vi.fn() },
}));

const catalog = { label: 'Janus price catalog 2026.11.16.1', kind: 'model', read_at: '2026-11-16' };
const field = (value: number | null, source: string, override: number | null = null) => ({
  value,
  source,
  override,
  automatic_value: source === 'admin' ? null : value,
  automatic_source: source === 'admin' ? 'unknown' : source,
  source_detail: source === 'reference' ? catalog : undefined,
});
const base = {
  upstream_id: 'u',
  upstream_name: 'xAI',
  adapter_type: 'openai_compatible',
  status: 'enabled',
  modalities: ['chat'],
  grant_count: 0,
  context_window: 100,
  rate_out_nanousd: 0,
  rate_cached_nanousd: 0,
  rate_cache_write_5m_nanousd: 0,
  rate_cache_write_1h_nanousd: 0,
  rate_effective_from: '2026-09-01T00:00:00Z',
  discovered_at: '2026-09-01T00:00:00Z',
};
// Aligned; the provider's API prices its input tokens.
const aligned = {
  ...base,
  id: 'a',
  name: 'grok-aligned',
  display_name: '',
  catalog_aligned: true,
  rate_in_nanousd: 1e9,
  metadata: { rate_in_nanousd: field(1e9, 'upstream'), rate_out_nanousd: field(15e9, 'reference') },
};
// Custom: legacy $0 rates set by an administrator, and an admin-only line.
const custom = {
  ...base,
  id: 'c',
  name: 'claude-custom',
  display_name: '',
  catalog_aligned: false,
  rate_in_nanousd: 0,
  metadata: {
    rate_in_nanousd: field(0, 'admin', 0),
    rate_out_nanousd: field(0, 'admin', 0),
    price_request_nanousd: field(5e6, 'admin', 5e6),
  },
};
const customPlan: CatalogResetPlan = {
  model_id: 'c',
  model: 'claude-custom',
  available: true,
  catalog: 'Janus price catalog 2026.11.16.1',
  catalog_aligned: false,
  has_admin_prices: true,
  changed_lines: 3,
  provider_lines: 0,
  changes: [
    { line: 'price_request_nanousd', before: 5e6, before_source: 'admin', after: null, after_source: 'unknown', changed: true },
    { line: 'rate_in_nanousd', before: 0, before_source: 'admin', after: 3e9, after_source: 'reference', changed: true },
    { line: 'rate_out_nanousd', before: 0, before_source: 'admin', after: 15e9, after_source: 'reference', changed: true },
    {
      line: 'rate_cached_nanousd',
      before: 3e8,
      before_source: 'reference',
      after: 3e8,
      after_source: 'reference',
      changed: false,
    },
  ],
};
let plans: Record<string, CatalogResetPlan>;

function renderPage() {
  // StrictMode as in main.tsx: its doubled effects must not double requests.
  return render(
    <StrictMode>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}
      >
        <ToastProvider>
          <MemoryRouter>
            <AdminModelsPage />
          </MemoryRouter>
        </ToastProvider>
      </QueryClientProvider>
    </StrictMode>,
  );
}

beforeEach(() => {
  cleanup();
  vi.resetAllMocks();
  plans = {
    a: {
      ...customPlan,
      model_id: 'a',
      model: 'grok-aligned',
      catalog_aligned: true,
      has_admin_prices: false,
      changed_lines: 0,
      changes: [],
    },
    c: customPlan,
  };
  vi.mocked(api.get).mockImplementation(async (path: string) => {
    if (path.includes('/price-history')) return { versions: [] };
    if (path.endsWith('/pricing/catalog-preview')) return { plan: plans[path.split('/')[5] ?? ''] };
    return { models: [aligned, custom], counts: {} };
  });
  vi.mocked(api.post).mockImplementation(async (path: string) => {
    const bulk =
      path === '/api/v1/admin/models/pricing/catalog-preview' || path === '/api/v1/admin/models/pricing/reset-to-catalog';
    if (bulk) {
      const preview = path.endsWith('/catalog-preview');
      return {
        applied: !preview,
        counts: { [preview ? 'would_reset' : 'reset']: 1, skipped: 1 },
        results: [
          { model_id: 'a', model: 'grok-aligned', outcome: preview ? 'would_reset' : 'reset', changed_lines: 0 },
          { model_id: 'c', model: 'claude-custom', outcome: 'skipped', reason: 'prices_would_change', changed_lines: 3 },
        ],
      };
    }
    const id = path.split('/')[5] ?? '';
    return { plan: plans[id], applied: true, model: { ...custom, catalog_aligned: true, metadata: {} } };
  });
  vi.mocked(api.patch).mockImplementation(async (_path: string, body?: unknown) => ({
    model: { ...aligned, ...(body as object) },
  }));
});

async function open(name: string) {
  await userEvent.click(await screen.findByRole('button', { name: `Edit ${name}` }));
  return screen.getByRole('dialog', { name: `Edit model — ${name}` });
}

const previewCalls = (id: string) =>
  vi.mocked(api.get).mock.calls.filter(([path]) => path === `/api/v1/admin/models/${id}/pricing/catalog-preview`);

it('opening the drawer asks for one read-only preview and writes nothing', async () => {
  renderPage();
  const dialog = await open('claude-custom');
  await waitFor(() =>
    expect((within(dialog).getByRole('button', { name: 'Reset to Janus price catalog' }) as HTMLButtonElement).disabled).toBe(
      false,
    ),
  );
  expect(previewCalls('c')).toHaveLength(1);
  expect(api.post).not.toHaveBeenCalled();
  expect(api.patch).not.toHaveBeenCalled();
  expect(within(dialog).queryByText('Working out the prices…')).toBeNull();
});

it('a failed preview shows its reason and Try again instead of working forever', async () => {
  let fail = true;
  vi.mocked(api.get).mockImplementation(async (path: string) => {
    if (path.includes('/price-history')) return { versions: [] };
    if (path.endsWith('/pricing/catalog-preview')) {
      if (fail)
        throw new ApiError(500, { message: 'The price catalog could not be read.', code: 'server_error', type: 'server_error' });
      return { plan: customPlan };
    }
    return { models: [aligned, custom], counts: {} };
  });
  renderPage();
  const dialog = await open('claude-custom');
  const error = await within(dialog).findByTestId('catalog-preview-error');
  expect(error.getAttribute('role')).toBe('alert');
  expect(error.textContent).toContain('Could not work out the catalog prices: The price catalog could not be read.');
  expect(within(dialog).queryByText('Working out the prices…')).toBeNull();
  // Not retried behind the user's back: one request until they ask again.
  expect(previewCalls('c')).toHaveLength(1);
  expect((within(dialog).getByRole('button', { name: 'Reset to Janus price catalog' }) as HTMLButtonElement).disabled).toBe(true);
  fail = false;
  await userEvent.click(within(error).getByRole('button', { name: 'Try again' }));
  await waitFor(() =>
    expect((within(dialog).getByRole('button', { name: 'Reset to Janus price catalog' }) as HTMLButtonElement).disabled).toBe(
      false,
    ),
  );
  expect(previewCalls('c')).toHaveLength(2);
  expect(within(dialog).queryByTestId('catalog-preview-error')).toBeNull();
  expect(api.post).not.toHaveBeenCalled();
});

it('an unreachable gateway does not promise an automatic retry', async () => {
  vi.mocked(api.get).mockImplementation(async (path: string) => {
    if (path.includes('/price-history')) return { versions: [] };
    if (path.endsWith('/pricing/catalog-preview')) {
      throw new ApiError(0, {
        message: 'The gateway could not be reached. Check your connection — Janus will retry automatically.',
        code: 'network_error',
        type: 'server_error',
      });
    }
    return { models: [aligned, custom], counts: {} };
  });
  renderPage();
  const dialog = await open('claude-custom');
  const error = await within(dialog).findByTestId('catalog-preview-error');
  expect(error.textContent).toContain('Could not work out the catalog prices: the gateway could not be reached.');
  expect(error.textContent).not.toContain('retry automatically');
});

it('marks each model in the list as aligned or with custom prices', async () => {
  renderPage();
  const row = (await screen.findByText('grok-aligned')).closest('tr')!;
  expect(within(row).getByText('Aligned')).toBeTruthy();
  const other = screen.getByText('claude-custom').closest('tr')!;
  expect(within(other).getByText('Custom prices')).toBeTruthy();
});

it('says when the provider’s own API price applies to an aligned model, and asks before an edit stops alignment', async () => {
  renderPage();
  const dialog = await open('grok-aligned');
  const toggle = within(dialog).getByRole('switch', { name: 'Keep aligned to Janus price catalog' }) as HTMLInputElement;
  expect(toggle.checked).toBe(true);
  expect(within(dialog).getByTestId('provider-price-note').textContent).toContain('xAI publishes its own prices through its API');
  await userEvent.click(within(dialog).getByRole('button', { name: 'Change the price per 1M output tokens' }));
  fireEvent.change(within(dialog).getByLabelText('US dollars per 1M output tokens'), { target: { value: '2' } });
  expect(within(dialog).getByText(/Saving these changes stops following/)).toBeTruthy();
  await userEvent.click(within(dialog).getByRole('button', { name: 'Save' }));
  const ask = await screen.findByRole('dialog', { name: 'Stop following the Janus price catalog?' });
  expect(ask.textContent).toContain('This stops following the Janus price catalog for this model');
  expect(api.patch).not.toHaveBeenCalled();
  await userEvent.click(within(ask).getByRole('button', { name: 'Save and stop following' }));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('/api/v1/admin/models/a', { rate_out_usd_per_mtok: 2 }));
});

it('turning the switch off saves at once and changes no price', async () => {
  renderPage();
  const dialog = await open('grok-aligned');
  await userEvent.click(within(dialog).getByRole('switch', { name: 'Keep aligned to Janus price catalog' }));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('/api/v1/admin/models/a', { catalog_aligned: false }));
});

it('resets to the catalog after showing every line before and after', async () => {
  renderPage();
  const dialog = await open('claude-custom');
  const toggle = within(dialog).getByRole('switch', { name: 'Keep aligned to Janus price catalog' }) as HTMLInputElement;
  expect(toggle.checked).toBe(false);
  await waitFor(() =>
    expect((within(dialog).getByRole('button', { name: 'Reset to Janus price catalog' }) as HTMLButtonElement).disabled).toBe(
      false,
    ),
  );
  await userEvent.click(within(dialog).getByRole('button', { name: 'Reset to Janus price catalog' }));
  const confirm = await screen.findByRole('dialog', { name: 'Reset to Janus price catalog?' });
  const list = within(confirm).getByRole('list', { name: 'Price changes' });
  const items = within(list)
    .getAllByRole('listitem')
    .map((li) => li.textContent);
  expect(items).toEqual([
    'Per 1M input tokensNot charged → $3.00',
    'Per 1M output tokensNot charged → $15.00',
    'Per request$0.005 → Removed',
  ]);
  expect(confirm.textContent).toContain('1 other line(s) stay the same.');
  expect(confirm.textContent).toContain('Janus price catalog 2026.11.16.1');
  await userEvent.click(within(confirm).getByRole('button', { name: 'Reset prices' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/admin/models/c/pricing/reset-to-catalog'));
  // The drawer stays open on the updated model.
  await waitFor(() =>
    expect(
      (within(dialog).getByRole('switch', { name: 'Keep aligned to Janus price catalog' }) as HTMLInputElement).checked,
    ).toBe(true),
  );
});

it('turning the switch on is the same confirmed reset', async () => {
  renderPage();
  const dialog = await open('claude-custom');
  const toggle = within(dialog).getByRole('switch', { name: 'Keep aligned to Janus price catalog' }) as HTMLInputElement;
  await waitFor(() => expect(toggle.disabled).toBe(false));
  await userEvent.click(toggle);
  const confirm = await screen.findByRole('dialog', { name: 'Keep aligned to Janus price catalog?' });
  await userEvent.click(within(confirm).getByRole('button', { name: 'Keep aligned' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/admin/models/c/pricing/reset-to-catalog'));
});

it('disables the reset and says why when the catalog has nothing for the model', async () => {
  plans.c = {
    ...customPlan,
    available: false,
    reason: 'no_provider',
    message: 'The Janus price catalog lists prices for providers’ own APIs only',
    changes: [],
    changed_lines: 0,
  };
  renderPage();
  const dialog = await open('claude-custom');
  const reason = await within(dialog).findByTestId('catalog-reset-reason');
  expect(reason.textContent).toBe('Not available. The Janus price catalog lists prices for providers’ own APIs only.');
  expect((within(dialog).getByRole('button', { name: 'Reset to Janus price catalog' }) as HTMLButtonElement).disabled).toBe(true);
  expect((within(dialog).getByRole('switch', { name: 'Keep aligned to Janus price catalog' }) as HTMLInputElement).disabled).toBe(
    true,
  );
});

it('keeps selected models aligned only where no price changes', async () => {
  renderPage();
  await userEvent.click(await screen.findByRole('checkbox', { name: 'Select grok-aligned' }));
  await userEvent.click(screen.getByRole('checkbox', { name: 'Select claude-custom' }));
  await userEvent.click(screen.getByRole('button', { name: 'Keep aligned' }));
  const confirm = await screen.findByRole('dialog', { name: 'Keep 2 model(s) aligned to the Janus price catalog?' });
  await within(confirm).findByTestId('catalog-bulk-plan');
  // The preview is its own read-only endpoint; nothing went to the reset yet.
  expect(api.post).toHaveBeenCalledTimes(1);
  expect(api.post).toHaveBeenCalledWith('/api/v1/admin/models/pricing/catalog-preview', {
    ids: expect.arrayContaining(['a', 'c']),
    only_if_unchanged: true,
  });
  expect(confirm.textContent).toContain('1 model(s) will be kept aligned.');
  expect(confirm.textContent).toContain('its prices would change, so reset it instead');
  await userEvent.click(within(confirm).getByRole('button', { name: 'Keep aligned' }));
  await waitFor(() =>
    expect(api.post).toHaveBeenCalledWith('/api/v1/admin/models/pricing/reset-to-catalog', {
      ids: expect.arrayContaining(['a', 'c']),
      only_if_unchanged: true,
    }),
  );
});
