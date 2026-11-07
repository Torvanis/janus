import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { AsyncSection, Badge, ConfirmDialog, useToast } from '../../components/ui';
import { formatDateTime } from '../../lib/format';
import { t } from '../../lib/i18n';

/**
 * Admin → Settings → Certificates → Janus listener: what Janus serves on its
 * own port. A self-signed certificate is a stopgap: the proxy in front of
 * Janus trusts the downloaded certificate, and each regeneration means
 * installing the new one there.
 */
export interface ListenerTLSInfo {
  mode: 'none' | 'mounted' | 'self_signed';
  subject?: string;
  names?: string[];
  sha256?: string;
  not_before?: string;
  not_after?: string;
  expires_soon: boolean;
  expired: boolean;
  required: boolean;
  cert_path?: string;
  last_error?: string;
  generated_at?: string;
  generated_by?: string;
}

export function ListenerTLSCard(): ReactNode {
  const toast = useToast();
  const client = useQueryClient();
  const query = useQuery({
    queryKey: ['admin', 'listener-tls'],
    queryFn: () => api.get<{ listener: ListenerTLSInfo }>('/api/v1/admin/listener-tls'),
  });
  const [confirm, setConfirm] = useState(false);
  const regenerate = useMutation({
    mutationFn: () => api.post<{ listener: ListenerTLSInfo }>('/api/v1/admin/listener-tls/regenerate', {}),
    onSuccess: () => {
      setConfirm(false);
      toast(t('adminListenerTls.regenerated'));
      void client.invalidateQueries({ queryKey: ['admin', 'listener-tls'] });
    },
    onError: (e: Error) => toast(e.message, 'danger'),
  });

  return (
    <section className="card" id="listener-tls" aria-labelledby="listener-tls-heading">
      <div className="card-header">
        <h2 id="listener-tls-heading">{t('adminListenerTls.title')}</h2>
      </div>
      <AsyncSection query={query}>
        {({ listener }) => (
          <div className="stack">
            {listener.mode === 'none' ? (
              <p className="small muted">{t('adminListenerTls.none')}</p>
            ) : (
              <>
                {listener.mode === 'self_signed' ? (
                  <div className="banner banner-warning" role="note">
                    <span className="small">{t('adminListenerTls.selfSignedNote')}</span>
                  </div>
                ) : null}
                <dl className="small" style={{ margin: 0 }}>
                  <dt>{t('adminListenerTls.mode')}</dt>
                  <dd>
                    {listener.mode === 'mounted' ? t('adminListenerTls.modeMounted') : t('adminListenerTls.modeSelfSigned')}
                    {listener.cert_path ? <span className="mono"> ({listener.cert_path})</span> : null}
                  </dd>
                  <dt>{t('adminListenerTls.names')}</dt>
                  <dd className="mono">{(listener.names ?? []).join(', ')}</dd>
                  <dt>{t('adminListenerTls.expires')}</dt>
                  <dd>
                    {formatDateTime(listener.not_after)}{' '}
                    {listener.expired ? (
                      <Badge tone="danger">{t('adminCerts.expired')}</Badge>
                    ) : listener.expires_soon ? (
                      <Badge tone="warning">{t('adminCerts.expiresSoon')}</Badge>
                    ) : null}
                  </dd>
                  <dt>SHA-256</dt>
                  <dd className="mono" style={{ overflowWrap: 'anywhere' }}>
                    {listener.sha256}
                  </dd>
                </dl>
                {listener.last_error ? (
                  <div className="banner banner-warning" role="alert">
                    <span className="small">{listener.last_error}</span>
                  </div>
                ) : null}
                <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
                  <a className="btn btn-sm" href="/api/v1/admin/listener-tls/certificate.pem" download>
                    {t('adminListenerTls.download')}
                  </a>
                  {listener.mode === 'self_signed' ? (
                    <button type="button" className="btn btn-sm" onClick={() => setConfirm(true)}>
                      {t('adminListenerTls.regenerate')}
                    </button>
                  ) : null}
                </div>
              </>
            )}
            <p className="small muted">
              {listener.required ? t('adminListenerTls.required') : t('adminListenerTls.notRequired')}
            </p>
          </div>
        )}
      </AsyncSection>
      <ConfirmDialog
        open={confirm}
        onClose={() => setConfirm(false)}
        onConfirm={() => regenerate.mutate()}
        title={t('adminListenerTls.regenerateTitle')}
        consequence={t('adminListenerTls.regenerateConsequence')}
        confirmLabel={t('adminListenerTls.regenerate')}
        busy={regenerate.isPending}
      />
    </section>
  );
}
