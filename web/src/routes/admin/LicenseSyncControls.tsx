import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import type { LicenseSync, LicenseSyncPut } from '../../lib/types';
import { LICENSE_POLL_MS, useLicenseClock } from '../../lib/licenseNotice';
import { formatDateTime } from '../../lib/format';
import { t } from '../../lib/i18n';

const endpoint = '/api/v1/admin/system/license/sync';

export function LicenseSyncControls(): ReactNode {
  const client = useQueryClient();
  const [enabled, setEnabled] = useState<boolean | undefined>();
  const [token, setToken] = useState('');
  const [clearToken, setClearToken] = useState(false);
  const [message, setMessage] = useState('');
  const query = useQuery({
    queryKey: ['admin', 'license', 'sync'],
    queryFn: async () => {
      const result = await api.get<{ license_sync?: LicenseSync }>(endpoint);
      if (!result?.license_sync || typeof result.license_sync.enabled !== 'boolean') {
        throw new Error('License sync settings unavailable');
      }
      return result.license_sync;
    },
    retry: false,
    refetchInterval: LICENSE_POLL_MS,
    refetchIntervalInBackground: true,
  });
  const data = query.data;
  const now = useLicenseClock(data?.fresh_until);
  const refresh = () => {
    void client.invalidateQueries({ queryKey: ['admin', 'license'] });
    void client.invalidateQueries({ queryKey: ['me'] });
  };
  const save = useMutation({
    mutationFn: (body: LicenseSyncPut) => api.put(endpoint, body),
    onSuccess: () => {
      setEnabled(undefined);
      setToken('');
      setClearToken(false);
      setMessage(t('licenseSync.saved'));
      refresh();
    },
    onError: () => {
      setToken('');
      setMessage(t('licenseSync.saveError'));
      refresh();
    },
  });
  const sync = useMutation({
    mutationFn: () => api.post(endpoint, {}),
    onSuccess: () => {
      setMessage(t('licenseSync.synced'));
      refresh();
    },
    onError: () => {
      setMessage(t('licenseSync.syncError'));
      refresh();
    },
  });
  const unsupported = query.error instanceof ApiError && query.error.status === 404;
  const unavailable = !data || typeof data.enabled !== 'boolean' || query.isError;
  const blocked = data?.mode === 'offline' || data?.mode === 'file';
  const busy = save.isPending || sync.isPending;
  const stale = data?.health === 'healthy' && !(Date.parse(data.fresh_until ?? '') > now);
  const health = stale ? 'stale' : data?.health;
  return (
    <section className="stack" aria-labelledby="license-sync-heading">
      <h3 id="license-sync-heading">{t('licenseSync.title')}</h3>
      <p className="muted small">{t('licenseSync.intro')}</p>
      {unsupported ? <p role="status">{t('licenseSync.unsupported')}</p> : null}
      {!unsupported && unavailable ? <p role="status">{t('licenseSync.unavailable')}</p> : null}
      {data?.mode === 'offline' ? <p role="status">{t('licenseSync.offline')}</p> : null}
      {data?.mode === 'file' ? <p role="status">{t('licenseSync.file')}</p> : null}
      <form
        className="stack"
        onSubmit={(event) => {
          event.preventDefault();
          const body: LicenseSyncPut = {};
          if (!data?.enabled_managed && enabled !== undefined) body.enabled = enabled;
          if (!data?.token_managed) {
            if (clearToken) body.clear_token = true;
            else if (token) body.token = token;
          }
          setMessage('');
          save.mutate(body);
        }}
      >
        <label>
          <input
            type="checkbox"
            aria-label={t('licenseSync.enable')}
            checked={enabled ?? data?.enabled ?? false}
            disabled={unavailable || busy || data?.enabled_managed === true}
            onChange={(event) => setEnabled(event.target.checked)}
          />{' '}
          {t('licenseSync.enable')}
        </label>
        {data?.enabled_managed ? <p className="muted small">{t('licenseSync.enabledManaged')}</p> : null}
        <label>
          {t('licenseSync.token')}
          <input
            aria-label={t('licenseSync.token')}
            type="password"
            autoComplete="new-password"
            value={token}
            disabled={unavailable || busy || data?.token_managed === true || clearToken}
            onChange={(event) => setToken(event.target.value)}
          />
        </label>
        <p className="muted small">
          {data?.has_token ? t('licenseSync.hasToken') : t('licenseSync.noToken')} {t('licenseSync.tokenHelp')}
        </p>
        {data?.token_managed ? <p className="muted small">{t('licenseSync.tokenManaged')}</p> : null}
        <label>
          <input
            type="checkbox"
            aria-label={t('licenseSync.clear')}
            checked={clearToken}
            disabled={unavailable || busy || data?.token_managed === true || !data?.has_token}
            onChange={(event) => {
              setClearToken(event.target.checked);
              setToken('');
            }}
          />{' '}
          {t('licenseSync.clear')}
        </label>
        <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
          <button
            type="submit"
            className="btn"
            disabled={unavailable || busy || (enabled === undefined && !token && !clearToken)}
          >
            {t('licenseSync.save')}
          </button>
          <button
            type="button"
            className="btn"
            disabled={
              unavailable ||
              blocked ||
              busy ||
              !data?.enabled ||
              !data?.has_token ||
              enabled !== undefined ||
              !!token ||
              clearToken
            }
            onClick={() => {
              setMessage('');
              sync.mutate();
            }}
          >
            {t('licenseSync.now')}
          </button>
        </div>
      </form>
      {message ? <p role="status">{message}</p> : null}
      {!unavailable ? (
        <div className="stack small" role="status">
          <p>
            License sync health: {['never', 'healthy', 'stale', 'error', 'disabled'].includes(health ?? '') ? health : 'unknown'}
          </p>
          {health === 'error' || health === 'stale' ? <p>{t('licenseSync.healthHelp')}</p> : null}
          <p>Last attempt: {data?.last_attempt_at ? formatDateTime(data.last_attempt_at) : 'Never'}</p>
          <p>Last successful sync: {data?.last_success_at ? formatDateTime(data.last_success_at) : 'Never'}</p>
          {data?.fresh_until ? <p>Renewal confirmation valid until: {formatDateTime(data.fresh_until)}</p> : null}
          {data?.subscription ? (
            <>
              <p>
                Subscription:{' '}
                {['active', 'trialing', 'past_due', 'unpaid', 'canceled', 'incomplete', 'incomplete_expired', 'paused'].includes(
                  data.subscription.status,
                )
                  ? data.subscription.status
                  : 'unknown'}
              </p>
              {data.subscription.cancel_at_period_end ? <p>{t('licenseSync.canceled')}</p> : null}
              {!data.subscription.auto_renew ? <p>{t('licenseSync.unconfirmed')}</p> : null}
              {data.subscription.paid_through ? <p>Paid through: {formatDateTime(data.subscription.paid_through)}</p> : null}
            </>
          ) : null}
        </div>
      ) : null}
    </section>
  );
}
