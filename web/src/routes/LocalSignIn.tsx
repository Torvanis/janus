import { useState, type FormEvent, type ReactNode } from 'react';
import { ApiError, api } from '../lib/api';
import { t } from '../lib/i18n';

/**
 * Local sign-in: email + password, then a TOTP code when the account has
 * one. The server answers JSON (never redirects) so the form can show the
 * exact reason inline; on success the browser navigates to redirect_to.
 * Wrong email and wrong password are the same message on purpose.
 */
export type SignInProvider = { slug: string; name: string; url: string };

export function LocalSignIn({ redirectTo, providers = [] }: { redirectTo: string; providers?: SignInProvider[] }): ReactNode {
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');
  const [pending, setPending] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submitPassword(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const res = await api.post<{ signed_in: boolean; redirect_to?: string; totp_required?: boolean; pending?: string }>(
        '/auth/local/login',
        {
          email,
          password,
          redirect_to: redirectTo,
        },
      );
      if (res.totp_required && res.pending) {
        setPending(res.pending);
      } else if (res.signed_in && res.redirect_to) {
        window.location.assign(res.redirect_to);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function submitCode(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const res = await api.post<{ signed_in: boolean; redirect_to: string }>('/auth/local/totp', {
        pending,
        code,
        redirect_to: redirectTo,
      });
      window.location.assign(res.redirect_to);
    } catch (err) {
      if (err instanceof ApiError && err.code === 'pending_expired') {
        setPending(null);
        setCode('');
      }
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  if (pending) {
    return (
      <form className="stack" onSubmit={(event) => void submitCode(event)} aria-label={t('login.totpTitle')}>
        <p className="secondary" style={{ margin: 0 }}>
          {t('login.totpBody')}
        </p>
        <label className="field">
          <span className="field-label">{t('login.totpCode')}</span>
          <input
            aria-label={t('login.totpCode')}
            className="input"
            inputMode="numeric"
            autoComplete="one-time-code"
            value={code}
            onChange={(event) => setCode(event.target.value)}
            autoFocus
            style={{ fontFamily: 'var(--janus-font-mono)', letterSpacing: '0.15em' }}
          />
          <span className="small muted">{t('login.totpHint')}</span>
        </label>
        {error ? (
          <div className="banner banner-danger" role="alert">
            <span>{error}</span>
          </div>
        ) : null}
        <button type="submit" className="btn btn-primary btn-block" disabled={busy || code.trim().length < 6}>
          {t('login.verify')}
        </button>
        <button type="button" className="btn btn-ghost btn-block" onClick={() => setPending(null)}>
          {t('login.back')}
        </button>
      </form>
    );
  }

  return (
    <form className="stack" onSubmit={(event) => void submitPassword(event)} aria-label={t('login.localTitle')}>
      <label className="field">
        <span className="field-label">{t('login.workEmail')}</span>
        <input
          aria-label={t('login.workEmail')}
          className="input"
          type="email"
          autoComplete="username"
          value={email}
          onChange={(event) => setEmail(event.target.value)}
          placeholder="you@example.com"
          autoFocus
        />
      </label>
      <label className="field">
        <span className="field-label">{t('login.password')}</span>
        <input
          aria-label={t('login.password')}
          className="input"
          type="password"
          autoComplete="current-password"
          value={password}
          onChange={(event) => setPassword(event.target.value)}
        />
      </label>
      {error ? (
        <div className="banner banner-danger" role="alert">
          <span>{error}</span>
        </div>
      ) : null}
      <button type="submit" className="btn btn-primary btn-block" disabled={busy || !email || !password}>
        {t('login.signIn')}
      </button>
      {providers.length > 0 ? (
        <>
          <div className="auth-divider">
            <span>{t('login.or')}</span>
          </div>
          {providers.map((p) => (
            <a key={p.slug || 'sso'} className="btn btn-block" href={p.url}>
              {p.slug ? t('login.continueWith', { name: p.name }) : t('login.continueSso')}
            </a>
          ))}
        </>
      ) : null}
    </form>
  );
}

/**
 * First-run setup: shown only when the gateway has no IdP, no dev auth and
 * no local account yet. Creates the first administrator and signs them in.
 * The endpoint 404s forever after, so there is no second chance to abuse it.
 */
export function FirstRunSetup(): ReactNode {
  const [email, setEmail] = useState('');
  const [name, setName] = useState('');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const mismatch = confirm.length > 0 && confirm !== password;
  const tooShort = password.length > 0 && password.length < 12;

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (mismatch || tooShort) return;
    setBusy(true);
    setError(null);
    try {
      const res = await api.post<{ signed_in: boolean; redirect_to: string }>('/auth/local/setup', { email, name, password });
      window.location.assign(res.redirect_to);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="stack" onSubmit={(event) => void submit(event)} aria-label={t('login.setupTitle')}>
      <div className="banner banner-info">
        <div>
          <strong>{t('login.setupTitle')}</strong>
          <div className="small" style={{ marginTop: 4 }}>
            {t('login.setupBody')}
          </div>
        </div>
      </div>
      <label className="field">
        <span className="field-label">{t('login.workEmail')}</span>
        <input
          aria-label={t('login.workEmail')}
          className="input"
          type="email"
          autoComplete="username"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          autoFocus
        />
      </label>
      <label className="field">
        <span className="field-label">{t('login.name')}</span>
        <input
          aria-label={t('login.name')}
          className="input"
          autoComplete="name"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
      </label>
      <label className="field">
        <span className="field-label">{t('login.password')}</span>
        <input
          aria-label={t('login.password')}
          className="input"
          type="password"
          autoComplete="new-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          aria-invalid={tooShort}
        />
        <span className="small muted">{t('login.passwordHint')}</span>
      </label>
      <label className="field">
        <span className="field-label">{t('login.confirmPassword')}</span>
        <input
          aria-label={t('login.confirmPassword')}
          className="input"
          type="password"
          autoComplete="new-password"
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          aria-invalid={mismatch}
        />
        {mismatch ? (
          <span className="field-error" role="alert">
            {t('login.passwordMismatch')}
          </span>
        ) : null}
      </label>
      {error ? (
        <div className="banner banner-danger" role="alert">
          <span>{error}</span>
        </div>
      ) : null}
      <button type="submit" className="btn btn-primary btn-block" disabled={busy || !email || password.length < 12 || mismatch}>
        {t('login.createAdmin')}
      </button>
    </form>
  );
}
