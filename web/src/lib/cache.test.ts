/**
 * Unit tests for the canonical cache-hit-rate helper — the single place the
 * formula lives. Locks the definition (cached reads over input tokens, which
 * include cached tokens and cache writes as providers bill them), the [0,1] clamp, and the undefined zero-denominator case
 * that must never surface as NaN, Infinity, or 0%.
 */
import { describe, expect, it } from 'vitest';
import { cacheHitRate, cacheMeta, formatCacheHitRate, hasCacheActivity, type CacheTotals } from './cache';

function totals(overrides: Partial<CacheTotals> = {}): CacheTotals {
  return {
    tokens_in: 0,
    tokens_cached: 0,
    tokens_cache_write_5m: 0,
    tokens_cache_write_1h: 0,
    ...overrides,
  };
}

describe('cacheHitRate', () => {
  it('computes cached / tokens_in (input tokens include cached tokens and cache writes)', () => {
    // 950 fresh + 50 read + 100 written = 1,100 input tokens.
    const rate = cacheHitRate(totals({ tokens_cached: 50, tokens_in: 1100, tokens_cache_write_5m: 100 }));
    expect(rate).toBeCloseTo(50 / 1100, 10);
  });

  it('never adds the cache parts to tokens_in a second time', () => {
    // The worked example: 50,000 fresh + 40,000 read = 90,000 input tokens, 44%.
    expect(cacheHitRate(totals({ tokens_cached: 40_000, tokens_in: 90_000 }))).toBeCloseTo(0.444, 3);
    const rate = cacheHitRate(
      totals({ tokens_cached: 300, tokens_in: 1200, tokens_cache_write_5m: 100, tokens_cache_write_1h: 100 }),
    );
    expect(rate).toBeCloseTo(300 / 1200, 10);
  });

  it('is undefined when tokens_in is zero, even with cached reads', () => {
    expect(cacheHitRate(totals())).toBeUndefined();
    expect(cacheHitRate(totals({ tokens_cached: 50 }))).toBeUndefined();
  });

  it('clamps to [0, 1]', () => {
    expect(cacheHitRate(totals({ tokens_cached: 5000, tokens_in: 1000 }))).toBe(1);
    expect(cacheHitRate(totals({ tokens_cached: -5, tokens_in: 1000 }))).toBe(0);
  });

  it('never returns NaN or Infinity', () => {
    for (const rate of [
      cacheHitRate(totals()),
      cacheHitRate(totals({ tokens_cached: 1 })),
      cacheHitRate(totals({ tokens_cached: 1, tokens_in: Number.POSITIVE_INFINITY })),
    ]) {
      expect(rate === undefined || Number.isFinite(rate)).toBe(true);
    }
  });
});

describe('hasCacheActivity', () => {
  it('is false when reads and writes are all zero', () => {
    expect(hasCacheActivity(totals({ tokens_in: 9999 }))).toBe(false);
  });

  it('is true for cache reads or either cache-write TTL', () => {
    expect(hasCacheActivity(totals({ tokens_cached: 1 }))).toBe(true);
    expect(hasCacheActivity(totals({ tokens_cache_write_5m: 1 }))).toBe(true);
    expect(hasCacheActivity(totals({ tokens_cache_write_1h: 1 }))).toBe(true);
  });
});

describe('formatCacheHitRate', () => {
  it('renders a locale percentage with at most one fraction digit', () => {
    expect(formatCacheHitRate(50 / 1100)).toBe('4.5%');
    expect(formatCacheHitRate(0.375)).toBe('37.5%');
    expect(formatCacheHitRate(1)).toBe('100%');
  });

  it('renders an em-dash for the undefined rate — never NaN or 0%', () => {
    expect(formatCacheHitRate(undefined)).toBe('—');
    expect(formatCacheHitRate(Number.NaN)).toBe('—');
  });
});

describe('cacheMeta', () => {
  it('is hidden (undefined) when there is no cache activity', () => {
    expect(cacheMeta(totals({ tokens_in: 1000 }))).toBeUndefined();
  });

  it('renders the rate and the served-from-cache count together', () => {
    expect(cacheMeta(totals({ tokens_cached: 50, tokens_in: 1100, tokens_cache_write_5m: 100 }))).toBe(
      '4.5% cache hit rate · 50 served from cache',
    );
  });

  it('renders an em-dash rate when activity exists but tokens_in is zero', () => {
    // Degenerate but possible shape: cached reads recorded with no input-side
    // tokens. The metric stays visible; the rate is explicitly undefined.
    expect(cacheMeta(totals({ tokens_cached: 50 }))).toBe('— cache hit rate · 50 served from cache');
  });
});
