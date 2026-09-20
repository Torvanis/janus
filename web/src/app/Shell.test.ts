import { describe, expect, it } from 'vitest';
import { deriveSignal } from './Shell';
import type { GlobalDashboard, TimePoint, Totals } from '../lib/types';

function totals(overrides: Partial<Totals> = {}): Totals {
  return {
    tokens_in: 0,
    tokens_out: 0,
    tokens_cached: 0,
    tokens_cache_write_5m: 0,
    tokens_cache_write_1h: 0,
    cost_nanousd: 0,
    request_count: 0,
    error_count: 0,
    ...overrides,
  };
}

function dashboard(series: TimePoint[]): GlobalDashboard {
  return {
    top_models: [],
    totals_30d: totals(),
    active_users_15m: 0,
    series,
    new_models: [],
    leaderboards_enabled: false,
  };
}

describe('deriveSignal', () => {
  it('accumulates tokens_in and tokens_out separately across the last 3 buckets', () => {
    const data = dashboard([
      // An older 4th bucket that must be ignored (only the last 3 count).
      { bucket: '2026-08-26T00:00:00Z', totals: totals({ tokens_in: 999_999, tokens_out: 999_999 }) },
      { bucket: '2026-08-26T00:05:00Z', totals: totals({ tokens_in: 6000, tokens_out: 4000, request_count: 10 }) },
      { bucket: '2026-08-26T00:10:00Z', totals: totals({ tokens_in: 9000, tokens_out: 1000, request_count: 5 }) },
      { bucket: '2026-08-26T00:15:00Z', totals: totals({ tokens_in: 3000, tokens_out: 2000, request_count: 15 }) },
    ]);

    const signal = deriveSignal(data);

    // 3 buckets × 5 minutes = 15 minutes of accumulation.
    expect(signal.tokensInPerMinute).toBeCloseTo(18000 / 15);
    expect(signal.tokensOutPerMinute).toBeCloseTo(7000 / 15);
    expect(signal.tokensPerMinute).toBeCloseTo(25000 / 15);
    // The split always sums to the combined figure fed to the ambient canvas.
    expect(signal.tokensInPerMinute + signal.tokensOutPerMinute).toBeCloseTo(signal.tokensPerMinute);
    expect(signal.requestsPerMinute).toBeCloseTo(30 / 15);
  });

  it('keeps the combined tokensPerMinute identical to the pre-split derivation', () => {
    const data = dashboard([
      { bucket: '2026-08-26T00:05:00Z', totals: totals({ tokens_in: 1200, tokens_out: 300 }) },
      { bucket: '2026-08-26T00:10:00Z', totals: totals({ tokens_in: 450, tokens_out: 50 }) },
    ]);

    const signal = deriveSignal(data);

    // Legacy behaviour: sum(tokens_in + tokens_out) / (buckets × 5 min).
    expect(signal.tokensPerMinute).toBeCloseTo((1200 + 300 + 450 + 50) / 10);
  });

  it('degrades costPerMinute to zero on local-only zero-cost buckets while token/request rates flow', () => {
    // In local-only mode (JANUS_LOCAL_ONLY) every recorded cost_nanousd is 0.
    // The ambient signal needs no gating: its cost warmth simply reads zero
    // while the token and request rates keep driving the canvas as usual.
    const data = dashboard([
      {
        bucket: '2026-08-26T00:05:00Z',
        totals: totals({ tokens_in: 3000, tokens_out: 1500, request_count: 30, cost_nanousd: 0 }),
      },
      {
        bucket: '2026-08-26T00:10:00Z',
        totals: totals({ tokens_in: 1500, tokens_out: 500, request_count: 20, cost_nanousd: 0 }),
      },
    ]);

    const signal = deriveSignal(data);

    expect(signal.costPerMinute).toBe(0);
    expect(signal.tokensPerMinute).toBeCloseTo(6500 / 10);
    expect(signal.requestsPerMinute).toBeCloseTo(50 / 10);
  });

  it('returns an all-zero fallback for undefined data', () => {
    expect(deriveSignal(undefined)).toEqual({
      tokensPerMinute: 0,
      tokensInPerMinute: 0,
      tokensOutPerMinute: 0,
      requestsPerMinute: 0,
      costPerMinute: 0,
      errorRate: 0,
      // No data means no jobs, so the ambient layer draws no shooting stars.
      recentJobs: [],
    });
  });

  it('returns an all-zero fallback for an empty series', () => {
    const signal = deriveSignal(dashboard([]));
    expect(signal.tokensPerMinute).toBe(0);
    expect(signal.tokensInPerMinute).toBe(0);
    expect(signal.tokensOutPerMinute).toBe(0);
  });
});
