import { useEffect, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { AsyncSection, Badge, ConfirmDialog, Field, useToast } from '../../components/ui';
import { formatDateTime } from '../../lib/format';

/**
 * Administration → Settings → Security: sessions, the sign-in notice,
 * lockout, passwords, audit forwarding (syslog / SIEM) and smart-card
 * (CAC/PIV) sign-in. One policy document, saved together.
 */
export interface SecurityPolicy {
  sessions: { idle_minutes_user: number; idle_minutes_admin: number; absolute_minutes: number; max_per_user: number };
  banner: { enabled: boolean; title: string; text: string; button: string };
  lockout: { max_failures: number; window_minutes: number; lock_minutes: number };
  passwords: {
    min_length: number;
    require_upper: boolean;
    require_lower: boolean;
    require_digit: boolean;
    require_special: boolean;
    min_changed_chars: number;
    history: number;
    min_age_hours: number;
    max_age_days: number;
  };
  syslog: {
    enabled: boolean;
    protocol: 'udp' | 'tcp' | 'tls';
    address: string;
    format: 'rfc5424' | 'cef';
    facility: number;
    app_name: string;
    server_name: string;
    client_cert_file: string;
    client_key_file: string;
  };
  smart_card: {
    enabled: boolean;
    source: 'direct' | 'proxy';
    header: string;
    header_format: 'xfcc' | 'pem' | 'base64';
    match: ('upn' | 'email' | 'edipi')[];
    provision: boolean;
    require_for_admins: boolean;
  };
}

interface SyslogStatus {
  enabled: boolean;
  destination?: string;
  sent: number;
  dropped: number;
  failed: number;
  queued: number;
  last_sent_at?: string;
  last_error?: string;
  last_error_at?: string;
}

export interface Baseline {
  id: string;
  name: string;
  summary: string;
  values: SecurityPolicy;
  gaps: string[];
}

export interface SecurityDoc {
  policy: SecurityPolicy;
  effective: SecurityPolicy;
  defaults: SecurityPolicy;
  stig_values: SecurityPolicy;
  stig_profile: boolean;
  stig_gaps: string[];
  dod_banner: string;
  baselines?: Baseline[];
  limits: { max_absolute_minutes: number; max_idle_minutes: number };
  smart_card_direct_available: boolean;
  syslog_status?: SyslogStatus;
}

interface Lockout {
  login: string;
  failed: number;
  locked_at: string;
  locked_until?: string;
  until_unlocked: boolean;
}

const MATCH_LABEL: Record<string, string> = {
  upn: 'User principal name (UPN)',
  email: 'Email on the card',
  edipi: 'DoD ID number (EDIPI)',
};

export function SecuritySettingsPage(): ReactNode {
  const query = useQuery({
    queryKey: ['admin', 'security'],
    queryFn: () => api.get<SecurityDoc>('/api/v1/admin/security/policy'),
  });
  return <AsyncSection query={query}>{(doc) => <SecurityForm doc={doc} />}</AsyncSection>;
}

function num(v: string): number {
  const n = Number.parseInt(v, 10);
  return Number.isFinite(n) ? n : 0;
}

function SecurityForm({ doc }: { doc: SecurityDoc }): ReactNode {
  const toast = useToast();
  const client = useQueryClient();
  const [draft, setDraft] = useState<SecurityPolicy>(doc.policy);
  const [confirmBaseline, setConfirmBaseline] = useState(false);
  // Older servers send only the STIG; show it as the one baseline.
  const stigBaseline: Baseline = {
    id: 'dod-stig',
    name: 'DoD STIG (defense)',
    summary: '',
    values: doc.stig_values,
    gaps: doc.stig_gaps,
  };
  const baselines: Baseline[] = doc.baselines?.length ? doc.baselines : [stigBaseline];
  const [baselineId, setBaselineId] = useState(doc.stig_profile ? 'dod-stig' : (baselines[0]?.id ?? 'dod-stig'));
  const baseline = baselines.find((b) => b.id === baselineId) ?? baselines[0] ?? stigBaseline;
  useEffect(() => setDraft(doc.policy), [doc.policy]);
  const dirty = JSON.stringify(draft) !== JSON.stringify(doc.policy);

  const save = useMutation({
    mutationFn: (p: SecurityPolicy) => api.put<SecurityDoc>('/api/v1/admin/security/policy', p),
    onSuccess: (next) => {
      client.setQueryData(['admin', 'security'], next);
      void client.invalidateQueries({ queryKey: ['me'] });
      toast('Security settings saved. Other replicas apply them within 10 seconds.');
    },
    onError: (e: Error) => toast(e.message, 'danger'),
  });

  const set = <K extends keyof SecurityPolicy>(section: K, patch: Partial<SecurityPolicy[K]>) =>
    setDraft((d) => ({ ...d, [section]: { ...d[section], ...patch } }));
  const gaps = doc.stig_profile ? [] : baseline.gaps;

  return (
    <form
      className="stack"
      style={{ gap: 'var(--janus-space-5)' }}
      aria-label="Security settings"
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate(draft);
      }}
    >
      <section className="card stack" aria-labelledby="sec-baseline">
        <div className="row-between wrap" style={{ gap: 'var(--janus-space-3)' }}>
          <h2 id="sec-baseline" style={{ margin: 0 }}>
            Baseline
          </h2>
          {doc.stig_profile ? (
            <Badge tone="success">STIG profile on</Badge>
          ) : gaps.length === 0 ? (
            <Badge tone="success">Meets {baseline.name.replace(/ \(.*\)$/, '')}</Badge>
          ) : (
            <Badge tone="neutral">
              {gaps.length} below {baseline.name.replace(/ \(.*\)$/, '')}
            </Badge>
          )}
        </div>
        {doc.stig_profile ? (
          <p className="small muted" style={{ margin: 0 }}>
            The STIG profile is on (JANUS_STIG_PROFILE), so nothing here can be set weaker than the Application Security and
            Development STIG. Stricter values are fine.
          </p>
        ) : (
          <>
            <p className="small muted" style={{ margin: 0 }}>
              Defaults keep Janus as it was. To meet a published standard, start from its baseline, review the values, then save.
            </p>
            {gaps.length > 0 ? (
              <details>
                <summary className="small">What differs from {baseline.name.replace(/ \(.*\)$/, '')}</summary>
                <ul className="small" style={{ margin: 'var(--janus-space-2) 0 0' }}>
                  {gaps.map((g) => (
                    <li key={g}>{g}</li>
                  ))}
                </ul>
              </details>
            ) : null}
          </>
        )}
        <div className="row wrap" style={{ alignItems: 'flex-end' }}>
          <label className="stack" style={{ gap: 'var(--janus-space-1)', minWidth: 0, flex: '1 1 16rem', maxWidth: '24rem' }}>
            <span className="small">Start from</span>
            <select
              className="select"
              value={baseline.id}
              title={baseline.summary || undefined}
              onChange={(e) => setBaselineId(e.target.value)}
              disabled={doc.stig_profile}
            >
              {baselines.map((b) => (
                <option key={b.id} value={b.id} title={b.summary || undefined}>
                  {b.name}
                </option>
              ))}
            </select>
          </label>
          <button
            type="button"
            className="btn btn-sm"
            title={baseline.summary || undefined}
            onClick={() => setConfirmBaseline(true)}
          >
            Apply baseline
          </button>
          <button
            type="button"
            className="btn btn-sm btn-ghost"
            onClick={() => setDraft(doc.defaults)}
            disabled={doc.stig_profile}
          >
            Reset to defaults
          </button>
        </div>
        {baseline.summary ? (
          <p className="small muted" style={{ margin: 0 }}>
            {baseline.summary}
          </p>
        ) : null}
      </section>

      <section className="card stack" aria-labelledby="sec-sessions">
        <h2 id="sec-sessions" style={{ margin: 0 }}>
          Sessions
        </h2>
        <div className="grid grid-tiles">
          <Field label="Idle sign-out, people (minutes)">
            <input
              className="input"
              type="number"
              min={1}
              max={doc.limits.max_idle_minutes}
              value={draft.sessions.idle_minutes_user}
              onChange={(e) => set('sessions', { idle_minutes_user: num(e.target.value) })}
            />
          </Field>
          <Field label="Idle sign-out, administrators (minutes)">
            <input
              className="input"
              type="number"
              min={1}
              max={doc.limits.max_idle_minutes}
              value={draft.sessions.idle_minutes_admin}
              onChange={(e) => set('sessions', { idle_minutes_admin: num(e.target.value) })}
            />
          </Field>
          <Field label="Longest session (minutes)" hint="Sign in again after this, active or not.">
            <input
              className="input"
              type="number"
              min={5}
              max={doc.limits.max_absolute_minutes}
              value={draft.sessions.absolute_minutes}
              onChange={(e) => set('sessions', { absolute_minutes: num(e.target.value) })}
            />
          </Field>
          <Field label="Sessions per person" hint="0 = no limit. At the limit, a new sign-in ends the oldest.">
            <input
              className="input"
              type="number"
              min={0}
              max={100}
              value={draft.sessions.max_per_user}
              onChange={(e) => set('sessions', { max_per_user: num(e.target.value) })}
            />
          </Field>
        </div>
        <p className="small muted" style={{ margin: 0 }}>
          The deployment caps these at {doc.limits.max_idle_minutes} minutes idle and {doc.limits.max_absolute_minutes} minutes in
          total (JANUS_SESSION_IDLE_TIMEOUT_HOURS, JANUS_SESSION_TTL_HOURS). Background refreshes do not count as activity; people
          get a minute&rsquo;s warning before an idle sign-out.
        </p>
      </section>

      <section className="card stack" aria-labelledby="sec-banner">
        <h2 id="sec-banner" style={{ margin: 0 }}>
          Sign-in notice
        </h2>
        <label className="switch">
          <input
            type="checkbox"
            checked={draft.banner.enabled}
            onChange={(e) => set('banner', { enabled: e.target.checked })}
            aria-label="Show a notice before sign-in"
          />
          <span>
            <strong>Show a notice before sign-in</strong>
            <span className="small muted" style={{ display: 'block' }}>
              Everyone must acknowledge it before any sign-in method is offered. Each acknowledgment is audited.
            </span>
          </span>
        </label>
        <div className="grid grid-halves">
          <Field label="Title">
            <input
              className="input"
              value={draft.banner.title}
              maxLength={200}
              onChange={(e) => set('banner', { title: e.target.value })}
            />
          </Field>
          <Field label="Button">
            <input
              className="input"
              value={draft.banner.button}
              maxLength={60}
              onChange={(e) => set('banner', { button: e.target.value })}
            />
          </Field>
        </div>
        <Field label="Text" hint="Plain text; blank lines start new paragraphs.">
          <textarea
            className="input"
            rows={8}
            value={draft.banner.text}
            maxLength={20000}
            onChange={(e) => set('banner', { text: e.target.value })}
          />
        </Field>
        <div className="row wrap">
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => set('banner', { text: doc.dod_banner, title: 'U.S. Government Information System' })}
          >
            Use the DoD Notice and Consent text
          </button>
        </div>
      </section>

      <section className="card stack" aria-labelledby="sec-lockout">
        <h2 id="sec-lockout" style={{ margin: 0 }}>
          Lockout
        </h2>
        <div className="grid grid-tiles">
          <Field label="Failed attempts">
            <input
              className="input"
              type="number"
              min={1}
              max={100}
              value={draft.lockout.max_failures}
              onChange={(e) => set('lockout', { max_failures: num(e.target.value) })}
            />
          </Field>
          <Field label="Within (minutes)">
            <input
              className="input"
              type="number"
              min={1}
              max={1440}
              value={draft.lockout.window_minutes}
              onChange={(e) => set('lockout', { window_minutes: num(e.target.value) })}
            />
          </Field>
          <Field label="Locked for (minutes)" hint="0 = until an administrator unlocks it.">
            <input
              className="input"
              type="number"
              min={0}
              max={10080}
              value={draft.lockout.lock_minutes}
              onChange={(e) => set('lockout', { lock_minutes: num(e.target.value) })}
            />
          </Field>
        </div>
        <p className="small muted" style={{ margin: 0 }}>
          Applies to Janus passwords and directory (LDAP) sign-in. Identity providers and smart cards enforce their own.
        </p>
        <LockoutList />
      </section>

      <section className="card stack" aria-labelledby="sec-passwords">
        <h2 id="sec-passwords" style={{ margin: 0 }}>
          Passwords
        </h2>
        <p className="small muted" style={{ margin: 0 }}>
          For Janus accounts. New rules apply at the next password change; a password past the maximum age must be changed at the
          next sign-in.
        </p>
        <div className="grid grid-tiles">
          <Field label="Minimum length">
            <input
              className="input"
              type="number"
              min={8}
              max={128}
              value={draft.passwords.min_length}
              onChange={(e) => set('passwords', { min_length: num(e.target.value) })}
            />
          </Field>
          <Field label="Characters changed" hint="Versus the current password. 0 = off.">
            <input
              className="input"
              type="number"
              min={0}
              max={draft.passwords.min_length}
              value={draft.passwords.min_changed_chars}
              onChange={(e) => set('passwords', { min_changed_chars: num(e.target.value) })}
            />
          </Field>
          <Field label="Remembered passwords" hint="Cannot be reused. 0 = off.">
            <input
              className="input"
              type="number"
              min={0}
              max={24}
              value={draft.passwords.history}
              onChange={(e) => set('passwords', { history: num(e.target.value) })}
            />
          </Field>
          <Field label="Minimum age (hours)" hint="0 = off. Administrator resets are exempt.">
            <input
              className="input"
              type="number"
              min={0}
              max={720}
              value={draft.passwords.min_age_hours}
              onChange={(e) => set('passwords', { min_age_hours: num(e.target.value) })}
            />
          </Field>
          <Field label="Maximum age (days)" hint="0 = never expires.">
            <input
              className="input"
              type="number"
              min={0}
              max={3650}
              value={draft.passwords.max_age_days}
              onChange={(e) => set('passwords', { max_age_days: num(e.target.value) })}
            />
          </Field>
        </div>
        <fieldset className="stack" style={{ border: 0, padding: 0, margin: 0, gap: 'var(--janus-space-2)' }}>
          <legend className="field-label">Must include</legend>
          {(
            [
              ['require_upper', 'An upper-case letter'],
              ['require_lower', 'A lower-case letter'],
              ['require_digit', 'A digit'],
              ['require_special', 'A special character'],
            ] as const
          ).map(([k, label]) => (
            <label key={k} className="row" style={{ gap: 'var(--janus-space-2)' }}>
              <input type="checkbox" checked={draft.passwords[k]} onChange={(e) => set('passwords', { [k]: e.target.checked })} />
              <span>{label}</span>
            </label>
          ))}
        </fieldset>
      </section>

      <SyslogSection draft={draft} set={set} status={doc.syslog_status} />

      <section className="card stack" aria-labelledby="sec-card">
        <h2 id="sec-card" style={{ margin: 0 }}>
          Smart card (CAC/PIV)
        </h2>
        <label className="switch">
          <input
            type="checkbox"
            checked={draft.smart_card.enabled}
            onChange={(e) => set('smart_card', { enabled: e.target.checked })}
            aria-label="Offer smart-card sign-in"
          />
          <span>
            <strong>Offer smart-card sign-in</strong>
            <span className="small muted" style={{ display: 'block' }}>
              The card&rsquo;s certificate must chain to an authority under Settings → Certificates (the DoD roots, or your own).
              The operating system&rsquo;s public roots never count. Revocation follows JANUS_TLS_REVOCATION.
            </span>
          </span>
        </label>
        <div className="grid grid-halves">
          <Field label="Certificate comes from">
            <select
              className="select"
              value={draft.smart_card.source}
              onChange={(e) => set('smart_card', { source: e.target.value as 'direct' | 'proxy' })}
            >
              <option value="direct">Janus (Janus serves TLS and asks the browser)</option>
              <option value="proxy">The proxy in front of Janus (a header)</option>
            </select>
          </Field>
          {draft.smart_card.source === 'proxy' ? (
            <>
              <Field label="Header" hint="Accepted only from JANUS_TRUSTED_PROXIES.">
                <input
                  className="input mono"
                  value={draft.smart_card.header}
                  onChange={(e) => set('smart_card', { header: e.target.value })}
                />
              </Field>
              <Field label="Header format">
                <select
                  className="select"
                  value={draft.smart_card.header_format}
                  onChange={(e) => set('smart_card', { header_format: e.target.value as 'xfcc' | 'pem' | 'base64' })}
                >
                  <option value="xfcc">X-Forwarded-Client-Cert (Envoy, Istio)</option>
                  <option value="pem">URL-escaped PEM (nginx, Traefik)</option>
                  <option value="base64">Base64 DER or PEM</option>
                </select>
              </Field>
            </>
          ) : null}
        </div>
        {draft.smart_card.source === 'direct' && !doc.smart_card_direct_available ? (
          <div className="banner banner-warning" role="note">
            <span className="small">
              Janus is not serving TLS itself, so it cannot ask for the card. Turn on listener TLS, or have the proxy forward the
              certificate.
            </span>
          </div>
        ) : null}
        <fieldset className="stack" style={{ border: 0, padding: 0, margin: 0, gap: 'var(--janus-space-2)' }}>
          <legend className="field-label">Match the card to a person by (in this order)</legend>
          {(['upn', 'email', 'edipi'] as const).map((m) => (
            <label key={m} className="row" style={{ gap: 'var(--janus-space-2)' }}>
              <input
                type="checkbox"
                checked={draft.smart_card.match.includes(m)}
                onChange={(e) =>
                  set('smart_card', {
                    match: e.target.checked
                      ? (['upn', 'email', 'edipi'] as const).filter((x) => x === m || draft.smart_card.match.includes(x))
                      : draft.smart_card.match.filter((x) => x !== m),
                  })
                }
              />
              <span>{MATCH_LABEL[m]}</span>
            </label>
          ))}
          <span className="field-hint">
            A UPN or DoD ID number can also be set on a person (People → the person → Smart card).
          </span>
        </fieldset>
        <label className="row" style={{ gap: 'var(--janus-space-2)' }}>
          <input
            type="checkbox"
            checked={draft.smart_card.provision}
            onChange={(e) => set('smart_card', { provision: e.target.checked })}
          />
          <span>Create an account for a valid card that matches nobody</span>
        </label>
        <label className="row" style={{ gap: 'var(--janus-space-2)' }}>
          <input
            type="checkbox"
            checked={draft.smart_card.require_for_admins}
            onChange={(e) => set('smart_card', { require_for_admins: e.target.checked })}
          />
          <span>Administrators must sign in with a card</span>
        </label>
      </section>

      <div
        className="row"
        style={{ position: 'sticky', bottom: 0, padding: 'var(--janus-space-3) 0', background: 'var(--janus-color-bg-canvas)' }}
      >
        <button type="submit" className="btn btn-primary" disabled={!dirty || save.isPending}>
          {save.isPending ? 'Saving…' : 'Save security settings'}
        </button>
        <button type="button" className="btn btn-ghost" disabled={!dirty} onClick={() => setDraft(doc.policy)}>
          Discard changes
        </button>
      </div>

      <ConfirmDialog
        open={confirmBaseline}
        title={`Apply ${baseline.name}?`}
        confirmLabel="Apply"
        onClose={() => setConfirmBaseline(false)}
        onConfirm={() => {
          setDraft(baseline.values);
          setConfirmBaseline(false);
        }}
        consequence={`${baseline.summary ? baseline.summary + ' ' : ''}${
          baseline.id === 'nist-800-63b'
            ? 'It turns off complexity rules and forced password changes; other values already stricter are kept.'
            : 'Values already stricter are kept.'
        } Nothing changes until you save.`}
      />
    </form>
  );
}

function SyslogSection({
  draft,
  set,
  status,
}: {
  draft: SecurityPolicy;
  set: <K extends keyof SecurityPolicy>(section: K, patch: Partial<SecurityPolicy[K]>) => void;
  status?: SyslogStatus;
}): ReactNode {
  const toast = useToast();
  const test = useMutation({
    mutationFn: () =>
      api.post<{ ok: boolean; message?: string; error?: string }>('/api/v1/admin/security/syslog/test', { syslog: draft.syslog }),
    onSuccess: (r) => toast(r.ok ? (r.message ?? 'Test event sent.') : `Not sent: ${r.error}`, r.ok ? 'success' : 'danger'),
    onError: (e: Error) => toast(e.message, 'danger'),
  });
  const s = draft.syslog;
  return (
    <section className="card stack" aria-labelledby="sec-syslog">
      <h2 id="sec-syslog" style={{ margin: 0 }}>
        Audit forwarding (syslog / SIEM)
      </h2>
      <label className="switch">
        <input
          type="checkbox"
          checked={s.enabled}
          onChange={(e) => set('syslog', { enabled: e.target.checked })}
          aria-label="Forward audit records"
        />
        <span>
          <strong>Forward every audit record</strong>
          <span className="small muted" style={{ display: 'block' }}>
            Sign-ins, failures, lockouts, access denied and every administrative change, as they are committed. Records also stay
            in Janus.
          </span>
        </span>
      </label>
      <div className="grid grid-tiles">
        <Field label="Server (host:port)">
          <input
            className="input mono"
            placeholder="siem.example.mil:6514"
            value={s.address}
            onChange={(e) => set('syslog', { address: e.target.value })}
          />
        </Field>
        <Field label="Transport">
          <select
            className="select"
            value={s.protocol}
            onChange={(e) => set('syslog', { protocol: e.target.value as 'udp' | 'tcp' | 'tls' })}
          >
            <option value="tls">TLS (RFC 5425)</option>
            <option value="tcp">TCP</option>
            <option value="udp">UDP</option>
          </select>
        </Field>
        <Field label="Format">
          <select
            className="select"
            value={s.format}
            onChange={(e) => set('syslog', { format: e.target.value as 'rfc5424' | 'cef' })}
          >
            <option value="rfc5424">RFC 5424 with structured data</option>
            <option value="cef">CEF (ArcSight)</option>
          </select>
        </Field>
        <Field label="Facility" hint="10 = authpriv, 13 = log audit, 16–23 = local0–7.">
          <input
            className="input"
            type="number"
            min={0}
            max={23}
            value={s.facility}
            onChange={(e) => set('syslog', { facility: num(e.target.value) })}
          />
        </Field>
        <Field label="App name" hint="Tells this Janus apart from other applications.">
          <input className="input mono" value={s.app_name} onChange={(e) => set('syslog', { app_name: e.target.value })} />
        </Field>
      </div>
      {s.protocol === 'tls' ? (
        <div className="grid grid-tiles">
          <Field label="TLS server name" hint="Optional: when it differs from the host.">
            <input
              className="input mono"
              value={s.server_name}
              onChange={(e) => set('syslog', { server_name: e.target.value })}
            />
          </Field>
          <Field label="Client certificate file" hint="Optional, mounted in the pod.">
            <input
              className="input mono"
              value={s.client_cert_file}
              onChange={(e) => set('syslog', { client_cert_file: e.target.value })}
            />
          </Field>
          <Field label="Client key file">
            <input
              className="input mono"
              value={s.client_key_file}
              onChange={(e) => set('syslog', { client_key_file: e.target.value })}
            />
          </Field>
        </div>
      ) : null}
      <div className="row wrap" style={{ gap: 'var(--janus-space-3)' }}>
        <button type="button" className="btn btn-sm" disabled={!s.address || test.isPending} onClick={() => test.mutate()}>
          {test.isPending ? 'Sending…' : 'Send a test event'}
        </button>
        {status?.enabled ? (
          <span className="small muted" role="status">
            {status.destination}: {status.sent} sent
            {status.dropped ? `, ${status.dropped} dropped` : ''}
            {status.failed ? `, ${status.failed} failed` : ''}
            {status.last_sent_at ? ` · last ${formatDateTime(status.last_sent_at)}` : ''}
          </span>
        ) : null}
      </div>
      {status?.enabled && status.last_error ? (
        <div className="banner banner-danger" role="alert">
          <span className="small">
            Last error ({formatDateTime(status.last_error_at)}): {status.last_error}
          </span>
        </div>
      ) : null}
    </section>
  );
}

function LockoutList(): ReactNode {
  const toast = useToast();
  const client = useQueryClient();
  const q = useQuery({
    queryKey: ['admin', 'security', 'lockouts'],
    queryFn: () => api.get<{ lockouts: Lockout[] }>('/api/v1/admin/security/lockouts'),
  });
  const unlock = useMutation({
    mutationFn: (login: string) => api.post('/api/v1/admin/security/lockouts/unlock', { login }),
    onSuccess: (_d, login) => {
      toast(`${login} unlocked.`);
      void client.invalidateQueries({ queryKey: ['admin', 'security', 'lockouts'] });
    },
    onError: (e: Error) => toast(e.message, 'danger'),
  });
  const rows = q.data?.lockouts ?? [];
  return (
    <div className="stack" style={{ gap: 'var(--janus-space-2)' }}>
      <h3 className="field-label" style={{ margin: 0 }}>
        Locked now
      </h3>
      {rows.length === 0 ? (
        <p className="small muted" style={{ margin: 0 }}>
          Nobody is locked out.
        </p>
      ) : (
        <div className="table-wrap">
          <table className="data">
            <thead>
              <tr>
                <th scope="col">Sign-in name</th>
                <th scope="col">Locked</th>
                <th scope="col">Until</th>
                <th scope="col">
                  <span className="sr-only">Unlock</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.map((l) => (
                <tr key={l.login}>
                  <td className="mono small">{l.login}</td>
                  <td className="small">{formatDateTime(l.locked_at)}</td>
                  <td className="small">{l.until_unlocked ? 'An administrator unlocks it' : formatDateTime(l.locked_until)}</td>
                  <td style={{ textAlign: 'right' }}>
                    <button
                      type="button"
                      className="btn btn-sm"
                      disabled={unlock.isPending}
                      onClick={() => unlock.mutate(l.login)}
                    >
                      Unlock
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
