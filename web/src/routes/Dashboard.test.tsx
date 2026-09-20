/**
 * Route-level behavior tests for the personal Dashboard.
 *
 * Token-direction picker (Throughput chart): when the Tokens metric is active
 * the viewer can choose In, Out, or Total (default), Total renders the stacked
 * in/out split whose footer equals the combined totals, In/Out plot only that
 * direction, and the choice is URL-persisted. Requests and Spend keep the
 * single-series render with no picker.
 *
 * Unified cache metric: the Input-tokens tile renders the SAME cache line as
 * the org Explore page (both go through lib/cache.ts), shown only when the
 * window saw any cache activity and never as a bare "0 served from cache".
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../lib/api';
import type { PersonalDashboard, Totals } from '../lib/types';
import { DashboardPage } from './Dashboard';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

function totals(overrides: Partial<Totals> = {}): Totals {
  return {
    tokens_in: 0,
    tokens_out: 0,
    tokens_cached: 0,
    tokens_cache_write_5m: 0,
    tokens_cache_write_1h: 0,
    cost_nanousd: 0,
    request_count: 0,
    error_count: 0,
    ...overrides,
  };
}

function personal(overrides: Partial<PersonalDashboard> = {}): PersonalDashboard {
  return {
    range: 'day',
    start: '2026-08-26T00:00:00Z',
    end: '2026-08-27T00:00:00Z',
    totals: totals({ tokens_in: 950, tokens_out: 400, request_count: 12 }),
    per_model: [],
    per_modality: [],
    per_token: [],
    series: [],
    recent_requests: [],
    ...overrides,
  };
}

/** Dashboard payload with distinguishable per-direction series values. */
function personalWithSeries(): PersonalDashboard {
  return personal({
    totals: totals({ tokens_in: 300, tokens_out: 200, request_count: 12 }),
    series: [
      { bucket: '2026-08-26T00:00:00Z', totals: totals({ tokens_in: 100, tokens_out: 50, request_count: 5 }) },
      { bucket: '2026-08-26T01:00:00Z', totals: totals({ tokens_in: 200, tokens_out: 150, request_count: 7 }) },
    ],
  });
}

function mockApi(dashboard: PersonalDashboard): void {
  mocked.get.mockImplementation((path: string) => {
    if (path.startsWith('/api/v1/dashboard/quota')) return Promise.resolve({ quotas: [] });
    if (path.startsWith('/api/v1/dashboard/personal')) return Promise.resolve(dashboard);
    return Promise.reject(new Error(`unexpected api.get(${path})`));
  });
}

/** Exposes the router's current query string so URL persistence is testable. */
function LocationProbe() {
  const location = useLocation();
  return <div data-testid="location-search">{location.search}</div>;
}

function renderDashboard(initialEntry = '/') {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <DashboardPage />
        <LocationProbe />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
});

it('labels previews, ranks quotas by utilization, and links to complete collections', async () => {
  const snapshot = personal();
  const quotaRows = [10, 20, 30, 90].map((percent, i) => ({
    id: `q${i}`,
    percent,
    unit: 'count',
    metric_label: `quota-${i}`,
    window_label: 'Daily',
    current_value: percent,
    limit_value: 100,
    reset_at: '2026-09-20T00:00:00Z',
  }));
  mocked.get.mockImplementation(async (path: string) => {
    if (path.startsWith('/api/v1/dashboard/quota')) return { quotas: quotaRows };
    if (path.startsWith('/api/v1/dashboard/personal')) return snapshot;
    throw new Error(`unexpected ${path}`);
  });
  renderDashboard();
  expect(await screen.findByText(/3 most utilized of 4/)).toBeTruthy();
  expect(screen.getAllByRole('img').map((img) => img.getAttribute('aria-label'))).toEqual([
    'quota-3 Daily: 90% used',
    'quota-2 Daily: 30% used',
    'quota-1 Daily: 20% used',
  ]);
  expect(screen.getByText(/requests across all dates/)).toBeTruthy();
  expect(screen.getByRole('link', { name: /all quotas/i }).getAttribute('href')).toBe('/quota');
  expect(screen.getByRole('link', { name: /full request log/i }).getAttribute('href')).toBe('/requests');
});

describe('Throughput — token direction picker', () => {
  beforeEach(() => {
    mockApi(personalWithSeries());
  });

  it('shows the picker for the default Tokens metric and stacks in/out by default', async () => {
    const { container } = renderDashboard();

    await screen.findByText('Throughput');

    const picker = screen.getByRole('group', { name: 'Token direction' });
    const buttons = within(picker).getAllByRole('button');
    expect(buttons.map((button) => button.textContent)).toEqual(['In', 'Out', 'Total']);
    expect(within(picker).getByRole('button', { name: 'Total' }).getAttribute('aria-pressed')).toBe('true');

    // Total mode: stacked layers, tokens-in at the baseline, tokens-out on top.
    const boundaries = container.querySelectorAll('path[data-series]');
    expect(Array.from(boundaries).map((path) => path.getAttribute('data-series'))).toEqual(['tokens_in', 'tokens_out']);
    const legend = screen.getByRole('list', { name: 'Chart series' });
    expect(within(legend).getByText('Tokens in')).toBeTruthy();
    expect(within(legend).getByText('Tokens out')).toBeTruthy();

    // Bucket sums 150 and 350 → peak 350, total 500 (combined in+out).
    expect(screen.getByText('peak 350')).toBeTruthy();
    expect(screen.getByText('total 500')).toBeTruthy();
  });

  it('plots only tokens-in after clicking In, with a matching footer and URL param', async () => {
    const user = userEvent.setup();
    const { container } = renderDashboard();

    await screen.findByText('Throughput');
    await user.click(screen.getByRole('button', { name: 'In' }));

    const boundaries = container.querySelectorAll('path[data-series]');
    expect(Array.from(boundaries).map((path) => path.getAttribute('data-series'))).toEqual(['tokens_in']);

    // tokens_in = [100, 200] → peak 200, total 300.
    expect(screen.getByText('peak 200')).toBeTruthy();
    expect(screen.getByText('total 300')).toBeTruthy();
    expect(screen.getByTestId('location-search').textContent).toContain('direction=in');
  });

  it('plots only tokens-out after clicking Out, with a matching footer', async () => {
    const user = userEvent.setup();
    const { container } = renderDashboard();

    await screen.findByText('Throughput');
    await user.click(screen.getByRole('button', { name: 'Out' }));

    const boundaries = container.querySelectorAll('path[data-series]');
    expect(Array.from(boundaries).map((path) => path.getAttribute('data-series'))).toEqual(['tokens_out']);

    // tokens_out = [50, 150] → peak 150, total 200.
    expect(screen.getByText('peak 150')).toBeTruthy();
    expect(screen.getByText('total 200')).toBeTruthy();
    expect(screen.getByTestId('location-search').textContent).toContain('direction=out');
  });

  it('restores the direction from the URL on load', async () => {
    const { container } = renderDashboard('/?direction=out');

    await screen.findByText('Throughput');

    expect(screen.getByRole('button', { name: 'Out' }).getAttribute('aria-pressed')).toBe('true');
    const boundaries = container.querySelectorAll('path[data-series]');
    expect(Array.from(boundaries).map((path) => path.getAttribute('data-series'))).toEqual(['tokens_out']);
    expect(screen.getByText('total 200')).toBeTruthy();
  });

  it('hides the picker and keeps the single-series render for requests and spend', async () => {
    const { container } = renderDashboard('/?metric=requests');

    await screen.findByText('Throughput');

    expect(screen.queryByRole('group', { name: 'Token direction' })).toBeNull();
    expect(screen.queryByRole('list', { name: 'Chart series' })).toBeNull();
    expect(container.querySelectorAll('path[data-series]').length).toBe(0);

    // Single-series footer reflects request counts: peak 7, total 12.
    expect(screen.getByText('peak 7')).toBeTruthy();
    expect(screen.getByText('total 12')).toBeTruthy();

    cleanup();
    renderDashboard('/?metric=cost');
    await screen.findByText('Throughput');
    expect(screen.queryByRole('group', { name: 'Token direction' })).toBeNull();
  });
});

describe('Recent request attribution', () => {
  it('shows the recorded historical team without a current membership lookup', async () => {
    mockApi(
      personal({
        recent_requests: [
          {
            id: 'historical-request',
            created_at: '2026-08-26T00:00:00Z',
            model: 'historical-model',
            modality: 'chat',
            http_status: 200,
            latency_ms: 20,
            tokens_in: 100,
            tokens_out: 20,
            cost_nanousd: 0,
            team_ids: 'former-team',
            team_names: ['Historical Research'],
          },
        ] as PersonalDashboard['recent_requests'],
      }),
    );
    renderDashboard();

    const table = (await screen.findByText('historical-model')).closest('table')!;
    expect(within(table).getByRole('columnheader', { name: 'Charged to' })).toBeTruthy();
    expect(within(table).getByRole('cell', { name: 'Historical Research' })).toBeTruthy();
    expect(mocked.get.mock.calls.every(([path]) => path.startsWith('/api/v1/dashboard/'))).toBe(true);
  });
});

describe('Input-tokens tile cache metric', () => {
  it('shows the canonical hit rate plus the served-from-cache count', async () => {
    // 50 / (950 + 100 + 0) = 4.8% — byte-identical to the Explore rendering
    // for the same Totals (both surfaces call lib/cache.ts cacheMeta()).
    mockApi(
      personal({
        totals: totals({
          tokens_in: 950,
          tokens_out: 400,
          tokens_cached: 50,
          tokens_cache_write_5m: 100,
          tokens_cache_write_1h: 0,
          request_count: 12,
        }),
      }),
    );
    renderDashboard();

    const tile = (await screen.findByText('Input tokens')).closest('article')!;
    expect(within(tile).getByText('950')).toBeTruthy();
    expect(within(tile).getByText('4.8% cache hit rate · 50 served from cache')).toBeTruthy();
  });

  it('hides the cache metric entirely when the window has no cache activity', async () => {
    mockApi(personal()); // requests exist, but zero cache reads and writes
    renderDashboard();

    const tile = (await screen.findByText('Input tokens')).closest('article')!;
    expect(within(tile).getByText('950')).toBeTruthy();
    // No "0 served from cache", no NaN/Infinity, no dangling rate.
    expect(within(tile).queryByText(/served from cache/)).toBeNull();
    expect(within(tile).queryByText(/cache hit rate/)).toBeNull();
    expect(within(tile).queryByText(/NaN|Infinity/)).toBeNull();
  });

  it('counts cache writes as activity even when no read was ever served', async () => {
    mockApi(
      personal({
        totals: totals({ tokens_in: 1000, tokens_cache_write_1h: 200, request_count: 3 }),
      }),
    );
    renderDashboard();

    const tile = (await screen.findByText('Input tokens')).closest('article')!;
    expect(within(tile).getByText('0% cache hit rate · 0 served from cache')).toBeTruthy();
  });
});
