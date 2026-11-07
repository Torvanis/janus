import { useState, type FormEvent, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { AsyncSection, Badge, ConfirmDialog, Field, useToast } from '../../components/ui';
import { formatDateTime } from '../../lib/format';
import { t } from '../../lib/i18n';

/**
 * Admin → Settings → Certificates: the outbound trust store. Every outbound
 * TLS connection (AI providers, identity providers, LDAP, object storage,
 * webhooks, email, OTLP) trusts the host's roots plus what is listed here.
 * Mounted sources (DoD roots, JANUS_CA_BUNDLE, JANUS_CA_DIR) are read-only.
 */
export interface TrustedCertificate {
  id?: string;
  name: string;
  source: 'dod' | 'file' | 'directory' | 'uploaded';
  path?: string;
  subject: string;
  issuer: string;
  sha256: string;
  is_ca: boolean;
  not_before: string;
  not_after: string;
  read_only: boolean;
  created_by?: string;
  created_at?: string;
  expires_soon: boolean;
  expired: boolean;
}

export interface TrustStoreResponse {
  trust_store: {
    dod_roots: boolean;
    bundle_file?: string;
    directory?: string;
    certificates: TrustedCertificate[];
    last_problem?: string;
    untrusted_allowed: boolean;
    expiring_soon: number;
    expired: number;
  };
  untrusted_tls_available: boolean;
  expiry_warning_days: number;
}

const SOURCE_LABEL: Record<TrustedCertificate['source'], () => string> = {
  dod: () => t('adminCerts.sourceDod'),
  file: () => t('adminCerts.sourceFile'),
  directory: () => t('adminCerts.sourceDirectory'),
  uploaded: () => t('adminCerts.sourceUploaded'),
};

export function CertificatesCard(): ReactNode {
  const toast = useToast();
  const client = useQueryClient();
  const query = useQuery({
    queryKey: ['admin', 'certificates'],
    queryFn: () => api.get<TrustStoreResponse>('/api/v1/admin/certificates'),
  });
  const [name, setName] = useState('');
  const [pem, setPem] = useState('');
  const [error, setError] = useState('');
  const [removing, setRemoving] = useState<TrustedCertificate | null>(null);

  const upload = useMutation({
    mutationFn: () => api.post('/api/v1/admin/certificates', { name, pem }),
    onSuccess: () => {
      setName('');
      setPem('');
      setError('');
      toast(t('adminCerts.added'));
      void client.invalidateQueries({ queryKey: ['admin', 'certificates'] });
    },
    onError: (e: Error) => setError(e.message),
  });
  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/v1/admin/certificates/${id}`),
    onSuccess: () => {
      setRemoving(null);
      toast(t('adminCerts.removed'));
      void client.invalidateQueries({ queryKey: ['admin', 'certificates'] });
    },
    onError: (e: Error) => toast(e.message, 'danger'),
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!pem.trim()) {
      setError(t('adminCerts.pemRequired'));
      return;
    }
    upload.mutate();
  };

  const onFile = async (file: File | undefined) => {
    if (!file) return;
    setPem(await file.text());
    if (!name) setName(file.name.replace(/\.(pem|crt|cer)$/i, ''));
  };

  return (
    <section className="card" id="certificates" aria-labelledby="certificates-heading">
      <div className="card-header">
        <h2 id="certificates-heading">{t('adminCerts.title')}</h2>
      </div>
      <p className="small muted">{t('adminCerts.intro')}</p>
      <AsyncSection query={query}>
        {(data) => (
          <div className="stack">
            {data.trust_store.last_problem ? (
              <div className="banner banner-warning" role="alert">
                <span className="small">{data.trust_store.last_problem}</span>
              </div>
            ) : null}
            <ul className="small" style={{ margin: 0, paddingLeft: '1.2em' }} aria-label={t('adminCerts.sources')}>
              <li>{data.trust_store.dod_roots ? t('adminCerts.dodOn') : t('adminCerts.dodOff')}</li>
              {data.trust_store.bundle_file ? (
                <li>
                  JANUS_CA_BUNDLE: <span className="mono">{data.trust_store.bundle_file}</span>
                </li>
              ) : null}
              {data.trust_store.directory ? (
                <li>
                  JANUS_CA_DIR: <span className="mono">{data.trust_store.directory}</span>
                </li>
              ) : null}
              <li>{data.untrusted_tls_available ? t('adminCerts.untrustedAvailable') : t('adminCerts.untrustedRefused')}</li>
            </ul>
            {data.trust_store.certificates.length === 0 ? (
              <p className="small muted">{t('adminCerts.none')}</p>
            ) : (
              <table className="table">
                <thead>
                  <tr>
                    <th>{t('adminCerts.colName')}</th>
                    <th>{t('adminCerts.colSource')}</th>
                    <th>{t('adminCerts.colExpires')}</th>
                    <th aria-label={t('adminCerts.colActions')} />
                  </tr>
                </thead>
                <tbody>
                  {data.trust_store.certificates.map((c) => (
                    <tr key={`${c.source}-${c.sha256}`}>
                      <td>
                        <div>{c.name}</div>
                        <div className="small muted mono" title={c.sha256} style={{ overflowWrap: 'anywhere' }}>
                          {c.subject}
                        </div>
                      </td>
                      <td>
                        {SOURCE_LABEL[c.source]()}
                        {c.read_only ? <div className="small muted">{t('adminCerts.managedByDeployment')}</div> : null}
                      </td>
                      <td>
                        {formatDateTime(c.not_after)}{' '}
                        {c.expired ? (
                          <Badge tone="danger">{t('adminCerts.expired')}</Badge>
                        ) : c.expires_soon ? (
                          <Badge tone="warning">{t('adminCerts.expiresSoon')}</Badge>
                        ) : null}
                      </td>
                      <td>
                        {!c.read_only && c.id ? (
                          <button type="button" className="btn btn-sm btn-danger" onClick={() => setRemoving(c)}>
                            {t('adminCerts.remove')}
                          </button>
                        ) : null}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            <form className="stack" onSubmit={submit} aria-label={t('adminCerts.uploadTitle')}>
              <h3 className="small" style={{ margin: 0 }}>
                {t('adminCerts.uploadTitle')}
              </h3>
              <Field label={t('adminCerts.name')} hint={t('adminCerts.nameHint')}>
                <input className="input" value={name} onChange={(e) => setName(e.target.value)} maxLength={200} />
              </Field>
              <Field label={t('adminCerts.pem')} hint={t('adminCerts.pemHint')} error={error || undefined}>
                <textarea
                  className="input mono"
                  rows={6}
                  value={pem}
                  onChange={(e) => setPem(e.target.value)}
                  placeholder="-----BEGIN CERTIFICATE-----"
                />
              </Field>
              <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
                <label className="btn btn-sm">
                  {t('adminCerts.chooseFile')}
                  <input type="file" accept=".pem,.crt,.cer" hidden onChange={(e) => void onFile(e.target.files?.[0])} />
                </label>
                <button type="submit" className="btn btn-primary btn-sm" disabled={upload.isPending}>
                  {t('adminCerts.add')}
                </button>
              </div>
            </form>
          </div>
        )}
      </AsyncSection>
      <ConfirmDialog
        open={removing !== null}
        onClose={() => setRemoving(null)}
        onConfirm={() => removing?.id && remove.mutate(removing.id)}
        title={t('adminCerts.removeTitle', { name: removing?.name ?? '' })}
        consequence={t('adminCerts.removeConsequence')}
        confirmLabel={t('adminCerts.remove')}
        busy={remove.isPending}
      />
    </section>
  );
}
