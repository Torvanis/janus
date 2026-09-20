import { describe, expect, it } from 'vitest';
import {
  formatBytes,
  formatDuration,
  formatRate,
  formatTokenCount,
  formatUSD,
  quotaTone,
  statusTone,
  usdFromNano,
} from './format';

describe('money formatting', () => {
  it('converts nano-USD to dollars exactly', () => {
    expect(usdFromNano(1_000_000_000)).toBe(1);
    expect(usdFromNano(2_500_000_000)).toBe(2.5);
  });

  it('keeps sub-cent precision visible for tiny per-call costs', () => {
    // A fraction of a cent must not collapse to "$0.00" — per-request costs are
    // routinely smaller than a cent and users need to see them.
    expect(formatUSD(3_000)).toContain('0.000003');
    expect(formatUSD(0)).toContain('0.00');
  });

  it('renders rate cards per million tokens', () => {
    expect(formatRate(0)).toBe('not priced');
    expect(formatRate(10_000_000_000)).toContain('/ Mtok');
  });
});

describe('status tone mapping', () => {
  it('maps HTTP status classes to the visual tone', () => {
    expect(statusTone(200)).toBe('success');
    expect(statusTone(429)).toBe('warning');
    expect(statusTone(403)).toBe('warning');
    expect(statusTone(503)).toBe('danger');
  });

  it('escalates quota tone at the documented thresholds', () => {
    expect(quotaTone(10)).toBe('success');
    expect(quotaTone(79.9)).toBe('success');
    expect(quotaTone(80)).toBe('warning');
    expect(quotaTone(100)).toBe('danger');
    expect(quotaTone(140)).toBe('danger');
  });
});

describe('unit formatting', () => {
  it('formats durations at human scale', () => {
    expect(formatDuration(420)).toBe('420 ms');
    expect(formatDuration(1500)).toBe('1.5 s');
    expect(formatDuration(120_000)).toBe('2 min');
  });

  it('formats byte counts', () => {
    expect(formatBytes(0)).toBe('0 B');
    expect(formatBytes(512)).toBe('512 B');
    expect(formatBytes(2048)).toBe('2.0 KB');
  });
});

describe('token count formatting (context windows)', () => {
  it('renders a neutral placeholder when the window is unknown (0)', () => {
    expect(formatTokenCount(0)).toBe('—');
    expect(formatTokenCount(undefined)).toBe('—');
    expect(formatTokenCount(-1)).toBe('—');
  });

  it('compacts exact multiples of 1K and 1M', () => {
    expect(formatTokenCount(200_000)).toBe('200K tokens');
    expect(formatTokenCount(128_000)).toBe('128K tokens');
    expect(formatTokenCount(32_000)).toBe('32K tokens');
    expect(formatTokenCount(1_000_000)).toBe('1M tokens');
    expect(formatTokenCount(2_000_000)).toBe('2M tokens');
  });

  it('keeps exact counts for windows that are not round multiples', () => {
    // 8,191 must not round up to a misleading "8K"; and gpt-4.1's window is
    // 1,047,576 exactly, not "1M".
    expect(formatTokenCount(8_191)).toBe(`${new Intl.NumberFormat().format(8191)} tokens`);
    expect(formatTokenCount(1_047_576)).toBe(`${new Intl.NumberFormat().format(1047576)} tokens`);
  });
});
