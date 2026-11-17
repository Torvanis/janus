/**
 * Model pop-up — time to first token, output size and cache hit rate over the
 * last 7 days, read from GET /api/v1/models/{id}/performance (a managed model
 * asks with its own id; the server answers for the target). A model that does
 * not report cached tokens says so instead of showing 0%.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../lib/api';
import type { ManagedModel, Me, Model, ModelPerformanceSummary } from '../lib/types';
import { ToastProvider } from '../components/ui';
import { ModelsPage } from './Models';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});

const me = { id: 'user-1', email: 'ada@example.com', name: 'Ada', role: 'user', endpoint: 'http://localhost:8080/v1' } as Me;

vi.mock('../app/session', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../app/session')>();
  return { ...actual, useLocalOnly: () => false, useMe: () => me };
});

const mocked = vi.mocked(api);

function model(id: string, name: string): Model {
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
  } as Model;
}

const alias = {
  id: 'mm-1',
  name: 'fast',
  description: '',
  target_model_id: 'm-cached',
  target_public_name: 'cached-model',
  target_upstream_name: 'vLLM',
  modalities: ['chat'],
  context_window: 262_144,
  servable: true,
  broken: false,
  grant_source: 'all users',
} as unknown as ManagedModel;

function summary(overrides: Partial<ModelPerformanceSummary> = {}): ModelPerformanceSummary {
  return {
    model_id: 'm-cached',
    days: 7,
    buckets: [],
    bucket_seconds: 86400,
    requests: 300,
    avg_ttft_ms: 820,
    ttft_samples: 120,
    ttft_ms_series: [null, null, 700, 800, 900, 850, 790],
    avg_output_tokens: 1950,
    output_tokens_series: [null, null, 1900, 2000, 1950, 1900, 2000],
    cache_reported: true,
    cache_hit_rate: 0.086,
    cache_hit_rate_series: [null, null, 0.05, 0.1, 0.08, 0.09, 0.1],
    ...overrides,
  };
}

const figures: Record<string, ModelPerformanceSummary> = {
  'm-cached': summary(),
  'm-plain': summary({
    // One measured day is not a trend: no sparkline for output size.
    output_tokens_series: [null, null, null, null, null, null, 240],
    model_id: 'm-plain',
    cache_reported: false,
    cache_hit_rate: null,
    cache_hit_rate_series: Array(7).fill(null),
  }),
  'm-idle': summary({
    model_id: 'm-idle',
    requests: 0,
    avg_ttft_ms: null,
    ttft_samples: 0,
    ttft_ms_series: Array(7).fill(null),
    avg_output_tokens: null,
    output_tokens_series: Array(7).fill(null),
    cache_reported: false,
    cache_hit_rate: null,
    cache_hit_rate_series: Array(7).fill(null),
  }),
};

function renderAt() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={['/models']}>
          <ModelsPage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocked.get.mockImplementation(async (path: string) => {
    const m = /^\/api\/v1\/models\/([^/]+)\/performance$/.exec(path);
    if (m) {
      // The alias asks with its own id; the server answers for its target.
      const id = m[1] === 'mm-1' ? 'm-cached' : m[1]!;
      return figures[id];
    }
    if (path === '/api/v1/models') {
      return {
        models: [model('m-cached', 'cached-model'), model('m-plain', 'plain-model'), model('m-idle', 'idle-model')],
        managed_models: [alias],
      };
    }
    throw new Error(`unexpected GET ${path}`);
  });
});

/** Opens a card's pop-up and returns the value cell of the row labeled `label`. */
async function rowFor(name: string, label: string): Promise<HTMLElement> {
  fireEvent.click(screen.getByRole('button', { name }));
  const dialog = await screen.findByRole('dialog');
  const labelEl = within(dialog).getByText(label);
  return labelEl.nextElementSibling as HTMLElement;
}

describe('model pop-up response figures', () => {
  it('shows time to first token, output size and cache hit rate, each with a sparkline', async () => {
    renderAt();
    await screen.findByText('cached-model');
    const ttft = await rowFor('cached-model', 'First token (7d)');
    await vi.waitFor(() => expect(ttft.textContent).toContain('820 ms'));
    expect(mocked.get).toHaveBeenCalledWith('/api/v1/models/m-cached/performance');
    const dialog = screen.getByRole('dialog');
    const output = within(dialog).getByText('Output / request (7d)').nextElementSibling!;
    expect(output.textContent).toContain('1,950 tokens');
    const cache = within(dialog).getByText('Cache hit rate (7d)').nextElementSibling!;
    expect(cache.textContent).toContain('8.6%');
    const sparks = dialog.querySelectorAll('svg.sparkline');
    expect(sparks).toHaveLength(3);
    // One point per measured day; the two empty days are a gap, not zeros.
    expect(sparks[0]!.getAttribute('data-points')).toBe('5');
    expect(sparks[0]!.querySelector('path')!.getAttribute('d')).toMatch(/^M/);
  });

  it('says "Not reported" for a model that never reports cached tokens, never 0%', async () => {
    renderAt();
    await screen.findByText('plain-model');
    const cache = await rowFor('plain-model', 'Cache hit rate (7d)');
    await vi.waitFor(() => expect(cache.textContent).toBe('Not reported'));
    expect(cache.textContent).not.toContain('%');
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText('Output / request (7d)').nextElementSibling!.querySelector('svg.sparkline')).toBeNull();
    // Only first token keeps a sparkline: one-point output, no cache line.
    expect(dialog.querySelectorAll('svg.sparkline')).toHaveLength(1);
  });

  it('says plainly when nothing was measured', async () => {
    renderAt();
    await screen.findByText('idle-model');
    const ttft = await rowFor('idle-model', 'First token (7d)');
    await vi.waitFor(() => expect(ttft.textContent).toBe('No streamed answers in the last 7 days.'));
    expect(screen.getByRole('dialog').querySelectorAll('svg.sparkline')).toHaveLength(0);
  });

  it("asks with the managed model's own id and shows its target's figures", async () => {
    renderAt();
    await screen.findByText('fast');
    const ttft = await rowFor('fast', 'First token (7d)');
    await vi.waitFor(() => expect(ttft.textContent).toContain('820 ms'));
    expect(mocked.get).toHaveBeenCalledWith('/api/v1/models/mm-1/performance');
  });
});
