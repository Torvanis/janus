import { useEffect, useState, type FormEvent, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { Badge, ConfirmDialog, Field, useToast } from '../../components/ui';
import { t } from '../../lib/i18n';

/**
 * Admin → Provisioning → Directory (LDAP / Active Directory). Business: ldap.
 * One directory per gateway. The bind password is write-only; Test resolves
 * a user without their password so filters and admin-group mapping can be
 * checked before saving.
 */
export interface DirectoryView {
  url: string;
  start_tls: boolean;
  skip_verify: boolean;
  bind_dn: string;
  has_bind_password: boolean;
  base_dn: string;
  user_filter: string;
  email_attr: string;
  name_attr: string;
  groups_attr: string;
  group_filter: string;
  group_name_attr: string;
  admin_groups: string[];
  enabled: boolean;
  updated_at: string;
}

type Draft = {
  url: string;
  start_tls: boolean;
  skip_verify: boolean;
  bind_dn: string;
  bind_password: string;
  base_dn: string;
  user_filter: string;
  email_attr: string;
  name_attr: string;
  groups_attr: string;
  group_filter: string;
  admin_groups: string;
  enabled: boolean;
};

const AD_FILTER = '(&(objectClass=user)(|(mail={login})(userPrincipalName={login})(sAMAccountName={login})))';
const empty: Draft = {
  url: '',
  start_tls: false,
  skip_verify: false,
  bind_dn: '',
  bind_password: '',
  base_dn: '',
  user_filter: AD_FILTER,
  email_attr: 'mail',
  name_attr: 'displayName',
  groups_attr: 'memberOf',
  group_filter: '',
  admin_groups: '',
  enabled: true,
};

function toDraft(d: DirectoryView): Draft {
  return {
    url: d.url,
    start_tls: d.start_tls,
    skip_verify: d.skip_verify,
    bind_dn: d.bind_dn,
    bind_password: '',
    base_dn: d.base_dn,
    user_filter: d.user_filter,
    email_attr: d.email_attr,
    name_attr: d.name_attr,
    groups_attr: d.groups_attr,
    group_filter: d.group_filter,
    admin_groups: d.admin_groups.join(', '),
    enabled: d.enabled,
  };
}

function payload(d: Draft) {
  return {
    url: d.url,
    start_tls: d.start_tls,
    skip_verify: d.skip_verify,
    bind_dn: d.bind_dn,
    bind_password: d.bind_password || undefined,
    base_dn: d.base_dn,
    user_filter: d.user_filter,
    email_attr: d.email_attr,
    name_attr: d.name_attr,
    groups_attr: d.groups_attr,
    group_filter: d.group_filter,
    admin_groups: d.admin_groups
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean),
    enabled: d.enabled,
  };
}

type TestResult = {
  ok: boolean;
  error?: string;
  user?: { dn: string; email: string; name: string; groups: string[]; would_be_admin: boolean };
};

export function DirectoryCard(): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const query = useQuery({
    queryKey: ['admin', 'directory'],
    queryFn: () => api.get<{ directory: DirectoryView | null; licensed: boolean }>('/api/v1/admin/directory'),
  });
  const [draft, setDraft] = useState<Draft>(empty);
  const [dirty, setDirty] = useState(false);
  const [testLogin, setTestLogin] = useState('');
  const [result, setResult] = useState<TestResult | null>(null);
  const [confirmRemove, setConfirmRemove] = useState(false);

  useEffect(() => {
    if (query.data && !dirty) setDraft(query.data.directory ? toDraft(query.data.directory) : empty);
  }, [query.data, dirty]);

  const licensed = query.data?.licensed ?? false;
  const configured = Boolean(query.data?.directory);
  const invalidate = () => void queryClient.invalidateQueries({ queryKey: ['admin', 'directory'] });
  const set = (k: keyof Draft) => (e: { target: { value: string } }) => {
    setDirty(true);
    setDraft((d) => ({ ...d, [k]: e.target.value }));
  };
  const setBool = (k: keyof Draft) => (e: { target: { checked: boolean } }) => {
    setDirty(true);
    setDraft((d) => ({ ...d, [k]: e.target.checked }));
  };

  const test = useMutation({
    mutationFn: () => api.post<TestResult>('/api/v1/admin/directory/test', { ...payload(draft), login: testLogin }),
    onSuccess: setResult,
    onError: (error: Error) => setResult({ ok: false, error: error.message }),
  });
  const save = useMutation({
    mutationFn: () => api.put<DirectoryView>('/api/v1/admin/directory', payload(draft)),
    onSuccess: () => {
      toast(t('adminDir.saved'));
      setDirty(false);
      invalidate();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });
  const remove = useMutation({
    mutationFn: () => api.del('/api/v1/admin/directory'),
    onSuccess: () => {
      toast(t('adminDir.removed'));
      setConfirmRemove(false);
      setDirty(false);
      setDraft(empty);
      invalidate();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    save.mutate();
  }

  return (
    <section className="card stack" aria-labelledby="dir-title">
      <div className="row" style={{ justifyContent: 'space-between', alignItems: 'baseline' }}>
        <div>
          <h2 id="dir-title">
            {t('adminDir.title')}{' '}
            {configured ? (
              <Badge tone={query.data?.directory?.enabled ? 'success' : 'neutral'}>
                {query.data?.directory?.enabled ? t('adminDir.enabled') : t('adminDir.disabled')}
              </Badge>
            ) : null}
          </h2>
          <p className="muted small">{t('adminDir.intro')}</p>
        </div>
        {configured ? (
          <button type="button" className="btn btn-ghost btn-sm" onClick={() => setConfirmRemove(true)} disabled={!licensed}>
            {t('adminDir.remove')}
          </button>
        ) : null}
      </div>
      {!licensed ? (
        <div className="banner banner-info">
          <div>
            <strong>{t('adminDir.upsellTitle')}</strong>
            <div className="small" style={{ marginTop: 4 }}>
              {t('adminDir.upsellBody')}
            </div>
          </div>
        </div>
      ) : null}
      <form className="stack" onSubmit={submit}>
        <fieldset className="stack" disabled={!licensed} style={{ border: 0, padding: 0, margin: 0 }}>
          <div className="grid grid-halves">
            <Field label={t('adminDir.url')} hint={t('adminDir.urlHint')}>
              <input
                className="input"
                aria-label={t('adminDir.url')}
                value={draft.url}
                onChange={set('url')}
                placeholder="ldaps://ad.example.com:636"
                required
              />
            </Field>
            <Field label={t('adminDir.baseDn')}>
              <input
                className="input"
                aria-label={t('adminDir.baseDn')}
                value={draft.base_dn}
                onChange={set('base_dn')}
                placeholder="dc=example,dc=com"
                required
              />
            </Field>
          </div>
          <div className="row wrap" style={{ gap: 'var(--janus-space-4)' }}>
            <label className="row" style={{ gap: 'var(--janus-space-2)' }}>
              <input type="checkbox" checked={draft.start_tls} onChange={setBool('start_tls')} />
              <span>{t('adminDir.startTls')}</span>
            </label>
            <label className="row" style={{ gap: 'var(--janus-space-2)' }}>
              <input type="checkbox" checked={draft.skip_verify} onChange={setBool('skip_verify')} />
              <span>{t('adminDir.skipVerify')}</span>
            </label>
          </div>
          <div className="grid grid-halves">
            <Field label={t('adminDir.bindDn')} hint={t('adminDir.bindDnHint')}>
              <input
                className="input"
                aria-label={t('adminDir.bindDn')}
                value={draft.bind_dn}
                onChange={set('bind_dn')}
                placeholder="cn=janus,ou=service,dc=example,dc=com"
                autoComplete="off"
              />
            </Field>
            <Field label={t('adminDir.bindPassword')} hint={configured ? t('adminDir.bindPasswordKeep') : undefined}>
              <input
                className="input"
                aria-label={t('adminDir.bindPassword')}
                type="password"
                value={draft.bind_password}
                onChange={set('bind_password')}
                autoComplete="new-password"
              />
            </Field>
          </div>
          <Field label={t('adminDir.userFilter')} hint={t('adminDir.userFilterHint')}>
            <input
              className="input mono"
              aria-label={t('adminDir.userFilter')}
              value={draft.user_filter}
              onChange={set('user_filter')}
            />
          </Field>
          <div className="grid grid-halves">
            <Field label={t('adminDir.emailAttr')}>
              <input
                className="input"
                aria-label={t('adminDir.emailAttr')}
                value={draft.email_attr}
                onChange={set('email_attr')}
              />
            </Field>
            <Field label={t('adminDir.nameAttr')}>
              <input className="input" aria-label={t('adminDir.nameAttr')} value={draft.name_attr} onChange={set('name_attr')} />
            </Field>
          </div>
          <div className="grid grid-halves">
            <Field label={t('adminDir.groupsAttr')} hint={t('adminDir.groupsAttrHint')}>
              <input
                className="input"
                aria-label={t('adminDir.groupsAttr')}
                value={draft.groups_attr}
                onChange={set('groups_attr')}
              />
            </Field>
            <Field label={t('adminDir.groupFilter')} hint={t('adminDir.groupFilterHint')}>
              <input
                className="input mono"
                aria-label={t('adminDir.groupFilter')}
                value={draft.group_filter}
                onChange={set('group_filter')}
                placeholder="(&(objectClass=group)(member={dn}))"
                disabled={Boolean(draft.groups_attr)}
              />
            </Field>
          </div>
          <Field label={t('adminDir.adminGroups')} hint={t('adminDir.adminGroupsHint')}>
            <input
              className="input"
              aria-label={t('adminDir.adminGroups')}
              value={draft.admin_groups}
              onChange={set('admin_groups')}
              placeholder="Janus Admins"
            />
          </Field>
          <label className="row" style={{ gap: 'var(--janus-space-2)' }}>
            <input type="checkbox" checked={draft.enabled} onChange={setBool('enabled')} />
            <span>{t('adminDir.enabledLabel')}</span>
          </label>

          <div className="card stack" style={{ background: 'var(--janus-surface-2)' }}>
            <strong>{t('adminDir.testTitle')}</strong>
            <p className="small muted">{t('adminDir.testBody')}</p>
            <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
              <input
                className="input"
                aria-label={t('adminDir.testLogin')}
                value={testLogin}
                onChange={(e) => setTestLogin(e.target.value)}
                placeholder="ada@example.com"
                style={{ flex: 1 }}
              />
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => test.mutate()}
                disabled={!draft.url || !draft.base_dn || test.isPending}
              >
                {t('adminDir.test')}
              </button>
            </div>
            {result ? (
              <div className={`banner ${result.ok ? 'banner-success' : 'banner-danger'}`} role="status">
                {result.ok ? (
                  result.user ? (
                    <div className="small stack" style={{ gap: 2 }}>
                      <span>
                        <strong>{result.user.email || '—'}</strong> · {result.user.name || '—'} ·{' '}
                        {result.user.would_be_admin ? t('adminDir.wouldBeAdmin') : t('adminDir.wouldBeUser')}
                      </span>
                      <code style={{ wordBreak: 'break-all' }}>{result.user.dn}</code>
                      <span>{t('adminDir.groups', { list: result.user.groups.join(', ') || '—' })}</span>
                    </div>
                  ) : (
                    <div className="small">{t('adminDir.bindOk')}</div>
                  )
                ) : (
                  <div className="small">{t('adminDir.testFailed', { error: result.error ?? '' })}</div>
                )}
              </div>
            ) : null}
          </div>

          <div className="row" style={{ justifyContent: 'flex-end', gap: 'var(--janus-space-2)' }}>
            <button type="submit" className="btn btn-primary" disabled={save.isPending || !dirty}>
              {configured ? t('adminDir.save') : t('adminDir.connect')}
            </button>
          </div>
        </fieldset>
      </form>
      <ConfirmDialog
        open={confirmRemove}
        title={t('adminDir.removeTitle')}
        consequence={t('adminDir.removeBody')}
        onClose={() => setConfirmRemove(false)}
        onConfirm={() => remove.mutate()}
        confirmLabel={t('adminDir.remove')}
        busy={remove.isPending}
      />
    </section>
  );
}
