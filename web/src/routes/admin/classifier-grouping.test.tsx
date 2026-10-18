/**
 * Classifier grouping in the admin UI: the request log shows how each
 * request was classified (column, filter, drawer with the classifier wait
 * and background labelling), troubleshooting captures list the classifier
 * calls, the Performance tab starts a benchmark and reads its result, and
 * the call profile saves an override.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { ClassificationCategory, ClassifierBenchmark, ClassifierProfileView, DecisionModelView } from '../../lib/types';
import { ToastProvider } from '../../components/ui';
import { AdminRequestsPage } from './Requests';
import { PerformancePanel } from './InsightsPerformance';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});
vi.mock('../../app/session', () => ({ useLocalOnly: () => false }));

const mocked = vi.mocked(api);

const cat = (slug: string, name: string, parent = ''): ClassificationCategory => ({
  id: `c-${slug}`,
  slug,
  name,
  description: '',
  parent_slug: parent,
  builtin: !parent,
  enabled: true,
  sort_order: 1,
  created_at: '',
  updated_at: '',
});
const categories = [cat('coding', 'Coding'), cat('rust', 'Rust', 'coding'), cat('legal', 'Legal')];

const base = {
  created_at: '2026-10-05T10:00:00Z',
  user_id: 'u1',
  user_name: 'Ada',
  token_id: 't',
  model: 'gpt-4o',
  endpoint_path: '/v1/chat/completions',
  http_method: 'POST',
  modality: 'chat',
  streaming: false,
  request_bytes: 1,
  response_bytes: 1,
  tokens_in: 1,
  tokens_out: 1,
  tokens_cached: 0,
  token_accounting_method: 'upstream_reported',
  cost_nanousd: 0,
  http_status: 200,
  latency_ms: 900,
  ttfb_ms: 100,
  error_code: '',
};
const inline = {
  ...base,
  id: 'evt-inline',
  request_id: 'req-inline',
  classifier_ms: 182,
  classifier_calls: 1,
  classification: 'coding/rust',
  classification_tag: {
    category: 'coding',
    subcategory: 'rust',
    confidence: 0.97,
    mode: 'inline',
    status: 'ok',
    classifier: 'Nimble / nimble-9b',
    latency_ms: 0,
    queue_ms: 0,
    calls: 0,
  },
  has_capture: true,
};
const background = {
  ...base,
  id: 'evt-bg',
  request_id: 'req-bg',
  user_name: 'Bob',
  classification_tag: {
    category: 'legal',
    confidence: 0.81,
    mode: 'async',
    status: 'ok',
    classifier: 'Nimble / nimble-9b',
    latency_ms: 240,
    queue_ms: 12,
    calls: 1,
  },
};

function mockRequestsApi() {
  mocked.get.mockImplementation((path: string) => {
    if (path.startsWith('/api/v1/requests')) return Promise.resolve({ requests: [inline, background], total_count: 2 });
    if (path === '/api/v1/admin/classification/categories') return Promise.resolve({ categories });
    if (path.startsWith('/api/v1/admin/models')) return Promise.resolve({ models: [], counts: {}, modalities: [] });
    if (path.endsWith('/security')) return Promise.resolve({ classifier_runs: [], other_violations: [] });
    if (path === '/api/v1/admin/troubleshooting/captures/evt-inline/classifiers') {
      return Promise.resolve({
        classifier_runs: [
          {
            feature: 'prompt_injection',
            classifier: 'nimble-9b',
            protocol: 'systemone',
            mode: 'inline',
            started_at: '2026-10-05T10:00:00Z',
            latency_ms: 170,
            status: 'ok',
            questions: ['prompt_injection'],
            request: { state: 'hello', questions: { prompt_injection: { type: 'noul' } } },
            response: { answers: { prompt_injection: { noul: 0.02 } } },
          },
          {
            feature: 'content_safety+usage_insights',
            classifier: 'nimble-9b',
            protocol: 'systemone',
            mode: 'inline',
            started_at: '2026-10-05T10:00:00Z',
            latency_ms: 180,
            status: 'timeout',
            error: 'context deadline exceeded',
          },
        ],
      });
    }
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
}

function renderRequests(path = '/admin/requests') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/admin/requests" element={<AdminRequestsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  window.localStorage.clear();
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('request log — classification', () => {
  it('tags each request with its label and filters by category', async () => {
    mockRequestsApi();
    const user = userEvent.setup();
    renderRequests();
    expect(await screen.findByRole('cell', { name: 'Coding › Rust' })).toBeTruthy();
    expect(screen.getByRole('cell', { name: 'Legal' })).toBeTruthy();

    const filter = await screen.findByRole('combobox', { name: 'Classified as' });
    expect(within(filter).getByRole('option', { name: 'Coding › Rust' })).toBeTruthy();
    await user.selectOptions(filter, 'coding/rust');
    await waitFor(() => {
      const calls = mocked.get.mock.calls.map(([p]) => p as string).filter((p) => p.startsWith('/api/v1/requests'));
      expect(calls.at(-1)).toContain('classified=coding%2Frust');
    });
  });

  it('explains inline vs background labelling and the wait in the drawer, with captured classifier calls', async () => {
    mockRequestsApi();
    const user = userEvent.setup();
    renderRequests();
    await user.click(await screen.findByRole('cell', { name: 'Coding › Rust' }));
    const drawer = await screen.findByRole('dialog');
    expect(within(drawer).getByText('Inline')).toBeTruthy();
    expect(within(drawer).getByText('97% sure')).toBeTruthy();
    expect(within(drawer).getByText('182 ms')).toBeTruthy();
    expect(within(drawer).getByText(/1 classifier call before forwarding/)).toBeTruthy();
    // Captured classifier calls, each with feature, status and wire bodies.
    expect(await within(drawer).findByText('Prompt injection')).toBeTruthy();
    expect(within(drawer).getByText('Content safety + Usage insights')).toBeTruthy();
    expect(within(drawer).getByText('timeout')).toBeTruthy();
    expect(within(drawer).getByText('context deadline exceeded')).toBeTruthy();
  });

  it('shows background labelling as not delaying the request', async () => {
    mockRequestsApi();
    const user = userEvent.setup();
    renderRequests();
    await user.click(await screen.findByRole('cell', { name: 'Legal' }));
    const drawer = await screen.findByRole('dialog');
    expect(within(drawer).getByText('Background')).toBeTruthy();
    expect(within(drawer).getByText(/did not delay the request/)).toBeTruthy();
    // No guard or inline wait on this request.
    expect(within(drawer).getByText('None')).toBeTruthy();
  });

  it('shows that router instructions were added to a routed request', async () => {
    const routed = {
      ...base,
      id: 'evt-routed',
      request_id: 'req-routed',
      user_name: 'Kid',
      model: 'tutor-model',
      requested_model_name: 'auto/family',
      route_reason: 'matched:homework_done_for_them;instructions=router+route',
    };
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/requests')) return Promise.resolve({ requests: [routed], total_count: 1 });
      if (path === '/api/v1/admin/classification/categories') return Promise.resolve({ categories });
      if (path.startsWith('/api/v1/admin/models')) return Promise.resolve({ models: [], counts: {}, modalities: [] });
      if (path.endsWith('/security')) return Promise.resolve({ classifier_runs: [], other_violations: [] });
      if (path.startsWith('/api/v1/admin/routing/decisions')) return Promise.resolve({ decisions: [] });
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
    const user = userEvent.setup();
    renderRequests();
    await user.click(await screen.findByRole('cell', { name: /Kid/ }));
    const drawer = await screen.findByRole('dialog');
    expect(within(drawer).getByText(/auto\/family → tutor-model · Matched a route/)).toBeTruthy();
    expect(within(drawer).getByText('Router instructions added: router + route')).toBeTruthy();
  });
});

const profiles: ClassifierProfileView[] = [
  {
    upstream_id: 'up-n',
    upstream_name: 'Nimble (ai)',
    model: 'nimble-9b',
    model_id: 'm-n',
    role: 'decision',
    call_style: '',
    max_options: 0,
    wire_model: '',
    dialect: '',
    effective_dialect: 'systemone',
    effective_style: 'separate',
    effective_max_options: 255,
    used_by: ['Prompt injection', 'Usage insights'],
  },
];

const done: ClassifierBenchmark = {
  id: 'b1',
  created_at: '2026-10-05T10:00:00Z',
  finished_at: '2026-10-05T10:01:00Z',
  created_by: 'u1',
  status: 'done',
  config: { budget_ms: 500, levels: [1, 4, 16] },
  result: {
    plan: ['prompt injection: nimble-9b (decision model, separate questions)'],
    single_ms: 140,
    max_concurrent: 4,
    levels: [
      {
        concurrency: 1,
        requests: 24,
        errors: 0,
        p50_ms: 140,
        p95_ms: 160,
        max_ms: 170,
        per_second: 7,
        calls_per_request: 2,
        within_budget: true,
      },
      {
        concurrency: 4,
        requests: 24,
        errors: 0,
        p50_ms: 260,
        p95_ms: 420,
        max_ms: 480,
        per_second: 14,
        calls_per_request: 2,
        within_budget: true,
      },
      {
        concurrency: 16,
        requests: 32,
        errors: 1,
        p50_ms: 900,
        p95_ms: 1400,
        max_ms: 1600,
        per_second: 17,
        calls_per_request: 2,
        within_budget: false,
      },
    ],
  },
};

function renderPerformance() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <PerformancePanel />
      </ToastProvider>
    </QueryClientProvider>,
  );
}

describe('Usage insights — performance', () => {
  it('reports the measured wait and the most simultaneous requests within budget', async () => {
    mocked.get.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/classification/benchmarks') {
        return Promise.resolve({
          benchmarks: [done],
          defaults: {
            injection_model_id: 'm-n',
            safety_model_id: '',
            usage_insights: true,
            insights_mode: '',
            style: '',
            levels: [1, 2, 4, 8, 16, 32],
            requests_per_level: 24,
            budget_ms: 1000,
          },
        });
      }
      if (path === '/api/v1/admin/secgw/classifiers') {
        return Promise.resolve({ classifiers: [{ id: 'm-n', name: 'nimble-9b', classifier_role: 'decision' }] });
      }
      if (path === '/api/v1/admin/classification/profiles') return Promise.resolve({ profiles });
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
    mocked.post.mockResolvedValue({ ...done, id: 'b2', status: 'running', result: {} });
    const user = userEvent.setup();
    renderPerformance();

    const single = (await screen.findByText('Added wait, one at a time')).closest('article') as HTMLElement;
    expect(within(single).getByText('140 ms')).toBeTruthy();
    const tiles = screen.getByText('Simultaneous requests within budget').closest('article') as HTMLElement;
    expect(within(tiles).getByText('4')).toBeTruthy();
    expect(screen.getByText(/separate questions/)).toBeTruthy();
    // Over-budget level is called out.
    const rows = screen.getAllByRole('row');
    expect(within(rows[3] as HTMLElement).getByText('No')).toBeTruthy();

    // The form is pre-filled from the current configuration and posts it.
    expect((screen.getByRole('combobox', { name: 'Prompt-injection classifier' }) as HTMLSelectElement).value).toBe('m-n');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Usage insights in the benchmark' }), 'inline');
    await user.click(screen.getByRole('button', { name: 'Run benchmark' }));
    await waitFor(() => expect(mocked.post).toHaveBeenCalled());
    const [url, body] = mocked.post.mock.calls[0] as [string, Record<string, unknown>];
    expect(url).toBe('/api/v1/admin/classification/benchmarks');
    expect(body).toMatchObject({ injection_model_id: 'm-n', usage_insights: true, insights_mode: 'inline', budget_ms: 1000 });
  });

  it('shows each classifier\u2019s automatic call style and saves an override', async () => {
    mocked.get.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/classification/benchmarks') {
        return Promise.resolve({ benchmarks: [], defaults: { levels: [1], budget_ms: 1000, requests_per_level: 24 } });
      }
      if (path === '/api/v1/admin/secgw/classifiers') return Promise.resolve({ classifiers: [] });
      if (path === '/api/v1/admin/classification/profiles') return Promise.resolve({ profiles });
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
    mocked.put.mockResolvedValue({});
    const user = userEvent.setup();
    renderPerformance();
    expect(await screen.findByText('Automatic: one request per question, in parallel.')).toBeTruthy();
    const row = screen.getByText('nimble-9b').closest('.insights-profile') as HTMLElement;
    expect(within(row).getByText('Usage insights')).toBeTruthy();
    expect(within(row).getByText('Prompt injection')).toBeTruthy();
    await user.selectOptions(screen.getByRole('combobox', { name: 'How nimble-9b is asked several questions' }), 'combined');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() =>
      expect(mocked.put).toHaveBeenCalledWith('/api/v1/admin/classification/profiles', {
        upstream_id: 'up-n',
        model: 'nimble-9b',
        call_style: 'combined',
        max_options: 0,
        wire_model: '',
        dialect: '',
      }),
    );
    // Pricing and access settings are not sent when untouched.
    expect(mocked.put).toHaveBeenCalledTimes(1);
  });

  it('prices a decision model and guards who can call it, in words', async () => {
    const jev: DecisionModelView = {
      id: 'm-n',
      name: 'nimble-9b',
      upstream_id: 'up-n',
      upstream_name: 'Openrouter',
      status: 'enabled',
      dialect: 'systemone',
      dialect_source: 'detected',
      wire_model: '~typesafe/jev-latest',
      endpoint: 'https://openrouter.ai/api/alpha/decisions',
      cost_source: '',
      request_fee_usd: 0,
      usd_per_mtok_in: 0.042,
      usd_per_mtok_out: 0,
      usd_per_mtok_cached: 0,
      provider_reports_cost: true,
      grant_count: 2,
      guarding: ['General Observation'],
      grant_when_guarding: false,
      grantable: false,
      not_grantable_reason: 'it blocks requests in General Observation',
    };
    mocked.get.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/classification/benchmarks') {
        return Promise.resolve({ benchmarks: [], defaults: { levels: [1], budget_ms: 1000, requests_per_level: 24 } });
      }
      if (path === '/api/v1/admin/secgw/classifiers') return Promise.resolve({ classifiers: [] });
      if (path === '/api/v1/admin/classification/profiles') return Promise.resolve({ profiles });
      if (path === '/api/v1/admin/decision-models') return Promise.resolve({ models: [jev] });
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
    mocked.put.mockResolvedValue({});
    const user = userEvent.setup();
    renderPerformance();
    expect(
      await screen.findByText('Automatic: System One, from the provider\u2019s address.'.replace('\u2019', "'")),
    ).toBeTruthy();
    expect(
      screen.getByText(
        'The charge Openrouter reports with each answer, or the rate card ($0.042 per million tokens read) when it sends none.',
      ),
    ).toBeTruthy();
    expect(screen.getByText(/It blocks requests in General Observation/)).toBeTruthy();
    expect(screen.getByText('2 existing grants will be refused while it guards.')).toBeTruthy();

    await user.selectOptions(screen.getByRole('combobox', { name: 'How calls to nimble-9b are priced' }), 'card');
    await user.type(screen.getByRole('spinbutton', { name: 'Fee per call for nimble-9b' }), '0.001');
    expect(screen.getByText('Rate card: $0.042 per million tokens read plus $0.001 per call.')).toBeTruthy();
    await user.click(screen.getByRole('checkbox', { name: 'Allow granting it anyway' }));
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() =>
      expect(mocked.put).toHaveBeenCalledWith('/api/v1/admin/decision-models/m-n/settings', {
        cost_source: 'card',
        request_fee_usd: 0.001,
        grant_when_guarding: true,
      }),
    );
    // The call profile was not touched, so it is not re-sent.
    expect(mocked.put).toHaveBeenCalledTimes(1);
  });

  it('folds away decision models that are neither enabled nor in use', async () => {
    const extra: ClassifierProfileView = { ...profiles[0]!, model: 'upstage/solar-decide', model_id: 'm-s', used_by: [] };
    mocked.get.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/classification/benchmarks') {
        return Promise.resolve({ benchmarks: [], defaults: { levels: [1], budget_ms: 1000, requests_per_level: 24 } });
      }
      if (path === '/api/v1/admin/secgw/classifiers') return Promise.resolve({ classifiers: [] });
      if (path === '/api/v1/admin/classification/profiles') return Promise.resolve({ profiles: [...profiles, extra] });
      if (path === '/api/v1/admin/decision-models') return Promise.resolve({ models: [] });
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
    const user = userEvent.setup();
    renderPerformance();
    expect(await screen.findByText('nimble-9b')).toBeTruthy();
    expect(screen.queryByText('upstage/solar-decide')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Show 1 more not enabled (enable them under Models)' }));
    expect(screen.getByText('upstage/solar-decide')).toBeTruthy();
  });
});
