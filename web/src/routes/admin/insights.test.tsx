/**
 * Usage insights: the classifier form saves the settings the admin chose
 * (provider, decision model, mode), the Try it panel posts the UNSAVED draft
 * and shows category, confidence and classifier time, and a field error from
 * the server lands on that field. The Categories tab adds a subcategory under
 * the chosen parent and the Overview reports categories and classifier time.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import type { ClassificationCategory, ClassificationDocument, ClassificationReport } from '../../lib/types';
import { ToastProvider } from '../../components/ui';
import { InsightsPage } from './Insights';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});
vi.mock('../../app/session', () => ({ useLocalOnly: () => false }));

const mocked = vi.mocked(api);

const doc: ClassificationDocument = {
  settings: {
    enabled: false,
    mode: 'async',
    protocol: 'systemone',
    upstream_id: '',
    model: '',
    sample_rate: 100,
    timeout_ms: 5000,
    text_scope: 'last_user',
    include_system_prompt: false,
    max_chars: 6000,
    include_service_tokens: true,
    concurrency: 4,
    queue_size: 1000,
    subcategories: true,
    retention_days: 180,
    updated_at: '',
  },
  upstreams: [
    {
      id: 'or',
      name: 'OpenRouter',
      adapter_type: 'openai_compatible',
      base_url: 'https://openrouter.ai/api',
      enabled: true,
      models: ['openai/gpt-4.1-mini'],
      decision_models: ['~typesafe/jev-latest'],
      systemone_url: 'https://openrouter.ai/api/alpha/decisions',
    },
    {
      id: 'nim',
      name: 'Nimble',
      adapter_type: 'ollama',
      base_url: 'http://nimble:8000',
      enabled: true,
      models: [],
      decision_models: ['nimble:9b-q8_0'],
      systemone_url: 'http://nimble:8000/v1/systemone',
    },
  ],
  running: false,
  queue_depth: 0,
  limits: { top_level: 26, subcategories: 25 },
};

const categories: ClassificationCategory[] = [
  {
    id: 'cat_coding',
    slug: 'coding',
    name: 'Coding',
    description: 'Code.',
    parent_slug: '',
    builtin: true,
    enabled: true,
    sort_order: 10,
    created_at: '',
    updated_at: '',
  },
  {
    id: 'c-rust',
    slug: 'rust',
    name: 'Rust',
    description: 'Rust code.',
    parent_slug: 'coding',
    builtin: false,
    enabled: true,
    sort_order: 1000,
    created_at: '',
    updated_at: '',
  },
  {
    id: 'cat_security',
    slug: 'security',
    name: 'Security',
    description: 'Sec.',
    parent_slug: '',
    builtin: true,
    enabled: true,
    sort_order: 20,
    created_at: '',
    updated_at: '',
  },
  {
    id: 'cat_other',
    slug: 'other',
    name: 'Other',
    description: 'None.',
    parent_slug: '',
    builtin: true,
    enabled: true,
    sort_order: 30,
    created_at: '',
    updated_at: '',
  },
];

const report: ClassificationReport = {
  from: '',
  to: '',
  bucket: 'day',
  classified: 10,
  categories: [
    {
      category: 'coding',
      count: 7,
      share: 0.7,
      avg_confidence: 0.91,
      low_confidence: 1,
      subcategories: [{ key: 'rust', count: 5, avg_confidence: 0.9 }],
      top_models: [{ key: 'gpt-5', count: 7, avg_confidence: 0.9 }],
      top_people: [{ key: 'u1', label: 'Ada', count: 7, avg_confidence: 0.9 }],
    },
    {
      category: 'security',
      count: 3,
      share: 0.3,
      avg_confidence: 0.8,
      low_confidence: 0,
      subcategories: [],
      top_models: [],
      top_people: [],
    },
  ],
  series: [{ bucket: '2026-10-04', counts: { coding: 7, security: 3 } }],
  by_team: [],
  health: {
    attempts: 12,
    ok: 10,
    errors: 1,
    timeouts: 1,
    dropped: 0,
    avg_ms: 310,
    p50_ms: 280,
    p95_ms: 900,
    max_ms: 1200,
    avg_queue_ms: 4,
    tokens_in: 6000,
    tokens_out: 60,
    cost_nanousd: 250000,
    by_classifier: [
      { classifier: 'OpenRouter / ~typesafe/jev-latest', mode: 'async', count: 12, avg_ms: 310, p50_ms: 280, p95_ms: 900 },
    ],
  },
};

function mockGets() {
  mocked.get.mockImplementation((path: string) => {
    if (path === '/api/v1/admin/classification') return Promise.resolve(doc);
    if (path === '/api/v1/admin/classification/categories') return Promise.resolve({ categories });
    if (path.startsWith('/api/v1/admin/classification/report')) return Promise.resolve(report);
    if (path.startsWith('/api/v1/admin/classification/results')) return Promise.resolve({ results: [] });
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
}

function renderAt(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route path="/admin/insights/:tab" element={<InsightsPage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('Usage insights', () => {
  it('saves the chosen decision model and tests the unsaved draft', async () => {
    mockGets();
    mocked.post.mockResolvedValue({
      status: 'ok',
      error: '',
      category: 'coding',
      subcategory: 'rust',
      confidence: 0.97,
      probabilities: { coding: 0.97, security: 0.02 },
      latency_ms: 164,
      classifier: 'OpenRouter / ~typesafe/jev-latest',
      model: 'jev-1.13',
      protocol: 'systemone',
      input_chars: 90,
      tokens_in: 565,
      tokens_out: 157,
      cost_nanousd: 23730,
    });
    mocked.put.mockResolvedValue({
      ...doc,
      settings: { ...doc.settings, enabled: true, upstream_id: 'or', model: '~typesafe/jev-latest', mode: 'inline' },
      running: true,
    });
    const user = userEvent.setup();
    renderAt('/admin/insights/classifier');

    await user.click(await screen.findByRole('checkbox', { name: 'Classify requests' }));
    await user.click(screen.getByRole('button', { name: 'Inline' }));
    await user.selectOptions(screen.getByLabelText('Provider'), 'or');
    expect(screen.getByText(/openrouter\.ai\/api\/alpha\/decisions/)).toBeTruthy();
    await user.type(screen.getByLabelText('Model'), '~typesafe/jev-latest');

    await user.click(screen.getByRole('button', { name: 'Classify' }));
    await waitFor(() => expect(mocked.post).toHaveBeenCalled());
    const [path, body] = mocked.post.mock.calls[0] as [string, { settings: { model: string; mode: string } }];
    expect(path).toBe('/api/v1/admin/classification/test');
    expect(body.settings.model).toBe('~typesafe/jev-latest');
    expect(body.settings.mode).toBe('inline');
    expect(await screen.findByText('164 ms')).toBeTruthy();
    expect(screen.getByText('97%')).toBeTruthy();
    expect(screen.getByText('› Rust')).toBeTruthy();

    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    const saved = mocked.put.mock.calls[0]?.[1] as { enabled: boolean; upstream_id: string; mode: string };
    expect(saved).toMatchObject({ enabled: true, upstream_id: 'or', mode: 'inline' });
  });

  it('puts a server field error on its field', async () => {
    mockGets();
    mocked.put.mockRejectedValue(
      new ApiError(400, {
        message: 'choose the provider and model',
        code: 'invalid_request_error',
        type: 'invalid_request_error',
        param: 'model',
      }),
    );
    const user = userEvent.setup();
    renderAt('/admin/insights/classifier');
    await user.click(await screen.findByRole('checkbox', { name: 'Classify requests' }));
    await user.click(screen.getByRole('button', { name: 'Save' }));
    const field = (await screen.findByText('choose the provider and model')).closest('label');
    expect(field).not.toBeNull();
    expect(within(field as HTMLElement).getByLabelText('Model')).toBeTruthy();
  });

  it('adds a subcategory under the chosen parent', async () => {
    mockGets();
    mocked.post.mockResolvedValue({
      category: {
        ...categories[1],
        id: 'new',
        slug: 'reverse_engineering',
        name: 'Reverse engineering',
        parent_slug: 'security',
      },
    });
    const user = userEvent.setup();
    renderAt('/admin/insights/categories');
    expect(await screen.findByRole('rowheader', { name: /Rust/ })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Delete Coding' })).toBeNull();

    await user.click(screen.getByRole('button', { name: 'Add subcategory' }));
    const drawer = await screen.findByRole('dialog', { name: 'Add subcategory' });
    await user.selectOptions(within(drawer).getByLabelText('Parent category'), 'security');
    expect(within(drawer).queryByRole('option', { name: 'Other' })).toBeNull();
    await user.type(within(drawer).getByLabelText('Name'), 'Reverse engineering');
    await user.type(within(drawer).getByLabelText('What belongs here'), 'Disassembling binaries and firmware.');
    await user.click(within(drawer).getByRole('button', { name: 'Add' }));
    await waitFor(() =>
      expect(mocked.post).toHaveBeenCalledWith('/api/v1/admin/classification/categories', {
        name: 'Reverse engineering',
        description: 'Disassembling binaries and firmware.',
        parent_slug: 'security',
      }),
    );
  });

  it('reports categories, classifier time and misses', async () => {
    mockGets();
    const user = userEvent.setup();
    renderAt('/admin/insights/overview');
    expect(await screen.findByText('Classification is off. Choose a classifier to start labelling requests.')).toBeTruthy();
    const table = (await screen.findByRole('button', { name: /Coding/ })).closest('table') as HTMLElement;
    expect(within(table).getByText('70%')).toBeTruthy();
    expect(within(table).getByText('Rust (5)')).toBeTruthy();
    expect(screen.getAllByText('280 ms').length).toBeGreaterThan(0);
    expect(screen.getByText('1 errors · 1 timeouts · 0 dropped')).toBeTruthy();
    await user.click(within(table).getByRole('button', { name: /Coding/ }));
    expect(within(table).getByText('Ada')).toBeTruthy();
  });
});
