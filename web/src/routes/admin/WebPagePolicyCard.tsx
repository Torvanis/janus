import type { ReactNode } from 'react';
import { Badge } from '../../components/ui';
import type { WebPagePolicy } from './ChatAdmin';

const TRUST: Record<WebPagePolicy['tls_trust'], string> = {
  system: 'System roots',
  custom: 'System roots + Settings → Certificates',
  'custom-only': 'Only Settings → Certificates (sites signed by your CAs)',
};

/**
 * Which web pages models may open (read_page, deep research). Set at startup
 * with JANUS_WEB_* variables; shown here read-only.
 */
export function WebPagePolicyCard({ policy }: { policy?: WebPagePolicy }): ReactNode {
  if (!policy) return null;
  const list = (v: string[]) => (v.length ? v.join(', ') : '—');
  return (
    <section className="card stack" aria-labelledby="web-page-policy-title">
      <div className="row-between" style={{ alignItems: 'flex-start', gap: 'var(--janus-space-3)' }}>
        <div>
          <h2 id="web-page-policy-title" style={{ margin: 0 }}>
            Pages models can open
          </h2>
          <p className="small secondary" style={{ margin: '4px 0 0', maxWidth: '62ch' }}>
            When a model opens a link from search results or deep research. Set when Janus starts (JANUS_WEB_* variables); this
            server itself, link-local and cloud metadata addresses are never opened.
          </p>
        </div>
        <Badge tone={policy.default ? 'neutral' : 'info'}>{policy.default ? 'Public internet only' : 'Custom'}</Badge>
      </div>
      <dl className="small" style={{ margin: 0 }} aria-label="Web page policy">
        <dt>Private networks and internal names</dt>
        <dd>{policy.allow_private ? 'Allowed (JANUS_WEB_ALLOW_PRIVATE)' : 'Blocked'}</dd>
        <dt>Only these networks</dt>
        <dd className="mono">{list(policy.allow_networks)}</dd>
        <dt>Never these networks</dt>
        <dd className="mono">{list(policy.deny_networks)}</dd>
        <dt>Only these sites</dt>
        <dd className="mono">{list(policy.allow_domains)}</dd>
        <dt>Never these sites</dt>
        <dd className="mono">{list(policy.deny_domains)}</dd>
        <dt>Ports</dt>
        <dd className="mono">{policy.ports.join(', ')}</dd>
        <dt>Connection</dt>
        <dd>
          {policy.https_only ? 'HTTPS only' : 'HTTP and HTTPS'} ·{' '}
          {policy.proxy ? (
            <>
              through <span className="mono">{policy.proxy}</span> (the proxy decides where names lead)
            </>
          ) : (
            'direct'
          )}
        </dd>
        <dt>Certificates trusted</dt>
        <dd>{TRUST[policy.tls_trust] ?? policy.tls_trust}</dd>
      </dl>
    </section>
  );
}
