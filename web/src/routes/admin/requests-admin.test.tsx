/**
 * Admin instance-wide request log viewer (/admin/requests): cross-user rows
 * with owner identity, filter/sort params sent to the API with the page
 * offset reset on filter change, an adjustable column set persisted
 * across remounts, and a drill-down drawer carrying the full request
 * detail plus the owning user's identity. Authorization itself is
 * server-enforced and covered by the Go handler tests.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { AdminRequestsPage } from './Requests';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

const eventMine = {
  id: 'evt-mine',
  created_at: '2025-01-02T10:00:00Z',
  user_id: 'user-a',
  user_email: 'ada@example.com',
  user_name: 'Ada Lovelace',
  token_id: 'tok-1',
  model: 'gpt-4o',
  endpoint_path: '/v1/chat/completions',
  http_method: 'POST',
  modality: 'chat',
  streaming: false,
  request_bytes: 512,
  response_bytes: 2048,
  tokens_in: 100,
  tokens_out: 40,
  tokens_cached: 0,
  token_accounting_method: 'upstream_reported',
  cost_nanousd: 1230000,
  finish_reason: 'stop',
  http_status: 200,
  latency_ms: 812,
  ttfb_ms: 120,
  client_user_agent: 'curl/8.0',
  client_ip: '10.0.0.1',
  x_forwarded_for: '',
  error_code: '',
  quota_violated: false,
  request_id: 'req-a',
  secgw_action: 'checked',
  secgw_violations: 0,
};

const eventForeign = {
  ...eventMine,
  id: 'evt-foreign',
  user_id: 'user-b',
  user_email: 'bob@example.com',
  user_name: 'Bob Babbage',
  model: 'claude-3-haiku',
  http_status: 429,
  error_code: 'quota_exceeded',
  request_id: 'req-b',
  secgw_action: 'observed',
  secgw_violations: 1,
  tokens_in_per_second: 833.3,
  tokens_out_per_second: 57.8,
  throughput_source: 'calculated',
};

function mockApi() {
  mocked.get.mockImplementation((path: string) => {
    if (path.startsWith('/api/v1/requests')) {
      return Promise.resolve({ requests: [eventMine, eventForeign], total_count: 120 });
    }
    if (path.startsWith('/api/v1/admin/users')) {
      return Promise.resolve({
        users: [
          { id: 'user-a', email: 'ada@example.com', name: 'Ada Lovelace', role: 'admin', is_active: true, groups: [] },
          { id: 'user-b', email: 'bob@example.com', name: 'Bob Babbage', role: 'user', is_active: true, groups: [] },
        ],
        total_count: 2,
      });
    }
    if (path.startsWith('/api/v1/admin/models')) {
      return Promise.resolve({ models: [], counts: {}, modalities: [] });
    }
    if (path === '/api/v1/admin/requests/evt-foreign/security') {
      return Promise.resolve({
        request_id: 'req-b',
        action: 'observed',
        classifier_runs: [
          {
            kind: 'prompt_injection',
            model_name: 'Llama-Prompt-Guard-2-86M',
            direction: 'ingress',
            latency_ms: 41,
            http_status: 200,
            findings: [
              {
                id: 'v-1',
                kind: 'prompt_injection',
                rule_id: 'MALICIOUS',
                action: 'observed',
                direction: 'ingress',
                classifier_score: 0.9994,
                created_at: '2025-01-02T10:00:00Z',
              },
            ],
          },
        ],
        other_violations: [],
      });
    }
    if (path === '/api/v1/admin/requests/evt-mine/security') {
      return Promise.resolve({
        request_id: 'req-a',
        action: 'checked',
        classifier_runs: [{ model_name: 'Llama-Prompt-Guard-2-86M', latency_ms: 38, http_status: 200, findings: [] }],
        other_violations: [],
      });
    }
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
}

function requestCalls(): string[] {
  return mocked.get.mock.calls.map(([path]) => path as string).filter((path) => path.startsWith('/api/v1/requests'));
}

function renderAt(initialEntry = '/admin/requests') {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <Routes>
          <Route path="/admin/requests" element={<AdminRequestsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  window.localStorage.clear();
  mockApi();
});

describe('request security preview completeness', () => {
  it.each([true, false])('labels request violation scope (has_more=%s)', async (hasMore) => {
    const get = mocked.get.getMockImplementation()!;
    mocked.get.mockImplementation((path: string) =>
      path.endsWith('/evt-mine/security')
        ? Promise.resolve({
            request_id: 'req-a',
            classifier_runs: [{ model_name: 'guard', latency_ms: 1, http_status: 200, findings: [] }],
            other_violations: [{ id: 'v-preview', kind: 'pii', rule_id: 'email', action: 'observed' }],
            violations_has_more: hasMore,
            violations_limit: 200,
            violations_scanned: 200,
            violations_scope_available: true,
          })
        : get(path),
    );
    renderAt();
    await userEvent.click((await screen.findByRole('cell', { name: 'gpt-4o' })).closest('tr')!);
    if (hasMore) {
      expect(await screen.findByText(/Additional request violations are omitted/)).toBeTruthy();
      expect(screen.getByText('No findings in bounded preview')).toBeTruthy();
    } else expect(await screen.findByText('1 non-classifier findings from all 200 request violations.')).toBeTruthy();
  });
});

describe('charged teams in the admin request list', () => {
  it('shows named charged teams and personal labels on their event rows', async () => {
    const defaultGet = mocked.get.getMockImplementation()!;
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/requests')) {
        return Promise.resolve({
          requests: [
            { ...eventMine, team_ids: 'team-platform', team_names: ['Platform'] },
            { ...eventForeign, team_ids: '', team_names: [] },
          ],
          total_count: 2,
        });
      }
      return defaultGet(path);
    });
    renderAt();

    expect(await screen.findByRole('columnheader', { name: 'Charged to' })).toBeTruthy();
    expect(screen.getByRole('cell', { name: 'Platform' }).closest('tr')?.textContent).toContain('Ada Lovelace');
    expect(screen.getByRole('cell', { name: 'Personal' }).closest('tr')?.textContent).toContain('Bob Babbage');
  });
});

describe('instance-wide rows with user identity', () => {
  it('requests scope=all and renders foreign users’ events with their identity', async () => {
    renderAt();

    // Rows owned by two different users render, each identifying its owner.
    expect(await screen.findByRole('cell', { name: 'Ada Lovelace' })).toBeTruthy();
    expect(await screen.findByRole('cell', { name: 'Bob Babbage' })).toBeTruthy();
    expect(screen.getByRole('cell', { name: 'claude-3-haiku' })).toBeTruthy();

    // The cross-user scope is requested explicitly.
    expect(requestCalls()[0]).toContain('scope=all');
  });
});

describe('filters, sorting, and page reset', () => {
  it('sends filter params and resets the page offset when a filter changes', async () => {
    const user = userEvent.setup();
    renderAt('/admin/requests?page=50');

    await screen.findByRole('cell', { name: 'Ada Lovelace' });
    expect(requestCalls().at(-1)).toContain('offset=50');

    await user.selectOptions(screen.getByLabelText('Status'), 'error');
    await waitFor(() => {
      const last = requestCalls().at(-1) ?? '';
      expect(last).toContain('status=error');
      expect(last).toContain('offset=0');
    });
  });

  it('filters by user via user_id and keeps scope=all', async () => {
    const user = userEvent.setup();
    renderAt();
    await screen.findByRole('cell', { name: 'Ada Lovelace' });

    await user.type(screen.getByLabelText('User'), 'Bob');
    await user.click(await screen.findByRole('option', { name: /Bob Babbage/ }));
    await waitFor(() => {
      const last = requestCalls().at(-1) ?? '';
      expect(last).toContain('user_id=user-b');
      expect(last).toContain('scope=all');
    });
  });

  it('sorting by a header sends the sort key', async () => {
    const user = userEvent.setup();
    renderAt();
    await screen.findByRole('cell', { name: 'Ada Lovelace' });

    await user.click(within(screen.getByRole('columnheader', { name: /Latency/ })).getByRole('button'));
    await waitFor(() => expect(requestCalls().at(-1)).toContain('sort=latency'));
  });
});

describe('adjustable columns', () => {
  it('hides a toggled-off column and persists the choice across a remount', async () => {
    const user = userEvent.setup();
    const first = renderAt();
    await screen.findByRole('cell', { name: 'Ada Lovelace' });

    // Default column set shows Cost; extras like Endpoint start hidden.
    expect(screen.getByRole('columnheader', { name: /Cost/ })).toBeTruthy();
    expect(screen.queryByRole('columnheader', { name: /Endpoint/ })).toBeNull();

    await user.click(screen.getByRole('button', { name: 'Columns' }));
    await user.click(screen.getByRole('checkbox', { name: 'Cost' }));
    expect(screen.queryByRole('columnheader', { name: /Cost/ })).toBeNull();

    // Show an extra column too.
    await user.click(screen.getByRole('checkbox', { name: 'Endpoint' }));
    expect(screen.getByRole('columnheader', { name: /Endpoint/ })).toBeTruthy();

    // The preference survives a full unmount/remount (localStorage-backed).
    first.unmount();
    renderAt();
    await screen.findByRole('cell', { name: 'Ada Lovelace' });
    expect(screen.queryByRole('columnheader', { name: /Cost/ })).toBeNull();
    expect(screen.getByRole('columnheader', { name: /Endpoint/ })).toBeTruthy();
  });
});

describe('drill-down drawer', () => {
  it('opens the full request detail with the owning user’s identity on row click', async () => {
    const user = userEvent.setup();
    renderAt();

    await user.click(await screen.findByRole('cell', { name: 'Bob Babbage' }));
    const drawer = await screen.findByRole('dialog');

    // Owner identity plus a link into the People page.
    expect(within(drawer).getByText(/bob@example.com/)).toBeTruthy();
    const peopleLink = within(drawer).getByRole('link', { name: 'View in People →' });
    expect(peopleLink.getAttribute('href')).toBe('/admin/users/user-b');

    // Full detail: endpoint, method, error code, metering, timing, client.
    expect(within(drawer).getByText('/v1/chat/completions')).toBeTruthy();
    expect(within(drawer).getByText('POST')).toBeTruthy();
    expect(within(drawer).getByText('quota_exceeded')).toBeTruthy();
    expect(within(drawer).getByText('curl/8.0')).toBeTruthy();
    expect(within(drawer).getByText('req-b')).toBeTruthy();
    expect(within(drawer).getByText('10.0.0.1')).toBeTruthy();
    // Throughput, labelled by origin so a gateway-clock estimate is never
    // mistaken for a provider measurement.
    expect(within(drawer).getByText('Input tokens / s: 833')).toBeTruthy();
    expect(within(drawer).getByText('Output tokens / s: 58')).toBeTruthy();
    expect(within(drawer).getByText('Calculated by gateway')).toBeTruthy();
  });

  it('names the counting method distinctly for cost-reported and unmetered media responses', async () => {
    const user = userEvent.setup();
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/requests')) {
        return Promise.resolve({
          requests: [
            {
              ...eventMine,
              id: 'evt-cost',
              user_name: 'Cost Case',
              token_accounting_method: 'upstream_reported_cost',
              modality: 'image',
            },
            {
              ...eventMine,
              id: 'evt-unmetered',
              user_name: 'Gap Case',
              token_accounting_method: 'unmetered_modality',
              modality: 'tts',
              tokens_in: 0,
              tokens_out: 0,
            },
          ],
          total_count: 2,
        });
      }
      if (path.startsWith('/api/v1/admin/users')) return Promise.resolve({ users: [], total_count: 0 });
      if (path.startsWith('/api/v1/models')) return Promise.resolve({ models: [] });
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
    renderAt();

    await user.click(await screen.findByRole('cell', { name: 'Cost Case' }));
    let drawer = await screen.findByRole('dialog');
    expect(within(drawer).getByText('Cost reported by upstream')).toBeTruthy();
    await user.keyboard('{Escape}');

    await user.click(await screen.findByRole('cell', { name: 'Gap Case' }));
    drawer = await screen.findByRole('dialog');
    expect(within(drawer).getByText('Not metered — upstream reported no usage')).toBeTruthy();
    // No throughput row: nothing was metered, so nothing can be derived.
    expect(within(drawer).queryByText(/tokens \/ s/)).toBeNull();
  });
});

describe('token columns — split, sortable, filterable (admin instance-wide log)', () => {
  it('renders Tokens in and Tokens out as two separate sortable columns instead of the combined cell', async () => {
    const user = userEvent.setup();
    renderAt();
    await screen.findByRole('cell', { name: 'Ada Lovelace' });

    expect(screen.getByRole('columnheader', { name: /Tokens in/ })).toBeTruthy();
    expect(screen.getByRole('columnheader', { name: /Tokens out/ })).toBeTruthy();
    expect(screen.queryByRole('columnheader', { name: /Tokens in \/ out/ })).toBeNull();
    // eventMine has tokens_in 100 / tokens_out 40: separate cells in its row,
    // no combined "100 / 40" cell anywhere.
    const row = screen.getByRole('cell', { name: 'Ada Lovelace' }).closest('tr');
    if (!row) throw new Error('row not found');
    expect(within(row).getByRole('cell', { name: '100' })).toBeTruthy();
    expect(within(row).getByRole('cell', { name: '40' })).toBeTruthy();
    expect(screen.queryByRole('cell', { name: '100 / 40' })).toBeNull();

    // Each token header sorts: descending first, ascending on the second click.
    await user.click(screen.getByRole('button', { name: /Tokens in/ }));
    await waitFor(() => expect(requestCalls().at(-1)).toContain('sort=tokens_in'));
    await user.click(screen.getByRole('button', { name: /Tokens in/ }));
    await waitFor(() => expect(requestCalls().at(-1)).toContain('sort=tokens_in_asc'));
    await user.click(screen.getByRole('button', { name: /Tokens out/ }));
    await waitFor(() => expect(requestCalls().at(-1)).toContain('sort=tokens_out'));
    expect(requestCalls().at(-1)).not.toContain('tokens_in_asc');
  });

  it('token-size filters send strict gt/lt params, reset the page, reach the CSV export, and clear together', async () => {
    const user = userEvent.setup();
    renderAt('/admin/requests?page=100');
    await screen.findByRole('cell', { name: 'Ada Lovelace' });
    expect(requestCalls().at(-1)).toContain('offset=100');

    const tokensIn = screen.getByLabelText('Tokens in');
    const tokensOut = screen.getByLabelText('Tokens out');
    // Every threshold is offered in both directions.
    const values = Array.from((tokensIn as HTMLSelectElement).options).map((o) => o.value);
    expect(values).toEqual([
      '',
      'lt:1000',
      'gt:1000',
      'lt:10000',
      'gt:10000',
      'lt:100000',
      'gt:100000',
      'lt:500000',
      'gt:500000',
    ]);
    expect(within(tokensIn).getByRole('option', { name: 'Greater than 500,000' })).toBeTruthy();
    expect(within(tokensOut).getByRole('option', { name: 'Smaller than 1,000' })).toBeTruthy();

    await user.selectOptions(tokensIn, 'gt:100000');
    await waitFor(() => {
      const last = requestCalls().at(-1) ?? '';
      expect(last).toContain('scope=all');
      expect(last).toContain('tokens_in_gt=100000');
      expect(last).not.toContain('tokens_in_lt');
      // Narrowing the list resets the page offset.
      expect(last).toContain('offset=0');
    });
    await user.selectOptions(tokensOut, 'lt:10000');
    await waitFor(() => {
      const last = requestCalls().at(-1) ?? '';
      expect(last).toContain('tokens_in_gt=100000');
      expect(last).toContain('tokens_out_lt=10000');
      expect(last).not.toContain('tokens_out_gt');
    });

    // The CSV export carries the same narrowing (plus the admin scope).
    const exportHref = screen.getByRole('link', { name: 'Export CSV' }).getAttribute('href') ?? '';
    expect(exportHref.startsWith('/api/v1/requests.csv?')).toBe(true);
    expect(exportHref).toContain('scope=all');
    expect(exportHref).toContain('tokens_in_gt=100000');
    expect(exportHref).toContain('tokens_out_lt=10000');

    await user.click(screen.getByRole('button', { name: 'Clear filters' }));
    await waitFor(() => {
      const last = requestCalls().at(-1) ?? '';
      expect(last).not.toContain('tokens_in_');
      expect(last).not.toContain('tokens_out_');
    });
    expect(screen.getByRole('link', { name: 'Export CSV' }).getAttribute('href')).not.toContain('tokens_');
  });

  it('migrates a stored column preference naming the old combined "tokens" column onto both new columns', async () => {
    window.localStorage.setItem('janus.adminRequests.columns', 'time,model,tokens');
    renderAt();
    await screen.findByRole('cell', { name: 'gpt-4o' });

    const headers = screen.getAllByRole('columnheader').map((th) => th.textContent ?? '');
    expect(headers.some((h) => h.includes('Tokens in'))).toBe(true);
    expect(headers.some((h) => h.includes('Tokens out'))).toBe(true);
    // Columns the stored preference did not name stay hidden.
    expect(headers.some((h) => h.includes('Cost'))).toBe(false);
    expect(headers.some((h) => h.includes('Latency'))).toBe(false);
  });
});

describe('admin requests — security gateway', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    cleanup();
    mockApi();
    window.localStorage.clear();
  });

  it("shows whether each request was inspected, and never lists the gateway's own classifier calls as requests", async () => {
    renderAt();
    // "Checked" is the clean case: a policy ran and found nothing, which has
    // no violation row anywhere and would otherwise be invisible.
    await waitFor(() => expect(screen.getByText('Checked')).toBeTruthy());
    expect(screen.getByText('1 found')).toBeTruthy();

    // Classifier calls are hidden by default: the request list must not ask
    // for them unless the admin opts in.
    const initial = requestCalls();
    expect(initial.some((path) => path.includes('include_internal=true'))).toBe(false);

    await userEvent.click(screen.getByRole('button', { name: /show gateway internals/i }));
    await waitFor(() => expect(requestCalls().some((path) => path.includes('include_internal=true'))).toBe(true));
  });

  it('breaks down the classifiers that ran on one request in its drawer', async () => {
    renderAt();
    await waitFor(() => expect(screen.getByText('1 found')).toBeTruthy());
    const row = screen.getAllByRole('row').find((candidate) => candidate.textContent?.includes('claude-3-haiku'));
    if (!row) throw new Error('no row for claude-3-haiku');
    await userEvent.click(row);

    const drawer = await screen.findByRole('dialog');
    await waitFor(() => expect(within(drawer).getByText('Llama-Prompt-Guard-2-86M')).toBeTruthy());
    expect(within(drawer).getByText(/MALICIOUS/)).toBeTruthy();
    expect(within(drawer).getByText(/0\.999/)).toBeTruthy();
  });
});
