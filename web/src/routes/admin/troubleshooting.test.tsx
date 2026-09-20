/**
 * Troubleshooting mode card on /admin/system and the per-row download on the
 * admin Requests page.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { TroubleshootingPut, TroubleshootingStatus, UsageEvent } from '../../lib/types';
import { ToastProvider } from '../../components/ui';
import { TroubleshootingCard } from './TroubleshootingCard';
import { AdminRequestsPage } from './Requests';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

const offStatus: TroubleshootingStatus = {
  active: false,
  session: null,
  stats: { count: 0, total_bytes: 0, oldest_at: '0001-01-01T00:00:00Z', newest_at: '0001-01-01T00:00:00Z' },
  backends: ['database', 'disk'],
  encryption_available: true,
  max_body_bytes_ceiling: 33554432,
  max_session_hours: 168,
  retention_last_run_at: '0001-01-01T00:00:00Z',
  warnings: [],
};

const activeStatus: TroubleshootingStatus = {
  ...offStatus,
  active: true,
  session: {
    id: 'ts-1',
    enabled: true,
    created_by: 'admin',
    created_at: '2026-08-20T10:00:00Z',
    updated_at: '2026-08-20T10:00:00Z',
    expires_at: new Date(Date.now() + 6 * 3_600_000).toISOString(),
    disabled_at: '0001-01-01T00:00:00Z',
    config: {
      filter: { match: 'any', models: ['gpt-4o'], error_codes: ['rate_limited'], outcome: '' },
      retention: { max_age_hours: 24, max_count: 500, max_bytes: 0 },
      storage: 'database',
      capture_request_body: true,
      capture_response_body: true,
      encrypt: false,
      max_body_bytes: 1048576,
    },
  },
  stats: { count: 12, total_bytes: 4096, oldest_at: '2026-08-20T10:05:00Z', newest_at: '2026-08-20T11:00:00Z' },
  warnings: ['Captured bodies are stored unencrypted.'],
};

function renderCard() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter>
          <TroubleshootingCard />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('troubleshooting card — off state', () => {
  it('shows Off, disables data actions, and enabling sends the assembled config', async () => {
    mocked.get.mockResolvedValue(offStatus);
    mocked.put.mockResolvedValue({ ...activeStatus, warnings: [] });
    renderCard();
    const user = userEvent.setup();

    expect(await screen.findByText('Off')).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Purge all captured data' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Run cleanup now' }) as HTMLButtonElement).disabled).toBe(true);
    // Defaults: failures only, encrypted, bounded retention → no warnings.
    expect(screen.queryByRole('alert')).toBeNull();

    // The default "failures only" rule is a Require (AND) rule; nothing else
    // is on screen yet, so the builder stays minimal.
    expect(screen.getByTestId('troubleshoot-gate-require')).toBeTruthy();
    expect(screen.queryByTestId('troubleshoot-gate-include')).toBeNull();
    expect(screen.queryByTestId('troubleshoot-gate-exclude')).toBeNull();

    // Include (OR): models and HTTP statuses.
    await user.click(screen.getByRole('button', { name: '+ Add include (OR)' }));
    const include = screen.getByTestId('troubleshoot-gate-include');
    await user.selectOptions(within(include).getAllByLabelText('Criterion').at(-1)!, 'models');
    await user.type(within(include).getByLabelText('Models'), 'gpt-4o, claude-3{Enter}');
    await user.click(screen.getByRole('button', { name: '+ Add include (OR)' }));
    await user.selectOptions(within(include).getAllByLabelText('Criterion').at(-1)!, 'httpStatuses');
    await user.type(within(include).getByLabelText('HTTP statuses'), '429, 500{Enter}');

    // Require (AND): a token bound next to the default outcome rule.
    await user.click(screen.getByRole('button', { name: '+ Add require (AND)' }));
    const require = screen.getByTestId('troubleshoot-gate-require');
    await user.selectOptions(within(require).getAllByLabelText('Criterion').at(-1)!, 'tokensIn');
    await user.type(within(require).getByLabelText(/^Tokens in greater than/), '10000');

    // Exclude (NOT): a service token whose traffic must never be captured.
    await user.click(screen.getByRole('button', { name: '+ Add exclude (NOT)' }));
    const exclude = screen.getByTestId('troubleshoot-gate-exclude');
    await user.selectOptions(within(exclude).getAllByLabelText('Criterion').at(-1)!, 'userIds');
    await user.type(within(exclude).getByLabelText('Users or service tokens'), 'svc-1{Enter}');

    await user.click(screen.getByRole('button', { name: 'Enable capture' }));

    await waitFor(() => expect(mocked.put).toHaveBeenCalledTimes(1));
    const [path, body] = mocked.put.mock.calls[0] as [string, TroubleshootingPut];
    expect(path).toBe('/api/v1/admin/troubleshooting');
    expect(body.enabled).toBe(true);
    expect(body.expires_in_hours).toBe(24);
    // Include rules are the top-level criteria under OR; the other gates
    // travel as optional sub-objects.
    expect(body.config.filter).toMatchObject({
      match: 'any',
      models: ['gpt-4o', 'claude-3'],
      http_statuses: [429, 500],
      outcome: '',
      require: { outcome: 'failure', tokens_in_gt: 10000 },
      exclude: { user_ids: ['svc-1'] },
    });
    expect(body.config.filter.tokens_in_lt).toBeUndefined();
    expect(body.config.filter.require?.tokens_in_lt).toBeUndefined();
    expect(body.config.filter.require?.models).toEqual([]);
    expect(body.config).toMatchObject({
      capture_request_body: true,
      capture_response_body: true,
      encrypt: true,
      storage: 'database',
      max_body_bytes: 1024 * 1024,
      retention: { max_age_hours: 24, max_count: 500, max_bytes: 256 * 1048576 },
    });
    expect(await screen.findByText('Capturing')).toBeTruthy();
  });

  it('organises rules into Include/Require/Exclude gates with badges, collapsing, chips and clearing', async () => {
    mocked.get.mockResolvedValue(offStatus);
    renderCard();
    const user = userEvent.setup();

    const builder = await screen.findByTestId('troubleshoot-filter-builder');
    // Only the gate that has a rule is rendered; the gate badge names the logic.
    const require = within(builder).getByTestId('troubleshoot-gate-require');
    expect(within(require).getByText('AND')).toBeTruthy();
    expect(within(require).getByText('Require')).toBeTruthy();
    expect(within(require).getByText('Requests must match all of these rules to be captured.')).toBeTruthy();
    expect((within(require).getByLabelText('Criterion') as HTMLSelectElement).value).toBe('outcome');
    expect((within(require).getByLabelText('Outcome') as HTMLSelectElement).value).toBe('failure');
    // The default filter (failures only) shows as exactly one criterion.
    const summary = within(builder).getByTestId('troubleshoot-filter-summary');
    expect(summary.textContent).toContain('1 criterion set');
    expect(within(summary).getByRole('button', { name: /^AND\s*Outcome: Failures only/ })).toBeTruthy();

    // Adding an Include rule opens the OR gate with an empty selector; a
    // criterion already used in the gate cannot be picked twice.
    await user.click(within(builder).getByRole('button', { name: '+ Add include (OR)' }));
    const include = within(builder).getByTestId('troubleshoot-gate-include');
    expect(within(include).getByText('OR')).toBeTruthy();
    expect((within(include).getByLabelText('Criterion') as HTMLSelectElement).value).toBe('');
    expect((within(builder).getByRole('button', { name: '+ Add include (OR)' }) as HTMLButtonElement).disabled).toBe(true);
    await user.selectOptions(within(include).getByLabelText('Criterion'), 'models');
    await user.type(within(include).getByLabelText('Models'), 'gpt-4o{Enter}claude-3,');
    expect(within(include).getByRole('button', { name: 'Remove gpt-4o' })).toBeTruthy();
    expect(within(include).getByRole('button', { name: 'Remove claude-3' })).toBeTruthy();
    await user.click(within(builder).getByRole('button', { name: '+ Add include (OR)' }));
    const second = within(include).getAllByLabelText('Criterion').at(-1) as HTMLSelectElement;
    expect((within(second).getByRole('option', { name: 'Models' }) as HTMLOptionElement).disabled).toBe(true);
    expect((within(second).getByRole('option', { name: 'Upstreams' }) as HTMLOptionElement).disabled).toBe(false);

    // Exclude (NOT) with a token bound; the summary groups chips by gate.
    await user.click(within(builder).getByRole('button', { name: '+ Add exclude (NOT)' }));
    const exclude = within(builder).getByTestId('troubleshoot-gate-exclude');
    expect(within(exclude).getByText('NOT')).toBeTruthy();
    await user.selectOptions(within(exclude).getByLabelText('Criterion'), 'tokensOut');
    await user.type(within(exclude).getByLabelText(/^Tokens out greater than/), '5000');
    expect(summary.textContent).toContain('3 criteria set');
    expect(within(summary).getByRole('button', { name: /^OR\s*Models: gpt-4o, claude-3 ×/ })).toBeTruthy();
    expect(within(summary).getByRole('button', { name: /^NOT\s*Tokens out > 5,000 ×/ })).toBeTruthy();

    // A gate collapses to its header (rules stay set) and expands again.
    await user.click(within(exclude).getByRole('button', { name: 'Collapse Exclude rules' }));
    expect(within(exclude).queryByLabelText(/^Tokens out greater than/)).toBeNull();
    expect(within(exclude).getByText('1 rule')).toBeTruthy();
    expect(summary.textContent).toContain('3 criteria set');
    await user.click(within(exclude).getByRole('button', { name: 'Expand Exclude rules' }));
    expect((within(exclude).getByLabelText(/^Tokens out greater than/) as HTMLInputElement).value).toBe('5000');

    // A chip clears exactly its rule; the rule row goes with it.
    await user.click(within(summary).getByRole('button', { name: /^NOT\s*Tokens out > 5,000 ×/ }));
    expect(within(builder).queryByTestId('troubleshoot-gate-exclude')).toBeNull();
    expect(summary.textContent).toContain('2 criteria set');

    // Removing a value chip, then the rule's trash button.
    await user.click(within(include).getByRole('button', { name: 'Remove claude-3' }));
    expect(within(summary).getByRole('button', { name: /^OR\s*Models: gpt-4o ×/ })).toBeTruthy();
    await user.click(within(include).getAllByRole('button', { name: 'Remove rule' })[0]!);
    expect(summary.textContent).toContain('1 criterion set');

    // Clear all empties every gate and surfaces the empty-filter warning.
    await user.click(within(summary).getByRole('button', { name: 'Clear all criteria' }));
    expect(within(builder).queryByTestId('troubleshoot-gate-require')).toBeNull();
    expect(within(builder).queryByTestId('troubleshoot-gate-include')).toBeNull();
    expect(summary.textContent).toContain('No criteria set');
    expect(within(builder).getByText(/every proxied request will be captured/)).toBeTruthy();
    expect(within(builder).getByText(/No rules yet/)).toBeTruthy();
  });

  it('shows a session saved by the old AND builder as Require rules', async () => {
    mocked.get.mockResolvedValue({
      ...activeStatus,
      session: {
        ...activeStatus.session!,
        config: {
          ...activeStatus.session!.config,
          filter: { match: 'all', models: ['gpt-4o'], outcome: 'failure', exclude: { user_ids: ['svc-9'] } },
        },
      },
      warnings: [],
    });
    renderCard();
    const require = await screen.findByTestId('troubleshoot-gate-require');
    expect(within(require).getByRole('button', { name: 'Remove gpt-4o' })).toBeTruthy();
    expect((within(require).getByLabelText('Outcome') as HTMLSelectElement).value).toBe('failure');
    expect(screen.queryByTestId('troubleshoot-gate-include')).toBeNull();
    const exclude = screen.getByTestId('troubleshoot-gate-exclude');
    expect(within(exclude).getByRole('button', { name: 'Remove svc-9' })).toBeTruthy();
  });

  it('blocks enabling while retention is unbounded and warns on unencrypted bodies', async () => {
    mocked.get.mockResolvedValue(offStatus);
    renderCard();
    const user = userEvent.setup();
    await screen.findByText('Off');

    await user.click(screen.getByLabelText(/Encrypt bodies at rest/));
    expect(screen.getByRole('alert').textContent).toContain('stored unencrypted');

    for (const label of [/^Delete after/, /^Keep at most \(requests\)/, /^Keep at most \(MB\)/]) {
      await user.clear(screen.getByLabelText(label));
    }
    expect(screen.getByText(/Set at least one retention bound/)).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Enable capture' }) as HTMLButtonElement).disabled).toBe(true);
    expect(mocked.put).not.toHaveBeenCalled();
  });
});

describe('troubleshooting card — active session', () => {
  it('seeds the form from the session, surfaces backend warnings, and disable/purge call their endpoints', async () => {
    mocked.get.mockResolvedValue(activeStatus);
    mocked.del.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/troubleshooting') return Promise.resolve(offStatus);
      if (path === '/api/v1/admin/troubleshooting/data') return Promise.resolve({ deleted: 12, bytes_freed: 4096 });
      return Promise.reject(new Error(`unexpected DELETE ${path}`));
    });
    mocked.post.mockResolvedValue({ deleted: 3, session_expired: false });
    renderCard();
    const user = userEvent.setup();

    expect(await screen.findByText('Capturing')).toBeTruthy();
    // Both the backend's computed warning and the form's live warning show.
    const alerts = screen.getAllByRole('alert');
    expect(alerts).toHaveLength(2);
    expect(alerts.map((a) => a.textContent).join(' ')).toContain('stored unencrypted');
    expect(screen.getByText('12 request(s) · 4.0 KB')).toBeTruthy();
    // OR criteria at the top level of the saved filter land in the Include gate.
    const include = screen.getByTestId('troubleshoot-gate-include');
    expect(within(include).getByRole('button', { name: 'Remove gpt-4o' })).toBeTruthy();
    expect(within(include).getByRole('button', { name: 'Remove rate_limited' })).toBeTruthy();
    expect(screen.queryByTestId('troubleshoot-gate-require')).toBeNull();
    expect((screen.getByLabelText(/Encrypt bodies at rest/) as HTMLInputElement).checked).toBe(false);
    // Save is idle until something changes.
    expect((screen.getByRole('button', { name: 'Save rules' }) as HTMLButtonElement).disabled).toBe(true);

    await user.click(screen.getByRole('button', { name: 'Run cleanup now' }));
    await waitFor(() => expect(mocked.post).toHaveBeenCalledWith('/api/v1/admin/troubleshooting/cleanup', {}));

    expect(screen.getByRole('link', { name: 'Export all (tar.gz)' }).getAttribute('href')).toBe(
      '/api/v1/admin/troubleshooting/export',
    );

    await user.click(screen.getByRole('button', { name: 'Purge all captured data' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/12 captured request\(s\) \(4.0 KB\)/)).toBeTruthy();
    await user.click(within(dialog).getByRole('button', { name: 'Purge everything' }));
    await waitFor(() => expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/troubleshooting/data'));

    await user.click(screen.getByRole('button', { name: 'Disable capture' }));
    await waitFor(() => expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/troubleshooting'));
    expect(await screen.findByText('Off')).toBeTruthy();
  });
});

describe('admin requests — capture download', () => {
  const base: UsageEvent = {
    id: 'evt-1',
    created_at: '2026-08-20T10:00:00Z',
    user_id: 'u1',
    token_id: 'tok',
    upstream_id: 'up',
    model_id: 'm',
    model: 'gpt-4o',
    endpoint_path: '/v1/chat/completions',
    http_method: 'POST',
    modality: 'chat',
    streaming: false,
    request_bytes: 10,
    response_bytes: 20,
    attachment_count: 0,
    tokens_in: 1,
    tokens_out: 2,
    tokens_cached: 0,
    tokens_cache_write_5m: 0,
    tokens_cache_write_1h: 0,
    token_accounting_method: 'exact',
    cost_nanousd: 0,
    finish_reason: 'stop',
    http_status: 500,
    latency_ms: 10,
    ttfb_ms: 5,
    upstream_latency_ms: 5,
    client_user_agent: '',
    client_ip: '',
    x_forwarded_for: '',
    referer: '',
    client_app: '',
    error_code: 'upstream_error',
    quota_violated: '',
    blocking_rule_id: '',
    request_id: 'req_abc',
  } as unknown as UsageEvent;

  function renderRequests() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <MemoryRouter initialEntries={['/admin/requests']}>
            <Routes>
              <Route path="/admin/requests" element={<AdminRequestsPage />} />
            </Routes>
          </MemoryRouter>
        </ToastProvider>
      </QueryClientProvider>,
    );
  }

  it('offers a download only for captured rows and in the detail drawer', async () => {
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/requests')) {
        return Promise.resolve({
          requests: [
            { ...base, has_capture: true },
            { ...base, id: 'evt-2', request_id: 'req_def', http_status: 200, error_code: '' },
          ],
          total_count: 2,
        });
      }
      if (path.startsWith('/api/v1/admin/users')) return Promise.resolve({ users: [], total_count: 0 });
      if (path.startsWith('/api/v1/admin/models')) return Promise.resolve({ models: [] });
      return Promise.resolve({});
    });
    renderRequests();
    const user = userEvent.setup();

    const link = await screen.findByRole('link', { name: 'Download captured payloads for request req_abc' });
    expect(link.getAttribute('href')).toBe('/api/v1/admin/requests/evt-1/download');
    expect(screen.queryByRole('link', { name: 'Download captured payloads for request req_def' })).toBeNull();
    expect(screen.getByRole('columnheader', { name: 'Capture' })).toBeTruthy();

    await user.click(screen.getAllByRole('cell', { name: 'gpt-4o' })[0]!);
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('link', { name: 'Download for analysis (tar.gz)' }).getAttribute('href')).toBe(
      '/api/v1/admin/requests/evt-1/download',
    );
  });

  it('renders no capture column when nothing on the page was captured', async () => {
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/requests')) return Promise.resolve({ requests: [base], total_count: 1 });
      if (path.startsWith('/api/v1/admin/users')) return Promise.resolve({ users: [], total_count: 0 });
      if (path.startsWith('/api/v1/admin/models')) return Promise.resolve({ models: [] });
      return Promise.resolve({});
    });
    renderRequests();
    await screen.findByRole('cell', { name: 'gpt-4o' });
    expect(screen.queryByRole('columnheader', { name: 'Capture' })).toBeNull();
  });
});

describe('troubleshooting card — lookups', () => {
  it('searches people as you type ("samp" → Casey Sample), shows the name, sends the id', async () => {
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/admin/troubleshooting')) return Promise.resolve(offStatus);
      if (path.startsWith('/api/v1/admin/users')) {
        const q = new URL(path, 'http://x').searchParams.get('search') ?? '';
        const all = [
          { id: 'u-casey', email: 'casey@example.org', name: 'Casey Sample' },
          { id: 'u-alice', email: 'alice@example.com', name: 'Alice Example' },
        ];
        return Promise.resolve({ users: all.filter((u) => `${u.name} ${u.email}`.toLowerCase().includes(q.toLowerCase())) });
      }
      if (path.startsWith('/api/v1/admin/service-tokens')) return Promise.resolve({ service_tokens: [] });
      return Promise.resolve({});
    });
    mocked.put.mockResolvedValue({ ...activeStatus, warnings: [] });
    renderCard();
    const user = userEvent.setup();
    await screen.findByText('Off');

    await user.click(screen.getByRole('button', { name: '+ Add include (OR)' }));
    const include = screen.getByTestId('troubleshoot-gate-include');
    await user.selectOptions(within(include).getAllByLabelText('Criterion').at(-1)!, 'userIds');
    const box = within(include).getByRole('combobox', { name: 'Users or service tokens' });
    await user.type(box, 'samp');

    // The search went to the server with what was typed…
    await waitFor(() =>
      expect(mocked.get.mock.calls.some(([p]) => String(p).includes('/admin/users?') && String(p).includes('search=samp'))).toBe(
        true,
      ),
    );
    // …and only the matching person is offered, by name with email beneath.
    const opt = await within(include).findByRole('option', { name: /Casey Sample/ });
    expect(within(include).queryByRole('option', { name: /Alice/ })).toBeNull();
    expect(opt.textContent).toContain('casey@example.org');
    await user.click(opt);
    expect(within(include).getByRole('button', { name: 'Remove Casey Sample' })).toBeTruthy();

    // The wire carries the id, never the display name.
    await user.click(screen.getByRole('button', { name: 'Enable capture' }));
    await waitFor(() => expect(mocked.put).toHaveBeenCalledTimes(1));
    const body = mocked.put.mock.calls[0]![1] as TroubleshootingPut;
    expect(body.config.filter.user_ids).toEqual(['u-casey']);
  });
});

describe('troubleshooting card — group lookup', () => {
  it('offers Groups as a rule, searches by name, shows the member count, sends the group id', async () => {
    mocked.get.mockImplementation((path: string) => {
      if (path.startsWith('/api/v1/admin/troubleshooting')) return Promise.resolve(offStatus);
      if (path.startsWith('/api/v1/admin/groups')) {
        return Promise.resolve({
          groups: [
            { id: 'g-legal', name: 'Legal', from_idp: true, member_count: 12 },
            { id: 'g-eng', name: 'Engineering', from_idp: false, member_count: 40 },
          ],
        });
      }
      return Promise.resolve({});
    });
    mocked.put.mockResolvedValue({ ...activeStatus, warnings: [] });
    renderCard();
    const user = userEvent.setup();
    await screen.findByText('Off');

    await user.click(screen.getByRole('button', { name: '+ Add include (OR)' }));
    const include = screen.getByTestId('troubleshoot-gate-include');
    await user.selectOptions(within(include).getAllByLabelText('Criterion').at(-1)!, 'groupIds');
    const box = within(include).getByRole('combobox', { name: 'Groups' });
    await user.type(box, 'leg');

    const opt = await within(include).findByRole('option', { name: /Legal/ });
    expect(within(include).queryByRole('option', { name: /Engineering/ })).toBeNull();
    expect(opt.textContent).toContain('12 members');
    await user.click(opt);
    expect(within(include).getByRole('button', { name: 'Remove Legal' })).toBeTruthy();

    await user.click(screen.getByRole('button', { name: 'Enable capture' }));
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    const body = mocked.put.mock.calls[0]![1] as TroubleshootingPut;
    expect(body.config.filter.group_ids).toEqual(['g-legal']);
  });
});
