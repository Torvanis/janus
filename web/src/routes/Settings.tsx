import { useEffect, useState, type ReactNode } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Link, useNavigate, useLocation } from 'react-router-dom';
import { RetainedPanel, RouteTopNav } from '../components/RouteTopNav';
import { api } from '../lib/api';
import { useAppearance, useUnsavedGuard } from '../lib/hooks';
import { ConfirmDialog, Field, useToast } from '../components/ui';
import { useSession } from '../app/session';
import { t } from '../lib/i18n';
import { SecurityCard } from './SecurityCard';

const TIMEZONES = ['', 'UTC', 'Europe/London', 'Europe/Berlin', 'America/New_York', 'America/Los_Angeles', 'Asia/Tokyo'];
const LOCALES = ['', 'en-GB', 'en-US', 'de-DE', 'fr-FR', 'ja-JP'];

export function SettingsPage(): ReactNode {
  const { me, config, refresh } = useSession();
  const location = useLocation();
  const requested = new URLSearchParams(location.search).get('tab');
  const tab = ['account', 'security', 'appearance', 'regional'].includes(requested ?? '') ? requested : 'account';
  const appearance = useAppearance();
  const toast = useToast();
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const [timezone, setTimezone] = useState(me?.timezone ?? '');
  const [locale, setLocale] = useState(me?.locale ?? '');
  const [confirmSignOutAll, setConfirmSignOutAll] = useState(false);

  useEffect(() => {
    setTimezone(me?.timezone ?? '');
    setLocale(me?.locale ?? '');
  }, [me?.timezone, me?.locale]);

  const dirty = timezone !== (me?.timezone ?? '') || locale !== (me?.locale ?? '');
  useUnsavedGuard(dirty);

  const save = useMutation({
    mutationFn: () => api.patch('/api/v1/me/preferences', { timezone, locale }),
    onSuccess: () => {
      toast(t('settings.preferencesSaved'));
      refresh();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const signOutEverywhere = useMutation({
    mutationFn: () => api.post('/api/v1/me/sessions/revoke'),
    onSuccess: () => {
      queryClient.clear();
      navigate('/auth/signed-out?reason=manual', { replace: true });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">Personal settings</h1>
          <p className="page-subtitle">{t('settings.subtitle')}</p>
        </div>
        {me?.role === 'admin' && (
          <Link className="btn" to="/admin/settings/general">
            Gateway settings
          </Link>
        )}
      </header>
      <RouteTopNav
        label="Personal settings"
        active={tab!}
        items={['account', 'security', 'appearance', 'regional'].map((id) => {
          const params = new URLSearchParams(location.search);
          params.set('tab', id);
          return { id, label: id[0]!.toUpperCase() + id.slice(1), to: { pathname: '/settings', search: params.toString() } };
        })}
      />

      <RetainedPanel active={tab === 'appearance'}>
        <section className="card">
          <div className="card-header">
            <h2>{t('settings.appearance')}</h2>
          </div>
          <div className="grid grid-halves">
            <Field label={t('settings.theme')} hint={t('settings.themeHint')}>
              <select
                className="select"
                value={appearance.theme}
                onChange={(event) => appearance.setTheme(event.target.value as typeof appearance.theme)}
              >
                <option value="system">{t('settings.themeSystem')}</option>
                <option value="dark">{t('settings.themeDark')}</option>
                <option value="light">{t('settings.themeLight')}</option>
              </select>
            </Field>

            <Field label={t('settings.motion')} hint={t('settings.motionHint')}>
              <select
                className="select"
                value={appearance.motion}
                onChange={(event) => appearance.setMotion(event.target.value as typeof appearance.motion)}
              >
                <option value="system">{t('settings.motionSystem')}</option>
                <option value="full">{t('settings.motionFull')}</option>
                <option value="reduced">{t('settings.motionReduced')}</option>
              </select>
            </Field>

            <Field label={t('settings.density')} hint={t('settings.densityHint')}>
              <select
                className="select"
                value={appearance.density}
                onChange={(event) => appearance.setDensity(event.target.value as typeof appearance.density)}
              >
                <option value="comfortable">{t('settings.densityComfortable')}</option>
                <option value="compact">{t('settings.densityCompact')}</option>
              </select>
            </Field>
          </div>
        </section>
      </RetainedPanel>

      <RetainedPanel active={tab === 'regional'}>
        <section className="card">
          <div className="card-header">
            <h2>{t('settings.regional')}</h2>
            {dirty ? <span className="small muted">{t('settings.unsavedChanges')}</span> : null}
          </div>
          <div className="grid grid-halves">
            <Field label={t('settings.timeZone')} hint={t('settings.timeZoneHint')}>
              <select className="select" value={timezone} onChange={(event) => setTimezone(event.target.value)}>
                {TIMEZONES.map((zone) => (
                  <option key={zone || 'auto'} value={zone}>
                    {zone || t('settings.automaticBrowser')}
                  </option>
                ))}
              </select>
            </Field>
            <Field label={t('settings.numberDateFormat')} hint={t('settings.localeHint')}>
              <select className="select" value={locale} onChange={(event) => setLocale(event.target.value)}>
                {LOCALES.map((value) => (
                  <option key={value || 'auto'} value={value}>
                    {value || t('settings.automaticBrowser')}
                  </option>
                ))}
              </select>
            </Field>
          </div>
          <div className="row" style={{ marginTop: 'var(--janus-space-4)' }}>
            <button type="button" className="btn btn-primary" onClick={() => save.mutate()} disabled={!dirty || save.isPending}>
              {save.isPending ? t('tables.saving') : t('settings.savePreferences')}
            </button>
            {dirty ? (
              <button
                type="button"
                className="btn"
                onClick={() => {
                  setTimezone(me?.timezone ?? '');
                  setLocale(me?.locale ?? '');
                }}
              >
                {t('settings.discardChanges')}
              </button>
            ) : null}
          </div>
        </section>
      </RetainedPanel>

      <RetainedPanel active={tab === 'security'}>
        <SecurityCard />
      </RetainedPanel>

      <RetainedPanel active={tab === 'account'}>
        <section className="card">
          <div className="card-header">
            <h2>{t('settings.account')}</h2>
          </div>
          <dl className="stack small" style={{ gap: 8 }}>
            <div className="row-between">
              <dt className="muted">{t('settings.signedInAs')}</dt>
              <dd style={{ margin: 0 }}>{me?.email}</dd>
            </div>
            <div className="row-between">
              <dt className="muted">{t('settings.role')}</dt>
              <dd style={{ margin: 0 }}>{me?.role}</dd>
            </div>
            <div className="row-between">
              <dt className="muted">{t('settings.groups')}</dt>
              <dd style={{ margin: 0 }}>{me?.groups.length ? me.groups.join(', ') : t('settings.none')}</dd>
            </div>
            <div className="row-between">
              <dt className="muted">{t('settings.activeSessions')}</dt>
              <dd style={{ margin: 0 }}>{me?.active_sessions ?? 1}</dd>
            </div>
            <div className="row-between">
              <dt className="muted">{t('settings.gatewayBuild')}</dt>
              <dd className="mono" style={{ margin: 0 }}>
                {config?.version} · {config?.build?.slice(0, 7)}
              </dd>
            </div>
          </dl>
          <div className="row" style={{ marginTop: 'var(--janus-space-4)' }}>
            <button type="button" className="btn btn-danger" onClick={() => setConfirmSignOutAll(true)}>
              {t('settings.signOutEverywhere')}
            </button>
          </div>
        </section>
      </RetainedPanel>

      <ConfirmDialog
        open={confirmSignOutAll}
        onClose={() => setConfirmSignOutAll(false)}
        onConfirm={() => signOutEverywhere.mutate()}
        title={t('settings.signOutTitle')}
        consequence={t('settings.signOutConsequence')}
        confirmLabel={t('settings.signOutEverywhere')}
        busy={signOutEverywhere.isPending}
      />
    </div>
  );
}
