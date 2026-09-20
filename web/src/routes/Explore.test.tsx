/**
 * Route-level behavior tests for the Org pulse page, focused on the Tokens
 * surfaces: the TOKENS (30 DAYS) tile shows the combined total plus an
 * explicit in/out split, and the live-throughput chart stacks tokens-in under
 * tokens-out when — and only when — the Tokens metric is selected, with a
 * URL-persisted In / Out / Total direction picker (default Total). Requests
 * and Spend must keep the pre-existing single-series render.
 *
 * The page's default metric (segmented selector) follows the instance-wide spend_emphasis feature
 * flag (surfaced through /api/v1/me feature_flags): usage emphasis (flag off,
 * the shipped default) opens on Tokens; spend emphasis opens on Spend. An
 * explicit ?metric= URL value always wins.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../lib/api';
import type { GlobalDashboard, Me, Totals } from '../lib/types';
import { SessionProvider } from '../app/session';
import { ExplorePage } from './Explore';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

// Instance mode switch: production code reads it from /api/v1/me via
// useLocalOnly(); the mock lets each test pick the mode directly. Defaults to
// false so every pre-existing test keeps exercising the unchanged flag-off
// render.
let localOnly = false;

vi.mock('../app/session', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../app/session')>();
  return { ...actual, useLocalOnly: () => localOnly };
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

function dashboard(overrides: Partial<GlobalDashboard> = {}): GlobalDashboard {
  return {
    top_models: [
      { key: 'm1', label: 'claude-fable-5', totals: totals({ tokens_in: 3000, tokens_out: 1200, request_count: 40 }) },
      { key: 'm2', label: 'gpt-4-turbo', totals: totals({ tokens_in: 1000, tokens_out: 800, request_count: 11 }) },
    ],
    totals_30d: totals({
      tokens_in: 4000,
      tokens_out: 2000,
      tokens_cached: 1500,
      request_count: 51,
      error_count: 13,
      cost_nanousd: 310_000_000,
    }),
    active_users_15m: 3,
    series: [
      { bucket: '2026-08-26T00:00:00Z', totals: totals({ tokens_in: 100, tokens_out: 50, request_count: 5 }) },
      { bucket: '2026-08-26T00:05:00Z', totals: totals({ tokens_in: 200, tokens_out: 150, request_count: 7 }) },
    ],
    new_models: [],
    leaderboards_enabled: false,
    ...overrides,
  };
}

/** Exposes the router's current query string so URL persistence is testable. */
function LocationProbe() {
  const location = useLocation();
  return <div data-testid="location-search">{location.search}</div>;
}

function renderExplore(initialEntry = '/explore') {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <ExplorePage />
        <LocationProbe />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function meFixture(flags: Record<string, boolean>): Me {
  return {
    id: 'u1',
    email: 'sam@example.com',
    name: 'Sam Doe',
    role: 'user',
    is_active: true,
    timezone: 'UTC',
    locale: 'en-US',
    groups: [],
    teams: [],
    leads_teams: [],
    active_sessions: 1,
    feature_flags: flags,
    endpoint: 'https://janus.example.com/v1',
  };
}

/** Renders the page inside a real SessionProvider so /api/v1/me feature_flags drive the default metric. */
function renderExploreWithFlags(flags: Record<string, boolean>, initialEntry = '/explore') {
  mocked.get.mockImplementation(((path: string) => {
    if (path === '/api/v1/me') return Promise.resolve(meFixture(flags));
    if (path === '/api/v1/config') {
      return Promise.resolve({
        public_url: 'https://janus.example.com',
        version: '1.0.0',
        build: 'abc',
        dev_auth: false,
        feature_flags: flags,
        provider_label: '',
      });
    }
    if (path.startsWith('/api/v1/dashboard/global')) return Promise.resolve(dashboard());
    return Promise.reject(new Error(`unexpected GET ${path}`));
  }) as typeof api.get);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <SessionProvider>
        <MemoryRouter initialEntries={[initialEntry]}>
          <ExplorePage />
        </MemoryRouter>
      </SessionProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  localOnly = false;
  mocked.get.mockResolvedValue(dashboard());
});

it('labels the five-model preview separately from the viewer-authorized catalog', async () => {
  renderExplore();
  expect(await screen.findByText(/up to 5.*newest discovery first/)).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Browse models available to you' }).getAttribute('href')).toBe('/models');
});

describe('TOKENS (30 DAYS) tile', () => {
  it('keeps the combined total and shows the explicit in/out split in the meta line', async () => {
    renderExplore();

    // Combined header value: 4,000 in + 2,000 out = 6,000.
    const tile = (await screen.findByText('Tokens (30 days)')).closest('article')!;
    expect(within(tile).getByText('6,000')).toBeTruthy();

    // Meta line 1: the explicit locale-formatted split.
    expect(within(tile).getByText('4,000 in · 2,000 out')).toBeTruthy();

    // Meta line 2: the unified cache metric — canonical hit rate
    // (1,500 / 4,000 = 37.5%) plus the served-from-cache count.
    expect(within(tile).getByText('37.5% cache hit rate · 1,500 served from cache')).toBeTruthy();
  });

  it('renders the canonical hit rate including cache-write tokens in the denominator', async () => {
    // 50 / (950 + 100 + 0) = 4.7619…% → 4.8% — the same string the personal
    // Dashboard renders for identical Totals (see Dashboard.test.tsx).
    mocked.get.mockResolvedValue(
      dashboard({
        totals_30d: totals({
          tokens_in: 950,
          tokens_out: 2000,
          tokens_cached: 50,
          tokens_cache_write_5m: 100,
          tokens_cache_write_1h: 0,
          request_count: 51,
        }),
      }),
    );
    renderExplore();

    const tile = (await screen.findByText('Tokens (30 days)')).closest('article')!;
    expect(within(tile).getByText('4.8% cache hit rate · 50 served from cache')).toBeTruthy();
  });

  it('omits the cache metric when there was no cache activity at all', async () => {
    mocked.get.mockResolvedValue(
      dashboard({
        totals_30d: totals({ tokens_in: 4000, tokens_out: 2000, tokens_cached: 0, request_count: 51 }),
      }),
    );
    renderExplore();

    const tile = (await screen.findByText('Tokens (30 days)')).closest('article')!;
    expect(within(tile).getByText('4,000 in · 2,000 out')).toBeTruthy();
    expect(within(tile).queryByText(/served from cache/)).toBeNull();
    expect(within(tile).queryByText(/cache hit rate/)).toBeNull();
  });
});

describe('live throughput — tokens metric', () => {
  it('renders a stacked in/out chart with a legend when metric=tokens', async () => {
    const { container } = renderExplore('/explore?metric=tokens');

    await screen.findByText('Live throughput');

    // Legend carries both i18n labels.
    const legend = screen.getByRole('list', { name: 'Chart series' });
    expect(within(legend).getByText('Tokens in')).toBeTruthy();
    expect(within(legend).getByText('Tokens out')).toBeTruthy();

    // Two stacked layers: tokens-in at the baseline, tokens-out on top.
    const boundaries = container.querySelectorAll('path[data-series]');
    expect(Array.from(boundaries).map((path) => path.getAttribute('data-series'))).toEqual(['tokens_in', 'tokens_out']);
  });

  it('keeps the footer peak/total on the combined in+out totals', async () => {
    renderExplore('/explore?metric=tokens');

    await screen.findByText('Live throughput');

    // Bucket sums: 100+50=150 and 200+150=350 → peak 350, total 500.
    expect(screen.getByText('peak 350')).toBeTruthy();
    expect(screen.getByText('total 500')).toBeTruthy();
  });

  it('switching the metric toggle to Tokens stacks the chart without a reload', async () => {
    const user = userEvent.setup();
    renderExplore('/explore?metric=requests');

    await screen.findByText('Live throughput');
    expect(screen.queryByRole('list', { name: 'Chart series' })).toBeNull();

    await user.click(screen.getByRole('button', { name: 'Tokens' }));

    const legend = await screen.findByRole('list', { name: 'Chart series' });
    expect(within(legend).getByText('Tokens in')).toBeTruthy();
    expect(within(legend).getByText('Tokens out')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Tokens' }).getAttribute('aria-pressed')).toBe('true');
  });
});

describe('live throughput — token direction picker', () => {
  it('offers In, Out, and Total when metric=tokens, defaulting to Total', async () => {
    renderExplore('/explore?metric=tokens');

    await screen.findByText('Live throughput');

    const picker = screen.getByRole('group', { name: 'Token direction' });
    const buttons = within(picker).getAllByRole('button');
    expect(buttons.map((button) => button.textContent)).toEqual(['In', 'Out', 'Total']);
    expect(within(picker).getByRole('button', { name: 'Total' }).getAttribute('aria-pressed')).toBe('true');
  });

  it('plots only tokens-in after clicking In, with a footer of that direction alone', async () => {
    const user = userEvent.setup();
    const { container } = renderExplore('/explore?metric=tokens');

    await screen.findByText('Live throughput');
    await user.click(screen.getByRole('button', { name: 'In' }));

    // Only the tokens_in layer remains.
    const boundaries = container.querySelectorAll('path[data-series]');
    expect(Array.from(boundaries).map((path) => path.getAttribute('data-series'))).toEqual(['tokens_in']);

    // tokens_in = [100, 200] → peak 200, total 300.
    expect(screen.getByText('peak 200')).toBeTruthy();
    expect(screen.getByText('total 300')).toBeTruthy();

    // The selection is URL-persisted, like the metric control.
    expect(screen.getByTestId('location-search').textContent).toContain('direction=in');
  });

  it('plots only tokens-out after clicking Out, with a footer of that direction alone', async () => {
    const user = userEvent.setup();
    const { container } = renderExplore('/explore?metric=tokens');

    await screen.findByText('Live throughput');
    await user.click(screen.getByRole('button', { name: 'Out' }));

    const boundaries = container.querySelectorAll('path[data-series]');
    expect(Array.from(boundaries).map((path) => path.getAttribute('data-series'))).toEqual(['tokens_out']);

    // tokens_out = [50, 150] → peak 150, total 200.
    expect(screen.getByText('peak 150')).toBeTruthy();
    expect(screen.getByText('total 200')).toBeTruthy();
    expect(screen.getByTestId('location-search').textContent).toContain('direction=out');
  });

  it('restores the direction from the URL on load', async () => {
    const { container } = renderExplore('/explore?metric=tokens&direction=in');

    await screen.findByText('Live throughput');

    expect(screen.getByRole('button', { name: 'In' }).getAttribute('aria-pressed')).toBe('true');
    const boundaries = container.querySelectorAll('path[data-series]');
    expect(Array.from(boundaries).map((path) => path.getAttribute('data-series'))).toEqual(['tokens_in']);
    expect(screen.getByText('total 300')).toBeTruthy();
  });

  it('returns to the combined stacked view when Total is selected again', async () => {
    const user = userEvent.setup();
    const { container } = renderExplore('/explore?metric=tokens&direction=out');

    await screen.findByText('Live throughput');
    await user.click(screen.getByRole('button', { name: 'Total' }));

    const boundaries = container.querySelectorAll('path[data-series]');
    expect(Array.from(boundaries).map((path) => path.getAttribute('data-series'))).toEqual(['tokens_in', 'tokens_out']);
    expect(screen.getByText('total 500')).toBeTruthy();

    // Total is the default, so the param is dropped from the URL.
    expect(screen.getByTestId('location-search').textContent).not.toContain('direction=');
  });

  it('is absent for the requests and spend metrics', async () => {
    renderExplore('/explore?metric=cost');
    await screen.findByText('Live throughput');
    expect(screen.queryByRole('group', { name: 'Token direction' })).toBeNull();

    cleanup();
    mocked.get.mockResolvedValue(dashboard());
    renderExplore('/explore?metric=requests');
    await screen.findByText('Live throughput');
    expect(screen.queryByRole('group', { name: 'Token direction' })).toBeNull();
  });
});

describe('live throughput — requests and spend stay single-series', () => {
  it('renders no stacked legend for the requests metric', async () => {
    const { container } = renderExplore('/explore?metric=requests');

    await screen.findByText('Live throughput');

    expect(screen.queryByRole('list', { name: 'Chart series' })).toBeNull();
    expect(container.querySelectorAll('path[data-series]').length).toBe(0);

    // Single-series footer still reflects request counts: peak 7, total 12.
    expect(screen.getByText('peak 7')).toBeTruthy();
    expect(screen.getByText('total 12')).toBeTruthy();
  });

  it('renders no stacked legend for the spend metric', async () => {
    const { container } = renderExplore('/explore?metric=cost');

    await screen.findByText('Live throughput');

    expect(screen.queryByRole('list', { name: 'Chart series' })).toBeNull();
    expect(container.querySelectorAll('path[data-series]').length).toBe(0);
  });
});

describe('local-only mode (JANUS_LOCAL_ONLY)', () => {
  it('hides the spend tile and the Spend metric option, keeping token surfaces intact', async () => {
    localOnly = true;
    renderExplore();

    // Token tile renders exactly as before…
    const tile = (await screen.findByText('Tokens (30 days)')).closest('article')!;
    expect(within(tile).getByText('6,000')).toBeTruthy();

    // …but no spend tile and no Spend metric toggle anywhere.
    expect(screen.queryByText('Spend (30 days)')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Spend' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Requests' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Tokens' })).toBeTruthy();
  });

  it('falls back to the requests metric when a bookmarked ?metric=cost URL is opened', async () => {
    localOnly = true;
    renderExplore('/explore?metric=cost');

    await screen.findByText('Live throughput');

    // The cost metric is unavailable, so the chart shows requests instead:
    // single-series footer with peak 7, total 12 (5 + 7 request buckets).
    expect(screen.getByText('peak 7')).toBeTruthy();
    expect(screen.getByText('total 12')).toBeTruthy();
  });

  it('keeps the spend tile and Spend option with the mode off (unchanged)', async () => {
    renderExplore();

    await screen.findByText('Tokens (30 days)');
    expect(screen.getByText('Spend (30 days)')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Spend' })).toBeTruthy();
  });
});

// the pressed segment follows the instance-wide emphasis setting
// unless the URL already names a metric, which always wins.
describe('emphasis default — the spend_emphasis flag picks the opening metric', () => {
  it('opens on Tokens (usage emphasis) when the flag is off, with the stacked in/out chart', async () => {
    renderExploreWithFlags({ spend_emphasis: false });

    await screen.findByText('Live throughput');
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Tokens' }).getAttribute('aria-pressed')).toBe('true');
    });
    expect(screen.getByRole('button', { name: 'Spend' }).getAttribute('aria-pressed')).toBe('false');

    // Usage emphasis renders the stacked tokens-in/tokens-out chart.
    const legend = await screen.findByRole('list', { name: 'Chart series' });
    expect(within(legend).getByText('Tokens in')).toBeTruthy();
    expect(within(legend).getByText('Tokens out')).toBeTruthy();
  });

  it('opens on Spend when the instance emphasises spend (flag on)', async () => {
    renderExploreWithFlags({ spend_emphasis: true });

    await screen.findByText('Live throughput');
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Spend' }).getAttribute('aria-pressed')).toBe('true');
    });
    expect(screen.getByRole('button', { name: 'Tokens' }).getAttribute('aria-pressed')).toBe('false');

    // Spend stays single-series: no stacked legend.
    expect(screen.queryByRole('list', { name: 'Chart series' })).toBeNull();
  });

  it('an explicit ?metric= URL value wins over spend emphasis', async () => {
    renderExploreWithFlags({ spend_emphasis: true }, '/explore?metric=requests');

    await screen.findByText('Live throughput');
    expect(screen.getByRole('button', { name: 'Requests' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByRole('button', { name: 'Spend' }).getAttribute('aria-pressed')).toBe('false');

    // Single-series footer reflects request counts: peak 7, total 12.
    expect(screen.getByText('peak 7')).toBeTruthy();
    expect(screen.getByText('total 12')).toBeTruthy();
  });

  it('a manual selector click still overrides the spend-emphasis default', async () => {
    const user = userEvent.setup();
    renderExploreWithFlags({ spend_emphasis: true });

    await screen.findByText('Live throughput');
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Spend' }).getAttribute('aria-pressed')).toBe('true');
    });

    await user.click(screen.getByRole('button', { name: 'Tokens' }));

    const legend = await screen.findByRole('list', { name: 'Chart series' });
    expect(within(legend).getByText('Tokens in')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Tokens' }).getAttribute('aria-pressed')).toBe('true');
  });
});
