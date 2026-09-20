/**
 * A03 Upstream detail — rate-card editor.
 *
 * The upstream drawer (/admin/upstreams/:id) lists the models discovered on
 * that upstream with their prices and opens the shared rate-card dialog, which
 * carries all five billing dimensions including the 5m/1h cache-write rates.
 * Saving PATCHes the canonical snake_case field names; $0 and $50/MTok both
 * succeed. Escape with the dialog open closes only the dialog, not the drawer.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { Model, Upstream } from '../../lib/types';
import { ToastProvider } from '../../components/ui';
import { UpstreamsPage } from './Upstreams';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

const upstreamFixture: Upstream = {
  id: 'up-1',
  name: 'Anthropic',
  adapter_type: 'anthropic',
  base_url: 'https://api.anthropic.example',
  api_key_mask: 'sk-…cdef',
  has_api_key: true,
  enabled: true,
  last_check_at: '2026-08-20T00:00:00Z',
  last_error: '',
  last_latency_ms: 42,
  model_count: 1,
  created_at: '2026-08-01T00:00:00Z',
};

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

function mockApi(models: Model[]) {
  mocked.get.mockImplementation((path: string) => {
    if (path === '/api/v1/admin/upstreams') {
      return Promise.resolve({ upstreams: [upstreamFixture], adapter_types: ['anthropic'] });
    }
    if (path === '/api/v1/admin/models?upstream_id=up-1') {
      return Promise.resolve({ models, counts: {}, modalities: [] });
    }
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
}

function renderDetail() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={['/admin/upstreams/up-1']}>
          <Routes>
            <Route path="/admin/upstreams" element={<UpstreamsPage />} />
            <Route path="/admin/upstreams/:id" element={<UpstreamsPage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

async function openRateDialog(): Promise<HTMLElement> {
  await userEvent.setup().click(await screen.findByRole('button', { name: 'Edit rates' }));
  return screen.getByRole('dialog', { name: /Rate card — claude-fable-5/ });
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocked.patch.mockImplementation(() => Promise.resolve({ model: modelFixture() }));
});

describe('A03 upstream detail — rate cards section', () => {
  it('lists the models discovered on this upstream with their prices', async () => {
    mockApi([modelFixture({ rate_in_nanousd: 10_000_000_000, rate_out_nanousd: 50_000_000_000 })]);
    renderDetail();

    const drawer = await screen.findByRole('dialog', { name: 'Edit Anthropic' });
    expect(within(drawer).getByText('Rate cards')).toBeTruthy();
    expect(within(drawer).getByText('claude-fable-5')).toBeTruthy();
    expect(within(drawer).getByText('$10.00 / Mtok')).toBeTruthy();
    expect(within(drawer).getByText('$50.00 / Mtok')).toBeTruthy();
  });

  it('shows an empty state when discovery has found nothing yet', async () => {
    mockApi([]);
    renderDetail();

    const drawer = await screen.findByRole('dialog', { name: 'Edit Anthropic' });
    expect(await within(drawer).findByText('No models discovered yet')).toBeTruthy();
  });

  it('renders an explicitly $0-priced model as a price, not "not priced"', async () => {
    mockApi([modelFixture({ rate_effective_from: '2026-08-23T00:00:00Z' })]);
    renderDetail();

    const drawer = await screen.findByRole('dialog', { name: 'Edit Anthropic' });
    await within(drawer).findByText('claude-fable-5');
    expect(within(drawer).getAllByText('$0.00 / Mtok')).toHaveLength(2);
    expect(within(drawer).queryByText('not priced')).toBeNull();
  });
});

describe('A03 upstream detail — rate-card dialog', () => {
  it('opens the dialog with all five rate fields', async () => {
    mockApi([modelFixture()]);
    renderDetail();
    await screen.findByRole('dialog', { name: 'Edit Anthropic' });
    const dialog = await openRateDialog();

    for (const label of [
      /^Input — USD per million tokens/,
      /^Output — USD per million tokens/,
      /^Cached input — USD per million tokens/,
      /^5m cache write — USD per million tokens/,
      /^1h cache write — USD per million tokens/,
    ]) {
      expect(within(dialog).getByLabelText(label)).toBeTruthy();
    }
    // Each cache-write tier explains its own lifetime and which providers bill it.
    expect(within(dialog).getByText(/5-minute lifetime/)).toBeTruthy();
    expect(within(dialog).getByText(/1-hour lifetime/)).toBeTruthy();
    // The upstream's adapter type (anthropic) tailors the provider guide.
    expect(within(dialog).getByTestId('rate-provider-guide').textContent).toMatch(/Anthropic/);
  });

  it('submits $0 and $50 — payload carries the snake_case cache-write fields', async () => {
    mockApi([modelFixture()]);
    renderDetail();
    await screen.findByRole('dialog', { name: 'Edit Anthropic' });
    const dialog = await openRateDialog();

    fireEvent.change(within(dialog).getByLabelText(/^Output — USD/), { target: { value: '50' } });
    fireEvent.change(within(dialog).getByLabelText(/^5m cache write — USD/), { target: { value: '0' } });
    fireEvent.change(within(dialog).getByLabelText(/^1h cache write — USD/), { target: { value: '50' } });
    await userEvent.setup().click(within(dialog).getByRole('button', { name: 'Save rate card' }));

    await waitFor(() =>
      expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/models/m-1', {
        rate_in_usd_per_mtok: 0,
        rate_out_usd_per_mtok: 50,
        rate_cached_usd_per_mtok: 0,
        rate_cache_write_5m_usd_per_mtok: 0,
        rate_cache_write_1h_usd_per_mtok: 50,
      }),
    );
    expect(await screen.findByText(/Rate card saved/)).toBeTruthy();
    // The upstream drawer is still open behind the closed dialog.
    expect(screen.getByRole('dialog', { name: 'Edit Anthropic' })).toBeTruthy();
  });

  it('rejects a negative rate inline without a request', async () => {
    mockApi([modelFixture()]);
    renderDetail();
    await screen.findByRole('dialog', { name: 'Edit Anthropic' });
    const dialog = await openRateDialog();

    fireEvent.change(within(dialog).getByLabelText(/^1h cache write — USD/), { target: { value: '-5' } });
    await userEvent.setup().click(within(dialog).getByRole('button', { name: 'Save rate card' }));

    const alert = await within(dialog).findByRole('alert');
    expect(alert.textContent).toContain('zero or higher');
    expect(mocked.patch).not.toHaveBeenCalled();
  });

  it('Escape closes only the rate dialog, keeping the upstream drawer open', async () => {
    mockApi([modelFixture()]);
    renderDetail();
    await screen.findByRole('dialog', { name: 'Edit Anthropic' });
    await openRateDialog();

    fireEvent.keyDown(window, { key: 'Escape' });

    await waitFor(() => expect(screen.queryByRole('dialog', { name: /Rate card — claude-fable-5/ })).toBeNull());
    expect(screen.getByRole('dialog', { name: 'Edit Anthropic' })).toBeTruthy();
  });
});
