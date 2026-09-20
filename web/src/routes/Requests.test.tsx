/**
 * Route-level behavior tests for the Requests page detail drawer, focused on
 * the prompt-cache metering rows: alongside "Cached input tokens" (cache
 * reads) the drawer must show both cache-write dimensions the gateway meters
 * and bills — the 5-minute and 1-hour TTL buckets — and render honest zeros
 * for requests with no cache activity.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../lib/api';
import type { UsageEvent } from '../lib/types';
import { RequestsPage } from './Requests';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

function event(overrides: Partial<UsageEvent> = {}): UsageEvent {
  return {
    id: 'evt-1',
    created_at: '2026-08-26T12:00:00Z',
    user_id: 'u1',
    token_id: 'tok1',
    model: 'claude-cache-test',
    endpoint_path: '/v1/chat/completions',
    http_method: 'POST',
    modality: 'chat',
    streaming: false,
    request_bytes: 1024,
    response_bytes: 2048,
    tokens_in: 1000,
    tokens_out: 500,
    tokens_cached: 0,
    tokens_cache_write_5m: 0,
    tokens_cache_write_1h: 0,
    token_accounting_method: 'upstream_reported',
    cost_nanousd: 12_000_000,
    finish_reason: 'stop',
    http_status: 200,
    latency_ms: 300,
    ttfb_ms: 120,
    client_user_agent: 'curl/8',
    client_ip: '10.0.0.1',
    x_forwarded_for: '',
    error_code: '',
    quota_violated: false,
    request_id: 'req-1',
    ...overrides,
  };
}

function renderRequests() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={['/requests']}>
        <RequestsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function mockRequests(events: UsageEvent[]) {
  mocked.get.mockImplementation((path: string) => {
    if (path.startsWith('/api/v1/requests')) {
      return Promise.resolve({ requests: events, total_count: events.length });
    }
    return Promise.resolve({ models: [] });
  });
}

/** The value cell rendered next to a labelled DetailRow in the drawer. */
function detailValue(label: string): string {
  const labelEl = screen.getByText(label);
  const valueEl = labelEl.nextElementSibling;
  if (!valueEl) throw new Error(`no value cell next to label "${label}"`);
  return valueEl.textContent ?? '';
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('charged teams in the request list', () => {
  it('shows named charged teams and personal labels on their event rows', async () => {
    mockRequests([
      event({ team_ids: 'team-platform', team_names: ['Platform'] }),
      event({ id: 'evt-personal', model: 'personal-model', team_ids: '', team_names: [] }),
    ]);
    renderRequests();

    expect(await screen.findByRole('columnheader', { name: 'Charged to' })).toBeTruthy();
    expect(screen.getByRole('cell', { name: 'Platform' }).closest('tr')?.textContent).toContain('claude-cache-test');
    expect(screen.getByRole('cell', { name: 'Personal' }).closest('tr')?.textContent).toContain('personal-model');
  });
});

describe('request detail drawer — cache metering rows', () => {
  it('shows cached-read and both cache-write token counts for a cache-active event', async () => {
    const user = userEvent.setup();
    mockRequests([event({ tokens_cached: 50, tokens_cache_write_5m: 100, tokens_cache_write_1h: 200 })]);
    renderRequests();

    await user.click(await screen.findByRole('cell', { name: 'claude-cache-test' }));
    await screen.findByText('Metering');

    expect(detailValue('Cached input tokens')).toBe('50');
    expect(detailValue('Cache-write tokens (5m)')).toBe('100');
    expect(detailValue('Cache-write tokens (1h)')).toBe('200');
  });

  it('formats large cache-write counts with the locale formatter', async () => {
    const user = userEvent.setup();
    mockRequests([event({ tokens_cached: 1500, tokens_cache_write_5m: 12_345, tokens_cache_write_1h: 6_789 })]);
    renderRequests();

    await user.click(await screen.findByRole('cell', { name: 'claude-cache-test' }));
    await screen.findByText('Metering');

    expect(detailValue('Cached input tokens')).toBe('1,500');
    expect(detailValue('Cache-write tokens (5m)')).toBe('12,345');
    expect(detailValue('Cache-write tokens (1h)')).toBe('6,789');
  });

  it('renders honest zeros for an event with no cache activity', async () => {
    const user = userEvent.setup();
    mockRequests([event()]);
    renderRequests();

    await user.click(await screen.findByRole('cell', { name: 'claude-cache-test' }));
    await screen.findByText('Metering');

    expect(detailValue('Cached input tokens')).toBe('0');
    expect(detailValue('Cache-write tokens (5m)')).toBe('0');
    expect(detailValue('Cache-write tokens (1h)')).toBe('0');

    // The rest of the metering section is unaffected.
    expect(detailValue('Input tokens')).toBe('1,000');
    expect(detailValue('Output tokens')).toBe('500');
  });
});

describe('token columns — split, sortable, filterable', () => {
  it('renders tokens in and tokens out as two separate sortable columns', async () => {
    const user = userEvent.setup();
    mockRequests([event({ tokens_in: 12_345, tokens_out: 678 })]);
    renderRequests();

    await screen.findByRole('cell', { name: 'claude-cache-test' });
    // Two distinct headers, each a sort button, and no combined "in / out" cell.
    const inHeader = screen.getByRole('button', { name: /Tokens in/ });
    const outHeader = screen.getByRole('button', { name: /Tokens out/ });
    expect(inHeader).toBeTruthy();
    expect(outHeader).toBeTruthy();
    expect(screen.getByRole('cell', { name: '12,345' })).toBeTruthy();
    expect(screen.getByRole('cell', { name: '678' })).toBeTruthy();
    expect(screen.queryByRole('cell', { name: '12,345 / 678' })).toBeNull();

    // Clicking a token header sorts by it (descending first), then flips.
    await user.click(outHeader);
    await vi.waitFor(() => {
      const paths = mocked.get.mock.calls.map((call) => String(call[0]));
      expect(paths.some((p) => p.startsWith('/api/v1/requests') && p.includes('sort=tokens_out'))).toBe(true);
    });
    await user.click(screen.getByRole('button', { name: /Tokens out/ }));
    await vi.waitFor(() => {
      const paths = mocked.get.mock.calls.map((call) => String(call[0]));
      expect(paths.some((p) => p.includes('sort=tokens_out_asc'))).toBe(true);
    });
  });

  it('token-size filters translate to strict gt/lt query params per direction', async () => {
    const user = userEvent.setup();
    mockRequests([event()]);
    renderRequests();
    await screen.findByRole('cell', { name: 'claude-cache-test' });

    await user.selectOptions(screen.getByLabelText('Tokens in'), 'gt:10000');
    await vi.waitFor(() => {
      const paths = mocked.get.mock.calls.map((call) => String(call[0]));
      expect(paths.some((p) => p.includes('tokens_in_gt=10000'))).toBe(true);
    });

    await user.selectOptions(screen.getByLabelText('Tokens out'), 'lt:1000');
    await vi.waitFor(() => {
      const paths = mocked.get.mock.calls.map((call) => String(call[0]));
      const last = paths.filter((p) => p.startsWith('/api/v1/requests?')).at(-1) ?? '';
      expect(last).toContain('tokens_in_gt=10000');
      expect(last).toContain('tokens_out_lt=1000');
      expect(last).not.toContain('tokens_out_gt');
    });

    // The CSV export link carries the same narrowing.
    const exportLink = screen.getByRole('link', { name: /Export CSV/ });
    expect(exportLink.getAttribute('href')).toContain('tokens_in_gt=10000');
    expect(exportLink.getAttribute('href')).toContain('tokens_out_lt=1000');

    // Clear filters removes both.
    await user.click(screen.getByRole('button', { name: 'Clear filters' }));
    await vi.waitFor(() => {
      const paths = mocked.get.mock.calls.map((call) => String(call[0]));
      const last = paths.filter((p) => p.startsWith('/api/v1/requests?')).at(-1) ?? '';
      expect(last).not.toContain('tokens_in_');
      expect(last).not.toContain('tokens_out_');
    });
  });
});
