/**
 * the team dashboard has NO per-graph metric
 * selector — a deliberate scope decision — so its two BarList charts (model
 * mix and member contribution) simply follow the instance-wide emphasis
 * setting surfaced through /api/v1/me feature_flags: spend_emphasis on →
 * charts show spend (USD), off (the shipped usage-emphasis default) →
 * charts show tokens (in + out).
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../lib/api';
import type { Me, Totals } from '../lib/types';
import { SessionProvider } from '../app/session';
import { TeamPage } from './Team';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);
it('renders request counts when Requests is selected in local-only mode', async () => {
  renderTeamWithFlags({ local_only: true });
  fireEvent.change(await screen.findByLabelText('Chart metric'), { target: { value: 'requests' } });
  expect(await screen.findByText('30')).toBeTruthy();
  expect(screen.queryByText('4.2k')).toBeNull();
  expect(screen.queryByRole('option', { name: 'Recorded cost' })).toBeNull();
});

function totals(overrides: Partial<Totals> = {}): Totals {
  return {
    tokens_in: 0,
    tokens_out: 0,
    tokens_cached: 0,
    // Required since the cache-hit-rate work (imp_AmUO7AfYqoAjGWs0) made the
    // prompt-cache write buckets part of the canonical Totals contract.
    tokens_cache_write_5m: 0,
    tokens_cache_write_1h: 0,
    cost_nanousd: 0,
    request_count: 0,
    error_count: 0,
    ...overrides,
  };
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
    local_only: flags.local_only === true,
    feature_flags: flags,
    endpoint: 'https://janus.example.com/v1',
  };
}

const team = {
  id: 't1',
  name: 'Data Science',
  lead_user_id: 'u9',
  lead_name: 'Lee',
  lead_can_edit_quotas: false,
  member_count: 2,
};

// Tile totals are deliberately tiny so the tile values ("15", "$0.99") can
// never collide with the per-model / per-member chart values under test.
const teamDashboard = {
  team,
  teams: [team],
  range: 'week',
  totals: totals({ tokens_in: 10, tokens_out: 5, cost_nanousd: 990_000_000, request_count: 40 }),
  per_model: [
    {
      key: 'm1',
      label: 'claude-fable-5',
      // tokens: 3,000 + 1,200 = 4,200 · spend: 250,000,000 nanoUSD = $0.25
      totals: totals({ tokens_in: 3000, tokens_out: 1200, cost_nanousd: 250_000_000, request_count: 30 }),
    },
  ],
  per_member: [
    {
      key: 'u2',
      label: 'Robin',
      // tokens: 1,000 + 500 = 1,500 · spend: 120,000,000 nanoUSD = $0.12
      totals: totals({ tokens_in: 1000, tokens_out: 500, cost_nanousd: 120_000_000, request_count: 10 }),
    },
  ],
  can_see_member_detail: true,
  member_count: 2,
};

/** Renders the page inside a real SessionProvider so /api/v1/me feature_flags drive the chart metric. */
function renderTeamWithFlags(
  flags: Record<string, boolean>,
  detail: Record<string, unknown> = { membership_role: 'member', actor_role: 'member', can_manage: false },
  leadTeams: unknown[] = [],
  workspace: 'personal' | 'administration' = 'personal',
) {
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
    if (path === '/api/v1/teams/t1') return Promise.resolve({ team, ...detail });
    if (path.startsWith('/api/v1/dashboard/team')) return Promise.resolve(teamDashboard);
    if (path === '/api/v1/lead/teams') return Promise.resolve({ teams: leadTeams, metrics: [], windows: [] });
    return Promise.reject(new Error(`unexpected GET ${path}`));
  }) as typeof api.get);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <SessionProvider>
        <MemoryRouter initialEntries={['/teams/t1']}>
          <TeamPage workspace={workspace} />
        </MemoryRouter>
      </SessionProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('usage-first team navigation', () => {
  it('shows delegated quota status without placing quota mutation forms in the usage dashboard', async () => {
    renderTeamWithFlags({}, { membership_role: 'leader', actor_role: 'leader', can_manage: true }, [
      { id: 't1', name: 'Data Science', can_edit_quotas: true, quotas: [], member_count: 2 },
    ]);
    await screen.findByRole('heading', { name: /quotas/i });
    expect(screen.queryByRole('button', { name: /add quota/i })).toBeNull();
    expect(screen.queryByRole('spinbutton')).toBeNull();
  });
  it('offers explicit metric selection while keeping the configured default', async () => {
    renderTeamWithFlags({ spend_emphasis: false });
    expect(await screen.findByText('4,200')).toBeTruthy();
    fireEvent.change(screen.getByRole('combobox', { name: 'Chart metric' }), { target: { value: 'requests' } });
    expect(screen.getByText('30')).toBeTruthy();
    expect(screen.getByRole('heading', { name: 'Requests by model' })).toBeTruthy();
    expect(screen.queryByText('4,200')).toBeNull();
  });
  it('keeps personal usage informational for admin members despite management permissions', async () => {
    renderTeamWithFlags({}, { membership_role: 'member', actor_role: 'admin', can_manage: true, pending_request_count: 2 });
    await screen.findByText('Team membership: Member');
    expect(screen.queryByRole('link', { name: '2 requests waiting' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Manage team' })).toBeNull();
    expect(screen.getByText('Team membership: Member')).toBeTruthy();
    expect(screen.getByText('Organization administrator')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Add member' })).toBeNull();
  });
  it('keeps manager controls and pending counts out of ordinary members view', async () => {
    renderTeamWithFlags({});
    await screen.findByText('4,200');
    expect(screen.queryByRole('link', { name: 'Manage team' })).toBeNull();
    expect(screen.queryByText(/requests waiting/)).toBeNull();
  });
});

describe('team charts follow the instance-wide emphasis setting', () => {
  it('shows token values (in + out) when usage is emphasised (spend_emphasis off)', async () => {
    renderTeamWithFlags({ spend_emphasis: false });

    // Model mix: 3,000 in + 1,200 out.
    expect(await screen.findByText('4,200')).toBeTruthy();
    // Member contribution: 1,000 in + 500 out.
    expect(screen.getByText('1,500')).toBeTruthy();

    // No spend values rendered in the charts.
    expect(screen.queryByText('$0.25')).toBeNull();
    expect(screen.queryByText('$0.12')).toBeNull();
  });

  it('shows spend (USD) values when the instance emphasises spend (flag on)', async () => {
    renderTeamWithFlags({ spend_emphasis: true });

    // Model mix: 250,000,000 nanoUSD.
    expect(await screen.findByText('$0.25')).toBeTruthy();
    // Member contribution: 120,000,000 nanoUSD.
    expect(screen.getByText('$0.12')).toBeTruthy();

    // No token totals rendered in the charts.
    expect(screen.queryByText('4,200')).toBeNull();
    expect(screen.queryByText('1,500')).toBeNull();
  });
});
