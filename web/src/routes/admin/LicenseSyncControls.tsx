import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import type { LicenseSync, LicenseSyncPut } from '../../lib/types';
import { LICENSE_POLL_MS, useLicenseClock } from '../../lib/licenseNotice';
import { formatDateTime } from '../../lib/format';
import { t } from '../../lib/i18n';
import { Badge, type Tone } from '../../components/ui';

const endpoint = '/api/v1/admin/system/license/sync';

const HEALTH = ['never', 'healthy', 'stale', 'error', 'disabled'] as const;
const SUBSCRIPTION = ['active', 'trialing', 'past_due', 'unpaid', 'canceled', 'incomplete', 'incomplete_expired', 'paused'];

function healthTone(health: string): Tone {
  switch (health) {
    case 'healthy':
      return 'success';
    case 'stale':
    case 'never':
      return 'warning';
    case 'error':
      return 'danger';
    default:
      return 'neutral';
  }
}

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
  const rawHealth = stale ? 'stale' : data?.health;
  const health = (HEALTH as readonly string[]).includes(rawHealth ?? '') ? (rawHealth as string) : 'unknown';
  const subscription = data?.subscription;
  const subscriptionStatus = subscription && SUBSCRIPTION.includes(subscription.status) ? subscription.status : 'unknown';
  return (
    <section className="license-sync" aria-labelledby="license-sync-heading">
      <h3 id="license-sync-heading" className="license-panel-title">
        {t('licenseSync.title')}
      </h3>
      {!data?.enabled ? <p className="muted small">{t('licenseSync.intro')}</p> : null}
      {unsupported ? (
        <p role="status" className="small">
          {t('licenseSync.unsupported')}
        </p>
      ) : null}
      {!unsupported && unavailable ? (
        <p role="status" className="small">
          {t('licenseSync.unavailable')}
        </p>
      ) : null}
      {data?.mode === 'offline' ? (
        <p role="status" className="small">
          {t('licenseSync.offline')}
        </p>
      ) : null}
      {data?.mode === 'file' ? (
        <p role="status" className="small">
          {t('licenseSync.file')}
        </p>
      ) : null}

      {!unavailable ? (
        <dl className="license-details" role="status">
          <div className="license-detail">
            <dt>Health</dt>
            <dd>
              <Badge tone={healthTone(health)} dot>
                {`Sync ${health}`}
              </Badge>
            </dd>
          </div>
          <div className="license-detail">
            <dt>{t('adminSystem.license.lastSync')}</dt>
            <dd>{data?.last_success_at ? formatDateTime(data.last_success_at) : t('adminSystem.license.never')}</dd>
          </div>
          {data?.last_attempt_at && data.last_attempt_at !== data.last_success_at ? (
            <div className="license-detail">
              <dt>{t('adminSystem.license.lastAttempt')}</dt>
              <dd>{formatDateTime(data.last_attempt_at)}</dd>
            </div>
          ) : null}
          {subscription && subscriptionStatus !== 'active' ? (
            <div className="license-detail">
              <dt>Subscription</dt>
              <dd>{subscriptionStatus.replace(/_/g, ' ')}</dd>
            </div>
          ) : null}
          {data?.fresh_until ? (
            <div className="license-detail">
              <dt>Renewal confirmed until</dt>
              <dd>{formatDateTime(data.fresh_until)}</dd>
            </div>
          ) : null}
        </dl>
      ) : null}
      {health === 'error' || health === 'stale' ? <p className="small license-sync-warn">{t('licenseSync.healthHelp')}</p> : null}
      {subscription?.cancel_at_period_end ? <p className="small license-sync-warn">{t('licenseSync.canceled')}</p> : null}
      {subscription && !subscription.auto_renew ? (
        <p className="small license-sync-warn">{t('licenseSync.unconfirmed')}</p>
      ) : null}

      <form
        className="license-sync-form"
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
        <label className="switch">
          <input
            type="checkbox"
            aria-label={t('licenseSync.enable')}
            checked={enabled ?? data?.enabled ?? false}
            disabled={unavailable || busy || data?.enabled_managed === true}
            onChange={(event) => setEnabled(event.target.checked)}
          />
          <span>{t('licenseSync.enable')}</span>
        </label>
        {data?.enabled_managed ? <p className="muted small">{t('licenseSync.enabledManaged')}</p> : null}
        <div className="field">
          <div className="license-field-head">
            <label className="field-label" htmlFor="license-sync-token">
              {t('licenseSync.token')}
            </label>
            <label className="license-check small license-clear">
              <input
                type="checkbox"
                aria-label={t('licenseSync.clear')}
                checked={clearToken}
                disabled={unavailable || busy || data?.token_managed === true || !data?.has_token}
                onChange={(event) => {
                  setClearToken(event.target.checked);
                  setToken('');
                }}
              />
              <span>{t('licenseSync.clear')}</span>
            </label>
          </div>
          <input
            id="license-sync-token"
            className="input"
            aria-label={t('licenseSync.token')}
            type="password"
            autoComplete="new-password"
            placeholder={data?.has_token ? t('licenseSync.tokenPlaceholder') : ''}
            value={token}
            disabled={unavailable || busy || data?.token_managed === true || clearToken}
            onChange={(event) => setToken(event.target.value)}
          />
          {data?.has_token ? null : (
            <span className="field-hint">
              {t('licenseSync.noToken')} {t('licenseSync.tokenHelp')}
            </span>
          )}
        </div>
        {data?.token_managed ? <p className="muted small">{t('licenseSync.tokenManaged')}</p> : null}
        <div className="license-actions">
          <button
            type="submit"
            className="btn btn-sm"
            disabled={unavailable || busy || (enabled === undefined && !token && !clearToken)}
          >
            {t('licenseSync.save')}
          </button>
          <button
            type="button"
            className="btn btn-sm"
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
      {message ? (
        <p role="status" className="small">
          {message}
        </p>
      ) : null}
    </section>
  );
}
