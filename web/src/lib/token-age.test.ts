import { describe, expect, it } from 'vitest';
import { isLongRevoked } from './types';

const now = Date.parse('2026-11-14T12:00:00Z');

describe('isLongRevoked', () => {
  it('is true only for tokens revoked more than 7 days before now', () => {
    expect(isLongRevoked({ revoked_at: '2026-11-07T11:59:59Z' }, now)).toBe(true);
    expect(isLongRevoked({ revoked_at: '2026-11-07T12:00:00Z' }, now)).toBe(false);
    expect(isLongRevoked({ revoked_at: '2026-11-13T12:00:00Z' }, now)).toBe(false);
  });

  it('never hides an active token, the legacy zero time, or an unreadable date', () => {
    expect(isLongRevoked({ revoked_at: '' }, now)).toBe(false);
    expect(isLongRevoked({ revoked_at: '0001-01-01T00:00:00Z' }, now)).toBe(false);
    expect(isLongRevoked({ revoked_at: 'not a date' }, now)).toBe(false);
  });
});
