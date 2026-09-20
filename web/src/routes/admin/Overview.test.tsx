/**
 * Route-level behavior tests for the admin Overview's Organisation-trend
 * chart, focused on the token-direction picker: it appears only when the
 * Tokens metric is active, defaults to Total (the stacked in/out split whose
 * footer equals the combined totals), In/Out plot only that direction, and
 * the choice is URL-persisted. The default Spend metric keeps the
 * single-series render with no picker.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { Totals } from '../../lib/types';
import { AdminOverview } from './Overview';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const { sessionMode } = vi.hoisted(() => ({ sessionMode: { localOnly: false } }));
vi.mock('../../app/session', () => ({
  useLocalOnly: () => sessionMode.localOnly,
  useDefaultMetric: () => 'tokens',
}));

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

function overviewData() {
  return {
    range: 'month',
    totals: totals({ tokens_in: 300, tokens_out: 200, request_count: 12, cost_nanousd: 5_000_000 }),
    series: [
      { bucket: '2026-08-01T00:00:00Z', totals: totals({ tokens_in: 100, tokens_out: 50, request_count: 5 }) },
      { bucket: '2026-08-02T00:00:00Z', totals: totals({ tokens_in: 200, tokens_out: 150, request_count: 7 }) },
    ],
    top_spenders: [],
    top_models: [],
    by_status: [],
    model_counts: {},
    near_breach: [],
    user_count: 4,
    active_users_15m: 2,
  };
}

/** Exposes the router's current query string so URL persistence is testable. */
function LocationProbe() {
  const location = useLocation();
  return <div data-testid="location-search">{location.search}</div>;
}

function renderOverview(initialEntry = '/admin') {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <AdminOverview />
        <LocationProbe />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  sessionMode.localOnly = false;
  mocked.get.mockResolvedValue(overviewData());
});

describe('Most popular teams', () => {
  it.each([undefined, []])('hides the entire card for missing or empty teams (%s)', async (topTeams) => {
    mocked.get.mockResolvedValue({ ...overviewData(), top_teams: topTeams });
    renderOverview();
    await screen.findByText('Organisation trend');
    expect(screen.queryByRole('heading', { name: 'Most popular teams' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Team reports' })).toBeNull();
  });

  it.each([false, true])('shows named teams and output tokens, never cost (local-only=%s)', async (localOnly) => {
    sessionMode.localOnly = localOnly;
    mocked.get.mockResolvedValue({
      ...overviewData(),
      top_teams: [
        {
          key: 'team-alpha',
          label: 'Research team',
          totals: totals({ tokens_out: 321, cost_nanousd: 5_000_000_000, request_count: 3 }),
        },
      ],
    });
    renderOverview('/admin?metric=cost');
    const heading = await screen.findByRole('heading', { name: 'Most popular teams' });
    const card = heading.closest('section')!;
    expect(within(card).getByText('Research team')).toBeTruthy();
    expect(within(card).getByText('321')).toBeTruthy();
    expect(card.textContent).not.toMatch(/\$|Spend/);
    expect(within(card).getByRole('link', { name: 'Team reports' }).getAttribute('href')).toBe('/reports');
  });
});

describe('Organisation trend — token direction picker', () => {
  it('is absent for the Spend metric, which stays single-series', async () => {
    const { container } = renderOverview('/admin?metric=cost');

    await screen.findByText('Organisation trend');

    expect(screen.queryByRole('group', { name: 'Token direction' })).toBeNull();
    expect(screen.queryByRole('list', { name: 'Chart series' })).toBeNull();
    expect(container.querySelectorAll('path[data-series]').length).toBe(0);
  });

  it('is absent for the requests metric', async () => {
    renderOverview('/admin?metric=requests');

    await screen.findByText('Organisation trend');

    expect(screen.queryByRole('group', { name: 'Token direction' })).toBeNull();
    // Single-series footer reflects request counts: peak 7, total 12.
    expect(screen.getByText('peak 7')).toBeTruthy();
    expect(screen.getByText('total 12')).toBeTruthy();
  });

  it('appears when metric=tokens and defaults to the stacked Total view', async () => {
    const { container } = renderOverview('/admin?metric=tokens');

    await screen.findByText('Organisation trend');

    const picker = screen.getByRole('group', { name: 'Token direction' });
    const buttons = within(picker).getAllByRole('button');
    expect(buttons.map((button) => button.textContent)).toEqual(['In', 'Out', 'Total']);
    expect(within(picker).getByRole('button', { name: 'Total' }).getAttribute('aria-pressed')).toBe('true');

    const boundaries = container.querySelectorAll('path[data-series]');
    expect(Array.from(boundaries).map((path) => path.getAttribute('data-series'))).toEqual(['tokens_in', 'tokens_out']);
    const legend = screen.getByRole('list', { name: 'Chart series' });
    expect(within(legend).getByText('Tokens in')).toBeTruthy();
    expect(within(legend).getByText('Tokens out')).toBeTruthy();

    // Bucket sums 150 and 350 → peak 350, total 500 (combined in+out).
    expect(screen.getByText('peak 350')).toBeTruthy();
    expect(screen.getByText('total 500')).toBeTruthy();
  });

  it('plots only the selected direction after clicking In or Out', async () => {
    const user = userEvent.setup();
    const { container } = renderOverview('/admin?metric=tokens');

    await screen.findByText('Organisation trend');
    await user.click(screen.getByRole('button', { name: 'In' }));

    // tokens_in = [100, 200] → peak 200, total 300.
    expect(Array.from(container.querySelectorAll('path[data-series]')).map((path) => path.getAttribute('data-series'))).toEqual([
      'tokens_in',
    ]);
    expect(screen.getByText('peak 200')).toBeTruthy();
    expect(screen.getByText('total 300')).toBeTruthy();
    expect(screen.getByTestId('location-search').textContent).toContain('direction=in');

    await user.click(screen.getByRole('button', { name: 'Out' }));

    // tokens_out = [50, 150] → peak 150, total 200.
    expect(Array.from(container.querySelectorAll('path[data-series]')).map((path) => path.getAttribute('data-series'))).toEqual([
      'tokens_out',
    ]);
    expect(screen.getByText('peak 150')).toBeTruthy();
    expect(screen.getByText('total 200')).toBeTruthy();
    expect(screen.getByTestId('location-search').textContent).toContain('direction=out');
  });

  it('restores the direction from the URL on load', async () => {
    const { container } = renderOverview('/admin?metric=tokens&direction=in');

    await screen.findByText('Organisation trend');

    expect(screen.getByRole('button', { name: 'In' }).getAttribute('aria-pressed')).toBe('true');
    expect(Array.from(container.querySelectorAll('path[data-series]')).map((path) => path.getAttribute('data-series'))).toEqual([
      'tokens_in',
    ]);
    expect(screen.getByText('total 300')).toBeTruthy();
  });
});
