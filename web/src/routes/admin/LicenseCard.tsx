import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { LicenseDocument, LicenseState, UpdateCheck } from '../../lib/types';
import { formatDateTime, formatNumber } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, Field, useToast, type Tone } from '../../components/ui';
import { DetailRow } from '../shared';
import { t } from '../../lib/i18n';
import { LICENSE_POLL_MS, renewalAttention, suppressExpiring, useLicenseClock } from '../../lib/licenseNotice';
import { LicenseSyncControls } from './LicenseSyncControls';

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

/** Outcome of the opt-in version check, one line plus an optional link. */
function UpdateStatus({ update }: { update: UpdateCheck }): ReactNode {
  if (update.offline) return <p className="muted small">{t('adminSystem.license.updatesOffline')}</p>;
  if (!update.enabled) return <p className="muted small">{t('adminSystem.license.updatesDisabled')}</p>;
  if (!update.checked_at) return <p className="muted small">{t('adminSystem.license.updatesPending')}</p>;
  let line: ReactNode;
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
      <div className="row" style={{ gap: 'var(--janus-space-2)', alignItems: 'center' }}>
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

/**
 * Admin → System license card: verified state, seat usage, claims, and the
 * paste-a-key form. Expiry never disrupts work, so the copy talks about what
 * is paused (creation), never what is lost.
 */
export function LicenseCard(): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [key, setKey] = useState('');
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
    mutationFn: (value: string) => api.put<{ license: LicenseState }>('/api/v1/admin/system/license', { key: value.trim() }),
    onSuccess: () => {
      toast(t('adminSystem.license.installed'));
      setKey('');
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
    <section className="card" aria-labelledby="license-heading">
      <div className="card-header" style={{ alignItems: 'flex-start' }}>
        <div>
          <h2 id="license-heading">{t('adminSystem.license.title')}</h2>
        </div>
        <AsyncSection query={doc}>
          {(data) => (
            <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
              <Badge tone="primary">{editionLabel(data.license.edition)}</Badge>
              <Badge tone={statusTone(data.license.status)} dot>
                {statusLabel(data.license.status)}
              </Badge>
            </div>
          )}
        </AsyncSection>
      </div>

      <AsyncSection query={doc}>
        {(data) => {
          const lic = data.license;
          const claims = lic.claims;
          const attention = renewalAttention(data.renewal_notice, now);
          const restricted = lic.status === 'expired' || lic.status === 'invalid';
          return (
            <div className="stack">
              {attention ? <div className="banner banner-warning" role="status">{attention}</div> : null}
              {restricted ? <div className="banner banner-danger">{t('adminSystem.license.restrictedNotice')}</div> : null}
              {lic.status === 'expiring' && lic.expires_at && !suppressExpiring(lic.status, data.renewal_notice, now) ? (
                <div className="banner banner-warning">
                  {t('adminSystem.license.expiringNotice', { time: formatDateTime(lic.expires_at) })}
                </div>
              ) : null}
              {lic.status === 'grace' && lic.expires_at ? (
                <div className="banner banner-warning">
                  {t('adminSystem.license.graceNotice', { time: formatDateTime(lic.expires_at) })}
                </div>
              ) : null}
              {lic.error ? <div className="banner banner-danger">{lic.error}</div> : null}
              {lic.clock_skew ? <div className="banner banner-warning">{t('adminSystem.license.clockSkew')}</div> : null}

              <div className="grid grid-halves">
                <div className="stack" style={{ gap: 'var(--janus-space-1)' }}>
                  <DetailRow label={t('adminSystem.license.seats')}>
                    {t('adminSystem.license.seatsUsage', {
                      count: formatNumber(data.seats_used),
                      limit: lic.seats > 0 ? formatNumber(lic.seats) : t('adminSystem.license.unlimited'),
                    })}
                  </DetailRow>
                  <DetailRow label={t('adminSystem.license.nodes')}>
                    {t('adminSystem.license.nodesUsage', {
                      count: formatNumber(data.nodes_live),
                      limit: lic.nodes > 0 ? formatNumber(lic.nodes) : t('adminSystem.license.unlimited'),
                    })}
                  </DetailRow>
                  {lic.nodes > 0 && data.nodes_live > lic.nodes ? (
                    <div className="banner banner-warning">
                      {t('adminSystem.license.nodesOver', {
                        count: formatNumber(data.nodes_live),
                        limit: formatNumber(lic.nodes),
                      })}
                    </div>
                  ) : null}
                  {claims ? (
                    <>
                      <DetailRow label={t('adminSystem.license.licensedTo')}>
                        {claims.org}
                        {claims.issued_to ? <span className="muted small"> · {claims.issued_to}</span> : null}
                      </DetailRow>
                      <DetailRow label={t('adminSystem.license.licenseId')}>
                        <code>{claims.license_id}</code>
                      </DetailRow>
                      {claims.site ? <DetailRow label={t('adminSystem.license.site')}>{claims.site}</DetailRow> : null}
                      <DetailRow label={t('adminSystem.license.term')}>{termLabel(claims.term)}</DetailRow>

                      {claims.maintenance_until ? (
                        <DetailRow label={t('adminSystem.license.maintenanceUntil')}>{claims.maintenance_until}</DetailRow>
                      ) : null}
                    </>
                  ) : null}
                  {lic.expires_at ? (
                    <DetailRow label={t('adminSystem.license.expires')}>{formatDateTime(lic.expires_at)}</DetailRow>
                  ) : null}
                  {lic.grace_until ? (
                    <DetailRow label={t('adminSystem.license.graceUntil')}>{formatDateTime(lic.grace_until)}</DetailRow>
                  ) : null}
                  <DetailRow label={t('adminSystem.license.instanceId')}>
                    <code>{data.instance_id}</code>
                  </DetailRow>
                  <DetailRow label={t('adminSystem.license.source')}>
                    {lic.source === 'file'
                      ? t('adminSystem.license.sourceFile', { name: data.file ?? '' })
                      : lic.source === 'database'
                        ? t('adminSystem.license.sourceDatabase')
                        : t('adminSystem.license.sourceNone')}
                  </DetailRow>
                </div>

                <div className="stack" style={{ gap: 'var(--janus-space-1)' }}>
                  <div style={{ fontWeight: 500 }}>{t('adminSystem.license.features')}</div>
                  {lic.features.length === 0 ? (
                    <p className="muted small">{t('adminSystem.license.noFeatures')}</p>
                  ) : (
                    <ul className="list-plain small">
                      {lic.features.map((f) => (
                        <li key={f}>{FEATURE_LABELS[f] ?? f}</li>
                      ))}
                    </ul>
                  )}
                  <a className="small" href={data.portal_url} target="_blank" rel="noreferrer">
                    {t('adminSystem.license.portal')}
                  </a>
                  <div style={{ fontWeight: 500, marginTop: 'var(--janus-space-3)' }}>
                    {t('adminSystem.license.updatesTitle')}
                  </div>
                  <UpdateStatus update={data.update} />
                </div>
              </div>

              <LicenseSyncControls />

              <form
                className="stack"
                onSubmit={(event) => {
                  event.preventDefault();
                  if (key.trim()) install.mutate(key);
                }}
              >
                <h3 style={{ margin: 0 }}>{t('adminSystem.license.installTitle')}</h3>
                {data.file && lic.source === 'file' ? <p className="muted small">{t('adminSystem.license.fileWins')}</p> : null}
                <Field label={t('adminSystem.license.keyLabel')} hint={t('adminSystem.license.installHelp')}>
                  <textarea
                    rows={4}
                    value={key}
                    onChange={(event) => setKey(event.target.value)}
                    spellCheck={false}
                    style={{ fontFamily: 'var(--janus-font-mono)', fontSize: '0.8rem' }}
                    placeholder="JANUS-LICENSE-1...."
                  />
                </Field>
                <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
                  <button type="submit" className="btn btn-primary" disabled={!key.trim() || install.isPending}>
                    {t('adminSystem.license.install')}
                  </button>
                  {lic.installed && lic.source === 'database' ? (
                    <button type="button" className="btn" onClick={() => setConfirmRemove(true)}>
                      {t('adminSystem.license.remove')}
                    </button>
                  ) : null}
                  {!lic.installed ? <span className="muted small">{t('adminSystem.license.getKey')}</span> : null}
                </div>
              </form>

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
