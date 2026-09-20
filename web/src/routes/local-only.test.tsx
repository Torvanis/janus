/**
 * Local-only mode (JANUS_LOCAL_ONLY) — UI contract.
 *
 * When the instance runs in local-only mode, cost tracking is off and every
 * cost/spend/pricing surface must vanish: the Dashboard Spend tile, cost
 * columns and the Spend chart-metric option; the Explore/Team spend tiles;
 * the Requests cost column and detail row; the admin Overview spend tile and
 * Top spenders card; the People/Teams spend columns; the Models pricing
 * columns and Rates action; and the USD quota option in the create dialog.
 * Token and request metrics must keep rendering exactly as before — and with
 * the mode OFF every one of those surfaces must render unchanged.
 *
 * The session hook is mocked (the flag arrives via /api/v1/me in production);
 * everything else runs through the real component tree with the HTTP layer
 * mocked, matching the other route-level tests.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../lib/api';
import type { GlobalDashboard, Me, Model, PersonalDashboard, Totals, UsageEvent } from '../lib/types';
import { ToastProvider } from '../components/ui';
import { DashboardPage } from './Dashboard';
import { ExplorePage } from './Explore';
import { RequestsPage } from './Requests';
import { ModelsPage as UserModelsPage } from './Models';
import { AdminOverview } from './admin/Overview';
import { AdminModelsPage } from './admin/Models';
import { AdminQuotasPage } from './admin/Quotas';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

// The single switch under test. Real code reads it from /api/v1/me via
// useLocalOnly(); the mock lets each test pick the instance mode directly.
let localOnly = false;

const me: Me = {
  id: 'user-1',
  email: 'ada@example.com',
  name: 'Ada',
  role: 'admin',
  is_active: true,
  timezone: 'UTC',
  locale: 'en',
  groups: [],
  teams: [],
  leads_teams: [],
  active_sessions: 1,
  feature_flags: {},
  endpoint: 'http://localhost:8080/v1',
};

vi.mock('../app/session', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../app/session')>();
  return {
    ...actual,
    useLocalOnly: () => localOnly,
    useMe: () => ({ ...me, local_only: localOnly }),
  };
});

const mocked = vi.mocked(api);

function totals(overrides: Partial<Totals> = {}): Totals {
  return {
    tokens_in: 0,
    tokens_out: 0,
    tokens_cached: 0,
    // Required on Totals since the cache-hit-rate work landed on main.
    tokens_cache_write_5m: 0,
    tokens_cache_write_1h: 0,
    cost_nanousd: 0,
    request_count: 0,
    error_count: 0,
    ...overrides,
  };
}

function usageEvent(overrides: Partial<UsageEvent> = {}): UsageEvent {
  return {
    id: 'ev-1',
    created_at: '2026-08-26T10:00:00Z',
    user_id: 'user-1',
    token_id: 'tok-1',
    model: 'claude-fable-5',
    endpoint_path: '/v1/chat/completions',
    http_method: 'POST',
    modality: 'chat',
    streaming: false,
    request_bytes: 100,
    response_bytes: 400,
    tokens_in: 100,
    tokens_out: 40,
    tokens_cached: 0,
    // Required on UsageEvent since the cache-write work landed on main.
    tokens_cache_write_5m: 0,
    tokens_cache_write_1h: 0,
    token_accounting_method: 'upstream_reported',
    cost_nanousd: 1_230_000,
    finish_reason: 'stop',
    http_status: 200,
    latency_ms: 900,
    ttfb_ms: 120,
    client_user_agent: '',
    client_ip: '',
    x_forwarded_for: '',
    error_code: '',
    quota_violated: false,
    request_id: 'req-1',
    ...overrides,
  };
}

function personalDashboard(): PersonalDashboard {
  return {
    range: 'day',
    start: '2026-08-26T00:00:00Z',
    end: '2026-08-27T00:00:00Z',
    totals: totals({ tokens_in: 4000, tokens_out: 2000, request_count: 51, cost_nanousd: 310_000_000 }),
    per_model: [],
    per_modality: [],
    per_token: [],
    series: [{ bucket: '2026-08-26T00:00:00Z', totals: totals({ request_count: 5 }) }],
    recent_requests: [usageEvent()],
  };
}

function globalDashboard(): GlobalDashboard {
  return {
    top_models: [],
    totals_30d: totals({ tokens_in: 4000, tokens_out: 2000, request_count: 51, cost_nanousd: 310_000_000 }),
    active_users_15m: 3,
    series: [],
    new_models: [],
    leaderboards_enabled: false,
  };
}

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
    // Required on Model since the per-model context window landed on main
    // (0 = unknown, matching the NOT NULL DEFAULT 0 column).
    context_window: 0,
    rate_in_nanousd: 10_000_000_000,
    rate_out_nanousd: 30_000_000_000,
    rate_cached_nanousd: 0,
    rate_cache_write_5m_nanousd: 0,
    rate_cache_write_1h_nanousd: 0,
    rate_effective_from: '0001-01-01T00:00:00Z',
    discovered_at: '2026-08-01T00:00:00Z',
    grant_count: 0,
    ...overrides,
  };
}

function renderAt(element: React.ReactElement, initialEntry = '/') {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[initialEntry]}>{element}</MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  localOnly = false;
});

describe('Dashboard — cost surfaces', () => {
  function mockDashboard() {
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/dashboard/personal')) return Promise.resolve(personalDashboard());
      if (path.startsWith('/api/v1/dashboard/quota')) return Promise.resolve({ quotas: [] });
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
  }

  it('flag off: Spend tile, Spend metric option, and Cost column render as today', async () => {
    mockDashboard();
    renderAt(<DashboardPage />, '/dashboard');

    expect((await screen.findAllByText('Spend')).length).toBeGreaterThan(0);
    expect(screen.getByRole('button', { name: 'Spend' })).toBeTruthy();
    expect(screen.getByRole('columnheader', { name: 'Cost' })).toBeTruthy();
    expect(screen.getByText('$0.001230')).toBeTruthy(); // the recent-request cost cell
  });

  it('flag on: no Spend tile, no Spend metric option, no Cost column — tokens/requests intact', async () => {
    localOnly = true;
    mockDashboard();
    renderAt(<DashboardPage />, '/dashboard');

    // Token metrics keep rendering.
    expect((await screen.findAllByText('Input tokens')).length).toBeGreaterThan(0);
    expect(screen.getByRole('button', { name: 'Requests' })).toBeTruthy();

    expect(screen.queryByText('Spend')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Spend' })).toBeNull();
    expect(screen.queryByRole('columnheader', { name: 'Cost' })).toBeNull();
    expect(screen.queryByText(/\$/)).toBeNull();
  });

  it('flag on: a stale ?metric=cost URL degrades to tokens instead of charting cost', async () => {
    localOnly = true;
    mockDashboard();
    renderAt(<DashboardPage />, '/dashboard?metric=cost');

    const tokensButton = await screen.findByRole('button', { name: 'Tokens' });
    expect(tokensButton.getAttribute('aria-pressed')).toBe('true');
  });
});

describe('Explore — spend tile and metric option', () => {
  beforeEach(() => {
    mocked.get.mockResolvedValue(globalDashboard());
  });

  it('flag off: the 30-day spend tile and Spend metric button render', async () => {
    renderAt(<ExplorePage />, '/explore');
    expect(await screen.findByText('Spend (30 days)')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Spend' })).toBeTruthy();
  });

  it('flag on: no spend tile, no Spend metric button — token tile intact', async () => {
    localOnly = true;
    renderAt(<ExplorePage />, '/explore');

    expect(await screen.findByText('Tokens (30 days)')).toBeTruthy();
    expect(screen.queryByText('Spend (30 days)')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Spend' })).toBeNull();
    expect(screen.queryByText(/\$/)).toBeNull();
  });
});

describe('Requests — cost column and detail row', () => {
  beforeEach(() => {
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/requests')) {
        return Promise.resolve({ requests: [usageEvent()], total_count: 1 });
      }
      if (path.startsWith('/api/v1/models')) return Promise.resolve({ models: [] });
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
  });

  it('flag off: the Cost column and detail row render as today', async () => {
    renderAt(<RequestsPage />, '/requests');

    expect(await screen.findByRole('columnheader', { name: /Cost/ })).toBeTruthy();
    fireEvent.click(
      screen
        .getAllByText('claude-fable-5')
        .find((el) => el.closest('tr'))!
        .closest('tr')!,
    );
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Cost')).toBeTruthy();
    expect(within(dialog).getByText('$0.001230')).toBeTruthy();
  });

  it('flag on: no Cost column, no cost detail row — tokens still shown', async () => {
    localOnly = true;
    renderAt(<RequestsPage />, '/requests');

    expect(await screen.findByRole('columnheader', { name: /Tokens in/ })).toBeTruthy();
    expect(screen.getByRole('columnheader', { name: /Tokens out/ })).toBeTruthy();
    expect(screen.queryByRole('columnheader', { name: /Cost/ })).toBeNull();

    fireEvent.click(
      screen
        .getAllByText('claude-fable-5')
        .find((el) => el.closest('tr'))!
        .closest('tr')!,
    );
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Input tokens')).toBeTruthy();
    expect(within(dialog).queryByText('Cost')).toBeNull();
    expect(within(dialog).queryByText(/\$/)).toBeNull();
  });
});

describe('User model catalog — per-model rates', () => {
  beforeEach(() => {
    mocked.get.mockResolvedValue({ models: [modelFixture({ status: 'enabled' })] });
  });

  it('flag off: rate rows and the price sort render as today', async () => {
    renderAt(<UserModelsPage />, '/models');
    expect(await screen.findByText('claude-fable-5')).toBeTruthy();
    expect(screen.getByText('Input')).toBeTruthy();
    expect(screen.getByRole('option', { name: 'Sort: most expensive' })).toBeTruthy();
  });

  it('flag on: no rates, no price sort — access and modality info intact', async () => {
    localOnly = true;
    renderAt(<UserModelsPage />, '/models');

    expect(await screen.findByText('claude-fable-5')).toBeTruthy();
    expect(screen.queryByText('Input')).toBeNull();
    expect(screen.queryByRole('option', { name: 'Sort: most expensive' })).toBeNull();
    expect(screen.queryByText(/\$/)).toBeNull();
    expect(screen.getAllByText('Chat').length).toBeGreaterThan(0);
  });
});

describe('Admin overview — spend tile, metric option, top spenders', () => {
  beforeEach(() => {
    mocked.get.mockResolvedValue({
      range: 'month',
      totals: totals({ tokens_in: 100, request_count: 10, cost_nanousd: 5_000_000_000 }),
      series: [],
      top_spenders: [{ key: 'u1', label: 'Ada', totals: totals({ cost_nanousd: 5_000_000_000 }) }],
      top_models: [],
      by_status: [],
      model_counts: {},
      near_breach: [],
      user_count: 4,
      active_users_15m: 2,
    });
  });

  it('flag off: Spend tile, Spend metric option, and Top spenders render', async () => {
    renderAt(<AdminOverview />, '/admin');
    expect((await screen.findAllByText('Spend')).length).toBeGreaterThan(0);
    expect(screen.getByRole('button', { name: 'Spend' })).toBeTruthy();
    // main renamed this card's heading to 'Top users' when it switched the
    // leaderboard from cost to output-token volume (adminOverview.topSpenders).
    expect(screen.getByText('Top users')).toBeTruthy();
  });

  it('flag on: no spend tile, no Spend metric option, no Top spenders — requests default', async () => {
    localOnly = true;
    renderAt(<AdminOverview />, '/admin');

    expect(await screen.findByText('Accounts')).toBeTruthy();
    expect(screen.queryByText('Spend')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Spend' })).toBeNull();
    expect(screen.queryByText('Top users')).toBeNull();
    expect(screen.queryByText(/\$/)).toBeNull();
    // The default cost metric degrades to requests.
    expect(screen.getByRole('button', { name: 'Requests' }).getAttribute('aria-pressed')).toBe('true');
  });
});

describe('Admin models — pricing columns, Rates action, enable gate', () => {
  beforeEach(() => {
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/admin/models')) {
        return Promise.resolve({ models: [modelFixture()], counts: {}, modalities: [] });
      }
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
    mocked.patch.mockResolvedValue({ model: modelFixture() });
  });

  it('flag off: rate columns and the Rates button render as today', async () => {
    renderAt(<AdminModelsPage />, '/admin/models');
    expect(await screen.findByRole('columnheader', { name: 'Input rate' })).toBeTruthy();
    expect(screen.getByRole('columnheader', { name: 'Output rate' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Edit claude-fable-5' })).toBeTruthy();
  });

  it('flag on: no rate columns, no Rates button, and enabling skips the pricing gate', async () => {
    localOnly = true;
    const user = userEvent.setup();
    renderAt(<AdminModelsPage />, '/admin/models');

    expect(await screen.findByText('claude-fable-5')).toBeTruthy();
    expect(screen.queryByRole('columnheader', { name: 'Input rate' })).toBeNull();
    expect(screen.queryByRole('columnheader', { name: 'Output rate' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Rates' })).toBeNull();
    expect(screen.queryByText(/\$/)).toBeNull();

    // A never-priced model enables directly: pricing is meaningless here.
    await user.click(screen.getByRole('button', { name: 'Enable' }));
    expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/models/m-1', { status: 'enabled' });
    expect(screen.queryByRole('dialog')).toBeNull();
  });
});

describe('Admin quotas — USD metric option in the create dialog', () => {
  function mockQuotas(metrics: Array<{ value: string; label: string }>) {
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/admin/quotas')) {
        return Promise.resolve({
          quotas: [],
          metrics,
          windows: [{ value: 'daily', label: 'per day' }],
        });
      }
      if (path.startsWith('/api/v1/admin/users')) return Promise.resolve({ users: [] });
      if (path.startsWith('/api/v1/admin/teams')) return Promise.resolve({ teams: [] });
      if (path.startsWith('/api/v1/admin/models')) return Promise.resolve({ models: [] });
      if (path.startsWith('/api/v1/admin/rate-limits')) {
        return Promise.resolve({ rules: [], enforcement_enabled: true });
      }
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
  }

  const allMetrics = [
    { value: 'tokens_in', label: 'Input tokens' },
    { value: 'tokens_out', label: 'Output tokens' },
    { value: 'cost_usd', label: 'Spend (USD)' },
    { value: 'requests', label: 'Requests' },
  ];

  it('flag off: the metric picker offers Spend (USD)', async () => {
    mockQuotas(allMetrics);
    const user = userEvent.setup();
    renderAt(<AdminQuotasPage />, '/admin/quotas');

    await user.click(await screen.findByRole('button', { name: 'New quota' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('option', { name: 'Spend (USD)' })).toBeTruthy();
  });

  it('flag on: no USD option, even when a stale response still contains it', async () => {
    localOnly = true;
    // The server filters the option itself; feeding the unfiltered list
    // proves the client-side defense holds against cached responses.
    mockQuotas(allMetrics);
    const user = userEvent.setup();
    renderAt(<AdminQuotasPage />, '/admin/quotas');

    await user.click(await screen.findByRole('button', { name: 'New quota' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).queryByRole('option', { name: 'Spend (USD)' })).toBeNull();
    expect(within(dialog).getByRole('option', { name: 'Input tokens' })).toBeTruthy();
    expect(within(dialog).getByRole('option', { name: 'Requests' })).toBeTruthy();
  });
});
