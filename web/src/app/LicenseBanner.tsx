import { useEffect, useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useSession } from './session';
import { formatRelative } from '../lib/format';
import { t } from '../lib/i18n';
import { renewalAttention, suppressExpiring, useLicenseClock } from '../lib/licenseNotice';

/**
 * Shell-wide license notice. Shown only in the states that need action
 * (expiring, grace, expired, invalid), plus independent renewal attention even for valid keys.
 * Admins get a link to the System page; everyone else sees why creation is
 * paused. Dismissal is per session and only for the soft states — an expired
 * or invalid key stays visible because creation really is blocked.
 */
export function LicenseBanner(): ReactNode {
  const { me } = useSession();
  const [dismissed, setDismissed] = useState<string | null>(null);
  const license = me?.license;
  const now = useLicenseClock(license?.renewal_notice?.fresh_until);
  const suppressed = suppressExpiring(license?.status ?? '', license?.renewal_notice, now);
  const attention = renewalAttention(license?.renewal_notice, now);
  const noticeKey = JSON.stringify([license, suppressed, attention]);
  useEffect(() => setDismissed(null), [noticeKey]);
  if (!license || (!attention && (license.status === 'valid' || suppressed))) return null;
  const soft = !attention && (license.status === 'expiring' || license.status === 'grace');
  if (soft && dismissed === noticeKey) return null;

  let message: string;
  switch (license.status) {
    case 'valid':
      message = '';
      break;
    case 'expiring':
      message = t('licenseBanner.expiring', { time: formatRelative(license.expires_at) });
      break;
    case 'grace':
      message = t('licenseBanner.grace', { time: formatRelative(license.expires_at) });
      break;
    case 'expired':
      message = t('licenseBanner.expired');
      break;
    default:
      message = t('licenseBanner.invalid');
  }
  const isAdmin = me?.role === 'admin';
  return (
    <div className={`banner ${license.status === 'expired' || license.status === 'invalid' ? 'banner-danger' : 'banner-warning'} license-banner`} role="status">
      <span>{message}{message && attention ? ' ' : ''}{attention}</span>
      <span className="row" style={{ gap: 'var(--janus-space-2)', marginLeft: 'auto' }}>
        {isAdmin ? (
          <Link className="btn btn-sm" to="/admin/settings/license">
            {t('licenseBanner.manage')}
          </Link>
        ) : null}
        {soft ? (
          <button type="button" className="btn btn-sm btn-ghost" onClick={() => setDismissed(noticeKey)}>
            {t('licenseBanner.dismiss')}
          </button>
        ) : null}
      </span>
    </div>
  );
}
