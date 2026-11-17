import { beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import { AdminModelsPage } from './Models';
vi.mock('../../lib/api', async (original) => ({
  ...(await original<typeof import('../../lib/api')>()),
  api: { get: vi.fn(), patch: vi.fn(), post: vi.fn() },
}));
const field = (value: number | null, source: string, auto: number | null = value, autoSource = source) => ({
  value,
  source,
  override: source === 'admin' ? value : null,
  automatic_value: auto,
  automatic_source: autoSource,
});
const catalog = {
  label: 'Janus price catalog 2026.11.16.1',
  kind: 'model',
  url: 'https://developers.openai.com/api/docs/pricing',
  read_at: '2026-11-16',
};
const typeDefault = {
  label: 'Janus price catalog 2026.11.16.1 (default for chat)',
  kind: 'default',
  type: 'chat',
  read_at: '2026-11-14',
};
const model = {
  id: 'm',
  name: 'native-model',
  display_name: 'alias',
  upstream_id: 'u',
  upstream_name: 'Provider',
  adapter_type: 'openai_compatible',
  status: 'enabled',
  modalities: ['chat'],
  grant_count: 0,
  context_window: 100,
  rate_in_nanousd: 2e9,
  rate_out_nanousd: 3e9,
  rate_cached_nanousd: 0,
  rate_cache_write_5m_nanousd: 0,
  rate_cache_write_1h_nanousd: 0,
  rate_effective_from: '2026-09-01T00:00:00Z',
  discovered_at: '2026-09-01T00:00:00Z',
  metadata: {
    context_window: field(100, 'admin', 200, 'upstream'),
    rate_in_nanousd: {
      ...field(2e9, 'admin', 1e9, 'upstream'),
      source_detail: { label: 'Admin override', set_at: '2026-11-10' },
      automatic_detail: { label: 'Provider API', read_at: '2026-11-15' },
    },
    rate_out_nanousd: { ...field(3e9, 'reference'), source_detail: catalog, automatic_detail: catalog },
    rate_cached_nanousd: { ...field(0, 'upstream'), source_detail: { label: 'Provider API', read_at: '2026-11-15' } },
    rate_cache_write_5m_nanousd: field(null, 'unknown'),
    rate_cache_write_1h_nanousd: field(null, 'unknown'),
    'rate_in_nanousd@prompt_over=272000': {
      ...field(20e9, 'reference'),
      source_detail: typeDefault,
      automatic_detail: typeDefault,
    },
    price_request_nanousd: {
      ...field(5e6, 'admin', null, 'unknown'),
      source_detail: { label: 'Admin override', set_at: '2026-11-12' },
    },
  },
  metadata_warnings: ['unsupported: pricing.input_cache_write'],
};
function renderPage() {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}>
      <ToastProvider>
        <MemoryRouter>
          <AdminModelsPage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}
beforeEach(() => {
  cleanup();
  vi.resetAllMocks();
  vi.mocked(api.get).mockImplementation(async (path: string) => {
    if (path.includes('/price-history')) {
      return {
        versions: [
          { effective_from: '2026-11-12T10:00:00Z', ...model, rate_in_nanousd: 2e9, lines: { price_request_nanousd: 5e6 } },
          { effective_from: '2026-09-01T00:00:00Z', ...model, rate_in_nanousd: 1e9 },
        ],
      };
    }
    // The drawer asks what a reset to the price catalog would change.
    if (path.endsWith('/pricing/catalog-preview')) {
      return {
        plan: {
          model_id: 'm',
          model: 'native-model',
          available: true,
          catalog_aligned: false,
          has_admin_prices: true,
          changes: [],
          changed_lines: 0,
          provider_lines: 0,
        },
      };
    }
    return { models: [model], counts: {} };
  });
  vi.mocked(api.patch).mockResolvedValue({ model });
});
async function open() {
  await userEvent.click(await screen.findByRole('button', { name: 'Edit native-model' }));
  return screen.getByRole('dialog', { name: 'Edit model — alias' });
}
function line(dialog: HTMLElement, key: string): HTMLElement {
  const row = dialog.querySelector<HTMLElement>(`[data-line="${key}"]`);
  if (!row) throw new Error(`no line ${key}`);
  return row;
}
const save = (dialog: HTMLElement) => within(dialog).getByRole('button', { name: 'Save' });

it('shows every line as rate per unit with its source and date; the old rate fields are gone', async () => {
  renderPage();
  const dialog = await open();
  expect((within(dialog).getByLabelText('Upstream model ID') as HTMLInputElement).readOnly).toBe(true);
  // Only the context window is a field now; prices are lines.
  expect(within(dialog).getAllByRole('spinbutton')).toHaveLength(1);
  expect(within(dialog).queryByLabelText(/^Input — USD per million tokens/)).toBeNull();

  const list = within(dialog).getByRole('list', { name: 'Price lines' });
  const rows = within(list).getAllByRole('listitem');
  expect(rows.map((r) => r.getAttribute('data-line'))).toEqual([
    'rate_in_nanousd',
    'rate_in_nanousd@prompt_over=272000',
    'rate_cached_nanousd',
    'rate_out_nanousd',
    'price_request_nanousd',
  ]);
  const input = line(dialog, 'rate_in_nanousd');
  expect(input.textContent).toContain('$2.00 per 1M input tokens');
  expect(within(input).getByText('Set by you')).toBeTruthy();
  expect(input.textContent).toContain('Nov 10, 2026');
  const out = line(dialog, 'rate_out_nanousd');
  expect(within(out).getByText('Janus price catalog 2026.11.16.1')).toBeTruthy();
  expect(within(out).getByRole('link', { name: 'price page' }).getAttribute('href')).toBe(catalog.url);
  expect(out.textContent).toContain('Nov 16, 2026');
  const cached = line(dialog, 'rate_cached_nanousd');
  expect(cached.textContent).toContain('Not charged per 1M cached input tokens');
  expect(within(cached).getByText('Reported by Provider')).toBeTruthy();
  const long = line(dialog, 'rate_in_nanousd@prompt_over=272000');
  expect(long.textContent).toContain('$20.00 per 1M input tokens when prompt over 272,000 tokens');
  expect(within(long).getByText('Janus price catalog 2026.11.16.1 (default for chat)')).toBeTruthy();
  // The catalog default is explained once, and unpriced token rates fold.
  expect(within(dialog).getByTestId('defaults-note').textContent).toContain('defaults for Provider chat models');
  expect(within(dialog).getByText(/^Not priced \(2\):/)).toBeTruthy();
  expect(within(dialog).queryByTestId('quote-note')).toBeNull();
  expect(within(dialog).getByText('unsupported: pricing.input_cache_write')).toBeTruthy();
  // Nothing changed: no Save button.
  expect(within(dialog).queryByRole('button', { name: 'Save' })).toBeNull();
});

it('editing a line makes it "Set by you", shows Save only then, and saves token rates and lines in one PATCH', async () => {
  renderPage();
  const dialog = await open();
  await userEvent.click(
    within(line(dialog, 'rate_out_nanousd')).getByRole('button', { name: /^Change the price per 1M output tokens/ }),
  );
  const input = within(dialog).getByLabelText('US dollars per 1M output tokens');
  expect((input as HTMLInputElement).value).toBe('3');
  fireEvent.change(input, { target: { value: '$4.50' } });
  expect(within(line(dialog, 'rate_out_nanousd')).getByText('Set by you')).toBeTruthy();
  expect(line(dialog, 'rate_out_nanousd').textContent).toContain('not saved yet');
  expect(within(dialog).getByRole('status').textContent).toBe('1 unsaved change(s)');
  await userEvent.click(within(line(dialog, 'price_request_nanousd')).getByRole('button', { name: /^Change/ }));
  fireEvent.change(within(dialog).getByLabelText('US dollars per request'), { target: { value: '0.01' } });
  await userEvent.click(save(dialog));
  await waitFor(() =>
    expect(api.patch).toHaveBeenCalledWith('/api/v1/admin/models/m', {
      rate_out_usd_per_mtok: 4.5,
      price_lines: { price_request_nanousd: 0.01 },
    }),
  );
});

it('reset to automatic shows the value it returns to and saves null', async () => {
  renderPage();
  const dialog = await open();
  const row = line(dialog, 'rate_in_nanousd');
  await userEvent.click(within(row).getByRole('button', { name: /^Change/ }));
  expect(row.textContent).toContain('Automatic price: $1.00 (Reported by Provider, Nov 15, 2026).');
  await userEvent.click(within(row).getByRole('button', { name: 'Reset to automatic' }));
  expect(row.textContent).toContain('$1.00 per 1M input tokens');
  expect(within(row).getByText('Reported by Provider')).toBeTruthy();
  await userEvent.click(save(dialog));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('/api/v1/admin/models/m', { rate_in_usd_per_mtok: null }));
});

it('removing an automatic line asks first and keeps it as "Not charged"; removing your own line deletes it', async () => {
  renderPage();
  const dialog = await open();
  const long = line(dialog, 'rate_in_nanousd@prompt_over=272000');
  await userEvent.click(within(long).getByRole('button', { name: /^Change/ }));
  await userEvent.click(within(long).getByRole('button', { name: 'Remove line' }));
  const confirm = within(long).getByRole('group', { name: 'Stop charging for this?' });
  expect(confirm.textContent).toContain('Janus price catalog 2026.11.16.1 (default for chat) prices this at $20.00');
  // Keeping the price changes nothing.
  await userEvent.click(within(confirm).getByRole('button', { name: 'Keep the price' }));
  expect(within(dialog).queryByRole('button', { name: 'Save' })).toBeNull();
  await userEvent.click(within(long).getByRole('button', { name: 'Remove line' }));
  await userEvent.click(within(long).getByRole('button', { name: 'Mark as not charged' }));
  expect(long.textContent).toContain('Not charged per 1M input tokens when prompt over 272,000 tokens');
  expect(within(long).getByText('Set by you')).toBeTruthy();

  const own = line(dialog, 'price_request_nanousd');
  await userEvent.click(within(own).getByRole('button', { name: /^Change/ }));
  await userEvent.click(within(own).getByRole('button', { name: 'Remove line' }));
  expect(own.textContent).toContain('Removed when you save');
  await userEvent.click(save(dialog));
  await waitFor(() =>
    expect(api.patch).toHaveBeenCalledWith('/api/v1/admin/models/m', {
      price_lines: { 'rate_in_nanousd@prompt_over=272000': 0, price_request_nanousd: null },
    }),
  );
});

it('removing your own line can be undone', async () => {
  renderPage();
  const dialog = await open();
  const own = line(dialog, 'price_request_nanousd');
  await userEvent.click(within(own).getByRole('button', { name: /^Change/ }));
  await userEvent.click(within(own).getByRole('button', { name: 'Remove line' }));
  await userEvent.click(within(own).getByRole('button', { name: 'Undo' }));
  expect(own.textContent).toContain('$0.005 per request');
  expect(within(dialog).queryByRole('button', { name: 'Save' })).toBeNull();
});

it('offers the units for the model type first; More units reveals the rest; adds a conditional line', async () => {
  renderPage();
  const dialog = await open();
  await userEvent.click(within(dialog).getByRole('button', { name: '+ Add line' }));
  const add = within(dialog).getByRole('group', { name: 'New price line' });
  const unit = within(add).getByRole('combobox', { name: 'Unit' });
  const options = () =>
    within(unit)
      .getAllByRole('option')
      .map((o) => o.textContent);
  expect(options()).toEqual([
    'Per 1M input tokens',
    'Per 1M cached input tokens',
    'Per 1M tokens written to the 5-minute cache',
    'Per 1M tokens written to the 1-hour cache',
    'Per 1M output tokens',
    'Per request',
  ]);
  await userEvent.click(within(add).getByRole('button', { name: 'More units' }));
  expect(options()).toContain('Per image');
  expect(options()).toContain('Per second of video');
  expect(options()).toContain('Per 1,000 pages');
  // Duplicate of an existing line is refused.
  await userEvent.selectOptions(unit, 'rate_in_nanousd');
  fireEvent.change(within(add).getByLabelText('Only when the prompt is over (tokens)'), { target: { value: '272000' } });
  fireEvent.change(within(add).getByLabelText('US dollars per 1M input tokens'), { target: { value: '5' } });
  await userEvent.click(within(add).getByRole('button', { name: 'Add' }));
  expect(within(add).getByRole('alert').textContent).toContain('already has a line');
  // An image line with conditions.
  await userEvent.selectOptions(unit, 'price_image_nanousd');
  fireEvent.change(within(add).getByLabelText('Quality'), { target: { value: 'High' } });
  fireEvent.change(within(add).getByLabelText('Size'), { target: { value: '1024x1024' } });
  fireEvent.change(within(add).getByLabelText('US dollars per image'), { target: { value: '0.17' } });
  await userEvent.click(within(add).getByRole('button', { name: 'Add' }));
  const image = line(dialog, 'price_image_nanousd@quality=high,size=1024x1024');
  expect(image.textContent).toContain('$0.17 per image when quality high, size 1024x1024');
  expect(within(image).getByText('Set by you')).toBeTruthy();
  // A token rate from the "Not priced" fold.
  await userEvent.click(within(dialog).getByRole('button', { name: '+ Add line' }));
  const again = within(dialog).getByRole('group', { name: 'New price line' });
  await userEvent.selectOptions(within(again).getByRole('combobox', { name: 'Unit' }), 'rate_cache_write_5m_nanousd');
  fireEvent.change(within(again).getByLabelText('US dollars per 1M tokens written to the 5-minute cache'), {
    target: { value: '3.75' },
  });
  await userEvent.click(within(again).getByRole('button', { name: 'Add' }));
  await userEvent.click(save(dialog));
  await waitFor(() =>
    expect(api.patch).toHaveBeenCalledWith('/api/v1/admin/models/m', {
      rate_cache_write_5m_usd_per_mtok: 3.75,
      price_lines: { 'price_image_nanousd@quality=high,size=1024x1024': 0.17 },
    }),
  );
});

it('an image model offers per image first', async () => {
  vi.mocked(api.get).mockResolvedValue({ models: [{ ...model, modalities: ['image'], metadata: {} }], counts: {} });
  renderPage();
  const dialog = await open();
  await userEvent.click(within(dialog).getByRole('button', { name: '+ Add line' }));
  const unit = within(dialog).getByRole('combobox', { name: 'Unit' });
  expect(
    within(unit)
      .getAllByRole('option')
      .map((o) => o.textContent),
  ).toEqual(['Per image', 'Per 1M input tokens', 'Per 1M output tokens', 'Per request']);
  expect(within(dialog).queryByText(/^Not priced \(/)).toBeTruthy();
});

it('validates prices: no negatives, a sensible maximum, at most 9 decimals; Save is blocked until fixed', async () => {
  renderPage();
  const dialog = await open();
  await userEvent.click(within(line(dialog, 'rate_out_nanousd')).getByRole('button', { name: /^Change/ }));
  const input = within(dialog).getByLabelText('US dollars per 1M output tokens');
  fireEvent.change(input, { target: { value: '-1' } });
  expect(within(line(dialog, 'rate_out_nanousd')).getByRole('alert').textContent).toContain('cannot be negative');
  fireEvent.change(input, { target: { value: '10000.01' } });
  expect(within(line(dialog, 'rate_out_nanousd')).getByRole('alert').textContent).toBe('Enter at most $10,000.00.');
  fireEvent.change(input, { target: { value: '0.0000000001' } });
  expect(within(line(dialog, 'rate_out_nanousd')).getByRole('alert').textContent).toBe('Use at most 9 decimal places.');
  await userEvent.click(save(dialog));
  expect(api.patch).not.toHaveBeenCalled();
  expect(within(dialog).getByText('Fix the prices marked below before saving.')).toBeTruthy();
  fireEvent.change(input, { target: { value: '' } });
  await userEvent.click(save(dialog));
  expect(within(line(dialog, 'rate_out_nanousd')).getByRole('alert').textContent).toBe('Enter a price, or 0 for not charged.');
  fireEvent.change(input, { target: { value: '0.000000001' } });
  await userEvent.click(save(dialog));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('/api/v1/admin/models/m', { rate_out_usd_per_mtok: 1e-9 }));
});

it('says when the provider quotes each call, and links to Pricing accuracy', async () => {
  vi.mocked(api.get).mockResolvedValue({ models: [{ ...model, cost_quoted: true }], counts: {} });
  renderPage();
  const dialog = await open();
  const note = within(dialog).getByTestId('quote-note');
  expect(note.textContent).toBe(
    'This provider quotes the cost of each call; these prices are used only for the estimate in Pricing accuracy.',
  );
  expect(within(note).getByRole('link', { name: 'Pricing accuracy' }).getAttribute('href')).toBe(
    '/admin/models/pricing-accuracy',
  );
});

it('lists the price history with what changed', async () => {
  renderPage();
  const dialog = await open();
  await userEvent.click(within(dialog).getByText('Price history'));
  expect(await within(dialog).findByText('per 1M input tokens: $1.00 → $2.00')).toBeTruthy();
  expect(within(dialog).getByText('per request: Not priced → $0.005')).toBeTruthy();
  expect(within(dialog).getByText(/First recorded price/)).toBeTruthy();
});

it('Discard drops every change and hides Save', async () => {
  renderPage();
  const dialog = await open();
  await userEvent.click(within(line(dialog, 'rate_out_nanousd')).getByRole('button', { name: /^Change/ }));
  fireEvent.change(within(dialog).getByLabelText('US dollars per 1M output tokens'), { target: { value: '9' } });
  await userEvent.click(within(dialog).getByRole('button', { name: 'Discard' }));
  expect(within(dialog).queryByRole('button', { name: 'Save' })).toBeNull();
  expect(line(dialog, 'rate_out_nanousd').textContent).toContain('$3.00 per 1M output tokens');
});

it('resets context independently, previews the automatic value and submits only that field', async () => {
  renderPage();
  const dialog = await open();
  await userEvent.click(within(dialog).getByRole('button', { name: 'Use automatic for Context window — tokens' }));
  expect((within(dialog).getByLabelText('Context window — tokens') as HTMLInputElement).value).toBe('200');
  await userEvent.click(save(dialog));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('/api/v1/admin/models/m', { context_window: null }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
});

it('validates integer context, preserves failed edits, and discards canceled drafts on reopen', async () => {
  renderPage();
  let dialog = await open();
  fireEvent.change(within(dialog).getByLabelText('Context window — tokens'), { target: { value: '1.5' } });
  await userEvent.click(save(dialog));
  expect(api.patch).not.toHaveBeenCalled();
  expect(within(dialog).getByRole('alert')).toBeTruthy();
  fireEvent.change(within(dialog).getByLabelText('Context window — tokens'), { target: { value: '300' } });
  vi.mocked(api.patch).mockRejectedValue(new Error('Save failed'));
  await userEvent.click(save(dialog));
  expect(await within(dialog).findByText('Save failed')).toBeTruthy();
  expect((within(dialog).getByLabelText('Context window — tokens') as HTMLInputElement).value).toBe('300');
  await userEvent.click(within(dialog).getByRole('button', { name: 'Close panel' }));
  dialog = await open();
  expect((within(dialog).getByLabelText('Context window — tokens') as HTMLInputElement).value).toBe('100');
});

it('shows missing and free prices distinctly in the Models table', async () => {
  vi.mocked(api.get).mockResolvedValue({
    models: [
      {
        ...model,
        rate_in_nanousd: 0,
        rate_out_nanousd: 0,
        metadata: { ...model.metadata, rate_in_nanousd: field(0, 'upstream'), rate_out_nanousd: field(null, 'unknown') },
      },
    ],
    counts: {},
  });
  renderPage();
  const row = (await screen.findByRole('button', { name: 'Edit native-model' })).closest('tr')!;
  expect(within(row).getByText('-not set-')).toBeTruthy();
  expect(within(row).getByText(/\$0\.00.*Mtok/)).toBeTruthy();
});
