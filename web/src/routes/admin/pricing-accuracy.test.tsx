/**
 * Administration → Models → Pricing accuracy: the provider's quote beside
 * Janus's own price, per provider and model, with flags for missing prices
 * and links to the calls that differ most; the request log finds one
 * request by id and its drawer shows Janus's price beside a quote.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { PriceAccuracyReport } from '../../lib/types';
import { ToastProvider } from '../../components/ui';
import { AdminModelsSection } from './PricingAccuracy';
import { AdminRequestsPage } from './Requests';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

const group = {
  quoted_calls: 0,
  compared_calls: 0,
  quoted_nanousd: 0,
  estimate_nanousd: 0,
  diff_nanousd: 0,
  diff_percent: null,
  within_1_percent: 0,
  within_5_percent: 0,
  no_price_calls: 0,
};

const report: PriceAccuracyReport = {
  start: '2026-10-16T00:00:00Z',
  end: '2026-11-16T00:00:00Z',
  totals: {
    ...group,
    quoted_calls: 12,
    compared_calls: 10,
    quoted_nanousd: 300_000_000,
    estimate_nanousd: 270_000_000,
    diff_nanousd: -30_000_000,
    diff_percent: -10,
    within_1_percent: 6,
    within_5_percent: 8,
    no_price_calls: 2,
  },
  providers: [
    {
      ...group,
      upstream_id: 'up-x',
      upstream_name: 'X.ai',
      quoted_calls: 12,
      compared_calls: 10,
      quoted_nanousd: 300_000_000,
      estimate_nanousd: 270_000_000,
      diff_nanousd: -30_000_000,
      diff_percent: -10,
      within_1_percent: 6,
      within_5_percent: 8,
      no_price_calls: 2,
    },
  ],
  models: [
    {
      ...group,
      upstream_id: 'up-x',
      upstream_name: 'X.ai',
      model_id: 'm-img',
      model: 'grok-imagine-image',
      quoted_calls: 2,
      compared_calls: 2,
      quoted_nanousd: 40_000_000,
      estimate_nanousd: 0,
      diff_nanousd: -40_000_000,
      diff_percent: -100,
      no_price_calls: 2,
      has_price_now: false,
    },
    {
      ...group,
      upstream_id: 'up-x',
      upstream_name: 'X.ai',
      model_id: 'm-chat',
      model: 'grok-4.6',
      quoted_calls: 10,
      compared_calls: 8,
      quoted_nanousd: 260_000_000,
      estimate_nanousd: 270_000_000,
      diff_nanousd: 10_000_000,
      diff_percent: 3.846,
      within_1_percent: 6,
      within_5_percent: 8,
    },
  ],
  worst: [
    {
      id: 'evt-1',
      request_id: 'req-worst',
      created_at: '2026-11-15T10:00:00Z',
      upstream_id: 'up-x',
      upstream_name: 'X.ai',
      model_id: 'm-img',
      model: 'grok-imagine-image',
      modality: 'image',
      internal: false,
      quoted_nanousd: 20_000_000,
      estimate_nanousd: 0,
      diff_nanousd: -20_000_000,
      diff_percent: -100,
    },
    {
      id: 'evt-2',
      request_id: 'req-guard',
      created_at: '2026-11-15T09:00:00Z',
      upstream_id: 'up-x',
      upstream_name: 'X.ai',
      model_id: 'm-chat',
      model: 'grok-4.6',
      modality: 'classification',
      internal: true,
      quoted_nanousd: 10_000_000,
      estimate_nanousd: 11_000_000,
      diff_nanousd: 1_000_000,
      diff_percent: 10,
    },
  ],
};

function renderAt(entry: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[entry]}>
          <Routes>
            <Route path="/admin/models" element={<AdminModelsSection />} />
            <Route path="/admin/models/:tab" element={<AdminModelsSection />} />
            <Route path="/admin/requests" element={<AdminRequestsPage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  window.localStorage.clear();
});

describe('pricing accuracy report', () => {
  beforeEach(() => {
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/admin/pricing/accuracy')) return Promise.resolve({ range: 'month', report });
      if (path.startsWith('/api/v1/admin/models')) return Promise.resolve({ models: [], counts: {}, modalities: [] });
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
  });

  it('is a tab of Models and compares the quotes with Janus prices in words', async () => {
    renderAt('/admin/models/pricing-accuracy');
    const nav = screen.getByRole('navigation', { name: 'Models' });
    expect(within(nav).getByRole('link', { name: 'Pricing accuracy' }).getAttribute('aria-current')).toBe('page');
    expect(within(nav).getByRole('link', { name: 'Models' }).getAttribute('href')).toBe('/admin/models');

    expect(await screen.findByText('Calls with a provider price')).toBeTruthy();
    expect(mocked.get).toHaveBeenCalledWith('/api/v1/admin/pricing/accuracy?range=month');
    expect(screen.getByText('10 compared; 2 without a Janus price on record')).toBeTruthy();
    expect(screen.getByText('$0.0300 (10%) less than providers charged')).toBeTruthy();
    expect(screen.getByText('60% / 80%')).toBeTruthy();
    expect(screen.getByText('2 calls with no Janus price')).toBeTruthy();

    const byModel = screen.getByRole('region', { name: 'By model' });
    const img = within(byModel).getByRole('rowheader', { name: /grok-imagine-image/ });
    expect(within(img).getByText('No Janus price')).toBeTruthy();
    expect(within(img).getByRole('link', { name: 'Set prices' }).getAttribute('href')).toBe('/admin/models?edit=m-img');
    const chat = within(byModel)
      .getByRole('rowheader', { name: /grok-4.6/ })
      .closest('tr')!;
    expect(within(chat).getByText('8')).toBeTruthy();
    expect(within(chat).getByText('of 10')).toBeTruthy();
    expect(within(chat).getByText('(3.8%)')).toBeTruthy();
    expect(within(chat).queryByText('No Janus price')).toBeNull();

    const worst = screen.getByRole('region', { name: 'Largest differences' });
    const links = within(worst).getAllByRole('link', { name: 'View request' });
    expect(links[0]!.getAttribute('href')).toBe('/admin/requests?request_id=req-worst');
    // A call Janus made for itself is listed only with gateway internals shown.
    expect(links[1]!.getAttribute('href')).toBe('/admin/requests?request_id=req-guard&include_internal=true');
  });

  it('asks the server again when the range changes', async () => {
    renderAt('/admin/models/pricing-accuracy');
    await screen.findByText('Calls with a provider price');
    await userEvent.click(screen.getByRole('button', { name: '7 d' }));
    expect(mocked.get).toHaveBeenCalledWith('/api/v1/admin/pricing/accuracy?range=week');
  });

  it('says so when no call was quoted', async () => {
    mocked.get.mockImplementation((path: string) =>
      path.startsWith('/api/v1/admin/pricing/accuracy')
        ? Promise.resolve({ range: 'month', report: { ...report, totals: group, models: [], providers: [], worst: [] } })
        : Promise.reject(new Error(path)),
    );
    renderAt('/admin/models/pricing-accuracy');
    expect(await screen.findByText('No calls with a provider price in this period')).toBeTruthy();
  });

  it('shows the tabs on the model list too, and an unknown tab is not found', async () => {
    renderAt('/admin/models');
    const nav = screen.getByRole('navigation', { name: 'Models' });
    expect(within(nav).getByRole('link', { name: 'Models' }).getAttribute('aria-current')).toBe('page');
    cleanup();
    renderAt('/admin/models/nope');
    expect(screen.queryByRole('navigation', { name: 'Models' })).toBeNull();
  });
});

describe('request log: one request and its Janus price', () => {
  const quoted = {
    id: 'evt-q',
    created_at: '2026-11-15T10:00:00Z',
    user_id: 'user-a',
    user_email: 'ada@example.com',
    user_name: 'Ada Lovelace',
    token_id: 'tok-1',
    model: 'grok-4.6',
    endpoint_path: '/v1/chat/completions',
    http_method: 'POST',
    modality: 'chat',
    streaming: false,
    request_bytes: 10,
    response_bytes: 10,
    tokens_in: 100,
    tokens_out: 40,
    tokens_cached: 0,
    tokens_cache_write_5m: 0,
    tokens_cache_write_1h: 0,
    token_accounting_method: 'upstream_reported_cost',
    cost_quoted_by: 'X.ai',
    cost_nanousd: 10_000_000,
    estimated_cost_nanousd: 11_000_000,
    finish_reason: 'stop',
    http_status: 200,
    latency_ms: 800,
    ttfb_ms: 100,
    client_user_agent: 'curl/8.0',
    client_ip: '10.0.0.1',
    x_forwarded_for: '',
    error_code: '',
    quota_violated: false,
    request_id: 'req-worst',
  };

  beforeEach(() => {
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/requests/facets')) return Promise.resolve({ facets: [] });
      if (path.startsWith('/api/v1/requests')) return Promise.resolve({ requests: [quoted], total_count: 1 });
      if (path.startsWith('/api/v1/admin/requests/'))
        return Promise.resolve({ request_id: 'req-worst', classifier_runs: [], other_violations: [] });
      if (path.startsWith('/api/v1/admin/classification/categories')) return Promise.resolve({ categories: [] });
      return Promise.resolve({});
    });
  });

  it('narrows to the linked request and shows Janus’s price beside the quote', async () => {
    renderAt('/admin/requests?request_id=req-worst');
    expect(await screen.findByText('req-worst', { selector: '.banner .mono' })).toBeTruthy();
    const listCall = mocked.get.mock.calls.map(([p]) => p as string).find((p) => p.startsWith('/api/v1/requests?'));
    expect(listCall).toContain('request_id=req-worst');
    await userEvent.click((await screen.findByRole('cell', { name: 'grok-4.6' })).closest('tr')!);
    expect(await screen.findByText('Janus price')).toBeTruthy();
    expect(screen.getByText(/\$0\.001000 \(10%\) more than the provider charged/)).toBeTruthy();
  });
});
