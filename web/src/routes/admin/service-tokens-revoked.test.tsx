/**
 * Admin → Service tokens hides tokens revoked more than 7 days ago, the same
 * way the user Tokens page does: a count, "Show all revoked", and the choice
 * kept in the URL (?revoked=all).
 */
import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { ServiceTokensPage } from './ServiceTokens';
import { ToastProvider } from '../../components/ui';

function daysAgo(days: number): string {
  return new Date(Date.now() - days * 24 * 60 * 60 * 1000).toISOString();
}

function row(id: string, name: string, revokedAt = '') {
  return {
    id,
    name,
    description: '',
    prefix: `janus_svc_${id}`,
    created_by_user_id: 'u-1',
    created_at: '2026-01-01T00:00:00Z',
    last_used_at: '',
    expires_at: '',
    revoked_at: revokedAt,
    status: revokedAt ? ('revoked' as const) : ('active' as const),
    grant_count: 0,
    spend_30d_usd: 0,
    requests_30d: 0,
    tokens_in_30d: 0,
    tokens_out_30d: 0,
    errors_30d: 0,
    top_model_30d: '',
    model_count_30d: 0,
  };
}

const active = row('st-a', 'live-integration');
const recent = row('st-r', 'recently-revoked', daysAgo(6));
const old1 = row('st-o1', 'old-ci', daysAgo(8));
const old2 = row('st-o2', 'old-batch', daysAgo(90));

function serve(rows: unknown[]) {
  const fetchMock = vi.fn(async (_input: RequestInfo | URL) => {
    const body = { service_tokens: rows, total_count: rows.length };
    return {
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => body,
      text: async () => JSON.stringify(body),
    } as Response;
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function renderAt(url: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[url]}>
          <Routes>
            <Route path="/admin/service-tokens" element={<ServiceTokensPage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe('service tokens revoked more than 7 days ago', () => {
  it('hides them by default, says how many, and shows them on request', async () => {
    serve([active, recent, old1, old2]);
    const user = userEvent.setup();
    renderAt('/admin/service-tokens');

    expect(await screen.findByRole('link', { name: 'live-integration' })).toBeTruthy();
    expect(screen.getByRole('link', { name: 'recently-revoked' })).toBeTruthy();
    expect(screen.queryByRole('link', { name: 'old-ci' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'old-batch' })).toBeNull();
    expect(screen.getByText('2 tokens revoked more than 7 days ago are hidden.')).toBeTruthy();

    await user.click(screen.getByRole('button', { name: 'Show all revoked' }));
    expect(screen.getByRole('link', { name: 'old-ci' })).toBeTruthy();
    expect(screen.getByRole('link', { name: 'old-batch' })).toBeTruthy();

    await user.click(screen.getByRole('button', { name: 'Hide tokens revoked more than 7 days ago' }));
    expect(screen.queryByRole('link', { name: 'old-ci' })).toBeNull();
    expect(screen.getByText('2 tokens revoked more than 7 days ago are hidden.')).toBeTruthy();
  });

  it('counts only what the server returned for the current filters', async () => {
    // The server applies search/status; the page hides from that result, so
    // the count matches the filtered list.
    const fetchMock = serve([old1]);
    renderAt('/admin/service-tokens?status=revoked&q=ci');

    expect(await screen.findByText('1 token revoked more than 7 days ago is hidden.')).toBeTruthy();
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain('status=revoked');
    // Filters are on, so the title is the no-match one.
    expect(screen.getByText('No matching service tokens')).toBeTruthy();
  });

  it('keeps the choice in the URL', async () => {
    serve([active, old1]);
    renderAt('/admin/service-tokens?revoked=all');

    expect(await screen.findByRole('link', { name: 'old-ci' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Hide tokens revoked more than 7 days ago' })).toBeTruthy();
  });

  it('offers to show them, or to issue a token, when every token is an old revoked one', async () => {
    serve([old1, old2]);
    const user = userEvent.setup();
    renderAt('/admin/service-tokens');

    expect(await screen.findByText('No active or recently revoked tokens')).toBeTruthy();
    expect(screen.getByText('2 tokens revoked more than 7 days ago are hidden.')).toBeTruthy();
    expect(screen.getAllByRole('button', { name: 'Issue token' }).length).toBeGreaterThan(1);

    await user.click(screen.getByRole('button', { name: 'Show all revoked' }));
    expect(screen.getByRole('link', { name: 'old-ci' })).toBeTruthy();
  });

  it('shows no age note when nothing old is revoked', async () => {
    serve([active, recent]);
    renderAt('/admin/service-tokens');

    expect(await screen.findByRole('link', { name: 'recently-revoked' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Show all revoked' })).toBeNull();
    expect(screen.queryByRole('button', { name: /^Hide tokens revoked/ })).toBeNull();
  });
});
