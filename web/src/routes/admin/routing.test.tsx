/**
 * Auto-routing admin page: overview verdict, routers list → builder,
 * the builder's save payload and Try box, categories editor, classifier
 * Try routing, and the evaluations table.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import { RoutingPage } from './Routing';
import type { Router, RoutingCategory, RoutingDocument, RoutingTestResult } from '../../lib/routing';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

vi.mock('../../app/session', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../app/session')>();
  return { ...actual, useLocalOnly: () => false };
});

const mocked = vi.mocked(api);

const doc: RoutingDocument = {
  settings: {
    classifier_model_id: 'jev',
    max_concurrent_decisions: 32,
    difficulty_style: 'general',
    max_chars: 6000,
    eval_judge_model_id: 'judge',
    eval_max_concurrent: 2,
    eval_pause_above_in_flight: 50,
    eval_max_output_tokens: 2048,
    retention_days: 90,
  },
  classifiers: [
    {
      id: 'jev',
      name: 'jev-1',
      upstream_name: 'TypeSafe',
      hosting: 'external',
      status: 'enabled',
      style: 'combined',
      used_by_security: true,
    },
    {
      id: 'clef',
      name: 'clef-27b',
      upstream_name: 'llama',
      hosting: 'self_hosted',
      status: 'enabled',
      style: 'separate',
      used_by_security: false,
    },
  ],
  judge_models: [{ id: 'judge', kind: 'model', name: 'judge-model', upstream_name: 'OpenRouter' }],
  running: true,
  warnings: [],
  eval_queue: 0,
  in_flight_decisions: 0,
  max_categories: 25,
  thinking_levels: ['none', 'low', 'medium', 'high'],
};

const legal: RoutingCategory = {
  id: 'cat-legal',
  slug: 'legal',
  name: 'Legal',
  includes: 'Contracts, clauses, compliance questions.',
  excludes: 'Not general business advice.',
  linked_slug: '',
  min_confidence: 0.6,
  examples: ['Review this NDA'],
  builtin: false,
  enabled: true,
  sort_order: 1,
  used_by: ['auto/legal'],
};
const coding: RoutingCategory = {
  ...legal,
  id: 'cat-coding',
  slug: 'coding',
  name: 'Coding',
  builtin: true,
  used_by: [],
  examples: [],
};

const router: Router = {
  id: 'r-1',
  name: 'auto/legal',
  description: 'Legal on-prem',
  status: 'shadow',
  mode: 'shadow',
  enabled: true,
  grant_count: 1,
  created_at: '2026-10-01T00:00:00Z',
  updated_at: '2026-10-02T00:00:00Z',
  problems: [],
  config: {
    routes: [
      {
        id: 'legal',
        name: 'Legal on-prem',
        model_id: 'onprem',
        model_name: 'qwen-legal',
        model_hosting: 'self_hosted',
        categories: ['legal'],
        difficulty_min: 0,
        difficulty_max: 0,
        thinking: 'passthrough',
        restrict_to_granted: false,
        skip_when_over_quota: false,
        eval: {
          enabled: true,
          sample_pct: 5,
          max_per_day: 200,
          daily_budget_usd: 5,
          until: '2099-01-01T00:00:00Z',
          judge_model_id: '',
        },
      },
    ],
    default: { model_id: 'frontier', model_name: 'frontier-1', thinking: 'passthrough' },
    budget_ms: 800,
    sticky: true,
    sticky_minutes: 30,
    disclose: true,
  },
  stats: {
    router_id: 'r-1',
    requests: 40,
    routed: 10,
    defaulted: 30,
    timeouts: 0,
    errors: 0,
    shadow: 40,
    p50_ms: 210,
    p95_ms: 400,
    mix: [
      { route_id: 'legal', route_name: 'Legal on-prem', model_name: 'qwen-legal', requests: 10 },
      { route_id: 'default', route_name: '', model_name: 'frontier-1', requests: 30 },
    ],
    reasons: [],
    categories: [],
  },
  evaluations: [
    {
      router_id: 'r-1',
      route_id: 'legal',
      evaluated: 30,
      failed: 0,
      better: 6,
      ties: 4,
      worse: 20,
      consistency: 0.9,
      routed_cost_nanousd: 1,
      default_cost_nanousd: 4,
      eval_cost_nanousd: 1,
      routed_latency_ms: 1,
      default_latency_ms: 1,
      at_least_as_good_low: 0.2,
      at_least_as_good_high: 0.4,
      verdict: 'losing',
    },
  ],
};

const targets = [
  { id: 'onprem', kind: 'model', name: 'qwen-legal', upstream_name: 'llama', hosting: 'self_hosted' },
  { id: 'frontier', kind: 'model', name: 'frontier-1', upstream_name: 'OpenRouter', hosting: 'external' },
];

function routeGets(evalResults: unknown[] = []) {
  mocked.get.mockImplementation(async (path: string) => {
    if (path === '/api/v1/admin/routing') return doc;
    if (path.endsWith('/routing/routers')) return { routers: [router] };
    if (path.endsWith('/routing/routers/r-1'))
      return { router, grants: [{ id: 'g', grantee_name: 'Everyone', grantee_type: 'all' }] };
    if (path.endsWith('/routing/categories')) return { categories: [legal, coding], insights_categories: [], max_enabled: 25 };
    if (path.endsWith('/routing/targets')) return { targets };
    if (path.includes('/routing/evaluations/r-1')) return { results: evalResults };
    throw new Error(`unexpected GET ${path}`);
  });
}

function renderAt(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route path="/admin/routing" element={<RoutingPage />} />
            <Route path="/admin/routing/:tab" element={<RoutingPage />} />
            <Route path="/admin/routing/:tab/:id" element={<RoutingPage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  routeGets();
});

describe('Auto-routing overview', () => {
  it('leads with one verdict and names what needs attention', async () => {
    renderAt('/admin/routing');
    const hero = await screen.findByTestId('routing-health');
    expect(within(hero).getByText('Watching, not routing yet')).toBeTruthy();
    await waitFor(() => expect(within(hero).getByText(/“Legal on-prem” is losing to the default/)).toBeTruthy());
  });
});

describe('Routers', () => {
  it('lists routers with status, default, grants and evaluation verdict', async () => {
    renderAt('/admin/routing/routers');
    const row = (await screen.findByRole('rowheader', { name: /auto\/legal/ })).closest('tr')!;
    expect(within(row).getByText('Shadow')).toBeTruthy();
    expect(within(row).getByText('frontier-1')).toBeTruthy();
    expect(within(row).getByText('1 losing')).toBeTruthy();
  });

  it('saves the builder with the route moved and the status changed', async () => {
    mocked.put.mockResolvedValue({ router });
    renderAt('/admin/routing/routers/r-1');
    const user = userEvent.setup();
    await screen.findByRole('heading', { name: 'auto/legal' });
    await user.click(screen.getByRole('radio', { name: 'Live' }));
    await user.click(screen.getByRole('button', { name: 'Add route' }));
    await user.selectOptions(await screen.findByRole('combobox', { name: 'Send to' }), 'frontier');
    await user.click(screen.getByRole('radio', { name: 'Hard (4–5)' }));
    await user.selectOptions(screen.getByRole('combobox', { name: 'Thinking' }), 'high');
    await user.click(screen.getByRole('button', { name: 'Done' }));
    await user.click(screen.getByRole('button', { name: 'Move route 2 up' }));
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    const [path, body] = mocked.put.mock.calls[0]! as [string, { status: string; config: Router['config'] }];
    expect(path).toBe('/api/v1/admin/routing/routers/r-1');
    expect(body.status).toBe('live');
    expect(body.config.routes.map((r) => r.model_id)).toEqual(['frontier', 'onprem']);
    expect(body.config.routes[0]).toMatchObject({ difficulty_min: 3.5, difficulty_max: 0, thinking: 'high' });
  });

  it('warns when a hosted classifier reads prompts routed on-prem', async () => {
    renderAt('/admin/routing/routers/r-1');
    expect(
      await screen.findByText(/is externally hosted, and this router sends some requests to a self-hosted model/),
    ).toBeTruthy();
  });

  it('tests the on-screen router and shows the trace', async () => {
    const res: RoutingTestResult = {
      status: 'ok',
      category: 'legal',
      confidence: 0.93,
      probabilities: { legal: 0.93, none: 0.07 },
      difficulty: 2.1,
      latency_ms: 230,
      calls: 1,
      classifier: 'jev-1',
      routers: [
        {
          router_id: 'r-1',
          router_name: 'auto/legal',
          route_id: 'legal',
          route_name: 'Legal on-prem',
          model_id: 'onprem',
          model_name: 'qwen-legal',
          thinking: '',
          reason: 'matched:legal',
          trace: [{ route_id: 'legal', index: 0, outcome: 'matched', detail: 'Legal 93% ≥ 60%' }],
        },
      ],
    };
    mocked.post.mockResolvedValue(res);
    renderAt('/admin/routing/routers/r-1');
    const user = userEvent.setup();
    await screen.findByRole('heading', { name: 'auto/legal' });
    await user.click(screen.getByRole('button', { name: 'Contract clause' }));
    await user.click(screen.getByRole('button', { name: 'Route it' }));
    const result = await screen.findByTestId('try-result');
    expect(within(result).getByText('qwen-legal')).toBeTruthy();
    expect(within(result).getByText(/Legal 93% ≥ 60%/)).toBeTruthy();
    const body = mocked.post.mock.calls[0]![1] as { router_id: string; config: Router['config'] };
    expect(body.router_id).toBe('r-1');
    expect(body.config.default.model_id).toBe('frontier');
  });

  it('starts a new router from a template with the template’s routes', async () => {
    renderAt('/admin/routing/routers/new?template=onprem');
    expect(await screen.findByRole('heading', { name: 'auto/legal' })).toBeTruthy();
    expect(screen.getByText(/Choose your self-hosted or fine-tuned model/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Create router' })).toBeTruthy();
  });
});

describe('Categories', () => {
  it('edits a category’s descriptions, confidence and examples', async () => {
    mocked.patch.mockResolvedValue({});
    renderAt('/admin/routing/categories');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Edit Legal' }));
    const excludes = await screen.findByRole('textbox', { name: 'Does not include' });
    await user.clear(excludes);
    await user.type(excludes, 'Not HR policy questions.');
    await user.type(screen.getByRole('textbox', { name: 'Test examples' }), '{enter}Is this clause enforceable?');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(mocked.patch).toHaveBeenCalled());
    const [path, body] = mocked.patch.mock.calls[0]! as [string, { excludes: string; examples: string[] }];
    expect(path).toBe('/api/v1/admin/routing/categories/cat-legal');
    expect(body.excludes).toBe('Not HR policy questions.');
    expect(body.examples).toEqual(['Review this NDA', 'Is this clause enforceable?']);
  });
});

describe('Classifier', () => {
  it('compares an unsaved classifier choice in Try routing', async () => {
    mocked.post.mockResolvedValue({
      status: 'ok',
      category: 'coding',
      confidence: 0.8,
      probabilities: { coding: 0.8, legal: 0.2 },
      difficulty: 4.2,
      latency_ms: 900,
      calls: 2,
      classifier: 'clef-27b',
      routers: [],
    } satisfies RoutingTestResult);
    renderAt('/admin/routing/classifier');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('radio', { name: /clef-27b/ }));
    await user.type(screen.getByRole('textbox', { name: 'Prompt to classify' }), 'write a lock-free queue');
    await user.click(screen.getByRole('button', { name: 'Classify and route' }));
    await screen.findByText('4.2');
    expect(mocked.post).toHaveBeenCalledWith('/api/v1/admin/routing/test', {
      text: 'write a lock-free queue',
      classifier_model_id: 'clef',
    });
  });
});

describe('Evaluations', () => {
  it('shows each evaluated route with its verdict and interval', async () => {
    renderAt('/admin/routing/evaluations');
    const card = await screen.findByRole('article', { name: /auto\/legal: Legal on-prem/ });
    expect(within(card).getByText('Losing')).toBeTruthy();
    // The question the card answers names both models in words.
    expect(within(card).getByRole('heading').textContent).toMatch(/Is qwen-legal as good as frontier-1 for Legal\?/);
    expect(within(card).getByText(/qwen-legal is losing to frontier-1/)).toBeTruthy();
    expect(within(card).getByText(/likely 20%–40%/)).toBeTruthy();
    expect(within(card).getByText(/75% cheaper/)).toBeTruthy();
    expect(within(card).getByText(/qwen-legal better: 6/)).toBeTruthy();
  });

  it('opens a route’s verdicts with each model named', async () => {
    routeGets([
      {
        id: 'e1',
        created_at: new Date().toISOString(),
        request_id: 'q',
        route_id: 'legal',
        verdict: 'worse',
        routed_score: 3,
        default_score: 4.5,
        consistent: true,
        rationale: 'B cites the statute.',
        status: 'ok',
        eval_cost_nanousd: 1000,
      },
      {
        id: 'e2',
        created_at: new Date().toISOString(),
        request_id: 'q2',
        route_id: 'legal',
        verdict: '',
        routed_score: 0,
        default_score: 0,
        consistent: false,
        rationale: '',
        status: 'error',
        error: 'HTTP 400 from OpenAI: bad param',
        eval_cost_nanousd: 0,
      },
    ]);
    renderAt('/admin/routing/evaluations');
    const user = userEvent.setup();
    const card = await screen.findByRole('article', { name: /auto\/legal: Legal on-prem/ });
    await user.click(within(card).getByRole('button', { name: 'Verdicts and reasons' }));
    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByText('frontier-1 better')).toBeTruthy();
    expect(within(dialog).getByRole('columnheader', { name: 'qwen-legal' })).toBeTruthy();
    expect(within(dialog).getByText('3.0 / 5')).toBeTruthy();
    expect(within(dialog).getByText('HTTP 400 from OpenAI: bad param')).toBeTruthy();
  });
});

describe('Evaluations per pairing (release review D6, D2, D13)', () => {
  const older = {
    ...router.evaluations![0]!,
    routed_model_id: 'retired',
    current: false,
    evaluated: 12,
    better: 0,
    ties: 0,
    worse: 12,
    verdict: 'losing' as const,
    last_at: new Date().toISOString(),
  };
  const currentTally = {
    ...router.evaluations![0]!,
    routed_model_id: 'onprem',
    default_model_id: 'frontier',
    judge_model_id: 'judge',
    current: true,
    evaluated: 3,
    better: 3,
    ties: 0,
    worse: 0,
    skipped: 2,
    verdict: 'needs_data' as const,
  };
  function withEvals(evals: Router['evaluations'], docPatch: Partial<RoutingDocument> = {}) {
    const r = { ...router, evaluations: evals };
    mocked.get.mockImplementation(async (path: string) => {
      if (path === '/api/v1/admin/routing') return { ...doc, ...docPatch };
      if (path.endsWith('/routing/routers')) return { routers: [r] };
      if (path.endsWith('/routing/routers/r-1')) return { router: r, grants: [] };
      if (path.endsWith('/routing/categories')) return { categories: [legal, coding], insights_categories: [], max_enabled: 25 };
      if (path.endsWith('/routing/targets')) return { targets };
      if (path.includes('/routing/evaluations/r-1')) return { results: [] };
      throw new Error(`unexpected GET ${path}`);
    });
  }

  it('counts only the current pairing and keeps older ones as history', async () => {
    withEvals([currentTally, older]);
    renderAt('/admin/routing/evaluations');
    const card = await screen.findByRole('article', { name: /auto\/legal: Legal on-prem/ });
    // The old model's 12 losses do not make the current route "Losing".
    expect(within(card).getByText('Needs more data')).toBeTruthy();
    expect(within(card).queryByText('Losing')).toBeNull();
    expect(within(card).getByText(/qwen-legal better: 3/)).toBeTruthy();
    expect(within(card).getByText(/Earlier comparisons with other models \(1\): not counted above/)).toBeTruthy();
    expect(within(card).getByText(/2 sampled requests were not evaluated: a Security Gateway policy/)).toBeTruthy();
    expect(screen.getByText(/the tally starts again and earlier comparisons are kept as history/)).toBeTruthy();
  });

  it('lists only the current pairing’s verdicts in the drawer', async () => {
    withEvals([currentTally, older]);
    renderAt('/admin/routing/evaluations');
    const user = userEvent.setup();
    const card = await screen.findByRole('article', { name: /auto\/legal: Legal on-prem/ });
    await user.click(within(card).getByRole('button', { name: 'Verdicts and reasons' }));
    await waitFor(() =>
      expect(mocked.get).toHaveBeenCalledWith(
        '/api/v1/admin/routing/evaluations/r-1?route_id=legal&limit=50&routed_model_id=onprem&default_model_id=frontier&judge_model_id=judge',
      ),
    );
  });

  it('does not flag a router as losing for an old pairing', async () => {
    withEvals([older]);
    renderAt('/admin/routing/routers');
    const row = (await screen.findByRole('rowheader', { name: /auto\/legal/ })).closest('tr')!;
    expect(within(row).queryByText('1 losing')).toBeNull();
  });

  it('shows evaluation warnings and saves the evaluation limits', async () => {
    withEvals([currentTally], {
      eval_warnings: ['Router auto/legal, Legal on-prem: the evaluation judge is one of the two models it compares'],
    });
    mocked.put.mockResolvedValue(doc);
    renderAt('/admin/routing/evaluations');
    const user = userEvent.setup();
    expect(await screen.findByText(/the evaluation judge is one of the two models it compares/)).toBeTruthy();
    const conc = screen.getByLabelText('Evaluations at once per replica');
    await user.clear(conc);
    await user.type(conc, '1');
    await user.click(screen.getByRole('button', { name: 'Save limits' }));
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    const [path, body] = mocked.put.mock.calls[0]! as [string, Record<string, unknown>];
    expect(path).toBe('/api/v1/admin/routing/settings');
    expect(body).toMatchObject({
      eval_max_concurrent: 1,
      eval_pause_above_in_flight: 50,
      eval_max_output_tokens: 2048,
      eval_judge_model_id: 'judge',
    });
  });

  it('warns in the builder when an on-prem route is evaluated against a hosted default', async () => {
    renderAt('/admin/routing/routers/r-1');
    expect(await screen.findByText(/evaluation sends a sample of those prompts \(and the answers\) there/)).toBeTruthy();
  });
});

describe('Router builder in the section', () => {
  it('asks before an in-app link discards unsaved edits (release review D12)', async () => {
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    renderAt('/admin/routing/routers/r-1');
    const user = userEvent.setup();
    await screen.findByRole('heading', { name: 'auto/legal' });
    await user.click(screen.getByRole('radio', { name: 'Live' }));
    await user.click(screen.getByRole('link', { name: 'Back to all routers' }));
    expect(confirm).toHaveBeenCalled();
    // Staying keeps the draft on screen.
    expect(screen.getByRole('heading', { name: 'auto/legal' })).toBeTruthy();
    expect(screen.getByRole('radio', { name: 'Live' }).getAttribute('aria-checked')).toBe('true');
    confirm.mockRestore();
  });

  it('guards a new router’s draft too', async () => {
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    renderAt('/admin/routing/routers/new?template=onprem');
    const user = userEvent.setup();
    await screen.findByRole('heading', { name: 'auto/legal' });
    await user.type(screen.getByRole('textbox', { name: 'Description' }), ' draft');
    await user.click(screen.getByRole('link', { name: 'Evaluations' }));
    expect(confirm).toHaveBeenCalled();
    confirm.mockRestore();
  });

  it('keeps the section title and tabs, with a way back to the routers', async () => {
    renderAt('/admin/routing/routers/r-1');
    await screen.findByRole('heading', { name: 'auto/legal' });
    expect(screen.getByRole('heading', { name: 'Auto-routing', level: 1 })).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Evaluations' })).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Back to all routers' }).getAttribute('href')).toBe('/admin/routing/routers');
  });
});

describe('Router instructions', () => {
  it('saves router, default and route instructions with a counter', async () => {
    mocked.put.mockResolvedValue({ router });
    renderAt('/admin/routing/routers/r-1');
    const user = userEvent.setup();
    await screen.findByRole('heading', { name: 'auto/legal' });
    expect(screen.getByText(/added before the caller’s own system prompt/)).toBeTruthy();
    expect(screen.getByText(/are not scanned/)).toBeTruthy();
    const routerBox = screen.getByRole('textbox', { name: 'Router instructions' });
    await user.type(routerBox, 'You are talking with a child.');
    expect(screen.getByTestId('router-instructions-count').textContent).toBe('29 / 8,000');
    await user.type(screen.getByRole('textbox', { name: 'Default instructions' }), 'Be brief.');
    await user.click(screen.getByRole('button', { name: 'Edit route 1' }));
    await user.type(await screen.findByRole('textbox', { name: 'Route instructions' }), 'Cite the clause.');
    await user.click(screen.getByRole('button', { name: 'Done' }));
    expect(screen.getByText(/· instructions/)).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    const body = mocked.put.mock.calls[0]![1] as { config: Router['config'] };
    expect(body.config.instructions).toBe('You are talking with a child.');
    expect(body.config.default.instructions).toBe('Be brief.');
    expect(body.config.routes[0]!.instructions).toBe('Cite the clause.');
  });

  it('flags instructions over 8,000 characters', async () => {
    renderAt('/admin/routing/routers/r-1');
    await screen.findByRole('heading', { name: 'auto/legal' });
    const box = screen.getByRole('textbox', { name: 'Router instructions' });
    // Typing 8,001 characters key by key is slow; paste them.
    const user = userEvent.setup();
    box.focus();
    await user.paste('x'.repeat(8001));
    expect(screen.getByTestId('router-instructions-count').textContent).toBe('8,001 / 8,000');
    expect(screen.getByText('Router instructions are limited to 8,000 characters.')).toBeTruthy();
  });

  it('shows the server’s validation message', async () => {
    const { ApiError } = await import('../../lib/api');
    mocked.put.mockRejectedValue(
      new ApiError(400, {
        code: 'invalid_request',
        message: 'Router instructions are limited to 8,000 characters (they have 9,000).',
        param: 'instructions',
      } as never),
    );
    renderAt('/admin/routing/routers/r-1');
    const user = userEvent.setup();
    await screen.findByRole('heading', { name: 'auto/legal' });
    await user.type(screen.getByRole('textbox', { name: 'Router instructions' }), 'x');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect((await screen.findAllByText(/they have 9,000/)).length).toBeGreaterThan(0);
  });
});

describe('Family template', () => {
  it('fills instructions, turns sticky off and creates its categories before the router', async () => {
    const created: string[] = [];
    mocked.post.mockImplementation(async (path: string, body?: unknown) => {
      if (path.endsWith('/routing/categories')) {
        created.push((body as { name: string }).name);
        return {};
      }
      if (path.endsWith('/routing/routers')) return { router: { ...router, id: 'r-new' } };
      throw new Error(`unexpected POST ${path}`);
    });
    renderAt('/admin/routing/routers/new?template=family');
    const user = userEvent.setup();
    expect(await screen.findByRole('heading', { name: 'auto/family' })).toBeTruthy();
    const routerBox = screen.getByRole('textbox', { name: 'Router instructions' }) as HTMLTextAreaElement;
    expect(routerBox.value).toMatch(/talking with a child/);
    expect(routerBox.value).toMatch(/trusted adult/);
    expect((screen.getByRole('textbox', { name: 'Default instructions' }) as HTMLTextAreaElement).value).toBe('');
    expect(await screen.findByText(/also adds 5 routing categories/)).toBeTruthy();
    for (const route of [
      'Feeling low',
      'Not suitable for children',
      'Getting around parental controls',
      'Sharing personal information',
      'Homework done for them',
      'Homework learning',
    ]) {
      expect(screen.getAllByText(route).length).toBeGreaterThan(0);
    }
    expect((screen.getByRole('checkbox', { name: 'Keep conversations on one route' }) as HTMLInputElement).checked).toBe(false);
    await user.click(screen.getByRole('button', { name: 'Create router' }));
    await waitFor(() => expect(mocked.post.mock.calls.some(([p]) => String(p).endsWith('/routing/routers'))).toBe(true));
    expect(created).toEqual([
      'Feeling low or unsafe',
      'Not suitable for children',
      'Getting around parental controls',
      'Sharing personal information',
      'Homework done for them',
    ]);
    const calls = mocked.post.mock.calls.map(([p]) => String(p));
    expect(calls.findIndex((p) => p.endsWith('/routing/routers'))).toBe(calls.length - 1);
    const body = mocked.post.mock.calls.find(([p]) => String(p).endsWith('/routing/routers'))![1] as {
      config: Router['config'];
    };
    expect(body.config.sticky).toBe(false);
    expect(body.config.routes.map((r) => r.categories[0])).toEqual([
      'feeling_low_or_unsafe',
      'not_suitable_for_children',
      'getting_around_parental_controls',
      'sharing_personal_information',
      'homework_done_for_them',
      'homework_learning',
    ]);
    expect(body.config.routes[0]!.instructions).toMatch(/988/);
    expect(body.config.routes[4]!.instructions).toMatch(/Never give the final answer/);
    expect(body.config.routes[5]!.instructions).toBe('');
    expect(body.config.routes.every((r) => !('hint' in r))).toBe(true);
  });

  it('leaves categories the admin already has alone', async () => {
    const feeling: RoutingCategory = {
      ...legal,
      id: 'cat-feel',
      slug: 'feeling_low_or_unsafe',
      name: 'Feeling low or unsafe',
      includes: 'My own wording.',
    };
    mocked.get.mockImplementation(async (path: string) => {
      if (path === '/api/v1/admin/routing') return doc;
      if (path.endsWith('/routing/categories'))
        return { categories: [legal, coding, feeling], insights_categories: [], max_enabled: 25 };
      if (path.endsWith('/routing/targets')) return { targets };
      throw new Error(`unexpected GET ${path}`);
    });
    const created: string[] = [];
    mocked.post.mockImplementation(async (path: string, body?: unknown) => {
      if (path.endsWith('/routing/categories')) created.push((body as { name: string }).name);
      return { router: { ...router, id: 'r-new' } };
    });
    renderAt('/admin/routing/routers/new?template=family');
    const user = userEvent.setup();
    expect(await screen.findByText(/also adds 4 routing categories/)).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Create router' }));
    await waitFor(() => expect(created.length).toBe(4));
    expect(created).not.toContain('Feeling low or unsafe');
  });
});
