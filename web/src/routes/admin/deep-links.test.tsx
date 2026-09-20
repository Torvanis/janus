/**
 * Deep-link tests for the admin detail drawers (inventory A03/A07): opening
 * /admin/users/:id or /admin/upstreams/:id directly — as after a refresh or
 * from a shared link — must render the corresponding detail drawer, and
 * closing it must return to the list route. The drawers used to live in
 * component state, so a refresh silently dropped them.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import { PeoplePage } from './People';
import { UpstreamsPage } from './Upstreams';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

const adaUser = {
  id: 'user-1',
  email: 'ada@example.com',
  name: 'Ada Lovelace',
  role: 'user',
  is_active: true,
  groups: ['ml-platform'],
  last_login_at: '2025-01-02T10:00:00Z',
  spend_30d_usd: 1.25,
  tokens_out_30d: 1234567,
};

const emptyTotals = { tokens_in: 0, tokens_out: 0, tokens_cached: 0, cost_nanousd: 0, request_count: 0, error_count: 0 };

function usagePoint(bucket: string, overrides: Partial<typeof emptyTotals> = {}) {
  return { bucket, totals: { ...emptyTotals, ...overrides } };
}

const activeUsageSeries = [
  usagePoint('2025-01-01T00:00:00Z', { tokens_in: 100, tokens_out: 40, cost_nanousd: 1230000, request_count: 3 }),
  usagePoint('2025-01-02T00:00:00Z', { tokens_in: 50, tokens_out: 20, cost_nanousd: 450000, request_count: 1 }),
];

const userDetail = {
  user: adaUser,
  groups: ['ml-platform'],
  teams: [],
  effective_grants: [],
  tokens: [],
  quotas: [],
  recent_requests: [
    {
      id: 'req-1',
      model: 'gpt-4o',
      http_status: 200,
      created_at: '2025-01-02T09:59:00Z',
      cost_nanousd: 1230000,
      latency_ms: 812,
    },
    { id: 'req-2', model: 'gpt-4o-mini', http_status: 429, created_at: '2025-01-02T09:58:00Z', cost_nanousd: 0, latency_ms: 12 },
  ],
  usage_range: 'month',
  usage_series: activeUsageSeries,
  usage_totals: { ...emptyTotals, tokens_in: 150, tokens_out: 60, cost_nanousd: 1680000, request_count: 4 },
};

const primaryUpstream = {
  id: 'up-1',
  name: 'OpenAI production',
  adapter_type: 'openai_compatible',
  base_url: 'https://api.openai.com',
  api_key_mask: 'sk-…abcd',
  enabled: true,
  last_check_at: '2025-01-02T10:00:00Z',
  last_latency_ms: 42,
  last_error: '',
  model_count: 3,
};

function renderAt(initialEntry: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[initialEntry]}>
          <Routes>
            <Route path="/admin/users" element={<PeoplePage />} />
            <Route path="/admin/users/:id" element={<PeoplePage />} />
            <Route path="/admin/upstreams" element={<UpstreamsPage />} />
            <Route path="/admin/upstreams/:id" element={<UpstreamsPage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocked.get.mockImplementation((path: string) => {
    if (path.startsWith('/api/v1/admin/users/')) return Promise.resolve(userDetail);
    if (path.startsWith('/api/v1/admin/users')) {
      return Promise.resolve({ users: [adaUser], total_count: 1 });
    }
    // The users tab's group/team filter dropdowns fetch both rosters.
    if (path.startsWith('/api/v1/admin/groups')) return Promise.resolve({ groups: [] });
    if (path.startsWith('/api/v1/admin/teams')) return Promise.resolve({ teams: [] });
    if (path.startsWith('/api/v1/admin/upstreams')) {
      return Promise.resolve({ upstreams: [primaryUpstream], adapter_types: ['openai_compatible'] });
    }
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
});

describe('A07 /admin/users/:id deep link', () => {
  it('renders the user detail drawer directly from the URL', async () => {
    renderAt('/admin/users/user-1');

    // The drawer fetches and shows the addressed user, not just the list.
    await waitFor(() => expect(mocked.get).toHaveBeenCalledWith('/api/v1/admin/users/user-1'));
    const drawer = await screen.findByRole('dialog');
    expect(await within(drawer).findByText('ada@example.com')).toBeTruthy();

    // the admin drawer surfaces the user's recent requests.
    expect(await within(drawer).findByText('Recent requests')).toBeTruthy();
    expect(within(drawer).getAllByText(/gpt-4o/)).toHaveLength(2);
    expect(within(drawer).getByText('429')).toBeTruthy();
  });

  it('opens the drawer via navigation from a row and closes back to the list route', async () => {
    const user = userEvent.setup();
    renderAt('/admin/users');

    await user.click(await screen.findByRole('button', { name: /Ada Lovelace/ }));
    expect(await screen.findByRole('dialog')).toBeTruthy();
    await waitFor(() => expect(mocked.get).toHaveBeenCalledWith('/api/v1/admin/users/user-1'));

    await user.click(screen.getByRole('button', { name: 'Close panel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('switching section drops the open :id so the drawer does not reopen on return', async () => {
    const user = userEvent.setup();
    renderAt('/admin/users/user-1');

    expect(await screen.findByRole('dialog')).toBeTruthy();

    // Move to the Groups tab: the drawer closes with the :id gone from the URL…
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/admin/groups')) return Promise.resolve({ groups: [] });
      if (path.startsWith('/api/v1/admin/teams')) return Promise.resolve({ teams: [] });
      if (path.startsWith('/api/v1/admin/users/')) return Promise.resolve(userDetail);
      if (path.startsWith('/api/v1/admin/users')) return Promise.resolve({ users: [adaUser], total_count: 1 });
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
    await user.click(screen.getByRole('link', { name: 'Groups' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());

    // …so returning to Users shows the list without a resurrected drawer.
    await user.click(screen.getByRole('link', { name: 'Users' }));
    expect(await screen.findByRole('button', { name: /Ada Lovelace/ })).toBeTruthy();
    expect(screen.queryByRole('dialog')).toBeNull();
  });
});

describe('user detail drawer usage graph', () => {
  it('renders the usage-over-time chart from the API series with a working metric toggle', async () => {
    const user = userEvent.setup();
    renderAt('/admin/users/user-1');

    const drawer = await screen.findByRole('dialog');
    expect(await within(drawer).findByText('Usage over time')).toBeTruthy();

    // Default metric is spend; the chart is an SVG figure labelled "<metric> trend".
    expect(await within(drawer).findByRole('img', { name: 'Spend trend' })).toBeTruthy();

    // The toggle switches the same chart across requests, tokens, and spend.
    const toggle = within(drawer).getByRole('group', { name: 'Metric' });
    await user.click(within(toggle).getByRole('button', { name: 'Requests' }));
    expect(await within(drawer).findByRole('img', { name: 'Requests trend' })).toBeTruthy();
    await user.click(within(toggle).getByRole('button', { name: 'Tokens' }));
    expect(await within(drawer).findByRole('img', { name: 'Tokens trend' })).toBeTruthy();
    await user.click(within(toggle).getByRole('button', { name: 'Spend' }));
    expect(await within(drawer).findByRole('img', { name: 'Spend trend' })).toBeTruthy();
  });

  it('shows an empty state instead of a chart when the user has no usage in the window', async () => {
    const idleDetail = {
      ...userDetail,
      usage_series: [usagePoint('2025-01-01T00:00:00Z'), usagePoint('2025-01-02T00:00:00Z')],
      usage_totals: emptyTotals,
    };
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/admin/users/')) return Promise.resolve(idleDetail);
      if (path.startsWith('/api/v1/admin/users')) return Promise.resolve({ users: [adaUser], total_count: 1 });
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
    renderAt('/admin/users/user-1');

    const drawer = await screen.findByRole('dialog');
    expect(await within(drawer).findByText('Usage over time')).toBeTruthy();
    expect(await within(drawer).findByText('No activity in this range yet.')).toBeTruthy();
    // No misleading flat-line chart is drawn for an idle user.
    expect(within(drawer).queryByRole('img', { name: /trend/ })).toBeNull();

    // Every pre-existing drawer section still renders alongside the empty state.
    expect(within(drawer).getByText('ada@example.com')).toBeTruthy();
    expect(within(drawer).getByText('Recent requests')).toBeTruthy();
  });
});

describe('A03 /admin/upstreams/:id deep link', () => {
  it('renders the upstream edit drawer directly from the URL', async () => {
    renderAt('/admin/upstreams/up-1');

    expect(await screen.findByRole('dialog')).toBeTruthy();
    expect(await screen.findByText('Edit OpenAI production')).toBeTruthy();
    // The form is populated from the addressed upstream.
    expect((screen.getByDisplayValue('https://api.openai.com') as HTMLInputElement).value).toBe('https://api.openai.com');
  });

  it('opens the drawer via navigation from a row and closes back to the list route', async () => {
    const user = userEvent.setup();
    renderAt('/admin/upstreams');

    await user.click(await screen.findByRole('button', { name: 'OpenAI production' }));
    expect(await screen.findByText('Edit OpenAI production')).toBeTruthy();

    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByText('Edit OpenAI production')).toBeNull());
  });
});
