import { describe, expect, it } from 'vitest';
import { renewalAttention, suppressExpiring } from './licenseNotice';

describe('renewal suppression', () => {
  it('renders independent attention for valid signed licenses without exposing metadata', () => {
    for (const reason of ['revoked', 'subscription_attention', 'stale', 'network', 'credentials_rejected', 'metadata_invalid'])
      expect(renewalAttention({ suppress_expiring: false, reason })).toBeTruthy();
    expect(renewalAttention({ suppress_expiring: true, reason: 'auto_renew', fresh_until: '2000-01-01' })).toBeTruthy();
    for (const reason of ['manual', 'offline', 'file', 'never'])
      expect(renewalAttention({ suppress_expiring: false, reason })).toBeNull();
  });
  const now = Date.parse('2026-09-20T00:00:00Z');
  const notice = { suppress_expiring: true, reason: 'auto_renew', fresh_until: '2026-09-20T01:00:00Z' };
  it('only suppresses ordinary expiry with a future explicit deadline', () => {
    expect(suppressExpiring('expiring', notice, now)).toBe(true);
    for (const status of ['valid', 'grace', 'expired', 'invalid', 'revoked'])
      expect(suppressExpiring(status, notice, now)).toBe(false);
  });
  it('fails closed for old servers and invalid, absent, elapsed deadlines', () => {
    expect(suppressExpiring('expiring', undefined, now)).toBe(false);
    for (const fresh_until of [undefined, '', 'invalid', '2026-09-20T00:00:00Z', '2026-09-19T00:00:00Z'])
      expect(suppressExpiring('expiring', { ...notice, fresh_until }, now)).toBe(false);
    expect(suppressExpiring('expiring', { ...notice, suppress_expiring: false }, now)).toBe(false);
    expect(suppressExpiring('expiring', { ...notice, reason: 'future_unknown_reason' }, now)).toBe(false);
  });
});
