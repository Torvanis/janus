import { describe, expect, it } from 'vitest';
import { en, getLocale, setLocale, t, type MessageKey } from './i18n';

function leafKeys(node: unknown, prefix = ''): string[] {
  if (typeof node === 'string') return [prefix];
  const keys: string[] = [];
  for (const [name, child] of Object.entries(node as Record<string, unknown>)) {
    keys.push(...leafKeys(child, prefix ? `${prefix}.${name}` : name));
  }
  return keys;
}

describe('i18n catalog', () => {
  const keys = leafKeys(en);

  it('has at least the seed migration surface (UI kit + error pages)', () => {
    for (const required of [
      'common.loading',
      'common.cancel',
      'common.confirm',
      'errorState.title',
      'errorState.requestId',
      'pagination.range',
      'forbidden.title',
      'notFound.title',
      // Route pages (the brief requires all UI copy through the i18n layer).
      'requests.title',
      'dashboard.welcome',
      'quota.title',
      'team.quotasTitle',
      'adminUpstreams.title',
      'adminModels.title',
      // Shell throughput readout (split in/out token rates).
      'shell.reqPerMin',
      'shell.tokensInPerMin',
      'shell.tokensOutPerMin',
    ]) {
      expect(keys, `catalog is missing ${required}`).toContain(required);
    }
  });

  it('does not carry the retired combined tokens/min key', () => {
    // The combined readout key was replaced by the in/out pair; assert no
    // catalog leaf ends in the retired name (spelled as a pattern so the
    // repo-wide grep for the old key stays clean).
    expect(keys.filter((key) => /\.tokensPerMin$/.test(key))).toEqual([]);
  });

  it('renders the split throughput readout strings', () => {
    expect(t('shell.tokensInPerMin', { count: '1,000' })).toBe('1,000 in/min');
    expect(t('shell.tokensOutPerMin', { count: '500' })).toBe('500 out/min');
  });

  it('carries the split request-log token columns and size-filter labels', () => {
    // The combined "Tokens in / out" request-log column was split into two
    // sortable columns, each with a threshold filter (smaller/greater than
    // 1,000 / 10,000 / 100,000 / 500,000). Every label the Requests pages and
    // the admin People drawer render for that UI must resolve from the catalog.
    expect(t('tables.tokensIn')).toBe('Tokens in');
    expect(t('tables.tokensOut')).toBe('Tokens out');
    expect(t('shared.anyTokensIn')).toBe('Any tokens in');
    expect(t('shared.anyTokensOut')).toBe('Any tokens out');
    for (const count of ['1,000', '10,000', '100,000', '500,000']) {
      expect(t('shared.tokensSmallerThan', { count })).toBe(`Smaller than ${count}`);
      expect(t('shared.tokensGreaterThan', { count })).toBe(`Greater than ${count}`);
    }
  });

  it('contains no empty or whitespace-only strings', () => {
    for (const key of keys) {
      expect(t(key as MessageKey).trim().length, `${key} is empty`).toBeGreaterThan(0);
    }
  });

  it('resolves every key without falling back to the key itself', () => {
    for (const key of keys) {
      expect(t(key as MessageKey), `${key} did not resolve`).not.toBe(key);
    }
  });

  it('substitutes placeholders', () => {
    expect(t('errorState.requestId', { id: 'req_123' })).toBe('Request ID req_123');
    expect(t('pagination.range', { from: 1, to: 25, total: 100 })).toBe('1–25 of 100');
    expect(t('pagination.page', { page: 2, pages: 4 })).toBe('Page 2 of 4');
  });

  it('leaves no unsubstituted {placeholder} when params are supplied', () => {
    for (const key of keys) {
      const message = t(key as MessageKey, {
        id: 'x',
        role: 'x',
        from: 1,
        to: 2,
        total: 3,
        page: 1,
        pages: 2,
        name: 'x',
        count: 1,
        limit: 'x',
        time: 'x',
        percent: 5,
        current: 'x',
        latest: 'x',
        version: 'x',
        found: 1,
        fresh: 1,
        error: 'x',
        issuer: 'x',
        mask: 'x',
        action: 'x',
        model: 'x',
        metric: 'x',
        revoked: 1,
        failed: 1,
        list: 'x',
        tokensIn: 'x',
        tokensOut: 'x',
        rate: 'x',
        ms: 1,
        when: 'x',
        errors: 1,
        requests: 1,
        size: 'x',
        max: 'x',
        min: 1,
        value: 'x',
        provider: 'x',
        days: 30,
        gate: 'x',
        kinds: 'x',
        who: 'x',
        rule: 'x',
        score: 'x',
        offset: 1,
        length: 1,
        shown: 1,
      });
      expect(message, `${key} has an unresolved placeholder`).not.toMatch(/\{[a-zA-Z]+\}/);
    }
  });

  it('returns the key itself for unknown keys instead of crashing', () => {
    expect(t('does.not.exist' as MessageKey)).toBe('does.not.exist');
  });

  it('falls back to English for unknown locales', () => {
    setLocale('xx');
    expect(getLocale()).toBe('en');
    expect(t('common.loading')).toBe('Loading');
  });
});
