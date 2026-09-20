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
  api: { get: vi.fn(), patch: vi.fn() },
}));
const field = (value: number | null, source: string, auto: number | null = value, autoSource = source) => ({
  value,
  source,
  override: source === 'admin' ? value : null,
  automatic_value: auto,
  automatic_source: autoSource,
});
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
    rate_in_nanousd: field(2e9, 'admin', 1e9, 'upstream'),
    rate_out_nanousd: field(3e9, 'reference'),
    rate_cached_nanousd: field(0, 'upstream'),
    rate_cache_write_5m_nanousd: field(null, 'unknown'),
    rate_cache_write_1h_nanousd: field(null, 'unknown'),
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
  vi.mocked(api.get).mockResolvedValue({ models: [model], counts: {} });
  vi.mocked(api.patch).mockResolvedValue({ model });
});
async function open() {
  await userEvent.click(await screen.findByRole('button', { name: 'Edit native-model' }));
  return screen.getByRole('dialog', { name: 'Edit model — alias' });
}
it('replaces table actions with Edit and keeps Disable; shows readonly ID, all fields, sources, unknown vs free and warnings', async () => {
  renderPage();
  const dialog = await open();
  expect(screen.queryByRole('button', { name: 'Rates' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'Rename' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Disable' })).toBeTruthy();
  expect((within(dialog).getByLabelText('Upstream model ID') as HTMLInputElement).readOnly).toBe(true);
  expect((within(dialog).getByLabelText('Context window — tokens') as HTMLInputElement).value).toBe('100');
  expect(within(dialog).getAllByRole('spinbutton')).toHaveLength(6);
  expect((within(dialog).getByLabelText(/^5m cache write —/) as HTMLInputElement).placeholder).toBe('-not set-');
  expect((within(dialog).getByLabelText(/^Cached input —/) as HTMLInputElement).value).toBe('0');
  expect((within(dialog).getByLabelText(/^5m cache write —/) as HTMLInputElement).value).toBe('');
  expect(within(dialog).getAllByText('Admin override')).toHaveLength(2);
  expect(within(dialog).getByText('Reference')).toBeTruthy();
  expect(within(dialog).getByText('unsupported: pricing.input_cache_write')).toBeTruthy();
});
it('resets context independently, previews automatic value and submits only changed fields', async () => {
  renderPage();
  const dialog = await open();
  await userEvent.click(within(dialog).getByRole('button', { name: 'Use automatic for Context window — tokens' }));
  expect((within(dialog).getByLabelText('Context window — tokens') as HTMLInputElement).value).toBe('200');
  expect((within(dialog).getByLabelText(/^Input —/) as HTMLInputElement).value).toBe('2');
  await userEvent.click(within(dialog).getByRole('button', { name: 'Save' }));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('/api/v1/admin/models/m', { context_window: null }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
});
it('resets each rate independently and saves explicit free overrides without converting unknown fields to zero', async () => {
  renderPage();
  const dialog = await open();
  await userEvent.click(within(dialog).getByRole('button', { name: 'Use automatic for Input — USD per million tokens' }));
  fireEvent.change(within(dialog).getByLabelText(/^1h cache write —/), { target: { value: '0' } });
  await userEvent.click(within(dialog).getByRole('button', { name: 'Save' }));
  await waitFor(() =>
    expect(api.patch).toHaveBeenCalledWith('/api/v1/admin/models/m', {
      rate_in_usd_per_mtok: null,
      rate_cache_write_1h_usd_per_mtok: 0,
    }),
  );
});
it('validates integer context and rates, preserves failed edits, and discards cancelled drafts on reopen', async () => {
  renderPage();
  let dialog = await open();
  fireEvent.change(within(dialog).getByLabelText('Context window — tokens'), { target: { value: '1.5' } });
  await userEvent.click(within(dialog).getByRole('button', { name: 'Save' }));
  expect(api.patch).not.toHaveBeenCalled();
  expect(within(dialog).getByRole('alert')).toBeTruthy();
  fireEvent.change(within(dialog).getByLabelText('Context window — tokens'), { target: { value: '300' } });
  vi.mocked(api.patch).mockRejectedValue(new Error('Save failed'));
  await userEvent.click(within(dialog).getByRole('button', { name: 'Save' }));
  expect(await within(dialog).findByText('Save failed')).toBeTruthy();
  expect((within(dialog).getByLabelText('Context window — tokens') as HTMLInputElement).value).toBe('300');
  await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
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
