/**
 * Unit tests for the canonical cache-hit-rate helper — the single place the
 * formula lives. Locks the definition (cached reads over fresh input plus
 * cache writes), the [0,1] clamp, and the undefined zero-denominator case
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
  it('computes cached / (in + cache_write_5m + cache_write_1h)', () => {
    const rate = cacheHitRate(totals({ tokens_cached: 50, tokens_in: 950, tokens_cache_write_5m: 100 }));
    expect(rate).toBeCloseTo(50 / 1050, 10);
  });

  it('includes both cache-write TTLs in the denominator', () => {
    const rate = cacheHitRate(
      totals({ tokens_cached: 300, tokens_in: 1000, tokens_cache_write_5m: 100, tokens_cache_write_1h: 100 }),
    );
    expect(rate).toBeCloseTo(300 / 1200, 10);
  });

  it('is undefined when the denominator is zero, even with cached reads', () => {
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
    expect(formatCacheHitRate(50 / 1050)).toBe('4.8%');
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
    expect(cacheMeta(totals({ tokens_cached: 50, tokens_in: 950, tokens_cache_write_5m: 100 }))).toBe(
      '4.8% cache hit rate · 50 served from cache',
    );
  });

  it('renders an em-dash rate when activity exists but the denominator is zero', () => {
    // Degenerate but possible shape: cached reads recorded with no input-side
    // tokens. The metric stays visible; the rate is explicitly undefined.
    expect(cacheMeta(totals({ tokens_cached: 50 }))).toBe('— cache hit rate · 50 served from cache');
  });
});
