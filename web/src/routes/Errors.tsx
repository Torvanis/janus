import { useState, type ReactNode } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { t } from '../lib/i18n';

/**
 * 403 and 404 are deliberately distinct pages. Telling an authorisation failure
 * apart from missing data is what stops "why can't I see this?" support tickets.
 */
export function ForbiddenPage({ requiredRole, detail }: { requiredRole?: string; detail?: string }): ReactNode {
  return (
    <div className="page">
      <div className="state" style={{ minHeight: '60dvh' }}>
        <div className="overline">{t('forbidden.overline')}</div>
        <h1 className="state-title">{t('forbidden.title')}</h1>
        <p className="state-body">
          {detail ?? (requiredRole ? t('forbidden.bodyWithRole', { role: requiredRole }) : t('forbidden.body'))}{' '}
          {t('forbidden.appeal')}
        </p>
        <div className="row">
          <Link className="btn btn-primary" to="/dashboard">
            {t('common.backToDashboard')}
          </Link>
          <Link className="btn" to="/docs/api/errors">
            {t('forbidden.aboutPermissionErrors')}
          </Link>
        </div>
      </div>
    </div>
  );
}

export function NotFoundPage(): ReactNode {
  const [query, setQuery] = useState('');
  const navigate = useNavigate();

  return (
    <div className="page">
      <div className="state" style={{ minHeight: '60dvh' }}>
        <div className="overline">{t('notFound.overline')}</div>
        <h1 className="state-title">{t('notFound.title')}</h1>
        <p className="state-body">{t('notFound.body')}</p>
        <form
          className="row"
          onSubmit={(event) => {
            event.preventDefault();
            navigate(`/docs/search?q=${encodeURIComponent(query)}`);
          }}
        >
          <input
            className="input"
            placeholder={t('notFound.searchPlaceholder')}
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            aria-label={t('notFound.searchPlaceholder')}
            style={{ minWidth: 220 }}
          />
          <button type="submit" className="btn">
            {t('common.search')}
          </button>
        </form>
        <Link className="btn btn-primary" to="/dashboard">
          {t('common.backToDashboard')}
        </Link>
      </div>
    </div>
  );
}
