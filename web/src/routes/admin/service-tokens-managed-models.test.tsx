/**
 * Service tokens, managed models & grants — UI contracts.
 *
 * These cover the things a screenshot would catch: that the admin tables
 * actually expose the operational columns (spend, tokens, requests) rather
 * than a bare list, that the grant dialog can reach service tokens and managed
 * models at all, and that destructive or silently-wide actions explain
 * themselves before they happen.
 */
import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter, Routes, Route } from 'react-router-dom';
import { ServiceTokensPage } from './ServiceTokens';
import { ManagedModelsPage } from './ManagedModels';
import { GrantsPage } from './Grants';
import { ToastProvider } from '../../components/ui';

const tokenRow = {
  id: 'st-1',
  name: 'nightly-summariser',
  description: 'Batch job',
  prefix: 'janus_svc_AbCdEf',
  created_by_user_id: 'u-1',
  created_at: '2026-01-01T00:00:00Z',
  last_used_at: '2026-02-01T00:00:00Z',
  expires_at: '',
  revoked_at: '',
  status: 'active' as const,
  grant_count: 2,
  spend_30d_usd: 12.5,
  requests_30d: 412,
  tokens_in_30d: 90_000,
  tokens_out_30d: 10_000,
  errors_30d: 3,
  top_model_30d: 'gpt-4o-mini',
  model_count_30d: 2,
};

const managedRow = {
  id: 'mm-1',
  name: 'current-best',
  description: 'House default',
  status: 'enabled' as const,
  target_model_id: 'm-1',
  target_model_name: 'gpt-4o',
  target_public_name: 'GPT-4o',
  target_status: 'enabled',
  target_upstream_id: 'up-1',
  target_upstream_name: 'OpenAI',
  modalities: ['text'],
  context_window: 128_000,
  servable: true,
  broken: false,
  grant_count: 3,
  created_by_user_id: 'u-1',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-02-01T00:00:00Z',
  spend_30d_usd: 40.25,
  requests_30d: 900,
  tokens_in_30d: 500_000,
  tokens_out_30d: 100_000,
  errors_30d: 0,
  distinct_principals_30d: 7,
};

function mockFetch(routes: Record<string, unknown>) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString();
    const method = (init?.method ?? 'GET').toUpperCase();
    const key = Object.keys(routes).find((k) => {
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

function renderPage(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter>{ui}</MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe('ServiceTokensPage', () => {
  it('opens a direct detail URL independently of list filters and preserves unchanged fields when saving', async () => {
    const fetchMock = mockFetch({
      'GET /api/v1/admin/service-tokens/st-1': { service_token: tokenRow },
      'GET /api/v1/admin/service-tokens': { service_tokens: [], total_count: 0 },
      'PATCH /api/v1/admin/service-tokens/st-1': { service_token: tokenRow },
    });
    vi.stubGlobal('fetch', fetchMock);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <MemoryRouter initialEntries={['/admin/service-tokens/st-1?q=hidden']}>
            <Routes>
              <Route path="/admin/service-tokens/:id" element={<ServiceTokensPage />} />
              <Route path="/admin/service-tokens" element={<ServiceTokensPage />} />
            </Routes>
          </MemoryRouter>
        </ToastProvider>
      </QueryClientProvider>,
    );
    const dialog = await screen.findByRole('dialog');
    expect((within(dialog).getByRole('textbox', { name: /Name/ }) as HTMLInputElement).value).toBe(tokenRow.name);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'PATCH')).toBe(true));
    const init = fetchMock.mock.calls.find(([, init]) => init?.method === 'PATCH')?.[1];
    expect(JSON.parse(String(init?.body))).toEqual({ name: tokenRow.name, description: tokenRow.description });
  });
  it('exposes the operational columns an admin needs, not just a name list', async () => {
    vi.stubGlobal(
      'fetch',
      mockFetch({
        'GET /api/v1/admin/service-tokens': { service_tokens: [tokenRow], total_count: 1 },
      }),
    );
    renderPage(<ServiceTokensPage />);

    expect(await screen.findByRole('link', { name: 'nightly-summariser' })).toBeTruthy();
    const row = screen.getByRole('link', { name: 'nightly-summariser' }).closest('tr')!;
    // Spend, request count and combined token volume all have to be visible
    // on the row itself — that is the whole point of the table.
    expect(within(row).getByText('$12.50')).toBeTruthy();
    expect(within(row).getByText('412')).toBeTruthy();
    expect(within(row).getByText('100,000')).toBeTruthy();
    // And the model it actually spends that money on.
    expect(within(row).getByText(/gpt-4o-mini/)).toBeTruthy();
  });

  it('sorts by a chosen column through the API rather than in the page', async () => {
    const fetchMock = mockFetch({
      'GET /api/v1/admin/service-tokens': { service_tokens: [tokenRow], total_count: 1 },
    });
    vi.stubGlobal('fetch', fetchMock);
    renderPage(<ServiceTokensPage />);
    await screen.findByRole('link', { name: 'nightly-summariser' });

    await userEvent.click(screen.getByRole('button', { name: /Requests \(30d\)/ }));

    await waitFor(() => {
      const called = fetchMock.mock.calls.some(([url]) => String(url).includes('sort=requests'));
      expect(called).toBe(true);
    });
  });

  it('shows the plaintext exactly once and warns that it cannot be recovered', async () => {
    vi.stubGlobal(
      'fetch',
      mockFetch({
        'GET /api/v1/admin/service-tokens': { service_tokens: [], total_count: 0 },
        'POST /api/v1/admin/service-tokens': {
          value: 'janus_svc_PLAINTEXT_ONLY_ONCE',
          service_token: tokenRow,
        },
      }),
    );
    renderPage(<ServiceTokensPage />);

    await userEvent.click(await screen.findByRole('button', { name: 'Issue token' }));
    const issueDialog = await screen.findByRole('dialog');
    await userEvent.type(within(issueDialog).getByRole('textbox', { name: /Name/ }), 'nightly-summariser');
    await userEvent.click(within(issueDialog).getByRole('button', { name: /^Issue token$/ }));

    expect(await screen.findByText('janus_svc_PLAINTEXT_ONLY_ONCE')).toBeTruthy();
    expect(screen.getByText(/can never show it again/i)).toBeTruthy();
  });

  it('reminds the admin that a new token has no access until granted', async () => {
    vi.stubGlobal('fetch', mockFetch({ 'GET /api/v1/admin/service-tokens': { service_tokens: [], total_count: 0 } }));
    renderPage(<ServiceTokensPage />);

    await userEvent.click(await screen.findByRole('button', { name: 'Issue token' }));
    // The all_users isolation is deliberate but surprising; the issue dialog
    // has to say so or admins will file it as a bug.
    expect(screen.getByText(/does not reach service tokens/i)).toBeTruthy();
  });

  it('explains what a revocation actually does before confirming', async () => {
    vi.stubGlobal('fetch', mockFetch({ 'GET /api/v1/admin/service-tokens': { service_tokens: [tokenRow], total_count: 1 } }));
    renderPage(<ServiceTokensPage />);

    const row = (await screen.findByRole('link', { name: 'nightly-summariser' })).closest('tr')!;
    await userEvent.click(within(row).getByRole('button', { name: 'Revoke' }));

    expect(await screen.findByText(/start failing immediately/i)).toBeTruthy();
    expect(screen.getByText(/cannot be restored/i)).toBeTruthy();
  });
});

describe('ManagedModelsPage', () => {
  it('shows usage for the alias itself, including how many callers use it', async () => {
    vi.stubGlobal('fetch', mockFetch({ 'GET /api/v1/admin/managed-models': { managed_models: [managedRow], total_count: 1 } }));
    renderPage(<ManagedModelsPage />);

    const row = (await screen.findByText('current-best')).closest('tr')!;
    expect(within(row).getByText('GPT-4o')).toBeTruthy();
    expect(within(row).getByText('$40.25')).toBeTruthy();
    expect(within(row).getByText('900')).toBeTruthy();
    expect(within(row).getByText('600,000')).toBeTruthy();
    // "Is anyone actually using this alias" — the question before a repoint.
    expect(within(row).getByText('7')).toBeTruthy();
  });

  it('badges a broken alias and explains the fault', async () => {
    vi.stubGlobal(
      'fetch',
      mockFetch({
        'GET /api/v1/admin/managed-models': {
          managed_models: [
            {
              ...managedRow,
              broken: true,
              servable: false,
              broken_reason: 'Target model is disabled.',
            },
          ],
          total_count: 1,
        },
      }),
    );
    renderPage(<ManagedModelsPage />);

    expect(await screen.findByText('Broken')).toBeTruthy();
    expect(screen.getByTitle('Target model is disabled.')).toBeTruthy();
  });

  it('warns before repointing, naming what the alias moves away from', async () => {
    vi.stubGlobal(
      'fetch',
      mockFetch({
        'GET /api/v1/admin/managed-models': { managed_models: [managedRow], total_count: 1 },
        'GET /api/v1/admin/models': {
          models: [
            { id: 'm-1', name: 'gpt-4o', status: 'enabled', upstream_name: 'OpenAI' },
            { id: 'm-2', name: 'claude-opus', status: 'enabled', upstream_name: 'Anthropic' },
          ],
        },
      }),
    );
    renderPage(<ManagedModelsPage />);

    const row = (await screen.findByText('current-best')).closest('tr')!;
    await userEvent.click(within(row).getByRole('button', { name: 'Edit' }));
    await waitFor(() => expect(screen.getByRole('combobox', { name: /Points at/ })).toBeTruthy());
    await userEvent.selectOptions(screen.getByRole('combobox', { name: /Points at/ }), 'm-2');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    // Naming both endpoints is the point: "are you sure" alone tells the
    // admin nothing about the blast radius.
    const confirm = await screen.findByRole('dialog', { name: /Repoint current-best/ });
    expect(within(confirm).getByText(/GPT-4o/)).toBeTruthy();
    expect(within(confirm).getByText(/claude-opus/)).toBeTruthy();
  });
});

describe('GrantsPage — create dialog', () => {
  const grantsRoutes = {
    'GET /api/v1/admin/grants': { grants: [] },
    'GET /api/v1/admin/models': {
      models: [
        { id: 'm-1', name: 'gpt-4o', status: 'enabled', upstream_name: 'OpenAI' },
        { id: 'm-2', name: 'gpt-legacy', status: 'pending_approval', upstream_name: 'OpenAI' },
        { id: 'm-3', name: 'gpt-old', status: 'disabled', upstream_name: 'OpenAI' },
      ],
    },
    'GET /api/v1/admin/managed-models': {
      managed_models: [managedRow, { ...managedRow, id: 'mm-2', name: 'retired-alias', status: 'disabled' as const }],
    },
    'GET /api/v1/admin/groups': { groups: [] },
    'GET /api/v1/admin/users': { users: [] },
    'GET /api/v1/admin/service-tokens': { service_tokens: [tokenRow], total_count: 1 },
  };

  it('offers service tokens as a grantee', async () => {
    vi.stubGlobal('fetch', mockFetch(grantsRoutes));
    renderPage(<GrantsPage />);

    await userEvent.click(await screen.findByRole('button', { name: /Grant access/i }));
    const grantee = await screen.findByRole('combobox', { name: /Grant to/ });
    // The whole feature is unreachable without these two options.
    expect(within(grantee).getByRole('option', { name: 'One service token' })).toBeTruthy();
    expect(within(grantee).getByRole('option', { name: 'Every service token' })).toBeTruthy();

    await userEvent.selectOptions(grantee, 'service_token');
    const picker = await screen.findByRole('combobox', { name: /Service token/ });
    expect(within(picker).getByRole('option', { name: /nightly-summariser/ })).toBeTruthy();
  });

  it('lists enabled managed models and hides pending or disabled entries', async () => {
    vi.stubGlobal('fetch', mockFetch(grantsRoutes));
    renderPage(<GrantsPage />);

    await userEvent.click(await screen.findByRole('button', { name: /Grant access/i }));
    const models = await screen.findByRole('listbox', { name: /Models/ });

    // Enabled alias is offered, so an admin can approve it.
    expect(within(models).getByRole('option', { name: /current-best/ })).toBeTruthy();
    // Enabled real model is offered.
    expect(within(models).getByRole('option', { name: /gpt-4o/ })).toBeTruthy();
    // Pending / disabled / disabled-alias are all withheld — granting them
    // would create a grant that can never be exercised.
    expect(within(models).queryByRole('option', { name: /gpt-legacy/ })).toBeNull();
    expect(within(models).queryByRole('option', { name: /gpt-old/ })).toBeNull();
    expect(within(models).queryByRole('option', { name: /retired-alias/ })).toBeNull();
  });

  it('posts a managed-model grant with model_kind so it is not mistaken for a real model', async () => {
    const fetchMock = mockFetch(grantsRoutes);
    vi.stubGlobal('fetch', fetchMock);
    renderPage(<GrantsPage />);

    await userEvent.click(await screen.findByRole('button', { name: /Grant access/i }));
    const models = await screen.findByRole('listbox', { name: /Models/ });
    await userEvent.selectOptions(models, 'managed:mm-1');
    await userEvent.selectOptions(screen.getByRole('combobox', { name: /Grant to/ }), 'service_token');
    await userEvent.selectOptions(await screen.findByRole('combobox', { name: /Service token/ }), 'st-1');
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Grant access' }));

    await waitFor(() => {
      const post = fetchMock.mock.calls.find(
        ([url, init]) => String(url).includes('/admin/grants') && (init as RequestInit)?.method === 'POST',
      );
      expect(post).toBeTruthy();
      const body = JSON.parse(String((post![1] as RequestInit).body));
      expect(body.model_kind).toBe('managed');
      expect(body.model_ids).toEqual(['mm-1']);
      expect(body.grantee_type).toBe('service_token');
      expect(body.grantee_id).toBe('st-1');
    });
  });

  it('warns that an all-service-tokens grant does not touch people', async () => {
    vi.stubGlobal('fetch', mockFetch(grantsRoutes));
    renderPage(<GrantsPage />);

    await userEvent.click(await screen.findByRole('button', { name: /Grant access/i }));
    await userEvent.selectOptions(await screen.findByRole('listbox', { name: /Models/ }), 'model:m-1');
    await userEvent.selectOptions(screen.getByRole('combobox', { name: /Grant to/ }), 'all_service_tokens');
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Grant access' }));

    expect(await screen.findByText(/does not affect people/i)).toBeTruthy();
  });
});
