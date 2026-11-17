// Price lines (server: internal/usage/lines.go) in words, and the rules the
// pricing builder applies before it saves. A line key is <base> or
// <base>@<cond>=<v>,… (conditions in alphabetical order) and its value is
// nano-USD: per million tokens for the five token rates, per unit for the
// rest. The five unconditional token rates are saved through the
// rate_*_usd_per_mtok fields, every other line through price_lines.

import type { ModelRatePatch } from '../api/client';

export type ConditionName = 'prompt_over' | 'quality' | 'size' | 'resolution' | 'audio';

export type RatePatchField = keyof Pick<
  ModelRatePatch,
  | 'rate_in_usd_per_mtok'
  | 'rate_out_usd_per_mtok'
  | 'rate_cached_usd_per_mtok'
  | 'rate_cache_write_5m_usd_per_mtok'
  | 'rate_cache_write_1h_usd_per_mtok'
>;

export interface PriceUnit {
  base: string;
  /** "per 1M input tokens". */
  per: string;
  /** The conditions the builder offers for this unit. */
  conditions: readonly ConditionName[];
  /** The PATCH field for the unconditional rate (the five token rates only). */
  ratePatch?: RatePatchField;
}

/**
 * Every base Janus can bill, in the order the builder lists them. Tool calls
 * by kind are not here: the engine has no base for them yet.
 */
export const PRICE_UNITS: readonly PriceUnit[] = [
  { base: 'rate_in_nanousd', per: 'per 1M input tokens', conditions: ['prompt_over'], ratePatch: 'rate_in_usd_per_mtok' },
  {
    base: 'rate_cached_nanousd',
    per: 'per 1M cached input tokens',
    conditions: ['prompt_over'],
    ratePatch: 'rate_cached_usd_per_mtok',
  },
  {
    base: 'rate_cache_write_5m_nanousd',
    per: 'per 1M tokens written to the 5-minute cache',
    conditions: ['prompt_over'],
    ratePatch: 'rate_cache_write_5m_usd_per_mtok',
  },
  {
    base: 'rate_cache_write_1h_nanousd',
    per: 'per 1M tokens written to the 1-hour cache',
    conditions: ['prompt_over'],
    ratePatch: 'rate_cache_write_1h_usd_per_mtok',
  },
  { base: 'rate_out_nanousd', per: 'per 1M output tokens', conditions: ['prompt_over'], ratePatch: 'rate_out_usd_per_mtok' },
  { base: 'price_image_nanousd', per: 'per image', conditions: ['quality', 'size', 'resolution'] },
  { base: 'price_video_second_nanousd', per: 'per second of video', conditions: ['resolution', 'audio'] },
  { base: 'price_audio_minute_nanousd', per: 'per minute of audio', conditions: [] },
  { base: 'price_mchar_nanousd', per: 'per 1M characters', conditions: [] },
  { base: 'price_kpage_nanousd', per: 'per 1,000 pages', conditions: [] },
  { base: 'price_request_nanousd', per: 'per request', conditions: [] },
];

const UNIT_BY_BASE = new Map(PRICE_UNITS.map((u) => [u.base, u]));

export function priceUnit(base: string): PriceUnit | undefined {
  return UNIT_BY_BASE.get(base);
}

/** The fixed metadata fields: everything else in a model's metadata is a price line. */
export const FIXED_METADATA_FIELDS = new Set([
  'context_window',
  'rate_in_nanousd',
  'rate_out_nanousd',
  'rate_cached_nanousd',
  'rate_cache_write_5m_nanousd',
  'rate_cache_write_1h_nanousd',
]);

/** The five token rates without a condition (saved as rate_*_usd_per_mtok). */
export function isBaseTokenRate(key: string): boolean {
  return key !== 'context_window' && FIXED_METADATA_FIELDS.has(key);
}

export interface ParsedLine {
  base: string;
  conditions: Record<string, string>;
}

export function parseLineKey(key: string): ParsedLine {
  const [base = '', rest] = key.split('@', 2);
  const conditions: Record<string, string> = {};
  if (rest) {
    for (const part of rest.split(',')) {
      const [name = '', value = ''] = part.split('=', 2);
      conditions[name] = value;
    }
  }
  return { base, conditions };
}

/** The canonical key: empty conditions dropped, the rest in alphabetical order. */
export function lineKey(base: string, conditions: Record<string, string>): string {
  const parts = Object.entries(conditions)
    .filter(([, v]) => v !== '')
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([k, v]) => `${k}=${v}`);
  return parts.length ? `${base}@${parts.join(',')}` : base;
}

export function conditionText(name: string, value: string): string {
  switch (name) {
    case 'prompt_over':
      return `prompt over ${Number(value).toLocaleString('en-US')} tokens`;
    case 'audio':
      return value === 'yes' ? 'with sound' : 'without sound';
    case 'tier':
      return `service tier ${value}`;
    default:
      return `${name} ${value}`;
  }
}

/** "when prompt over 272,000 tokens", "" for an unconditional line. */
export function whenText(key: string): string {
  const { conditions } = parseLineKey(key);
  const names = Object.keys(conditions);
  if (names.length === 0) return '';
  return `when ${names.map((n) => conditionText(n, conditions[n] ?? '')).join(', ')}`;
}

/** "per 1M input tokens when prompt over 272,000 tokens". */
export function priceLineLabel(key: string): string {
  const { base } = parseLineKey(key);
  const per = priceUnit(base)?.per ?? base;
  const when = whenText(key);
  return when ? `${per} ${when}` : per;
}

/** nano-USD as dollars, with as many decimals as the price needs (up to 9). */
export function formatNanoUSD(nano: number): string {
  return `$${(nano / 1e9).toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 9 })}`;
}

/** nano-USD as the plain number an input shows ("2.5", "0.0045"). */
export function nanoToInput(nano: number): string {
  const whole = Math.trunc(nano / 1e9);
  const frac = String(nano % 1e9)
    .padStart(9, '0')
    .replace(/0+$/, '');
  return frac ? `${whole}.${frac}` : String(whole);
}

/** The most a builder line may be set to, in USD per its unit. */
export const MAX_PRICE_USD = 10_000;

export type PriceError = 'empty' | 'number' | 'decimals' | 'max';

/**
 * Reads a dollar amount the way an administrator types it ("2.50", "$0.04",
 * "1,200"). Exact to the nano-dollar: the decimals are counted, not rounded
 * through a float. A minus sign is not a number here, so a negative price
 * cannot be entered.
 */
export function parseUSD(text: string): { nano: number; usd: number } | { error: PriceError } {
  const s = text.trim().replace(/^\$/, '').replace(/,/g, '').trim();
  if (s === '') return { error: 'empty' };
  const m = /^(\d*)(?:\.(\d*))?$/.exec(s);
  if (!m || (m[1] === '' && !m[2])) return { error: 'number' };
  const whole = m[1] || '0';
  const frac = m[2] ?? '';
  if (frac.length > 9) return { error: 'decimals' };
  const usd = Number(`${whole}.${frac || '0'}`);
  if (usd > MAX_PRICE_USD) return { error: 'max' };
  return { nano: Number(whole) * 1e9 + Number(frac.padEnd(9, '0')), usd };
}

const CONDITION_VALUE = /^[a-z0-9][a-z0-9._-]{0,31}$/;

/** Condition values the server accepts (usage.ParseLine). */
export function validConditionValue(v: string): boolean {
  return CONDITION_VALUE.test(v);
}

const MODEL_TYPES = ['chat', 'image', 'video', 'tts', 'stt', 'embedding', 'rerank', 'moderation'];

/**
 * The model's type for pricing, read from its modalities the way the price
 * catalog reads it (pricecatalog.ModelType): the first modality that names a
 * type, "response" counting as chat.
 */
export function modelPriceType(modalities: readonly string[]): string {
  for (const m of modalities) {
    if (m === 'response') return 'chat';
    if (MODEL_TYPES.includes(m)) return m;
  }
  return '';
}

const TOKENS = [
  'rate_in_nanousd',
  'rate_cached_nanousd',
  'rate_cache_write_5m_nanousd',
  'rate_cache_write_1h_nanousd',
  'rate_out_nanousd',
];
const CHAT = [...TOKENS, 'price_request_nanousd'];
const RELEVANT: Record<string, readonly string[]> = {
  chat: CHAT,
  embedding: ['rate_in_nanousd', 'price_request_nanousd'],
  rerank: ['rate_in_nanousd', 'price_request_nanousd'],
  moderation: ['rate_in_nanousd', 'price_request_nanousd'],
  image: ['price_image_nanousd', 'rate_in_nanousd', 'rate_out_nanousd', 'price_request_nanousd'],
  video: ['price_video_second_nanousd', 'price_request_nanousd'],
  tts: ['price_mchar_nanousd', 'rate_in_nanousd', 'rate_out_nanousd', 'price_request_nanousd'],
  stt: ['price_audio_minute_nanousd', 'rate_in_nanousd', 'rate_out_nanousd', 'price_request_nanousd'],
};

/** The units that usually price a model of this type; the rest sit behind "More units". */
export function relevantBases(type: string): readonly string[] {
  return RELEVANT[type] ?? CHAT;
}

/** The token rates a model of this type shows even before they have a price. */
export function expectedTokenRates(type: string): string[] {
  return relevantBases(type).filter((b) => isBaseTokenRate(b));
}

/** Sort order: by unit as listed in PRICE_UNITS, then unconditional first, then by key. */
export function compareLines(a: string, b: string): number {
  const ia = PRICE_UNITS.findIndex((u) => u.base === parseLineKey(a).base);
  const ib = PRICE_UNITS.findIndex((u) => u.base === parseLineKey(b).base);
  return ia - ib || a.length - b.length || a.localeCompare(b);
}
