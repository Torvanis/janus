/**
 * Model catalog — 7-day activity and speed on each card: jobs per day with a
 * trend arrow against the previous seven days, and the typical generation
 * speed. Both show "—" when there is nothing to measure, never a fake zero.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../lib/api';
import type { ManagedModel, Me, Model, ModelActivity } from '../lib/types';
import { ToastProvider } from '../components/ui';
import { ModelsPage } from './Models';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});

const me: Me = {
  id: 'user-1',
  email: 'ada@example.com',
  name: 'Ada',
  role: 'user',
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
  return { ...actual, useLocalOnly: () => false, useMe: () => me };
});

const mocked = vi.mocked(api);

function activity(overrides: Partial<ModelActivity> = {}): ModelActivity {
  return {
    jobs_per_day_7d: 0,
    jobs_per_day_prev_7d: 0,
    jobs_trend: 'flat',
    jobs_change_percent: 0,
    tokens_per_second_7d: 0,
    speed_samples_7d: 0,
    ...overrides,
  };
}

function model(id: string, name: string, act: ModelActivity | undefined): Model {
  return {
    id,
    upstream_id: 'up-1',
    upstream_name: 'vLLM',
    adapter_type: 'openai_compatible',
    name,
    display_name: '',
    status: 'enabled',
    modalities: ['chat'],
    context_window: 262_144,
    rate_in_nanousd: 0,
    rate_out_nanousd: 0,
    rate_cached_nanousd: 0,
    rate_cache_write_5m_nanousd: 0,
    rate_cache_write_1h_nanousd: 0,
    rate_effective_from: '0001-01-01T00:00:00Z',
    discovered_at: '2026-08-01T00:00:00Z',
    grant_count: 1,
    grant_source: 'direct',
    request_count_10m: 0,
    error_count_10m: 0,
    error_rate_percent: 0,
    upstream_reachable: true,
    upstream_last_check_at: new Date().toISOString(),
    upstream_last_error: '',
    upstream_last_latency_ms: 4,
    health: 'healthy',
    activity: act,
  } as Model;
}

const busy = model(
  'm-busy',
  'busy-model',
  activity({ jobs_per_day_7d: 2706.4, jobs_per_day_prev_7d: 1200, jobs_trend: 'up', jobs_change_percent: 126, tokens_per_second_7d: 45.2, speed_samples_7d: 10406 }),
);
const quieter = model(
  'm-quiet',
  'quiet-model',
  activity({ jobs_per_day_7d: 4.3, jobs_per_day_prev_7d: 9, jobs_trend: 'down', jobs_change_percent: -52, tokens_per_second_7d: 3.6, speed_samples_7d: 4 }),
);
const steady = model('m-steady', 'steady-model', activity({ jobs_per_day_7d: 10, jobs_per_day_prev_7d: 10.4, jobs_trend: 'flat', jobs_change_percent: -4 }));
const brandNew = model('m-new', 'new-model', activity({ jobs_per_day_7d: 0.1, jobs_trend: 'new' }));
const idle = model('m-idle', 'idle-model', undefined);

const alias = {
  id: 'mm-1',
  name: 'fast',
  description: '',
  target_model_id: 'm-busy',
  target_public_name: 'busy-model',
  target_upstream_name: 'vLLM',
  modalities: ['chat'],
  context_window: 262_144,
  servable: true,
  broken: false,
  grant_source: 'all users',
  activity: busy.activity,
} as unknown as ManagedModel;

function renderAt(entry = '/models') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[entry]}>
          <ModelsPage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

function card(name: string): HTMLElement {
  return screen.getByRole('button', { name }) as HTMLElement;
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocked.get.mockResolvedValue({ models: [busy, quieter, steady, brandNew, idle], managed_models: [alias] });
});

describe('model card 7-day activity', () => {
  it('shows jobs/day with an up arrow and the generation speed', async () => {
    renderAt();
    await screen.findByRole('heading', { name: 'busy-model' });
    const c = card('busy-model');
    const jobs = within(c).getByTestId('model-jobs-per-day');
    expect(jobs.textContent).toContain('2,706');
    const arrow = within(jobs).getByRole('img');
    expect(arrow.textContent).toBe('▲');
    expect(arrow.getAttribute('aria-label')).toBe('Up 126% vs the previous 7 days (1,200/day)');
    expect(within(c).getByTestId('model-speed').textContent).toContain('45 tok/s');
  });

  it('shows a down arrow, one decimal for small values, and a flat dash within ±10%', async () => {
    renderAt();
    await screen.findByText('quiet-model');
    const quiet = within(card('quiet-model')).getByTestId('model-jobs-per-day');
    expect(quiet.textContent).toContain('4.3');
    expect(within(quiet).getByRole('img').textContent).toBe('▼');
    expect(within(card('quiet-model')).getByTestId('model-speed').textContent).toContain('3.6 tok/s');
    const flat = within(card('steady-model')).getByTestId('model-jobs-per-day');
    expect(within(flat).getByRole('img').textContent).toBe('–');
    expect(within(flat).getByRole('img').getAttribute('aria-label')).toContain('About the same');
  });

  it('labels a model with no baseline as new, and shows — when nothing was measured', async () => {
    renderAt();
    await screen.findByText('new-model');
    expect(within(card('new-model')).getByTestId('model-jobs-per-day').textContent).toContain('new');
    expect(within(card('new-model')).getByTestId('model-speed').textContent).toContain('—');
    expect(within(card('idle-model')).getByTestId('model-jobs-per-day').textContent).toContain('—');
    expect(within(card('idle-model')).getByTestId('model-speed').textContent).toContain('—');
  });

  it("gives a managed model its target's figures", async () => {
    renderAt();
    const aliasCard = (await screen.findByRole('article', { name: 'fast' })) as HTMLElement;
    expect(within(aliasCard).getByTestId('model-jobs-per-day').textContent).toContain('2,706');
    expect(within(aliasCard).getByTestId('model-speed').textContent).toContain('45 tok/s');
  });

  it('adds jobs/day and speed columns to the table view', async () => {
    renderAt('/models?view=table');
    await screen.findByRole('table');
    const row = screen.getAllByText('busy-model').map((el) => el.closest('tr')!).find((tr) => tr.dataset.clickable === 'true')!;
    expect(within(row).getByText('45 tok/s')).toBeTruthy();
    expect(row.textContent).toContain('2,706');
  });
});
