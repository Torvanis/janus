import { useState, type FormEvent, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { Badge, ConfirmDialog, Field, Modal, useToast } from '../../components/ui';
import { t } from '../../lib/i18n';

/**
 * Admin → Provisioning → Identity providers. Additional OIDC providers beyond
 * the env-configured one (Business: multi_oidc). Each provider gets its own
 * callback URL, shown here so the admin can paste it into the IdP.
 */
export interface IdentityProviderRow {
  id: string;
  slug: string;
  name: string;
  issuer_url: string;
  client_id: string;
  has_secret: boolean;
  scopes: string[];
  email_claim: string;
  name_claim: string;
  groups_claim: string;
  admin_groups: string[];
  enabled: boolean;
  sort_order: number;
  callback_url: string;
}

type Draft = {
  slug: string;
  name: string;
  issuer_url: string;
  client_id: string;
  client_secret: string;
  scopes: string;
  groups_claim: string;
  admin_groups: string;
  enabled: boolean;
};

const empty: Draft = {
  slug: '',
  name: '',
  issuer_url: '',
  client_id: '',
  client_secret: '',
  scopes: 'openid, email, profile',
  groups_claim: 'groups',
  admin_groups: '',
  enabled: true,
};

function toDraft(p: IdentityProviderRow): Draft {
  return {
    slug: p.slug,
    name: p.name,
    issuer_url: p.issuer_url,
    client_id: p.client_id,
    client_secret: '',
    scopes: p.scopes.join(', '),
    groups_claim: p.groups_claim,
    admin_groups: p.admin_groups.join(', '),
    enabled: p.enabled,
  };
}

function list(v: string): string[] {
  return v
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean);
}

export function IdentityProvidersCard(): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const providers = useQuery({
    queryKey: ['admin', 'identity-providers'],
    queryFn: () => api.get<{ providers: IdentityProviderRow[]; licensed: boolean }>('/api/v1/admin/identity-providers'),
  });
  const [editing, setEditing] = useState<IdentityProviderRow | 'new' | null>(null);
  const [pendingDelete, setPendingDelete] = useState<IdentityProviderRow | null>(null);
  const invalidate = () => void queryClient.invalidateQueries({ queryKey: ['admin', 'identity-providers'] });

  const remove = useMutation({
    mutationFn: (p: IdentityProviderRow) => api.del(`/api/v1/admin/identity-providers/${p.id}`),
    onSuccess: () => {
      toast(t('adminIdp.deleted'));
      setPendingDelete(null);
      invalidate();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const licensed = providers.data?.licensed ?? false;
  const rows = providers.data?.providers ?? [];

  return (
    <section className="card stack" aria-labelledby="idp-title">
      <div className="row" style={{ justifyContent: 'space-between', alignItems: 'baseline' }}>
        <div>
          <h2 id="idp-title">{t('adminIdp.title')}</h2>
          <p className="muted small">{t('adminIdp.intro')}</p>
        </div>
        <button type="button" className="btn btn-primary btn-sm" onClick={() => setEditing('new')} disabled={!licensed}>
          {t('adminIdp.add')}
        </button>
      </div>
      {!licensed ? (
        <div className="banner banner-info">
          <div>
            <strong>{t('adminIdp.upsellTitle')}</strong>
            <div className="small" style={{ marginTop: 4 }}>
              {t('adminIdp.upsellBody')}
            </div>
          </div>
        </div>
      ) : null}
      {rows.length === 0 ? (
        <p className="muted">{t('adminIdp.empty')}</p>
      ) : (
        <ul className="list-plain stack">
          {rows.map((p) => (
            <li key={p.id} className="row wrap" style={{ justifyContent: 'space-between', gap: 'var(--janus-space-3)' }}>
              <div className="stack" style={{ gap: 2 }}>
                <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
                  <strong>{p.name}</strong>
                  <code>{p.slug}</code>
                  <Badge tone={p.enabled ? 'success' : 'neutral'}>
                    {p.enabled ? t('adminIdp.enabled') : t('adminIdp.disabled')}
                  </Badge>
                </div>
                <span className="small muted">{p.issuer_url}</span>
                <span className="small muted">
                  {t('adminIdp.callback')}: <code>{p.callback_url}</code>
                </span>
              </div>
              <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
                <button type="button" className="btn btn-ghost btn-sm" onClick={() => setEditing(p)} disabled={!licensed}>
                  {t('adminIdp.edit')}
                </button>
                <button type="button" className="btn btn-ghost btn-sm" onClick={() => setPendingDelete(p)} disabled={!licensed}>
                  {t('adminIdp.delete')}
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
      <ProviderModal
        provider={editing}
        onClose={() => setEditing(null)}
        onSaved={() => {
          setEditing(null);
          invalidate();
        }}
      />
      <ConfirmDialog
        open={Boolean(pendingDelete)}
        title={t('adminIdp.deleteTitle', { name: pendingDelete?.name ?? '' })}
        consequence={t('adminIdp.deleteBody')}
        onClose={() => setPendingDelete(null)}
        onConfirm={() => {
          if (pendingDelete) remove.mutate(pendingDelete);
        }}
        confirmLabel={t('adminIdp.delete')}
        busy={remove.isPending}
      />
    </section>
  );
}

function ProviderModal({
  provider,
  onClose,
  onSaved,
}: {
  provider: IdentityProviderRow | 'new' | null;
  onClose: () => void;
  onSaved: () => void;
}): ReactNode {
  const toast = useToast();
  const isNew = provider === 'new';
  const [draft, setDraft] = useState<Draft>(() => (provider && provider !== 'new' ? toDraft(provider) : empty));
  const [probe, setProbe] = useState<{ ok: boolean; issuer?: string; error?: string } | null>(null);
  const set = (k: keyof Draft) => (e: { target: { value: string } }) => setDraft((d) => ({ ...d, [k]: e.target.value }));

  const test = useMutation({
    mutationFn: () =>
      api.post<{ ok: boolean; issuer?: string; error?: string }>('/api/v1/admin/identity-providers/test', {
        issuer_url: draft.issuer_url,
      }),
    onSuccess: setProbe,
    onError: (error: Error) => setProbe({ ok: false, error: error.message }),
  });

  const save = useMutation({
    mutationFn: () => {
      const body = {
        slug: draft.slug,
        name: draft.name,
        issuer_url: draft.issuer_url,
        client_id: draft.client_id,
        client_secret: draft.client_secret || undefined,
        scopes: list(draft.scopes),
        groups_claim: draft.groups_claim,
        admin_groups: list(draft.admin_groups),
        enabled: draft.enabled,
      };
      return isNew
        ? api.post<IdentityProviderRow>('/api/v1/admin/identity-providers', body)
        : api.put<IdentityProviderRow>(`/api/v1/admin/identity-providers/${(provider as IdentityProviderRow).id}`, body);
    },
    onSuccess: (p) => {
      toast(t('adminIdp.saved', { name: p.name }));
      onSaved();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    save.mutate();
  }

  const callbackPreview = draft.slug ? `${window.location.origin}/auth/callback/${draft.slug}` : '';

  return (
    <Modal
      open={Boolean(provider)}
      onClose={onClose}
      title={
        isNew ? t('adminIdp.addTitle') : t('adminIdp.editTitle', { name: (provider as IdentityProviderRow | null)?.name ?? '' })
      }
      description={t('adminIdp.formBody')}
    >
      <form className="stack" onSubmit={submit}>
        <div className="grid-2">
          <Field label={t('adminIdp.name')}>
            <input
              className="input"
              aria-label={t('adminIdp.name')}
              value={draft.name}
              onChange={set('name')}
              required
              autoFocus
            />
          </Field>
          <Field label={t('adminIdp.slug')} hint={t('adminIdp.slugHint')}>
            <input
              className="input"
              aria-label={t('adminIdp.slug')}
              value={draft.slug}
              onChange={set('slug')}
              pattern="[a-z0-9][a-z0-9-]{1,39}"
              required
            />
          </Field>
        </div>
        {callbackPreview ? (
          <p className="small muted">
            {t('adminIdp.callback')}: <code>{callbackPreview}</code>
          </p>
        ) : null}
        <Field label={t('adminIdp.issuer')} hint={t('adminIdp.issuerHint')}>
          <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
            <input
              className="input"
              aria-label={t('adminIdp.issuer')}
              value={draft.issuer_url}
              onChange={set('issuer_url')}
              placeholder="https://login.example.com/oauth2/default"
              required
              style={{ flex: 1 }}
            />
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => test.mutate()}
              disabled={!draft.issuer_url || test.isPending}
            >
              {t('adminIdp.test')}
            </button>
          </div>
        </Field>
        {probe ? (
          <div className={`banner ${probe.ok ? 'banner-success' : 'banner-danger'}`} role="status">
            <div className="small">
              {probe.ok
                ? t('adminIdp.testOk', { issuer: probe.issuer ?? '' })
                : t('adminIdp.testFailed', { error: probe.error ?? '' })}
            </div>
          </div>
        ) : null}
        <div className="grid-2">
          <Field label={t('adminIdp.clientId')}>
            <input
              className="input"
              aria-label={t('adminIdp.clientId')}
              value={draft.client_id}
              onChange={set('client_id')}
              required
              autoComplete="off"
            />
          </Field>
          <Field label={t('adminIdp.clientSecret')} hint={isNew ? undefined : t('adminIdp.secretKeep')}>
            <input
              className="input"
              aria-label={t('adminIdp.clientSecret')}
              type="password"
              value={draft.client_secret}
              onChange={set('client_secret')}
              required={isNew}
              autoComplete="new-password"
            />
          </Field>
        </div>
        <div className="grid-2">
          <Field label={t('adminIdp.scopes')}>
            <input className="input" aria-label={t('adminIdp.scopes')} value={draft.scopes} onChange={set('scopes')} />
          </Field>
          <Field label={t('adminIdp.groupsClaim')}>
            <input
              className="input"
              aria-label={t('adminIdp.groupsClaim')}
              value={draft.groups_claim}
              onChange={set('groups_claim')}
            />
          </Field>
        </div>
        <Field label={t('adminIdp.adminGroups')} hint={t('adminIdp.adminGroupsHint')}>
          <input
            className="input"
            aria-label={t('adminIdp.adminGroups')}
            value={draft.admin_groups}
            onChange={set('admin_groups')}
            placeholder="janus-admins"
          />
        </Field>
        <label className="row" style={{ gap: 'var(--janus-space-2)' }}>
          <input
            type="checkbox"
            checked={draft.enabled}
            onChange={(e) => setDraft((d) => ({ ...d, enabled: e.target.checked }))}
          />
          <span>{t('adminIdp.enabledLabel')}</span>
        </label>
        <div className="row" style={{ justifyContent: 'flex-end', gap: 'var(--janus-space-2)' }}>
          <button type="button" className="btn btn-ghost" onClick={onClose}>
            {t('common.cancel')}
          </button>
          <button type="submit" className="btn btn-primary" disabled={save.isPending}>
            {isNew ? t('adminIdp.create') : t('common.save')}
          </button>
        </div>
      </form>
    </Modal>
  );
}
