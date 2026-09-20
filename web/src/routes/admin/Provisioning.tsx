import { useEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { api } from '../../lib/api';
import { AsyncSection, Modal, Spinner } from '../../components/ui';
import { IdentityProvidersCard } from './IdentityProvidersCard';
import { DirectoryCard } from './DirectoryCard';

type Config = { base_url: string; enabled: boolean; documentation: string };
type ProvisioningToken = {
  id: string;
  name: string;
  prefix: string;
  created_at: string;
  expires_at: string | null;
  revoked_at: string | null;
  last_used_at: string | null;
};
const endpoint = '/api/v1/admin/scim';
function timestamp(value: string | null | undefined) {
  return !value || value.startsWith('0001-01-01T') ? null : value;
}
function dateLabel(value: string | null, fallback = '—') {
  if (!value) return fallback;
  const date = new Date(value);
  return Number.isFinite(date.getTime()) ? date.toLocaleString() : 'Unknown';
}

export function ProvisioningPage({ embedded = false }: { embedded?: boolean } = {}) {
  const config = useQuery({ queryKey: ['admin', 'scim', 'config'], queryFn: () => api.get<Config>(`${endpoint}/config`) });
  const tokens = useQuery({
    queryKey: ['admin', 'scim', 'tokens'],
    queryFn: async () => {
      const data = await api.get<{ tokens: ProvisioningToken[] | null }>(`${endpoint}/tokens`);
      return {
        tokens: (data.tokens ?? []).map((token) => ({
          ...token,
          expires_at: timestamp(token.expires_at),
          revoked_at: timestamp(token.revoked_at),
          last_used_at: timestamp(token.last_used_at),
        })),
      };
    },
  });
  const [action, setAction] = useState<{ kind: 'revoke' | 'rotate'; token: ProvisioningToken } | null>(null);
  const [notice, setNotice] = useState('');
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => window.clearInterval(timer);
  }, []);
  const [issuing, setIssuing] = useState(false);
  const [name, setName] = useState('');
  const [expiry, setExpiry] = useState('');
  const [secret, setSecret] = useState<string | null>(null);
  const [error, setError] = useState('');
  const [copyStatus, setCopyStatus] = useState('');
  const [busy, setBusy] = useState(false);
  const pending = useRef(false);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  const closeSecret = () => {
    setSecret(null);
    setCopyStatus('');
  };
  async function issue() {
    if (pending.current) return;
    if (!name.trim() || new TextEncoder().encode(name.trim()).length > 200) {
      setError('Enter a token name between 1 and 200 UTF-8 bytes.');
      return;
    }
    const date = new Date(expiry);
    if (!Number.isFinite(date.getTime()) || date.getTime() <= Date.now()) {
      setError('Choose a future expiry date and time.');
      return;
    }
    pending.current = true;
    setBusy(true);
    setError('');
    try {
      // Deliberately not a React Query mutation: never retain secrets in its cache.
      const result = await api.post<{ token: ProvisioningToken; secret: string }>(`${endpoint}/tokens`, {
        name: name.trim(),
        expires_at: date.toISOString(),
      });
      if (!mounted.current) return;
      setSecret(result.secret);
      setIssuing(false);
      setName('');
      setExpiry('');
      void tokens.refetch();
    } catch {
      if (mounted.current)
        setError('Could not issue token. Check the token list before retrying; the request may have completed.');
    } finally {
      pending.current = false;
      if (mounted.current) setBusy(false);
    }
  }
  async function changeToken() {
    if (!action || pending.current) return;
    pending.current = true;
    setBusy(true);
    setError('');
    setNotice('');
    try {
      const path = `${endpoint}/tokens/${encodeURIComponent(action.token.id)}`;
      if (action.kind === 'rotate') {
        const result = await api.post<{ token: ProvisioningToken; secret: string }>(`${path}/rotate`);
        if (mounted.current) {
          setCopyStatus('');
          setSecret(result.secret);
        }
      } else {
        await api.del(path);
        if (mounted.current) setNotice('Token revoked.');
      }
      if (mounted.current) {
        setAction(null);
        void tokens.refetch();
      }
    } catch {
      if (mounted.current) setError('Could not update token. Refresh the list before retrying; the request may have completed.');
    } finally {
      pending.current = false;
      if (mounted.current) setBusy(false);
    }
  }
  async function copySecret() {
    if (!secret) return;
    try {
      await navigator.clipboard.writeText(secret);
      if (mounted.current) setCopyStatus('Copied. Store it securely.');
    } catch {
      if (mounted.current) setCopyStatus('Clipboard unavailable. Select and copy the token manually.');
    }
  }
  return (
    <div className={embedded ? 'stack' : 'page stack'}>
      <header className="page-header">
        <div>
          {embedded ? <h2>Sign-in &amp; provisioning</h2> : <h1 className="page-title">Sign-in &amp; provisioning</h1>}
          <p className="page-subtitle">How people get in: additional identity providers, a directory, and SCIM provisioning.</p>
        </div>
      </header>
      <IdentityProvidersCard />

      <DirectoryCard />

      <section className="card stack" aria-labelledby="setup-title">
        <h2 id="setup-title">Set up SCIM provisioning</h2>
        <AsyncSection query={config} skeleton={<Spinner label="Loading provisioning configuration" />}>
          {(data) => (
            <>
              <p>{data.enabled ? 'SCIM endpoint enabled' : 'SCIM endpoint disabled — contact your gateway administrator.'}</p>
              <p>
                SCIM base URL: <code>{data.base_url}</code>
              </p>
            </>
          )}
        </AsyncSection>
        <ol className="stack">
          <li>
            OIDC and direct LDAP sign-in are separate from SCIM provisioning. Configure and test an OIDC provider or the LDAP
            directory above for sign-in. SCIM requires an identity provider with SCIM 2.0 support; an LDAP connection alone does
            not provision SCIM users or groups.
          </li>
          <li>
            Issue a provisioning token below. In your provider, select SCIM 2.0, enter the base URL, and configure bearer
            authentication with that token. Treat it as an administrator credential, not a user API token.
          </li>
          <li>
            Map Users: externalId must match the OIDC sub claim for providers without email_verified. Alternatively, map userName
            to the email returned by OIDC with email_verified=true. Existing users link through the matching identity; new
            provisioned users link on their first matching OIDC sign-in. A matching unverified email alone is not sufficient.
          </li>
          <li>
            Map Groups: send displayName and members referencing SCIM user IDs. Assign users and groups in the provider, then run
            its provisioning sync.
          </li>
          <li>
            <Link to="/teams">Map directory groups to teams</Link> in team details. Memberships reconcile automatically; manual
            team memberships are preserved when directory group membership is removed. No team grants are automatically created.
            Configure team access grants separately.
          </li>
          <li>
            Deactivating a provisioned user blocks access while preserving history. Test provisioning and deactivation with a test
            account before broad rollout.
          </li>
        </ol>
        <p>
          For a one-time import instead, use <Link to="/admin/users">People CSV import and example CSV</Link>.
        </p>
      </section>
      <section className="card stack">
        <div className="row-between">
          <h2>Provisioning tokens</h2>
          <button
            type="button"
            className="btn btn-primary"
            disabled={!config.data?.enabled || busy}
            onClick={() => {
              setError('');
              setIssuing(true);
            }}
          >
            Issue token
          </button>
        </div>
        <p className="small muted">
          Active means the credential has not expired or been revoked, not that the provider is connected. Last used reflects
          actual provisioning traffic.
        </p>
        <button type="button" className="btn" disabled={tokens.isFetching || busy} onClick={() => void tokens.refetch()}>
          Refresh tokens
        </button>
        {tokens.error && tokens.data && (
          <p role="alert">Could not refresh tokens. Displayed data may be stale. Refresh again before making changes.</p>
        )}
        <AsyncSection
          query={tokens}
          skeleton={<Spinner label="Loading provisioning tokens" />}
          empty={{
            when: (data) => data.tokens.length === 0,
            title: 'No provisioning tokens',
            body: 'Issue a token to configure your identity provider.',
          }}
        >
          {(data) => (
            <div className="table-wrap">
              <table className="data">
                <thead>
                  <tr>
                    {['Name / prefix', 'Status', 'Created', 'Expires', 'Revoked', 'Last used', 'Actions'].map((label) => (
                      <th scope="col" key={label}>
                        {label}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {data.tokens.map((token) => {
                    const expired = token.expires_at !== null && Date.parse(token.expires_at) <= now;
                    const status = token.revoked_at
                      ? 'Revoked'
                      : expired
                        ? 'Expired'
                        : token.expires_at && Date.parse(token.expires_at) - now < 7 * 86400000
                          ? 'Expires soon'
                          : 'Active';
                    return (
                      <tr key={token.id}>
                        <td>
                          {token.name}
                          <div className="small muted mono">{token.prefix}…</div>
                        </td>
                        <td>{status}</td>
                        <td>{dateLabel(token.created_at)}</td>
                        <td>{dateLabel(token.expires_at, 'No expiry')}</td>
                        <td>{dateLabel(token.revoked_at)}</td>
                        <td>{dateLabel(token.last_used_at, 'Never used')}</td>
                        <td>
                          <div className="row">
                            <button
                              className="btn btn-sm"
                              type="button"
                              aria-label={`Rotate ${token.name}`}
                              disabled={busy || !!token.revoked_at || expired || !config.data?.enabled}
                              onClick={() => {
                                setError('');
                                setAction({ kind: 'rotate', token });
                              }}
                            >
                              Rotate
                            </button>
                            <button
                              className="btn btn-sm"
                              type="button"
                              aria-label={`Revoke ${token.name}`}
                              disabled={busy || !!token.revoked_at}
                              onClick={() => {
                                setError('');
                                setAction({ kind: 'revoke', token });
                              }}
                            >
                              Revoke
                            </button>
                          </div>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </AsyncSection>
      </section>
      {notice && <p role="status">{notice}</p>}
      {action && (
        <Modal
          open
          onClose={() => {
            if (!busy) {
              setAction(null);
              setError('');
            }
          }}
          title={action.kind === 'rotate' ? 'Rotate provisioning token?' : 'Revoke provisioning token?'}
        >
          <p>
            {action.kind === 'rotate'
              ? `Rotating ${action.token.name} immediately invalidates the old token. Update your provider with the new secret to resume provisioning. Rotation retains the expiry; issue a new token to extend it.`
              : `Revoke ${action.token.name}? Provisioning using this token stops immediately. This cannot be undone.`}
          </p>
          {error && <p role="alert">{error}</p>}
          <div className="row">
            <button
              type="button"
              className="btn"
              disabled={busy}
              onClick={() => {
                setAction(null);
                setError('');
              }}
            >
              Cancel
            </button>
            <button type="button" className="btn btn-danger" disabled={busy} onClick={() => void changeToken()}>
              {busy ? 'Working…' : action.kind === 'rotate' ? 'Rotate token' : 'Revoke token'}
            </button>
          </div>
        </Modal>
      )}
      {issuing && (
        <Modal
          open
          onClose={() => {
            if (!busy) {
              setIssuing(false);
              setError('');
            }
          }}
          title="Issue provisioning token"
        >
          <form
            className="stack"
            noValidate
            onSubmit={(event) => {
              event.preventDefault();
              void issue();
            }}
          >
            <label className="stack">
              Token name
              <input autoComplete="off" value={name} maxLength={200} onChange={(event) => setName(event.target.value)} />
            </label>
            <label className="stack">
              Expires at (local time)
              <input type="datetime-local" value={expiry} onChange={(event) => setExpiry(event.target.value)} />
            </label>
            <p className="small muted">Choose a future expiry and schedule replacement before that time.</p>
            {error && <p role="alert">{error}</p>}
            <button className="btn btn-primary" type="submit" disabled={busy}>
              {busy ? 'Creating…' : 'Create token'}
            </button>
          </form>
        </Modal>
      )}
      {secret !== null && (
        <Modal open onClose={closeSecret} title="Copy your provisioning token">
          <p>Copy now — this secret is shown only once. Closing this dialog clears it; it cannot be retrieved later.</p>
          <label className="stack">
            Provisioning token
            <input readOnly autoComplete="off" spellCheck={false} value={secret} onFocus={(event) => event.target.select()} />
          </label>
          <p>
            Store it in your provider’s secret field or a password manager. Do not include it in URLs, logs, or support messages.
          </p>
          <div className="row">
            <button type="button" className="btn" onClick={() => void copySecret()}>
              Copy token
            </button>
            <button type="button" className="btn btn-primary" onClick={closeSecret}>
              Done
            </button>
          </div>
          {copyStatus && <p role="status">{copyStatus}</p>}
        </Modal>
      )}
    </div>
  );
}
