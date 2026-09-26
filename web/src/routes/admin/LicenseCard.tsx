import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { LicenseDocument, LicenseState, UpdateCheck } from '../../lib/types';
import { formatDateTime, formatNumber } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, useToast, type Tone } from '../../components/ui';
import { t } from '../../lib/i18n';
import { LICENSE_POLL_MS, useLicenseClock } from '../../lib/licenseNotice';
import { licenseVerdict, type LicenseVerdictLevel } from '../../lib/licenseHealth';
import { LicenseSyncControls } from './LicenseSyncControls';

const ACTIVATION_PREFIX = 'JANUS-ACTIVATION-1';

const FEATURE_LABELS: Record<string, string> = {
  scim: 'SCIM provisioning',
  ldap: 'LDAP',
  multi_oidc: 'Multiple identity providers',
  ha: 'High availability',
  guardrails_enforce: 'Guardrail enforcement',
  reports_scheduled: 'Scheduled reports',
  captures: 'Troubleshooting captures',
  audit_export: 'Audit export',
  model_fallbacks: 'Model fallbacks',
  email_alerts: 'Email alerting',
  airgap: 'Air-gap bundle',
  white_label: 'White label',
  sla: 'SLA',
};

export function statusTone(status: LicenseState['status']): Tone {
  switch (status) {
    case 'valid':
      return 'success';
    case 'expiring':
    case 'grace':
      return 'warning';
    default:
      return 'danger';
  }
}

function statusLabel(status: LicenseState['status']): string {
  switch (status) {
    case 'valid':
      return t('adminSystem.license.valid');
    case 'expiring':
      return t('adminSystem.license.expiring');
    case 'grace':
      return t('adminSystem.license.grace');
    case 'expired':
      return t('adminSystem.license.expired');
    default:
      return t('adminSystem.license.invalid');
  }
}

function editionLabel(edition: LicenseState['edition']): string {
  switch (edition) {
    case 'business':
      return t('adminSystem.license.business');
    case 'enterprise':
      return t('adminSystem.license.enterprise');
    default:
      return t('adminSystem.license.community');
  }
}

function termLabel(term: string): string {
  switch (term) {
    case 'perpetual':
      return t('adminSystem.license.perpetual');
    case 'trial':
      return t('adminSystem.license.trial');
    default:
      return t('adminSystem.license.subscription');
  }
}

/** Big status glyph: green check, amber exclamation, red cross. */
function VerdictIcon({ level, label }: { level: LicenseVerdictLevel; label: string }): ReactNode {
  return (
    <span className={`license-verdict-icon is-${level}`} role="img" aria-label={label}>
      <svg viewBox="0 0 24 24" width="28" height="28" aria-hidden="true" focusable="false">
        {level === 'ok' ? (
          <path
            d="M5 12.5l4.5 4.5L19 7.5"
            fill="none"
            stroke="currentColor"
            strokeWidth="2.6"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        ) : level === 'attention' ? (
          <>
            <path d="M12 5.5v8.5" fill="none" stroke="currentColor" strokeWidth="2.6" strokeLinecap="round" />
            <circle cx="12" cy="18.5" r="1.6" fill="currentColor" />
          </>
        ) : (
          <path d="M7 7l10 10M17 7L7 17" fill="none" stroke="currentColor" strokeWidth="2.6" strokeLinecap="round" />
        )}
      </svg>
    </span>
  );
}

/** Outcome of the opt-in version check, one compact line. */
function UpdateStatus({ update }: { update: UpdateCheck }): ReactNode {
  if (update.offline) return <p className="muted small">{t('adminSystem.license.updatesOffline')}</p>;
  if (!update.enabled) return <p className="muted small">{t('adminSystem.license.updatesDisabled')}</p>;
  if (!update.checked_at) return <p className="muted small">{t('adminSystem.license.updatesPending')}</p>;
  let line: string;
  let tone: Tone = 'success';
  if (update.error) {
    line = t('adminSystem.license.updatesError', { error: update.error });
    tone = 'warning';
  } else if (update.unsupported && update.min_supported) {
    line = t('adminSystem.license.updatesUnsupported', { current: update.current, min: update.min_supported });
    tone = 'danger';
  } else if (update.update_available && update.latest) {
    line = t('adminSystem.license.updatesAvailable', { latest: update.latest, current: update.current });
    tone = 'warning';
  } else {
    line = t('adminSystem.license.updatesCurrent', { version: update.current });
  }
  return (
    <div className="stack small" style={{ gap: 'var(--janus-space-1)' }}>
      <div>
        <Badge tone={tone} dot>
          {line}
        </Badge>
      </div>
      {update.advisory ? <p className="muted">{update.advisory}</p> : null}
      <p className="muted">
        {t('adminSystem.license.updatesChecked', { time: formatDateTime(update.checked_at) })}
        {update.notes_url ? (
          <>
            {' · '}
            <a href={update.notes_url} target="_blank" rel="noreferrer">
              {t('adminSystem.license.updatesNotes')}
            </a>
          </>
        ) : null}
      </p>
    </div>
  );
}

function Detail({ label, children, mono }: { label: string; children: ReactNode; mono?: boolean }): ReactNode {
  return (
    <div className="license-detail">
      <dt>{label}</dt>
      <dd className={mono ? 'mono' : undefined} title={mono && typeof children === 'string' ? children : undefined}>
        {children}
      </dd>
    </div>
  );
}

/**
 * Admin → Settings → License & updates. One verdict at the top (check / ! / X)
 * answers "is this gateway licensed and healthy?"; details, renewal and key
 * installation sit beneath it in a grid sized to fit one screen. Expiry never
 * disrupts work, so the copy talks about what is paused, never what is lost.
 */
export function LicenseCard(): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [key, setKey] = useState('');
  const [enableSync, setEnableSync] = useState(false);
  const isActivation = key.trim().startsWith(ACTIVATION_PREFIX);
  const [confirmRemove, setConfirmRemove] = useState(false);

  const doc = useQuery({
    queryKey: ['admin', 'license'],
    queryFn: () => api.get<LicenseDocument>('/api/v1/admin/system/license'),
    refetchInterval: LICENSE_POLL_MS,
    refetchIntervalInBackground: true,
  });
  const now = useLicenseClock(doc.data?.renewal_notice?.fresh_until);

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ['admin', 'license'] });
    void queryClient.invalidateQueries({ queryKey: ['me'] });
  };

  const install = useMutation({
    mutationFn: (value: string) => {
      const trimmed = value.trim();
      // The sync choice is sent only with an activation code, and only when
      // ticked, so installing never silently turns an existing setting off.
      const body: { key: string; enable_sync?: boolean } = { key: trimmed };
      if (trimmed.startsWith(ACTIVATION_PREFIX) && enableSync) body.enable_sync = true;
      return api.put<{ license: LicenseState; sync_error?: string }>('/api/v1/admin/system/license', body);
    },
    onSuccess: (result) => {
      if (result?.sync_error) toast(result.sync_error, 'danger');
      else toast(t('adminSystem.license.installed'));
      setKey('');
      setEnableSync(false);
      invalidate();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const remove = useMutation({
    mutationFn: () => api.del<{ license: LicenseState }>('/api/v1/admin/system/license'),
    onSuccess: () => {
      toast(t('adminSystem.license.removed'));
      setConfirmRemove(false);
      invalidate();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  return (
    <section className="card license-card" aria-labelledby="license-heading">
      <h2 id="license-heading" className="sr-only">
        {t('adminSystem.license.title')}
      </h2>
      <AsyncSection query={doc}>
        {(data) => {
          const lic = data.license;
          const claims = lic.claims;
          const verdict = licenseVerdict(data, now);
          return (
            <div className="license-layout">
              <div className={`license-hero is-${verdict.level}`} role="status">
                <VerdictIcon level={verdict.level} label={verdict.headline} />
                <div className="license-hero-body">
                  <div className="license-hero-title">
                    <span>{verdict.headline}</span>
                    <Badge tone="primary">{editionLabel(lic.edition)}</Badge>
                    {verdict.autoRenewing || lic.status === 'valid' ? null : (
                      <Badge tone={statusTone(lic.status)} dot>
                        {statusLabel(lic.status)}
                      </Badge>
                    )}
                  </div>
                  {verdict.reasons.length > 0 ? (
                    <ul className="license-hero-reasons">
                      {verdict.reasons.map((reason) => (
                        <li key={reason}>{reason}</li>
                      ))}
                    </ul>
                  ) : verdict.summary ? (
                    <p className="license-hero-summary">{verdict.summary}</p>
                  ) : null}
                  {lic.features.length === 0 ? (
                    <p className="muted small">{t('adminSystem.license.noFeatures')}</p>
                  ) : (
                    <ul className="license-features" aria-label={t('adminSystem.license.features')}>
                      {lic.features.map((f) => (
                        <li key={f}>{FEATURE_LABELS[f] ?? f}</li>
                      ))}
                    </ul>
                  )}
                </div>
                <a
                  className={`btn ${verdict.level === 'ok' ? 'btn-ghost' : 'btn-primary'} btn-sm license-hero-link`}
                  href={data.portal_url}
                  target="_blank"
                  rel="noreferrer"
                >
                  {t('adminSystem.license.portal')} ↗
                </a>
              </div>

              <div className="license-grid">
                <div className="license-panel">
                  <h3 className="license-panel-title">{t('adminSystem.license.detailsTitle')}</h3>
                  <dl className="license-details">
                    <Detail label={t('adminSystem.license.seats')}>
                      {t('adminSystem.license.seatsUsage', {
                        count: formatNumber(data.seats_used),
                        limit: lic.seats > 0 ? formatNumber(lic.seats) : t('adminSystem.license.unlimited'),
                      })}
                    </Detail>
                    <Detail label={t('adminSystem.license.nodes')}>
                      {t('adminSystem.license.nodesUsage', {
                        count: formatNumber(data.nodes_live),
                        limit: lic.nodes > 0 ? formatNumber(lic.nodes) : t('adminSystem.license.unlimited'),
                      })}
                    </Detail>
                    {claims ? (
                      <>
                        <Detail label={t('adminSystem.license.licensedTo')}>
                          {claims.org}
                          {claims.issued_to ? <span className="muted"> · {claims.issued_to}</span> : null}
                        </Detail>
                        <Detail label={t('adminSystem.license.licenseId')} mono>
                          {claims.license_id}
                        </Detail>
                        {claims.site ? <Detail label={t('adminSystem.license.site')}>{claims.site}</Detail> : null}
                        <Detail label={t('adminSystem.license.term')}>{termLabel(claims.term)}</Detail>
                        {claims.maintenance_until ? (
                          <Detail label={t('adminSystem.license.maintenanceUntil')}>{claims.maintenance_until}</Detail>
                        ) : null}
                      </>
                    ) : null}
                    {lic.expires_at ? (
                      <Detail label={t('adminSystem.license.expires')}>
                        {formatDateTime(lic.expires_at)}
                        {lic.grace_until ? (
                          <span className="license-detail-sub">
                            {t('adminSystem.license.graceUntil')} {formatDateTime(lic.grace_until)}
                          </span>
                        ) : null}
                      </Detail>
                    ) : null}
                    <Detail label={t('adminSystem.license.source')}>
                      {lic.source === 'file'
                        ? t('adminSystem.license.sourceFile', { name: data.file ?? '' })
                        : lic.source === 'database'
                          ? t('adminSystem.license.sourceDatabase')
                          : t('adminSystem.license.sourceNone')}
                    </Detail>
                    <Detail label={t('adminSystem.license.instanceId')} mono>
                      {data.instance_id}
                    </Detail>
                  </dl>
                </div>

                <div className="license-panel">
                  <LicenseSyncControls />
                </div>

                <div className="license-panel">
                  <form
                    className="license-install"
                    onSubmit={(event) => {
                      event.preventDefault();
                      if (key.trim()) install.mutate(key);
                    }}
                  >
                    <h3 className="license-panel-title">{t('adminSystem.license.installTitle')}</h3>
                    {data.file && lic.source === 'file' ? (
                      <p className="muted small">{t('adminSystem.license.fileWins')}</p>
                    ) : null}
                    <p className="field-hint">{t('adminSystem.license.installHelp')}</p>
                    <textarea
                      className="textarea license-key-input"
                      aria-label={t('adminSystem.license.keyLabel')}
                      rows={2}
                      value={key}
                      onChange={(event) => setKey(event.target.value)}
                      spellCheck={false}
                      placeholder="JANUS-ACTIVATION-1.... or JANUS-LICENSE-1...."
                    />
                    {isActivation ? (
                      <label className="license-check">
                        <input
                          type="checkbox"
                          aria-label={t('adminSystem.license.autoRenew')}
                          checked={enableSync}
                          onChange={(event) => setEnableSync(event.target.checked)}
                        />
                        <span>
                          {t('adminSystem.license.autoRenew')}
                          <span className="muted small" style={{ display: 'block' }}>
                            {t('adminSystem.license.autoRenewHelp')}
                          </span>
                        </span>
                      </label>
                    ) : null}
                    <div className="license-actions">
                      <button type="submit" className="btn btn-primary btn-sm" disabled={!key.trim() || install.isPending}>
                        {t('adminSystem.license.install')}
                      </button>
                      {lic.installed && lic.source === 'database' ? (
                        <button type="button" className="btn btn-ghost btn-sm" onClick={() => setConfirmRemove(true)}>
                          {t('adminSystem.license.remove')}
                        </button>
                      ) : null}
                      {!lic.installed ? <span className="muted small">{t('adminSystem.license.getKey')}</span> : null}
                    </div>
                  </form>

                  <h3 className="license-panel-title license-panel-divider">{t('adminSystem.license.updatesTitle')}</h3>
                  <UpdateStatus update={data.update} />
                </div>
              </div>

              <ConfirmDialog
                open={confirmRemove}
                onClose={() => setConfirmRemove(false)}
                onConfirm={() => remove.mutate()}
                title={t('adminSystem.license.removeConfirmTitle')}
                consequence={t('adminSystem.license.removeConfirmBody')}
                confirmLabel={t('adminSystem.license.remove')}
                busy={remove.isPending}
              />
            </div>
          );
        }}
      </AsyncSection>
    </section>
  );
}
