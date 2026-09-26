import { describe, expect, it } from 'vitest';
import type { LicenseDocument } from './types';
import { licenseVerdict } from './licenseHealth';

const NOW = Date.parse('2026-09-26T12:00:00Z');

function doc(overrides: Partial<LicenseDocument> = {}, license: Partial<LicenseDocument['license']> = {}): LicenseDocument {
  return {
    license: {
      installed: true,
      source: 'database',
      edition: 'business',
      status: 'valid',
      seats: 10,
      nodes: 3,
      features: ['scim'],
      checked_at: '',
      expires_at: '2027-01-01T00:00:00Z',
      claims: {
        key_id: '2026-09',
        license_id: 'JNS-BUS-TEST',
        org: 'Acme',
        issued_to: 'ops@acme.test',
        edition: 'business',
        seats: 10,
        nodes: 3,
        features: ['scim'],
        issued: '2026-09-01T00:00:00Z',
        exp: '2027-01-01T00:00:00Z',
        grace_days: 7,
        term: 'subscription',
      },
      ...license,
    },
    seats_used: 2,
    nodes_live: 2,
    seat_window: '30d',
    instance_id: 'inst',
    version: 'dev',
    portal_url: 'https://janusedge.com/portal',
    update: { enabled: false, offline: false, current: 'dev', update_available: false, unsupported: false },
    ...overrides,
  };
}

const freshRenewal = {
  renewal_notice: { suppress_expiring: true, reason: 'auto_renew', fresh_until: '2026-09-27T00:00:00Z' },
  license_sync: {
    enabled: true,
    has_token: true,
    mode: 'online' as const,
    health: 'healthy' as const,
    fresh_until: '2026-09-27T00:00:00Z',
    subscription: {
      schema_version: 1,
      status: 'active',
      auto_renew: true,
      cancel_at_period_end: false,
      paid_through: '2026-10-26T18:01:00Z',
    },
  },
};

describe('licenseVerdict', () => {
  it('is green for a valid key within limits', () => {
    const v = licenseVerdict(doc(), NOW);
    expect(v.level).toBe('ok');
    expect(v.headline).toBe('Everything looks good');
    expect(v.reasons).toEqual([]);
  });

  it('is green for an expiring subscription that is confirmed to renew automatically', () => {
    const v = licenseVerdict(doc(freshRenewal, { status: 'expiring', expires_at: '2026-10-26T18:01:00Z' }), NOW);
    expect(v.level).toBe('ok');
    expect(v.autoRenewing).toBe(true);
    expect(v.summary).toMatch(/Renews automatically/);
  });

  it('is green for Community with no key', () => {
    const v = licenseVerdict(doc({}, { installed: false, edition: 'community', claims: undefined, expires_at: undefined }), NOW);
    expect(v.level).toBe('ok');
    expect(v.summary).toMatch(/Community edition/);
  });

  it('needs attention when expiring without confirmed renewal', () => {
    const v = licenseVerdict(doc({}, { status: 'expiring' }), NOW);
    expect(v.level).toBe('attention');
    expect(v.reasons.join(' ')).toMatch(/Renew at the portal before/);
  });

  it('needs attention when renewal confirmation has gone stale', () => {
    const stale = { ...freshRenewal, renewal_notice: { ...freshRenewal.renewal_notice, fresh_until: '2026-09-26T00:00:00Z' } };
    const v = licenseVerdict(doc(stale, { status: 'expiring' }), NOW);
    expect(v.level).toBe('attention');
    expect(v.autoRenewing).toBe(false);
  });

  it('needs attention in grace, over seats or over nodes', () => {
    expect(licenseVerdict(doc({}, { status: 'grace' }), NOW).level).toBe('attention');
    expect(licenseVerdict(doc({ seats_used: 11 }), NOW).reasons.join(' ')).toMatch(/11 people were active/);
    expect(licenseVerdict(doc({ nodes_live: 4 }), NOW).level).toBe('attention');
  });

  it('needs attention when billing reports a scheduled cancellation', () => {
    const canceled = {
      ...freshRenewal,
      renewal_notice: { suppress_expiring: false, reason: 'subscription_attention' },
      license_sync: {
        ...freshRenewal.license_sync,
        subscription: { ...freshRenewal.license_sync.subscription, cancel_at_period_end: true },
      },
    };
    expect(licenseVerdict(doc(canceled), NOW).level).toBe('attention');
  });

  it.each(['expired', 'invalid'] as const)('is red when the key is %s', (status) => {
    const v = licenseVerdict(doc({}, { status, error: status === 'invalid' ? 'license: bad signature' : undefined }), NOW);
    expect(v.level).toBe('unlicensed');
    expect(v.headline).toBe('Not properly licensed');
    expect(v.reasons[0]).toMatch(/Creating new tokens/);
  });
});
