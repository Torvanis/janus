/**
 * Lower-left throughput readout: the per-minute line keeps its place and a
 * per-second line sits right under it, derived from the same live signal.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Shell, formatPerSecond } from './Shell';
import { SessionContext } from './session';

vi.mock('./ActiveTeamSelector', () => ({ ActiveTeamSelector: () => null }));
vi.mock('./LicenseBanner', () => ({ LicenseBanner: () => null }));
vi.mock('../components/AmbientCanvas', () => ({ AmbientCanvas: () => null }));

// Three 5-minute buckets: 9,000 tokens in and 2,700 out over 15 minutes =
// 600 in/min (10 in/s) and 180 out/min (3 out/s), 30 requests = 2 req/min.
const bucket = (tokensIn: number, tokensOut: number, requests: number) => ({
  bucket: '2026-10-01T00:00:00Z',
  totals: {
    tokens_in: tokensIn,
    tokens_out: tokensOut,
    tokens_cached: 0,
    tokens_cache_write_5m: 0,
    tokens_cache_write_1h: 0,
    cost_nanousd: 0,
    request_count: requests,
    error_count: 0,
  },
});
vi.mock('../lib/api', () => ({
  api: {
    get: vi.fn(async (url: string) => {
      if (url === '/api/v1/teams') return { teams: [] };
      if (url === '/api/v1/dashboard/global') {
        return { series: [bucket(3000, 900, 10), bucket(3000, 900, 10), bucket(3000, 900, 10)], recent_jobs: [] };
      }
      return {};
    }),
  },
}));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('throughput readout', () => {
  it('shows a per-second line directly under the per-minute line', async () => {
    vi.stubGlobal('matchMedia', vi.fn(() => ({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() })));
    render(
      <QueryClientProvider client={new QueryClient()}>
        <SessionContext.Provider
          value={
            {
              me: { id: 'me', name: 'Ada', email: 'ada@example.com', role: 'user', groups: [], teams: [] },
              config: null,
              refresh: vi.fn(),
              loading: false,
              error: null,
            } as any
          }
        >
          <MemoryRouter initialEntries={['/']}>
            <Shell>
              <p>Workspace</p>
            </Shell>
          </MemoryRouter>
        </SessionContext.Provider>
      </QueryClientProvider>,
    );
    const perSecond = await screen.findByTestId('throughput-per-second');
    expect(perSecond.textContent).toBe('10 in/s · 3.0 out/s');
    const perMinute = perSecond.previousElementSibling as HTMLElement;
    expect(perMinute.textContent).toBe('600 in/min · 180 out/min');
    expect(perMinute.previousElementSibling?.textContent).toBe('2 req/min');
  });

  it('formats small rates with one decimal so a quiet gateway is not shown as zero', () => {
    expect(formatPerSecond(0)).toBe('0');
    expect(formatPerSecond(0.25)).toBe('0.3');
    expect(formatPerSecond(9.94)).toBe('9.9');
    expect(formatPerSecond(10)).toBe('10');
    expect(formatPerSecond(1234.4)).toBe('1,234');
  });
});
