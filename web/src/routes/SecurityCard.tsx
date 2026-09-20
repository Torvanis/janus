import { useEffect, useState, type FormEvent, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useSearchParams } from 'react-router-dom';
import { api } from '../lib/api';
import { Badge, ConfirmDialog, Field, useToast } from '../components/ui';
import { t } from '../lib/i18n';

interface LocalStatus {
  has_password: boolean;
  totp_enabled: boolean;
  must_change: boolean;
  recovery_codes_left: number;
}

/**
 * Settings → Sign-in security. Password change (or add — an SSO user can
 * add a password as a break-glass), TOTP enrollment with QR + recovery
 * codes, TOTP removal. `?password=change` (set by a temporary password)
 * opens the password form with a notice and no "current password" field.
 */
export function SecurityCard(): ReactNode {
  const [params, setParams] = useSearchParams();
  const forced = params.get('password') === 'change';
  const queryClient = useQueryClient();
  const toast = useToast();
  const status = useQuery({ queryKey: ['me', 'local'], queryFn: () => api.get<LocalStatus>('/api/v1/me/local') });

  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const [showPassword, setShowPassword] = useState(forced);
  useEffect(() => {
    if (forced) setShowPassword(true);
  }, [forced]);

  const change = useMutation({
    mutationFn: () =>
      api.put<{ changed: boolean }>('/api/v1/me/local/password', { current_password: current, new_password: next }),
    onSuccess: () => {
      toast(t('security.passwordChanged'));
      setCurrent('');
      setNext('');
      setConfirm('');
      setShowPassword(false);
      if (forced) setParams({});
      void queryClient.invalidateQueries({ queryKey: ['me'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  // TOTP enrollment
  const [enroll, setEnroll] = useState<{ secret: string; otpauth_url: string } | null>(null);
  const [code, setCode] = useState('');
  const [recovery, setRecovery] = useState<string[] | null>(null);
  const start = useMutation({
    mutationFn: () => api.post<{ secret: string; otpauth_url: string }>('/api/v1/me/local/totp/start'),
    onSuccess: (data) => setEnroll(data),
    onError: (error: Error) => toast(error.message, 'danger'),
  });
  const confirmTotp = useMutation({
    mutationFn: () => api.post<{ enabled: boolean; recovery_codes: string[] }>('/api/v1/me/local/totp/confirm', { code }),
    onSuccess: (data) => {
      setEnroll(null);
      setCode('');
      setRecovery(data.recovery_codes);
      void queryClient.invalidateQueries({ queryKey: ['me', 'local'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });
  const [disableOpen, setDisableOpen] = useState(false);
  const [disablePassword, setDisablePassword] = useState('');
  const disable = useMutation({
    mutationFn: () => api.post<{ enabled: boolean }>('/api/v1/me/local/totp/disable', { password: disablePassword }),
    onSuccess: () => {
      toast(t('security.totpDisabled'));
      setDisableOpen(false);
      setDisablePassword('');
      void queryClient.invalidateQueries({ queryKey: ['me', 'local'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const s = status.data;
  const mismatch = confirm.length > 0 && confirm !== next;
  const canSubmit = next.length >= 12 && !mismatch && (forced || !s?.has_password || current.length > 0);

  function submitPassword(event: FormEvent) {
    event.preventDefault();
    if (canSubmit) change.mutate();
  }

  return (
    <section className="card" aria-labelledby="security-heading">
      <div className="card-header">
        <h2 id="security-heading">{t('security.title')}</h2>
        {s ? (
          <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
            <Badge tone={s.has_password ? 'success' : 'neutral'}>
              {s.has_password ? t('security.passwordSet') : t('security.noPassword')}
            </Badge>
            <Badge tone={s.totp_enabled ? 'success' : 'neutral'}>
              {s.totp_enabled ? t('security.totpOn') : t('security.totpOff')}
            </Badge>
          </div>
        ) : null}
      </div>

      {forced ? (
        <div className="banner banner-warning" role="alert">
          <span>{t('security.mustChange')}</span>
        </div>
      ) : null}

      <div className="stack">
        {/* Password */}
        {showPassword ? (
          <form className="stack" onSubmit={submitPassword}>
            {s?.has_password && !forced ? (
              <Field label={t('security.currentPassword')}>
                <input
                  aria-label={t('security.currentPassword')}
                  className="input"
                  type="password"
                  autoComplete="current-password"
                  value={current}
                  onChange={(e) => setCurrent(e.target.value)}
                />
              </Field>
            ) : null}
            <Field label={t('security.newPassword')} hint={t('login.passwordHint')}>
              <input
                aria-label={t('security.newPassword')}
                className="input"
                type="password"
                autoComplete="new-password"
                value={next}
                onChange={(e) => setNext(e.target.value)}
              />
            </Field>
            <Field label={t('login.confirmPassword')} error={mismatch ? t('login.passwordMismatch') : undefined}>
              <input
                aria-label={t('login.confirmPassword')}
                className="input"
                type="password"
                autoComplete="new-password"
                value={confirm}
                onChange={(e) => setConfirm(e.target.value)}
              />
            </Field>
            <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
              <button type="submit" className="btn btn-primary" disabled={!canSubmit || change.isPending}>
                {s?.has_password ? t('security.changePassword') : t('security.setPassword')}
              </button>
              {!forced ? (
                <button type="button" className="btn btn-ghost" onClick={() => setShowPassword(false)}>
                  {t('common.cancel')}
                </button>
              ) : null}
            </div>
            <p className="muted small">{t('security.passwordNote')}</p>
          </form>
        ) : (
          <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
            <button type="button" className="btn" onClick={() => setShowPassword(true)}>
              {s?.has_password ? t('security.changePassword') : t('security.setPassword')}
            </button>
            {!s?.has_password ? <span className="muted small">{t('security.breakGlass')}</span> : null}
          </div>
        )}

        {/* TOTP */}
        {s?.has_password ? (
          <div className="stack" style={{ borderTop: '1px solid var(--janus-color-border)', paddingTop: 'var(--janus-space-4)' }}>
            <h3 style={{ margin: 0 }}>{t('security.totpTitle')}</h3>
            {recovery ? (
              <div className="banner banner-info">
                <div className="stack" style={{ gap: 'var(--janus-space-2)' }}>
                  <strong>{t('security.recoveryTitle')}</strong>
                  <span className="small">{t('security.recoveryBody')}</span>
                  <pre className="mono" style={{ margin: 0, columns: 2 }}>
                    {recovery.join('\n')}
                  </pre>
                  <div>
                    <button type="button" className="btn btn-sm" onClick={() => setRecovery(null)}>
                      {t('security.recoverySaved')}
                    </button>
                  </div>
                </div>
              </div>
            ) : null}
            {enroll ? (
              <div className="stack">
                <p className="small" style={{ margin: 0 }}>
                  {t('security.enrollBody')}
                </p>
                <div className="row" style={{ gap: 'var(--janus-space-4)', alignItems: 'flex-start', flexWrap: 'wrap' }}>
                  <img
                    alt={t('security.qrAlt')}
                    width={168}
                    height={168}
                    style={{ background: '#fff', padding: 8, borderRadius: 8 }}
                    src={`/api/v1/me/local/totp/qr.png?v=${encodeURIComponent(enroll.secret.slice(0, 6))}`}
                  />
                  <div className="stack small" style={{ gap: 'var(--janus-space-1)' }}>
                    <span className="muted">{t('security.manualKey')}</span>
                    <code style={{ wordBreak: 'break-all' }}>{enroll.secret}</code>
                  </div>
                </div>
                <Field label={t('login.totpCode')}>
                  <input
                    aria-label={t('login.totpCode')}
                    className="input"
                    inputMode="numeric"
                    autoComplete="one-time-code"
                    value={code}
                    onChange={(e) => setCode(e.target.value)}
                    style={{ maxWidth: 200, fontFamily: 'var(--janus-font-mono)', letterSpacing: '0.15em' }}
                  />
                </Field>
                <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
                  <button
                    type="button"
                    className="btn btn-primary"
                    disabled={code.trim().length < 6 || confirmTotp.isPending}
                    onClick={() => confirmTotp.mutate()}
                  >
                    {t('security.enable')}
                  </button>
                  <button type="button" className="btn btn-ghost" onClick={() => setEnroll(null)}>
                    {t('common.cancel')}
                  </button>
                </div>
              </div>
            ) : s.totp_enabled ? (
              <div className="row" style={{ gap: 'var(--janus-space-3)', alignItems: 'center', flexWrap: 'wrap' }}>
                <span className="small">{t('security.totpEnabledBody', { count: s.recovery_codes_left })}</span>
                <button type="button" className="btn btn-sm" onClick={() => setDisableOpen(true)}>
                  {t('security.disable')}
                </button>
              </div>
            ) : (
              <div className="row" style={{ gap: 'var(--janus-space-3)', alignItems: 'center', flexWrap: 'wrap' }}>
                <span className="small muted">{t('security.totpOffBody')}</span>
                <button type="button" className="btn btn-sm" onClick={() => start.mutate()} disabled={start.isPending}>
                  {t('security.setUp')}
                </button>
              </div>
            )}
          </div>
        ) : null}
      </div>

      <ConfirmDialog
        open={disableOpen}
        onClose={() => setDisableOpen(false)}
        onConfirm={() => disable.mutate()}
        title={t('security.disableTitle')}
        consequence={t('security.disableBody')}
        confirmLabel={t('security.disable')}
        busy={disable.isPending}
        danger
      >
        <Field label={t('security.password')}>
          <input
            aria-label={t('security.password')}
            className="input"
            type="password"
            autoComplete="current-password"
            value={disablePassword}
            onChange={(e) => setDisablePassword(e.target.value)}
          />
        </Field>
      </ConfirmDialog>
    </section>
  );
}
