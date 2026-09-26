import type { LicenseDocument } from './types';
import { formatDateTime, formatNumber } from './format';
import { t } from './i18n';
import { renewalAttention, suppressExpiring } from './licenseNotice';

export type LicenseVerdictLevel = 'ok' | 'attention' | 'unlicensed';

export interface LicenseVerdict {
  level: LicenseVerdictLevel;
  /** One-line headline shown next to the status icon. */
  headline: string;
  /** Short context line under the headline. */
  summary: string;
  /** Every reason the page is not green, most severe first. Empty when ok. */
  reasons: string[];
  /** True when automatic renewal is confirmed and fresh. */
  autoRenewing: boolean;
}

/**
 * Collapses everything the license page knows into one verdict: a green check
 * when nothing needs doing, a yellow "attention" when work continues but an
 * administrator should act, and a red X when creation is paused because the
 * gateway is not properly licensed. The server remains the authority on each
 * input; this only ranks them for display.
 */
export function licenseVerdict(doc: LicenseDocument, now = Date.now()): LicenseVerdict {
  const lic = doc.license;
  const sync = doc.license_sync;
  const autoRenewing = suppressExpiring(lic.status, doc.renewal_notice, now);
  const reasons: string[] = [];
  const add = (reason: string | null | undefined) => {
    if (reason && !reasons.includes(reason)) reasons.push(reason);
  };

  const unlicensed = lic.status === 'expired' || lic.status === 'invalid';
  if (unlicensed) add(t('adminSystem.license.restrictedNotice'));
  if (lic.error) add(lic.error);
  if (lic.status === 'grace' && lic.expires_at) {
    add(t('adminSystem.license.graceNotice', { time: formatDateTime(lic.expires_at) }));
  }
  if (lic.status === 'expiring' && lic.expires_at && !autoRenewing) {
    add(t('adminSystem.license.expiringNotice', { time: formatDateTime(lic.expires_at) }));
  }
  // Renewal and sync problems collapse into one line; the renewal panel
  // below carries the specific cause next to its controls.
  const renewal = renewalAttention(doc.renewal_notice, now);
  const syncTrouble = sync?.enabled && (sync.health === 'error' || sync.health === 'stale');
  if (renewal || syncTrouble || sync?.subscription?.cancel_at_period_end) add(t('adminSystem.license.heroSyncIssue'));
  if (lic.nodes > 0 && doc.nodes_live > lic.nodes) {
    add(t('adminSystem.license.nodesOver', { count: formatNumber(doc.nodes_live), limit: formatNumber(lic.nodes) }));
  }
  if (lic.seats > 0 && doc.seats_used > lic.seats) {
    add(t('adminSystem.license.heroSeatsOver', { count: formatNumber(doc.seats_used), limit: formatNumber(lic.seats) }));
  }
  if (lic.clock_skew) add(t('adminSystem.license.clockSkew'));
  if (doc.update?.unsupported && doc.update.min_supported) {
    add(t('adminSystem.license.updatesUnsupported', { current: doc.update.current, min: doc.update.min_supported }));
  }

  if (unlicensed) {
    return { level: 'unlicensed', headline: t('adminSystem.license.heroUnlicensed'), summary: '', reasons, autoRenewing };
  }
  if (reasons.length > 0) {
    return { level: 'attention', headline: t('adminSystem.license.heroAttention'), summary: '', reasons, autoRenewing };
  }

  let summary: string;
  const paidThrough = sync?.subscription?.paid_through;
  if (!lic.installed || lic.edition === 'community') summary = t('adminSystem.license.heroCommunity');
  else if (autoRenewing && paidThrough) summary = t('adminSystem.license.heroAutoRenew', { time: formatDateTime(paidThrough) });
  else if (lic.claims?.term === 'perpetual') summary = t('adminSystem.license.heroPerpetual');
  else if (lic.expires_at) summary = t('adminSystem.license.heroValidUntil', { time: formatDateTime(lic.expires_at) });
  else summary = '';
  return { level: 'ok', headline: t('adminSystem.license.heroOk'), summary, reasons, autoRenewing };
}
