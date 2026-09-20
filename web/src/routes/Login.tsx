import { useState, type ReactNode } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import { usePublicConfig } from '../app/session';
import { useAppearance } from '../lib/hooks';
import { AmbientCanvas } from '../components/AmbientCanvas';
import { Spinner } from '../components/ui';
import { t } from '../lib/i18n';
import { FirstRunSetup, LocalSignIn, type SignInProvider } from './LocalSignIn';
import './login.css';

interface AuthHealth {
  status: 'ok' | 'unavailable';
  provider: string;
  dev_auth: boolean;
  local_auth?: boolean;
  needs_setup?: boolean;
  detail?: string;
}

export function LoginPage(): ReactNode {
  const [params] = useSearchParams();
  const appearance = useAppearance();
  const config = usePublicConfig();
  const redirectTo = params.get('redirect_uri') ?? '/dashboard';
  const errorMessage = params.get('error');

  const health = useQuery({
    queryKey: ['auth-health'],
    queryFn: () => api.get<AuthHealth>('/auth/health'),
    retry: 1,
  });

  const providerDown = health.data?.status === 'unavailable' || (health.isError && !health.data);
  const devAuth = health.data?.dev_auth ?? config.data?.dev_auth ?? false;
  const localAuth = health.data?.local_auth ?? false;
  const needsSetup = health.data?.needs_setup ?? false;
  const providers = useQuery({
    queryKey: ['auth-providers', redirectTo],
    queryFn: () => api.get<{ providers: SignInProvider[] }>(`/auth/providers?redirect_uri=${encodeURIComponent(redirectTo)}`),
    retry: 1,
  });
  const providerList = providers.data?.providers ?? [];

  return (
    <div className="auth-page">
      <AmbientCanvas
        signal={{
          tokensPerMinute: 4000,
          tokensInPerMinute: 2500,
          tokensOutPerMinute: 1500,
          requestsPerMinute: 30,
          costPerMinute: 0.4,
          errorRate: 0,
          // Signed-out preview: no live data, so no shooting stars.
          recentJobs: [],
        }}
        reducedMotion={appearance.reducedMotion}
      />
      <main className="auth-card">
        <div className="auth-brand">
          <svg viewBox="0 0 32 32" width="46" height="46" aria-hidden="true">
            <defs>
              <linearGradient id="janus-login-mark" x1="0" y1="0" x2="1" y2="1">
                <stop offset="0%" stopColor="#7c86ff" />
                <stop offset="100%" stopColor="#fbbf24" />
              </linearGradient>
            </defs>
            <circle cx="16" cy="16" r="14" fill="none" stroke="url(#janus-login-mark)" strokeWidth="2" />
            <path d="M16 4v24" stroke="url(#janus-login-mark)" strokeWidth="1.5" opacity="0.5" />
            <path d="M11 11c-2.5 2-2.5 8 0 10" fill="none" stroke="#7c86ff" strokeWidth="2" strokeLinecap="round" />
            <path d="M21 11c2.5 2 2.5 8 0 10" fill="none" stroke="#fbbf24" strokeWidth="2" strokeLinecap="round" />
          </svg>
          <h1>Janus</h1>
          <p className="secondary">{t('login.tagline')}</p>
        </div>

        {errorMessage ? (
          <div className="banner banner-danger" role="alert">
            <span>{errorMessage}</span>
          </div>
        ) : null}

        {health.isLoading ? (
          <div className="row" style={{ justifyContent: 'center', padding: 'var(--janus-space-4)' }}>
            <Spinner label={t('login.checkingProvider')} />
          </div>
        ) : needsSetup ? (
          <FirstRunSetup />
        ) : providerDown && localAuth ? (
          <div className="stack">
            <div className="banner banner-warning" role="alert">
              <div>
                <strong>{t('login.unavailableTitle')}</strong>
                <div className="small" style={{ marginTop: 4 }}>
                  {t('login.unavailableLocalBody')}
                </div>
              </div>
            </div>
            <LocalSignIn redirectTo={redirectTo} />
          </div>
        ) : providerDown ? (
          <div className="stack">
            <div className="banner banner-warning" role="alert">
              <div>
                <strong>{t('login.unavailableTitle')}</strong>
                <div className="small" style={{ marginTop: 4 }}>
                  {t('login.unavailableBody')}
                </div>
              </div>
            </div>
            <button type="button" className="btn btn-block" onClick={() => void health.refetch()}>
              {t('login.retry')}
            </button>
          </div>
        ) : devAuth ? (
          <DevSignIn redirectTo={redirectTo} />
        ) : localAuth ? (
          <LocalSignIn redirectTo={redirectTo} providers={providerList} />
        ) : (
          <div className="stack">
            {(providerList.length > 0
              ? providerList
              : [{ slug: '', name: '', url: `/auth/start?redirect_uri=${encodeURIComponent(redirectTo)}` }]
            ).map((p, i) => (
              <a key={p.slug || 'sso'} className={`btn btn-block ${i === 0 ? 'btn-primary' : ''}`} href={p.url}>
                {p.slug ? t('login.continueWith', { name: p.name }) : t('login.continueSso')}
              </a>
            ))}
          </div>
        )}

        <p className="small muted auth-provider">
          {providerDown ? t('login.providerUnreachable') : t('login.providerLabel', { name: health.data?.provider ?? '—' })}
        </p>

        <div className="auth-footer">
          <Link to="/docs/getting-started">{t('login.readQuickStart')}</Link>
          <span aria-hidden="true">·</span>
          <Link to="/docs">{t('login.documentation')}</Link>
        </div>
      </main>
    </div>
  );
}

/**
 * Local sign-in for evaluation deployments that have no identity provider yet
 * (JANUS_DEV_AUTH=true). The gateway refuses to enable this in production
 * configurations, and the banner makes the mode unmistakable.
 */
function DevSignIn({ redirectTo }: { redirectTo: string }): ReactNode {
  const [email, setEmail] = useState('');
  const [touched, setTouched] = useState(false);
  const valid = /.+@.+\..+/.test(email);

  return (
    <form
      className="stack"
      action="/auth/start"
      method="get"
      onSubmit={(event) => {
        if (!valid) {
          event.preventDefault();
          setTouched(true);
        }
      }}
    >
      <div className="banner banner-warning">
        <div>
          <strong>{t('login.devBannerTitle')}</strong>
          <div className="small" style={{ marginTop: 4 }}>
            {t('login.devBannerBody')}
          </div>
        </div>
      </div>
      <label className="field">
        <span className="field-label">{t('login.workEmail')}</span>
        <input
          className="input"
          name="email"
          type="email"
          value={email}
          placeholder="you@example.com"
          onChange={(event) => setEmail(event.target.value)}
          onBlur={() => setTouched(true)}
          aria-invalid={touched && !valid}
          autoComplete="email"
          autoFocus
        />
        {touched && !valid ? (
          <span className="field-error" role="alert">
            {t('login.emailError')}
          </span>
        ) : null}
      </label>
      <input type="hidden" name="redirect_uri" value={redirectTo} />
      <button type="submit" className="btn btn-primary btn-block" disabled={!valid}>
        {t('login.signIn')}
      </button>
    </form>
  );
}

export function SignedOutPage(): ReactNode {
  const [params] = useSearchParams();
  const appearance = useAppearance();
  const reason = params.get('reason');

  const message =
    reason === 'expired' ? t('login.signedOutExpired') : reason === 'idle' ? t('login.signedOutIdle') : t('login.signedOut');

  return (
    <div className="auth-page">
      <AmbientCanvas
        signal={{
          tokensPerMinute: 800,
          tokensInPerMinute: 500,
          tokensOutPerMinute: 300,
          requestsPerMinute: 6,
          costPerMinute: 0.05,
          errorRate: 0,
          // Signed-out preview: no live data, so no shooting stars.
          recentJobs: [],
        }}
        reducedMotion={appearance.reducedMotion}
      />
      <main className="auth-card">
        <div className="auth-brand">
          <h1>{t('login.signedOutTitle')}</h1>
          <p className="secondary">{message}</p>
        </div>
        <Link className="btn btn-primary btn-block" to="/auth/login">
          {t('login.signBackIn')}
        </Link>
        <div className="auth-footer">
          <Link to="/docs">{t('login.documentation')}</Link>
        </div>
      </main>
    </div>
  );
}
