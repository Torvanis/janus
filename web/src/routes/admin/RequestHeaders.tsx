import { useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { AsyncSection, Badge } from '../../components/ui';

/** One header as the request log stores it (values masked server-side). */
export interface LoggedHeader {
  name: string;
  values?: string[];
  masked?: boolean;
}

export interface HeaderChange {
  action: 'removed' | 'set' | 'appended' | 'not_allowed';
  name: string;
  rule: number;
}

export interface RequestHeaders {
  usage_id: string;
  policy_id: string;
  policy_name: string;
  target: string;
  inbound: LoggedHeader[];
  outbound: LoggedHeader[];
  changes: HeaderChange[];
}

const CHANGE_LABEL: Record<HeaderChange['action'], string> = {
  removed: 'Removed',
  set: 'Set',
  appended: 'Appended',
  not_allowed: 'Not on allow-list',
};

const CHANGE_TONE: Record<HeaderChange['action'], 'danger' | 'info' | 'warning'> = {
  removed: 'danger',
  set: 'info',
  appended: 'info',
  not_allowed: 'warning',
};

const TARGET_LABEL: Record<string, string> = {
  default: 'default policy',
  upstream: "upstream's own policy",
  personal_subscription: "provider's own policy",
};

type View = 'changes' | 'inbound' | 'outbound';

/**
 * The request's headers: what the caller sent, what the provider received,
 * and what the header policy changed between the two.
 */
export function RequestHeadersSection({ eventId }: { eventId: string }): ReactNode {
  const [view, setView] = useState<View>('changes');
  const q = useQuery({
    queryKey: ['request-headers', eventId],
    queryFn: () => api.get<{ headers: RequestHeaders | null }>(`/api/v1/admin/requests/${encodeURIComponent(eventId)}/headers`),
  });
  return (
    <AsyncSection query={q}>
      {(data) => {
        const h = data.headers;
        if (!h) {
          return (
            <p className="small muted" style={{ margin: 0 }}>
              No header record for this request. It was refused before reaching a provider, logged before header logging
              existed, or header logging is off.
            </p>
          );
        }
        return (
          <div className="stack" style={{ gap: 8 }}>
            <p className="small" style={{ margin: 0 }}>
              {h.policy_name ? (
                <>
                  Policy <Link to="/admin/rules/headers">{h.policy_name}</Link>{' '}
                  <span className="muted">({TARGET_LABEL[h.target] ?? h.target})</span>
                </>
              ) : (
                <span className="muted">No header policy applied; caller headers were forwarded unchanged.</span>
              )}
            </p>
            <div className="segmented" role="tablist" aria-label="Request headers">
              {(
                [
                  ['changes', `Changes (${h.changes.length})`],
                  ['inbound', `From caller (${h.inbound.length})`],
                  ['outbound', `To provider (${h.outbound.length})`],
                ] as [View, string][]
              ).map(([v, label]) => (
                <button key={v} type="button" role="tab" aria-selected={view === v} aria-pressed={view === v} onClick={() => setView(v)}>
                  {label}
                </button>
              ))}
            </div>
            {view === 'changes' ? (
              h.changes.length === 0 ? (
                <p className="small muted" style={{ margin: 0 }}>
                  The policy changed nothing on this request.
                </p>
              ) : (
                <table className="data">
                  <thead>
                    <tr>
                      <th scope="col">Header</th>
                      <th scope="col">Change</th>
                      <th scope="col">Rule</th>
                    </tr>
                  </thead>
                  <tbody>
                    {h.changes.map((c, i) => (
                      <tr key={`${c.name}-${i}`}>
                        <td className="mono small">{c.name}</td>
                        <td>
                          <Badge tone={CHANGE_TONE[c.action]}>{CHANGE_LABEL[c.action]}</Badge>
                        </td>
                        <td className="small muted">{c.rule > 0 ? `#${c.rule}` : 'allow-list'}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )
            ) : (
              <HeaderTable headers={view === 'inbound' ? h.inbound : h.outbound} />
            )}
          </div>
        );
      }}
    </AsyncSection>
  );
}

function HeaderTable({ headers }: { headers: LoggedHeader[] }): ReactNode {
  if (headers.length === 0) {
    return (
      <p className="small muted" style={{ margin: 0 }}>
        No headers.
      </p>
    );
  }
  return (
    <table className="data">
      <thead>
        <tr>
          <th scope="col">Header</th>
          <th scope="col">Value</th>
        </tr>
      </thead>
      <tbody>
        {headers.map((x) => (
          <tr key={x.name}>
            <td className="mono small" style={{ whiteSpace: 'nowrap' }}>
              {x.name}
            </td>
            <td className="mono small" style={{ wordBreak: 'break-all' }}>
              {x.values && x.values.length > 0 ? (
                <>
                  {x.values.join(', ')} {x.masked ? <Badge tone="neutral">masked</Badge> : null}
                </>
              ) : (
                <span className="muted">—</span>
              )}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
