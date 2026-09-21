import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it } from 'vitest';
import { LicenseBanner } from './LicenseBanner';
import { SessionContext } from './session';
import type { Me } from '../lib/types';

afterEach(cleanup);
function view(status: string, reason: string, role = 'admin') {
 const me = { role, license: { status, expires_at: '2027-01-01', renewal_notice: { reason, suppress_expiring: reason === 'auto_renew', fresh_until: '2099-01-01' } } } as Me;
 return <MemoryRouter><SessionContext.Provider value={{ me, config: null, loading: false, error: null, refresh: () => {} }}><LicenseBanner /></SessionContext.Provider></MemoryRouter>;
}
describe('signed rights and renewal attention are independent', () => {
 it.each(['revoked', 'subscription_attention', 'stale', 'network'])('shows %s even with a valid key', reason => {
  render(view('valid', reason));
  expect(screen.getByRole('status').textContent).toBeTruthy();
  expect(screen.getByRole('link').getAttribute('href')).toBe('/admin/settings/license');
  expect(screen.queryByRole('button')).toBeNull();
 });
 it('keeps healthy/manual valid keys silent and member warnings identity-free', () => {
  const v = render(view('valid', 'auto_renew'));expect(screen.queryByRole('status')).toBeNull();
  v.rerender(view('valid', 'manual'));expect(screen.queryByRole('status')).toBeNull();
  v.rerender(view('valid', 'revoked', 'user'));expect(screen.getByRole('status').textContent).toContain('Existing signed rights are unchanged');expect(screen.queryByRole('link')).toBeNull();
 });
 it('does not let a prior dismissal hide changed billing attention or hard expiry', () => {
  const v=render(view('expiring','manual'));fireEvent.click(screen.getByRole('button'));expect(screen.queryByRole('status')).toBeNull();
  v.rerender(view('valid','subscription_attention'));expect(screen.getByRole('status')).toBeTruthy();
  for(const status of ['grace','expired','invalid']) { v.rerender(view(status,'auto_renew'));expect(screen.getByRole('status')).toBeTruthy(); }
 });
});
