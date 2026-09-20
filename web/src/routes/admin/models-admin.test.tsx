/**
 * A04 Models admin — rate-card dialog with all five billing dimensions.
 *
 * The dialog gains "5m cache write" and "1h cache write" fields (prompt-cache
 * write pricing). The PATCH body carries the canonical snake_case names from
 * the Go handler (rate_cache_write_{5m,1h}_usd_per_mtok); $0 and $50/MTok are
 * both valid (no client-side ceiling), and an explicitly $0-priced model may
 * be enabled while a never-priced one may not.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { Model } from '../../lib/types';
import { ToastProvider } from '../../components/ui';
import { AdminModelsPage } from './Models';
import { RateCardDialog } from './RateCardDialog';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

function modelFixture(overrides: Partial<Model> = {}): Model {
  return {
    id: 'm-1',
    upstream_id: 'up-1',
    upstream_name: 'Anthropic',
    adapter_type: 'anthropic',
    name: 'claude-fable-5',
    display_name: '',
    status: 'disabled',
    modalities: ['chat'],
    rate_in_nanousd: 0,
    rate_out_nanousd: 0,
    rate_cached_nanousd: 0,
    rate_cache_write_5m_nanousd: 0,
    rate_cache_write_1h_nanousd: 0,
    context_window: 0,
    rate_effective_from: '0001-01-01T00:00:00Z',
    discovered_at: '2026-08-01T00:00:00Z',
    grant_count: 0,
    ...overrides,
  };
}

let rateModel: Model;
function mockModels(models: Model[]) {
  rateModel = models[0]!;
  mocked.get.mockImplementation((path: string) => {
    if (path.startsWith('/api/v1/admin/models')) {
      return Promise.resolve({ models, counts: {}, modalities: [] });
    }
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
}

function renderPage(ui = <AdminModelsPage />) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={['/admin/models']}>{ui}</MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

async function openDialog(): Promise<HTMLElement> {
  return screen.getByRole('dialog', { name: /Rate card — claude-fable-5/ });
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocked.patch.mockResolvedValue({ model: modelFixture() });
});

describe('A04 rate-card dialog — five billing dimensions', () => {
  it('renders all five rate fields with labels and helper text', async () => {
    mockModels([modelFixture()]);
    renderPage(<RateCardDialog model={rateModel} onClose={() => undefined} onSaved={() => undefined} />);
    const dialog = await openDialog();

    const labels = [
      /^Input — USD per million tokens/,
      /^Output — USD per million tokens/,
      /^Cached input — USD per million tokens/,
      /^5m cache write — USD per million tokens/,
      /^1h cache write — USD per million tokens/,
    ];
    for (const label of labels) {
      const input = within(dialog).getByLabelText(label) as HTMLInputElement;
      expect(input.type).toBe('number');
      expect(input.min).toBe('0');
      expect(input.max).toBe(''); // no client-side ceiling
      expect(input.className).toContain('input'); // token-styled control (focus ring, 44px target)
    }
    // Every dimension explains what it measures and which providers bill it.
    expect(within(dialog).getByText(/Every provider bills this; the sheets call it/)).toBeTruthy();
    expect(within(dialog).getByText(/5-minute lifetime/)).toBeTruthy();
    expect(within(dialog).getByText(/1-hour lifetime/)).toBeTruthy();
    // Universal vs optional dimensions are grouped and named as such.
    expect(within(dialog).getByRole('heading', { name: 'Billed by every provider' })).toBeTruthy();
    expect(within(dialog).getByRole('heading', { name: 'Optional — provider-specific' })).toBeTruthy();
    // The fixture is an Anthropic model: all three optional dimensions are
    // flagged as billed, and the guide maps Anthropic's sheet one-to-one.
    expect(within(dialog).getAllByText('Billed by Anthropic')).toHaveLength(3);
    expect(within(dialog).queryByText(/Typically \$0/)).toBeNull();
    expect(within(dialog).getByTestId('rate-provider-guide').textContent).toMatch(/Anthropic’s sheet maps one-to-one/);
    // Versioning note unchanged.
    expect(within(dialog).getByText(/future requests only/)).toBeTruthy();
  });

  it('marks dimensions the provider does not bill as typically $0 (OpenAI-compatible)', async () => {
    mockModels([modelFixture({ adapter_type: 'openai_compatible' })]);
    renderPage(<RateCardDialog model={rateModel} onClose={() => undefined} onSaved={() => undefined} />);
    const dialog = await openDialog();

    expect(within(dialog).getAllByText('Typically $0 for OpenAI-compatible providers')).toHaveLength(2);
    expect(within(dialog).getAllByText('Billed by OpenAI-compatible providers')).toHaveLength(1);
    expect(within(dialog).getByTestId('rate-provider-guide').textContent).toMatch(/cache writes are free/);
  });

  it('saves an explicit $0/$0 card — payload carries all five snake_case fields as 0', async () => {
    mockModels([modelFixture()]);
    renderPage(<RateCardDialog model={rateModel} onClose={() => undefined} onSaved={() => undefined} />);
    const dialog = await openDialog();

    await userEvent.setup().click(within(dialog).getByRole('button', { name: 'Save rate card' }));

    await waitFor(() =>
      expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/models/m-1', {
        rate_in_usd_per_mtok: 0,
        rate_out_usd_per_mtok: 0,
        rate_cached_usd_per_mtok: 0,
        rate_cache_write_5m_usd_per_mtok: 0,
        rate_cache_write_1h_usd_per_mtok: 0,
      }),
    );
    expect(await screen.findByText(/Rate card saved/)).toBeTruthy();
  });

  it('accepts large values — $50/MTok output and 1h cache write submit without a ceiling', async () => {
    mockModels([modelFixture()]);
    renderPage(<RateCardDialog model={rateModel} onClose={() => undefined} onSaved={() => undefined} />);
    const dialog = await openDialog();

    fireEvent.change(within(dialog).getByLabelText(/^Input — USD/), { target: { value: '10' } });
    fireEvent.change(within(dialog).getByLabelText(/^Output — USD/), { target: { value: '50' } });
    fireEvent.change(within(dialog).getByLabelText(/^Cached input — USD/), { target: { value: '1' } });
    fireEvent.change(within(dialog).getByLabelText(/^5m cache write — USD/), { target: { value: '12.5' } });
    fireEvent.change(within(dialog).getByLabelText(/^1h cache write — USD/), { target: { value: '50' } });
    await userEvent.setup().click(within(dialog).getByRole('button', { name: 'Save rate card' }));

    await waitFor(() =>
      expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/models/m-1', {
        rate_in_usd_per_mtok: 10,
        rate_out_usd_per_mtok: 50,
        rate_cached_usd_per_mtok: 1,
        rate_cache_write_5m_usd_per_mtok: 12.5,
        rate_cache_write_1h_usd_per_mtok: 50,
      }),
    );
  });

  it('rejects a negative cache-write rate with an inline error and no request', async () => {
    mockModels([modelFixture()]);
    renderPage(<RateCardDialog model={rateModel} onClose={() => undefined} onSaved={() => undefined} />);
    const dialog = await openDialog();

    fireEvent.change(within(dialog).getByLabelText(/^5m cache write — USD/), { target: { value: '-1' } });
    await userEvent.setup().click(within(dialog).getByRole('button', { name: 'Save rate card' }));

    const alert = await within(dialog).findByRole('alert');
    expect(alert.textContent).toContain('zero or higher');
    expect(mocked.patch).not.toHaveBeenCalled();
  });

  it('rejects a non-numeric rate with an inline error and no request', async () => {
    mockModels([modelFixture()]);
    renderPage(<RateCardDialog model={rateModel} onClose={() => undefined} onSaved={() => undefined} />);
    const dialog = await openDialog();

    fireEvent.change(within(dialog).getByLabelText(/^1h cache write — USD/), { target: { value: '' } });
    await userEvent.setup().click(within(dialog).getByRole('button', { name: 'Save rate card' }));

    const alert = await within(dialog).findByRole('alert');
    expect(alert.textContent).toContain('Enter a number');
    expect(mocked.patch).not.toHaveBeenCalled();
  });
});

describe('A04 enable guard — explicit $0 card vs never priced', () => {
  it('enables a model that was explicitly priced at $0', async () => {
    mockModels([modelFixture({ rate_effective_from: '2026-08-23T00:00:00Z' })]);
    renderPage();

    await userEvent.setup().click(await screen.findByRole('button', { name: 'Enable' }));

    await waitFor(() => expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/models/m-1', { status: 'enabled' }));
    // An explicit $0 price renders as a price, not "not priced".
    expect(screen.queryAllByText('not priced')).toHaveLength(0);
  });

  it('blocks enabling a never-priced model and opens the model drawer instead', async () => {
    mockModels([modelFixture()]);
    renderPage();

    expect((await screen.findAllByText('not priced')).length).toBeGreaterThan(0);
    await userEvent.setup().click(screen.getByRole('button', { name: 'Enable' }));

    expect(mocked.patch).not.toHaveBeenCalled();
    expect(screen.getByRole('dialog', { name: /Edit model — claude-fable-5/ })).toBeTruthy();
    expect(await screen.findByText(/Save a rate card first/)).toBeTruthy();
  });
});
