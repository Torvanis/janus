/**
 * Presentation helpers. Numbers, currency, and dates always render in the
 * viewer's locale and time zone (English-only strings in v1, but formatting is
 * locale-aware from day one).
 */

import { t } from './i18n';

const NANO_PER_USD = 1_000_000_000;

export function usdFromNano(nano: number): number {
  return nano / NANO_PER_USD;
}

/** Formats money, keeping sub-cent precision visible for tiny per-call costs. */
// Callers must gate on useLocalOnly() before rendering a formatted amount:
// in local-only mode (JANUS_LOCAL_ONLY) no cost/spend/pricing figure may
// appear anywhere, and formatting the recorded zeros as "$0.00" would be a
// misleading cost surface, not an absence of one.
export function formatUSD(nano: number, options?: { compact?: boolean }): string {
  const value = usdFromNano(nano);
  if (options?.compact && Math.abs(value) >= 1000) {
    return new Intl.NumberFormat(undefined, {
      style: 'currency',
      currency: 'USD',
      notation: 'compact',
      maximumFractionDigits: 1,
    }).format(value);
  }
  const digits = value !== 0 && Math.abs(value) < 0.01 ? 6 : 2;
  return new Intl.NumberFormat(undefined, {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: value === 0 ? 2 : digits,
    maximumFractionDigits: digits,
  }).format(value);
}

export function formatNumber(value: number, options?: { compact?: boolean }): string {
  if (options?.compact && Math.abs(value) >= 10_000) {
    return new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 }).format(value);
  }
  return new Intl.NumberFormat().format(value);
}

export function formatDateTime(value: string | undefined): string {
  if (!value || value.startsWith('0001-01-01')) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '—';
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(date);
}

export function formatDate(value: string | undefined): string {
  if (!value || value.startsWith('0001-01-01')) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '—';
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(date);
}

/** Relative time such as "4 minutes ago" or "in 3 hours". */
export function formatRelative(value: string | undefined): string {
  if (!value || value.startsWith('0001-01-01')) return 'never';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return 'never';
  const deltaSeconds = Math.round((date.getTime() - Date.now()) / 1000);
  const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });
  const divisions: Array<[number, Intl.RelativeTimeFormatUnit]> = [
    [60, 'second'],
    [60, 'minute'],
    [24, 'hour'],
    [7, 'day'],
    [4.34524, 'week'],
    [12, 'month'],
    [Number.POSITIVE_INFINITY, 'year'],
  ];
  let duration = deltaSeconds;
  for (const [amount, unit] of divisions) {
    if (Math.abs(duration) < amount) {
      return rtf.format(Math.round(duration), unit);
    }
    duration /= amount;
  }
  return rtf.format(Math.round(duration), 'year');
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  return `${Math.round(ms / 60_000)} min`;
}

export function formatBytes(bytes: number): string {
  if (!bytes) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB'];
  const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  return `${(bytes / 1024 ** exponent).toFixed(exponent === 0 ? 0 : 1)} ${units[exponent]}`;
}

/** Rate cards are quoted per million tokens. */
export function formatRate(nanoPerMillion: number): string {
  if (!nanoPerMillion) return 'not priced';
  return `${formatUSD(nanoPerMillion)} / Mtok`;
}

/**
 * Context windows and similar token capacities: exact multiples of 1K/1M
 * render compactly ("200K tokens", "1M tokens"); anything else keeps the
 * exact count ("8,191 tokens") rather than rounding to a misleading figure.
 * 0 (and negative/undefined) means unknown and renders a neutral placeholder.
 */
export function formatTokenCount(tokens: number | undefined): string {
  if (!tokens || tokens <= 0) return '—';
  if (tokens % 1_000_000 === 0) return `${formatNumber(tokens / 1_000_000)}M tokens`;
  if (tokens % 1_000 === 0) return `${formatNumber(tokens / 1_000)}K tokens`;
  return `${formatNumber(tokens)} tokens`;
}

export function titleCase(value: string): string {
  if (!value) return '';
  return value
    .split(/[_\s]+/)
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join(' ');
}

export function statusTone(status: number): 'success' | 'warning' | 'danger' | 'neutral' {
  if (status >= 500) return 'danger';
  if (status === 429 || status === 403 || status === 401) return 'warning';
  if (status >= 400) return 'warning';
  if (status >= 200 && status < 300) return 'success';
  return 'neutral';
}

export function quotaTone(percent: number): 'success' | 'warning' | 'danger' {
  if (percent >= 100) return 'danger';
  if (percent >= 80) return 'warning';
  return 'success';
}

/**
 * Error rate for the model catalog: one decimal ("2.5%"), whole numbers
 * without a trailing ".0" ("0%", "100%"). Undefined renders the placeholder.
 */
export function formatErrorRate(percent: number | undefined): string {
  if (percent === undefined || !Number.isFinite(percent)) return '—';
  const clamped = Math.min(100, Math.max(0, percent));
  return `${new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 }).format(clamped)}%`;
}

/** Badge tone for a model health verdict; mirrors the admin Upstreams page. */
export function healthTone(health: 'healthy' | 'degraded' | 'down' | 'unknown'): 'success' | 'warning' | 'danger' | 'neutral' {
  switch (health) {
    case 'healthy':
      return 'success';
    case 'degraded':
      return 'warning';
    case 'down':
      return 'danger';
    default:
      return 'neutral';
  }
}

/** Format one resolved dimension without presenting an unknown rate as free. */
export function formatModelRate(
  model: import('./types').Model,
  field: 'rate_in_nanousd' | 'rate_out_nanousd' | 'rate_cached_nanousd',
): string {
  const metadata = model.metadata?.[field];
  if (metadata?.value === null) return t('common.notSet');
  const known = metadata
    ? metadata.value !== null
    : Boolean(model.rate_effective_from) && !model.rate_effective_from.startsWith('0001-01-01');
  return model[field] === 0 && known ? `${formatUSD(0)} / Mtok` : formatRate(model[field]);
}
