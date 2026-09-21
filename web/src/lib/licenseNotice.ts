import { useEffect, useState } from 'react';
import type { RenewalNotice } from './types';
import { t } from './i18n';

export const LICENSE_POLL_MS = 60_000;

/** The server validates billing; the browser only accepts a still-fresh positive decision. */
export function suppressExpiring(status: string, notice: RenewalNotice | undefined, now = Date.now()): boolean {
  return (
    status === 'expiring' &&
    notice?.suppress_expiring === true &&
    notice.reason === 'auto_renew' &&
    typeof notice.fresh_until === 'string' &&
    Number.isFinite(Date.parse(notice.fresh_until)) &&
    Date.parse(notice.fresh_until) > now
  );
}

/** Advisory attention is independent of the signed entitlement status. */
export function renewalAttention(notice: RenewalNotice | undefined, now = Date.now()): string | null {
  if (!notice) return null;
  if (notice.reason === 'revoked') return t('licenseBanner.revoked');
  if (notice.reason === 'subscription_attention') return t('licenseBanner.subscriptionAttention');
  if (notice.reason === 'stale' || (notice.reason === 'auto_renew' && !(Date.parse(notice.fresh_until ?? '') > now)))
    return t('licenseBanner.stale');
  if (['unknown', 'unknown_subscription', 'network', 'credentials_rejected', 'credential', 'storage', 'rate_limited', 'upstream', 'response_size', 'response_invalid', 'identity_changed', 'signature_invalid', 'key_regression', 'unsafe_endpoint', 'revision_regression', 'metadata_invalid', 'request', 'local_refresh'].includes(notice.reason))
    return t('licenseBanner.syncAttention');
  return null;
}

/** Deadline timer works even when requests fail; focus catches throttled background tabs. */
export function useLicenseClock(deadline?: string): number {
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    const update = () => setNow(Date.now());
    update();
    const remaining = Date.parse(deadline ?? '') - Date.now();
    const timer = window.setTimeout(
      update,
      Number.isFinite(remaining) && remaining > 0 ? Math.min(remaining, 2_147_483_647) : LICENSE_POLL_MS,
    );
    const interval = window.setInterval(update, LICENSE_POLL_MS);
    window.addEventListener('focus', update);
    document.addEventListener('visibilitychange', update);
    return () => {
      clearTimeout(timer);
      clearInterval(interval);
      window.removeEventListener('focus', update);
      document.removeEventListener('visibilitychange', update);
    };
  }, [deadline]);
  return now;
}
