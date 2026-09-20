import { useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useSession } from './session';
import { formatRelative } from '../lib/format';
import { t } from '../lib/i18n';

/**
 * Shell-wide license notice. Shown only in the states that need action
 * (expiring, grace, expired, invalid); Community and valid keys are silent.
 * Admins get a link to the System page; everyone else sees why creation is
 * paused. Dismissal is per session and only for the soft states — an expired
 * or invalid key stays visible because creation really is blocked.
 */
export function LicenseBanner(): ReactNode {
  const { me } = useSession();
  const [dismissed, setDismissed] = useState(false);
  const license = me?.license;
  if (!license || license.status === 'valid') return null;
  const soft = license.status === 'expiring' || license.status === 'grace';
  if (soft && dismissed) return null;

  let message: string;
  switch (license.status) {
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
    <div className={`banner ${soft ? 'banner-warning' : 'banner-danger'} license-banner`} role="status">
      <span>{message}</span>
      <span className="row" style={{ gap: 'var(--janus-space-2)', marginLeft: 'auto' }}>
        {isAdmin ? (
          <Link className="btn btn-sm" to="/admin/settings/license">
            {t('licenseBanner.manage')}
          </Link>
        ) : null}
        {soft ? (
          <button type="button" className="btn btn-sm btn-ghost" onClick={() => setDismissed(true)}>
            {t('licenseBanner.dismiss')}
          </button>
        ) : null}
      </span>
    </div>
  );
}
