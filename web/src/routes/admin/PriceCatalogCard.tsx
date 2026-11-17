import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import { formatDateTime } from '../../lib/format';
import { Badge, type Tone } from '../../components/ui';
import { useLocalOnly } from '../../app/session';

// Admin → Settings → License & updates → Price catalog: the signed catalog
// Janus prices new models from, and its opt-in daily refresh. Environment
// variables win over these settings; the card shows them locked.

const endpoint = '/api/v1/admin/price-catalog';

export interface PriceCatalogStatus {
  catalog?: {
    version: string;
    label: string;
    publisher: string;
    generated_at: string;
    origin: 'bundled' | 'downloaded';
    url?: string;
    key_id?: string;
    entries: number;
    defaults: number;
  };
  bundled?: { version: string; generated_at: string };
  bundled_error?: string;
  settings: {
    refresh_enabled: boolean;
    refresh_locked: boolean;
    refresh_source: 'env' | 'settings' | 'offline';
    url: string;
    url_locked: boolean;
    url_source: 'env' | 'settings' | 'default';
    require_signature: boolean;
    offline: boolean;
    offline_blocked: boolean;
  };
  saved: { refresh_enabled: boolean; url?: string };
  status: {
    checked_at?: string;
    result?: 'updated' | 'not_modified' | 'up_to_date' | 'error';
    error?: string;
    version?: string;
    applied_at?: string;
    applied_version?: string;
    models_changed?: number;
    lines_changed?: number;
  };
  default_url: string;
  trusted_keys: { id: string; source: 'built-in' | 'env' }[];
}

const RESULT: Record<string, { text: string; tone: Tone }> = {
  updated: { text: 'Updated', tone: 'success' },
  not_modified: { text: 'No change', tone: 'neutral' },
  up_to_date: { text: 'Up to date', tone: 'neutral' },
  error: { text: 'Failed', tone: 'danger' },
};

export function PriceCatalogCard(): ReactNode {
  const localOnly = useLocalOnly();
  const client = useQueryClient();
  const [url, setUrl] = useState<string | undefined>();
  const [message, setMessage] = useState('');
  const query = useQuery({
    queryKey: ['admin', 'price-catalog'],
    queryFn: () => api.get<PriceCatalogStatus>(endpoint),
    retry: false,
  });
  const refresh = () => void client.invalidateQueries({ queryKey: ['admin', 'price-catalog'] });
  const save = useMutation({
    mutationFn: (body: { refresh_enabled?: boolean; url?: string }) => api.patch(`${endpoint}/settings`, body),
    onSuccess: () => {
      setUrl(undefined);
      setMessage('Saved.');
      refresh();
    },
    onError: (failure: Error) => setMessage(failure.message),
  });
  const check = useMutation({
    mutationFn: () => api.post(`${endpoint}/check`, {}),
    onSuccess: () => {
      setMessage('');
      refresh();
      void client.invalidateQueries({ queryKey: ['admin', 'models'] });
    },
    onError: (failure: Error) => {
      setMessage(failure.message);
      refresh();
    },
  });
  // Local-only mode hides every price; an older server has no catalog.
  if (localOnly) return null;
  if (query.error instanceof ApiError && query.error.status === 404) return null;
  const data = query.data;
  const s = data?.settings;
  const shownURL = url ?? (s?.url_source === 'settings' ? s.url : '');
  const result = data?.status.result ? RESULT[data.status.result] : undefined;
  return (
    <section className="card" id="price-catalog" aria-labelledby="price-catalog-heading">
      <div className="card-header">
        <h2 id="price-catalog-heading">Price catalog</h2>
      </div>
      <div className="stack">
        <p className="small muted">
          Known prices per provider and model, and defaults per model type, each with the page it was read from. New models are
          priced from it. A price the provider quotes for a call, an administrator’s price and a provider’s published price always
          win over it.
        </p>
        {query.isError ? (
          <p role="alert" className="small">
            Price catalog settings are unavailable.
          </p>
        ) : null}
        {data && s ? (
          <>
            {data.catalog ? (
              <dl
                className="small"
                style={{
                  display: 'grid',
                  gridTemplateColumns: 'max-content minmax(0, 1fr)',
                  columnGap: 'var(--janus-space-3)',
                  rowGap: 'var(--janus-space-1)',
                  margin: 0,
                }}
                data-testid="price-catalog-in-use"
              >
                <dt>In use</dt>
                <dd>
                  <strong>{data.catalog.label}</strong>{' '}
                  <Badge tone="neutral">{data.catalog.origin === 'bundled' ? 'Built in' : 'Downloaded'}</Badge>
                </dd>
                <dt>Contents</dt>
                <dd>
                  {data.catalog.entries} models, {data.catalog.defaults} type defaults · generated{' '}
                  {formatDateTime(data.catalog.generated_at)}
                </dd>
                {data.catalog.origin === 'downloaded' ? (
                  <>
                    <dt>From</dt>
                    <dd className="mono">{data.catalog.url}</dd>
                  </>
                ) : null}
                <dt>Signed by</dt>
                <dd className="mono">{data.catalog.key_id || 'not checked'}</dd>
              </dl>
            ) : (
              <p role="alert" className="small">
                No price catalog is in use{data.bundled_error ? `: ${data.bundled_error}` : ''}.
              </p>
            )}
            {!s.require_signature ? (
              <div className="banner banner-warning" role="status">
                <p>
                  Signature checking is off (JANUS_PRICE_CATALOG_REQUIRE_SIGNATURE=false): a downloaded catalog is used without
                  proof of who published it.
                </p>
              </div>
            ) : null}
            <label className="row small" style={{ gap: 'var(--janus-space-2)', alignItems: 'center' }}>
              <input
                type="checkbox"
                aria-label="Refresh the price catalog daily"
                checked={s.refresh_enabled}
                disabled={s.refresh_locked || save.isPending}
                onChange={(event) => save.mutate({ refresh_enabled: event.target.checked })}
              />
              Refresh the price catalog daily
            </label>
            {s.offline_blocked ? (
              <p className="small muted">
                Offline mode (JANUS_OFFLINE) keeps the refresh off. Set JANUS_PRICE_CATALOG_URL to a catalog this site hosts to
                allow it.
              </p>
            ) : s.refresh_locked ? (
              <p className="small muted">Set by JANUS_PRICE_CATALOG_REFRESH.</p>
            ) : null}
            <form
              className="row wrap"
              style={{ gap: 'var(--janus-space-2)', alignItems: 'end' }}
              onSubmit={(event) => {
                event.preventDefault();
                save.mutate({ url: shownURL.trim() });
              }}
            >
              <label className="field" style={{ flex: '1 1 320px' }}>
                <span className="field-label">Catalog URL</span>
                <input
                  className="input mono"
                  aria-label="Catalog URL"
                  placeholder={data.default_url}
                  value={s.url_locked ? s.url : shownURL}
                  disabled={s.url_locked || save.isPending}
                  onChange={(event) => setUrl(event.target.value)}
                />
              </label>
              <button className="btn" type="submit" disabled={s.url_locked || url === undefined || save.isPending}>
                Save URL
              </button>
            </form>
            <p className="small muted">
              {s.url_locked
                ? 'Set by JANUS_PRICE_CATALOG_URL.'
                : 'Empty uses the Janus catalog. Its signature is read from the same URL with .sig added.'}{' '}
              Nothing about this gateway is sent.
            </p>
            <div className="row wrap" style={{ gap: 'var(--janus-space-3)', alignItems: 'center' }}>
              <button
                className="btn btn-sm"
                type="button"
                disabled={!s.refresh_enabled || check.isPending}
                onClick={() => check.mutate()}
              >
                {check.isPending ? 'Checking…' : 'Check now'}
              </button>
              <span className="small" data-testid="price-catalog-last-check">
                {data.status.checked_at ? (
                  <>
                    Last checked {formatDateTime(data.status.checked_at)}
                    {result ? (
                      <>
                        {' '}
                        <Badge tone={result.tone}>{result.text}</Badge>
                      </>
                    ) : null}
                    {data.status.result === 'updated' && data.status.applied_version
                      ? ` · ${data.status.applied_version} applied, ${data.status.lines_changed ?? 0} prices changed on ${data.status.models_changed ?? 0} models`
                      : null}
                  </>
                ) : (
                  'Never checked'
                )}
              </span>
            </div>
            {data.status.error ? (
              <p className="small" style={{ color: 'var(--janus-color-danger-fg)' }}>
                {data.status.error}
              </p>
            ) : null}
            {message ? (
              <p className="small" role="status">
                {message}
              </p>
            ) : null}
          </>
        ) : null}
      </div>
    </section>
  );
}
