/**
 * Load-balanced pools in the managed-model dialog: building a pool, choosing
 * the context-aware policy, the payload sent, the license upsell, and the
 * live per-server health panel.
 */
import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router-dom';
import { ManagedModelsPage } from './ManagedModels';
import { ToastProvider } from '../../components/ui';
import { SessionContext } from '../../app/session';
import type { Me } from '../../lib/types';

const models = [
  { id: 'm-1', name: 'qwen3.8-27b', status: 'enabled', upstream_name: 'vllm-a', context_window: 32768 },
  { id: 'm-2', name: 'qwen3.8-27b', status: 'enabled', upstream_name: 'vllm-b', context_window: 32768 },
  { id: 'm-3', name: 'qwen3.8-27b', status: 'enabled', upstream_name: 'llama-a', context_window: 32768 },
];

const baseRow = {
  id: 'mm-1',
  name: 'qwen',
  description: '',
  status: 'enabled' as const,
  target_model_id: 'm-1',
  target_model_name: 'qwen3.8-27b',
  target_public_name: 'qwen3.8-27b',
  target_status: 'enabled',
  target_upstream_id: 'up-1',
  target_upstream_name: 'vllm-a',
  modalities: ['text'],
  context_window: 32768,
  servable: true,
  broken: false,
  fallback_model_id: '',
  fallback_triggers: [],
  fallback_broken: false,
  grant_count: 1,
  created_by_user_id: 'u-1',
  created_at: '2026-09-01T00:00:00Z',
  updated_at: '2026-09-01T00:00:00Z',
  spend_30d_usd: 0,
  requests_30d: 10,
  tokens_in_30d: 100,
  tokens_out_30d: 50,
  errors_30d: 0,
  distinct_principals_30d: 1,
};

const pooledRow = {
  ...baseRow,
  pool: {
    policy: 'context',
    affinity: 'bounded',
    spill_pct: 25,
    members: [
      {
        model_id: 'm-1',
        weight: 1,
        priority: 0,
        enabled: true,
        context_capacity: 0,
        public_name: 'qwen3.8-27b',
        upstream_name: 'vllm-a',
      },
      {
        model_id: 'm-2',
        weight: 1,
        priority: 0,
        enabled: true,
        context_capacity: 0,
        public_name: 'qwen3.8-27b',
        upstream_name: 'vllm-b',
      },
    ],
  },
};

type Call = { method: string; url: string; body: unknown };

function mockFetch(routes: Record<string, unknown>, calls: Call[]) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString();
    const method = (init?.method ?? 'GET').toUpperCase();
    calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    const key = Object.keys(routes)
      .sort((a, b) => b.length - a.length)
      .find((k) => {
        const [m, path] = k.split(' ');
        return m === method && url.startsWith(path ?? '');
      });
    const body = key ? routes[key] : {};
    return {
      ok: true,
      status: method === 'POST' ? 201 : 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => body,
      text: async () => JSON.stringify(body),
    } as Response;
  });
}

function renderPage(features: string[]) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const me = { license: { edition: features.length ? 'business' : 'community', features, status: 'valid' } } as unknown as Me;
  return render(
    <QueryClientProvider client={client}>
      <SessionContext.Provider value={{ me, loading: false, refresh: async () => {} } as never}>
        <ToastProvider>
          <MemoryRouter>
            <ManagedModelsPage />
          </MemoryRouter>
        </ToastProvider>
      </SessionContext.Provider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe('Managed model pools', () => {
  it('builds a context-aware pool and sends the member list', async () => {
    const calls: Call[] = [];
    vi.stubGlobal(
      'fetch',
      mockFetch(
        {
          'GET /api/v1/admin/managed-models': { managed_models: [baseRow], total_count: 1 },
          'GET /api/v1/admin/models': { models },
          'PATCH /api/v1/admin/managed-models/mm-1': { managed_model: pooledRow },
        },
        calls,
      ),
    );
    renderPage(['load_balancing', 'model_fallbacks']);
    const row = (await screen.findByText('qwen')).closest('tr')!;
    await userEvent.click(within(row).getByRole('button', { name: 'Edit' }));
    const editor = await screen.findByTestId('pool-editor');
    await waitFor(() => expect(within(editor).getAllByRole('option').length).toBeGreaterThan(1));
    await userEvent.selectOptions(within(editor).getByTestId('pool-add-select'), 'm-2');
    await userEvent.click(within(editor).getByRole('button', { name: 'Add a model' }));
    // With two members the single target picker gives way to the member list.
    expect(screen.queryByRole('combobox', { name: /Points at/ })).toBeNull();
    await userEvent.click(within(editor).getByRole('radio', { name: /Context aware/ }));
    // The context policy exposes a per-server capacity override.
    expect(within(editor).getAllByRole('spinbutton', { name: /Context capacity/ })).toHaveLength(2);
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(calls.some((c) => c.method === 'PATCH')).toBe(true));
    const patch = calls.find((c) => c.method === 'PATCH')!.body as Record<string, unknown>;
    expect(patch.target_model_id).toBeUndefined();
    expect(patch.pool).toMatchObject({
      policy: 'context',
      affinity: 'bounded',
      members: [
        { model_id: 'm-1', enabled: true },
        { model_id: 'm-2', enabled: true },
      ],
    });
  });

  it('shows the upsell and refuses to save a new pool without the license', async () => {
    const calls: Call[] = [];
    vi.stubGlobal(
      'fetch',
      mockFetch(
        {
          'GET /api/v1/admin/managed-models': { managed_models: [baseRow], total_count: 1 },
          'GET /api/v1/admin/models': { models },
        },
        calls,
      ),
    );
    renderPage([]);
    const row = (await screen.findByText('qwen')).closest('tr')!;
    await userEvent.click(within(row).getByRole('button', { name: 'Edit' }));
    const editor = await screen.findByTestId('pool-editor');
    expect(within(editor).getByText(/Load balancing across several models is a Business feature/)).toBeTruthy();
    expect((within(editor).getByTestId('pool-add-select') as HTMLSelectElement).disabled).toBe(true);
  });

  it('badges pools in the table and shows live server health in the editor', async () => {
    const calls: Call[] = [];
    vi.stubGlobal(
      'fetch',
      mockFetch(
        {
          'GET /api/v1/admin/managed-models/mm-1/pool-health': {
            checked_at: '2026-09-30T00:00:00Z',
            members: [
              {
                model_id: 'm-1',
                public_name: 'qwen3.8-27b',
                upstream_name: 'vllm-a',
                enabled: true,
                state: 'serving',
                load: 'live',
                engine: 'vllm',
                running: 3,
                waiting: 1,
                in_flight: 0,
                kv_usage: 0.4,
                context_used: 52_000,
                context_capacity: 130_000,
                prefix_hit_rate: 0.82,
              },
              {
                model_id: 'm-2',
                public_name: 'qwen3.8-27b',
                upstream_name: 'vllm-b',
                enabled: true,
                state: 'ejected',
                load: 'estimated',
                running: 0,
                waiting: 0,
                in_flight: 0,
                kv_usage: 0,
                context_used: 0,
                context_capacity: 0,
                ejected_until: '2099-01-01T00:00:00Z',
                ejected_reason: 'HTTP 503',
              },
            ],
          },
          'GET /api/v1/admin/managed-models': { managed_models: [pooledRow], total_count: 1 },
          'GET /api/v1/admin/models': { models },
        },
        calls,
      ),
    );
    renderPage(['load_balancing']);
    const row = (await screen.findByText('qwen')).closest('tr')!;
    expect(within(row).getByText('Pool of 2')).toBeTruthy();
    await userEvent.click(within(row).getByRole('button', { name: 'Edit' }));
    const health = await screen.findByTestId('pool-health');
    await waitFor(() => expect(within(health).getByText('Serving')).toBeTruthy());
    expect(within(health).getByText('Taken out')).toBeTruthy();
    expect(within(health).getByText(/Context 40% used/)).toBeTruthy();
    expect(within(health).getByText(/Cache hits 82%/)).toBeTruthy();
    expect(within(health).getByText(/only some report live load/)).toBeTruthy();
  });
});

describe('Managed model editor layout', () => {
  it('opens as a wide side panel with its actions pinned in the footer', async () => {
    vi.stubGlobal(
      'fetch',
      mockFetch(
        {
          'GET /api/v1/admin/managed-models': { managed_models: [pooledRow], total_count: 1 },
          'GET /api/v1/admin/models': { models },
        },
        [],
      ),
    );
    renderPage(['load_balancing', 'model_fallbacks']);
    const row = (await screen.findByText('qwen')).closest('tr')!;
    await userEvent.click(within(row).getByRole('button', { name: 'Edit' }));
    const panel = await screen.findByRole('dialog', { name: /qwen/ });
    expect(panel.tagName).toBe('ASIDE');
    expect(panel.classList.contains('drawer')).toBe(true);
    expect(panel.classList.contains('drawer-wide')).toBe(true);
    const footer = panel.querySelector('footer.drawer-footer');
    expect(footer).not.toBeNull();
    expect(within(footer as HTMLElement).getByRole('button', { name: 'Save' })).toBeTruthy();
  });
});
