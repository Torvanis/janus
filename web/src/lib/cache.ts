/**
 * The canonical cache-hit metric. This module is the ONLY place the formula
 * exists in the web app — every surface that shows cache usage (personal
 * Dashboard, org Explore) renders through cacheMeta() so the number and its
 * presence rules are identical everywhere.
 *
 * Definition: hitRate = tokens_cached / (tokens_in + tokens_cache_write_5m +
 * tokens_cache_write_1h), clamped to [0, 1]. When the denominator is zero the
 * rate is undefined and rendered as an em-dash — never NaN, Infinity, or 0%.
 */
import type { Totals } from './types';
import { formatNumber } from './format';
import { t } from './i18n';

/** The subset of Totals the cache metric needs; per-event rows satisfy it too. */
export type CacheTotals = Pick<Totals, 'tokens_in' | 'tokens_cached' | 'tokens_cache_write_5m' | 'tokens_cache_write_1h'>;

/**
 * The canonical cache hit rate in [0, 1], or undefined when the denominator
 * (all input-side tokens: fresh input plus cache writes) is zero.
 */
export function cacheHitRate(totals: CacheTotals): number | undefined {
  const denominator = totals.tokens_in + totals.tokens_cache_write_5m + totals.tokens_cache_write_1h;
  if (!Number.isFinite(denominator) || denominator <= 0) return undefined;
  return Math.min(1, Math.max(0, totals.tokens_cached / denominator));
}

/** True when the window saw any cache activity — reads or writes. */
export function hasCacheActivity(totals: CacheTotals): boolean {
  return totals.tokens_cached > 0 || totals.tokens_cache_write_5m > 0 || totals.tokens_cache_write_1h > 0;
}

/** Locale-aware percentage for a hit rate; em-dash for the undefined case. */
export function formatCacheHitRate(rate: number | undefined): string {
  if (rate === undefined || !Number.isFinite(rate)) return '—';
  return new Intl.NumberFormat(undefined, { style: 'percent', maximumFractionDigits: 1 }).format(rate);
}

/**
 * The single cache-metric line: hit-rate percentage plus the served-from-cache
 * count. Returns undefined (metric hidden) when there was no cache activity.
 */
export function cacheMeta(totals: CacheTotals): string | undefined {
  if (!hasCacheActivity(totals)) return undefined;
  return t('shared.cacheHitRate', {
    rate: formatCacheHitRate(cacheHitRate(totals)),
    count: formatNumber(totals.tokens_cached),
  });
}
