import { useMemo, useState, type ReactNode } from 'react';
import { sortCollection } from '../lib/collections';
import { Link } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import { isRenamedModel, modelHealth, publicModelName, type ManagedModel, type Model, type ModelHealth } from '../lib/types';
import { formatErrorRate, formatModelRate, formatRelative, formatTokenCount, healthTone, titleCase } from '../lib/format';
import { AsyncSection, Badge, CodeBlock, Drawer, EmptyState } from '../components/ui';
import { useDebounced, useUrlState } from '../lib/hooks';
import { useLocalOnly, useMe } from '../app/session';
import { t } from '../lib/i18n';
import { DetailRow, FilterSelect, SearchInput } from './shared';

export function ModelsPage(): ReactNode {
  const me = useMe();
  // Local-only mode: per-model rates are a pricing surface, so the catalog
  // hides them (and the price sort) while access/modality info stays.
  const localOnly = useLocalOnly();
  const [search, setSearch] = useUrlState('q', '');
  const [upstream, setUpstream] = useUrlState('upstream', '');
  const [modality, setModality] = useUrlState('modality', '');
  const [sort, setSort] = useUrlState<'name' | 'cost'>('sort', 'name');
  const [view, setView] = useUrlState<'cards' | 'table'>('view', 'cards');
  const [detail, setDetail] = useState<Model | null>(null);
  const debounced = useDebounced(search);

  const models = useQuery({
    queryKey: ['models'],
    queryFn: () => api.get<{ models: Model[]; managed_models: ManagedModel[] }>('/api/v1/models'),
  });

  // Managed models are rendered as their own cards above the real catalog.
  // They are aliases an admin repoints over time, so the card states plainly
  // what the alias currently resolves to — the point is to spare the user the
  // bookkeeping, not to hide which model they are talking to.
  const managedModels = useMemo(() => {
    const term = debounced.trim().toLowerCase();
    return (models.data?.managed_models ?? []).filter((mm) => {
      if (upstream && mm.target_upstream_name !== upstream) return false;
      if (modality && !mm.modalities.includes(modality)) return false;
      if (
        term &&
        !mm.name.toLowerCase().includes(term) &&
        !mm.description.toLowerCase().includes(term) &&
        !mm.target_public_name.toLowerCase().includes(term)
      ) {
        return false;
      }
      return true;
    });
  }, [models.data, debounced, upstream, modality]);

  const upstreams = useMemo(() => {
    const names = new Set([
      ...(models.data?.models ?? []).map((model) => model.upstream_name),
      ...(models.data?.managed_models ?? []).map((model) => model.target_upstream_name),
    ]);
    return [...names].sort();
  }, [models.data]);

  // Only the modalities actually detected on this gateway are offered, so
  // the dropdown never lists kinds no model here serves. Managed aliases
  // count too: an alias to an image model must be reachable under "Image"
  // even when no plain image model is granted.
  const modalities = useMemo(() => {
    const values = new Set([
      ...(models.data?.models ?? []).flatMap((model) => model.modalities),
      ...(models.data?.managed_models ?? []).flatMap((mm) => mm.modalities),
    ]);
    return [...values].sort();
  }, [models.data]);

  const filtered = useMemo(() => {
    const term = debounced.trim().toLowerCase();
    const list = (models.data?.models ?? []).filter((model) => {
      if (upstream && model.upstream_name !== upstream) return false;
      if (modality && !model.modalities.includes(modality)) return false;
      if (
        term &&
        !model.name.toLowerCase().includes(term) &&
        !model.display_name.toLowerCase().includes(term) &&
        !model.upstream_name.toLowerCase().includes(term)
      ) {
        return false;
      }
      return true;
    });
    return [...list].sort((a, b) =>
      sort === 'cost' ? b.rate_in_nanousd - a.rate_in_nanousd : publicModelName(a).localeCompare(publicModelName(b)),
    );
  }, [models.data, debounced, upstream, modality, sort]);

  // One ordered row model drives both presentations. Alias prices are not
  // included in the catalog contract, so they sort as unknown, never as free.
  const catalog = sortCollection<Model | ManagedModel>(
    [...filtered, ...managedModels],
    (row) =>
      sort === 'cost' && !localOnly
        ? 'rate_in_nanousd' in row
          ? row.rate_in_nanousd
          : null
        : 'target_model_id' in row
          ? row.name
          : publicModelName(row),
    sort !== 'cost' || localOnly,
  );

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('models.title')}</h1>
          <p className="page-subtitle">{t('models.subtitle')}</p>
        </div>
        <div className="segmented" role="group" aria-label={t('models.view')}>
          <button type="button" aria-pressed={view === 'cards'} onClick={() => setView('cards')}>
            {t('models.cards')}
          </button>
          <button type="button" aria-pressed={view === 'table'} onClick={() => setView('table')}>
            {t('models.table')}
          </button>
        </div>
      </header>

      <div className="row wrap">
        <SearchInput value={search} onChange={setSearch} placeholder={t('models.searchLabel')} label={t('models.searchLabel')} />
        <FilterSelect
          label={t('adminModels.colUpstream')}
          value={upstream}
          onChange={setUpstream}
          options={[
            { value: '', label: t('adminModels.allUpstreams') },
            ...upstreams.map((name) => ({ value: name, label: name })),
          ]}
        />
        <FilterSelect
          label={t('tables.modality')}
          value={modality}
          onChange={setModality}
          options={[
            { value: '', label: t('requests.allModalities') },
            ...modalities.map((value) => ({ value, label: titleCase(value) })),
          ]}
        />
        <FilterSelect
          label={t('models.sort')}
          value={sort}
          onChange={(next) => setSort(next as typeof sort)}
          options={[
            { value: 'name', label: t('models.sortName') },
            ...(localOnly ? [] : [{ value: 'cost', label: t('models.sortCost') }]),
          ]}
        />
      </div>

      <AsyncSection
        query={models}
        empty={{
          when: (data) => data.models.length === 0 && (data.managed_models?.length ?? 0) === 0,
          title: t('models.emptyTitle'),
          body: t('models.emptyBody'),
          action: (
            <Link className="btn" to="/help">
              {t('models.howAccessWorks')}
            </Link>
          ),
        }}
      >
        {() =>
          filtered.length === 0 && managedModels.length === 0 ? (
            <EmptyState
              title={t('models.noMatchTitle')}
              body={t('models.noMatchBody')}
              action={
                <button
                  type="button"
                  className="btn"
                  onClick={() => {
                    setSearch('');
                    setUpstream('');
                    setModality('');
                  }}
                >
                  {t('tables.clearFilters')}
                </button>
              }
            />
          ) : view === 'cards' ? (
            <div className="grid grid-tiles">
              {catalog.map((model) =>
                'target_model_id' in model ? (
                  <ManagedModelCard key={`alias-${model.id}`} model={model} />
                ) : (
                  <ModelCard key={model.id} model={model} localOnly={localOnly} onOpen={() => setDetail(model)} />
                ),
              )}
            </div>
          ) : (
            <section className="card card-flush">
              <div className="table-wrap">
                <table className="data">
                  <thead>
                    <tr>
                      <th scope="col">{t('tables.model')}</th>
                      <th scope="col">Type / target</th>
                      <th scope="col">{t('adminModels.colUpstream')}</th>
                      <th scope="col">{t('models.colModalities')}</th>
                      {localOnly ? null : (
                        <>
                          <th scope="col">{t('models.input')}</th>
                          <th scope="col">{t('models.output')}</th>
                        </>
                      )}
                      <th scope="col">{t('models.contextWindow')}</th>
                      <th scope="col">{t('models.colHealth')}</th>
                      <th scope="col">{t('models.colErrorRate')}</th>
                      <th scope="col">{t('models.colAccess')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {catalog.map((model) =>
                      'target_model_id' in model ? (
                        <tr key={`alias-${model.id}`}>
                          <td>
                            {model.name}
                            <div className="small muted">{model.description}</div>
                          </td>
                          <td>
                            <Badge tone="primary">Managed alias</Badge>
                            <div className="small">{model.target_public_name || '—'}</div>
                          </td>
                          <td>{model.target_upstream_name}</td>
                          <td>{model.modalities.map(titleCase).join(', ')}</td>
                          {localOnly ? null : (
                            <>
                              <td title="Billed at target rates">—</td>
                              <td title="Billed at target rates">—</td>
                            </>
                          )}
                          <td className="num">{formatTokenCount(model.context_window)}</td>
                          <td>
                            <Badge tone={model.servable ? 'success' : 'danger'}>
                              {model.servable ? t('models.managedServing') : t('models.managedUnavailable')}
                            </Badge>
                          </td>
                          <td>—</td>
                          <td>{grantSourceLabel(model.grant_source)}</td>
                        </tr>
                      ) : (
                        <tr
                          key={model.id}
                          data-clickable="true"
                          data-health={modelHealth(model)}
                          onClick={() => setDetail(model)}
                          tabIndex={0}
                          onKeyDown={(keyEvent) => {
                            if (keyEvent.key === 'Enter') setDetail(model);
                          }}
                        >
                          <td
                            title={isRenamedModel(model) ? t('adminModels.upstreamNameTitle', { name: model.name }) : undefined}
                          >
                            <div>{publicModelName(model)}</div>
                            {isRenamedModel(model) ? <div className="small muted mono">{model.name}</div> : null}
                          </td>
                          <td>
                            <Badge tone="neutral">Physical model</Badge>
                          </td>
                          <td className="small muted">{model.upstream_name}</td>
                          <td className="small">{model.modalities.map(titleCase).join(', ')}</td>
                          {localOnly ? null : (
                            <>
                              <td className="num small">{formatModelRate(model, 'rate_in_nanousd')}</td>
                              <td className="num small">{formatModelRate(model, 'rate_out_nanousd')}</td>
                            </>
                          )}
                          <td className="num small">{formatTokenCount(model.context_window)}</td>
                          <td>
                            <ModelHealthBadge model={model} />
                          </td>
                          <td className="num small" title={errorRateDetail(model)}>
                            {(model.request_count_10m ?? 0) > 0 ? formatErrorRate(model.error_rate_percent) : '—'}
                          </td>
                          <td className="small muted">{grantSourceLabel(model.grant_source)}</td>
                        </tr>
                      ),
                    )}
                  </tbody>
                </table>
              </div>
            </section>
          )
        }
      </AsyncSection>

      <Drawer
        open={Boolean(detail)}
        onClose={() => setDetail(null)}
        title={detail ? publicModelName(detail) : t('models.drawerFallbackTitle')}
      >
        {detail ? (
          <>
            <div className="stack">
              {isRenamedModel(detail) ? (
                <DetailRow label={t('models.upstreamModelName')}>
                  <span className="mono">{detail.name}</span>
                </DetailRow>
              ) : null}
              <DetailRow label={t('models.health')}>
                <ModelHealthBadge model={detail} />
              </DetailRow>
              <DetailRow label={t('models.errorRate10m')}>{errorRateDetail(detail)}</DetailRow>
              <DetailRow label={t('models.upstreamLatency')}>{upstreamProbeLabel(detail)}</DetailRow>
              <DetailRow label={t('adminModels.colUpstream')}>{detail.upstream_name}</DetailRow>
              <DetailRow label={t('models.providerType')}>{detail.adapter_type}</DetailRow>
              <DetailRow label={t('models.colModalities')}>{detail.modalities.map(titleCase).join(', ')}</DetailRow>
              <DetailRow label={t('models.contextWindow')}>{formatTokenCount(detail.context_window)}</DetailRow>
              {localOnly ? null : (
                <>
                  <DetailRow label={t('models.inputRate')}>{formatModelRate(detail, 'rate_in_nanousd')}</DetailRow>
                  <DetailRow label={t('models.outputRate')}>{formatModelRate(detail, 'rate_out_nanousd')}</DetailRow>
                  <DetailRow label={t('models.cachedInputRate')}>{formatModelRate(detail, 'rate_cached_nanousd')}</DetailRow>
                </>
              )}
              <DetailRow label={t('models.yourAccess')}>{grantSourceLabel(detail.grant_source)}</DetailRow>
            </div>
            <section>
              <h3 style={{ marginBottom: 'var(--janus-space-3)' }}>{t('models.useThisModel')}</h3>
              <CodeBlock
                language="bash"
                code={`curl ${me?.endpoint ?? '/v1'}/chat/completions \\
  -H "Authorization: Bearer $JANUS_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${publicModelName(detail)}",
    "messages": [{"role": "user", "content": "Hello"}]
  }'`}
              />
              <p className="small muted" style={{ marginTop: 'var(--janus-space-3)' }}>
                {t('models.snippetHelpPrefix')} <Link to="/help">{t('models.snippetHelpLink')}</Link>{' '}
                {t('models.snippetHelpSuffix')}
              </p>
            </section>
          </>
        ) : null}
      </Drawer>
    </div>
  );
}

/**
 * One catalog card. The health verdict drives the card's accent rail and, when
 * the model is down, a tinted background plus an explicit alert line — a
 * user should never have to open the card to learn that requests will fail.
 * Rates are a cost surface and disappear in local-only mode; health, context
 * window, and access do not.
 */
function ModelCard({ model, localOnly, onOpen }: { model: Model; localOnly: boolean; onOpen: () => void }): ReactNode {
  const health = modelHealth(model);
  const requests = model.request_count_10m ?? 0;
  const hasTraffic = requests > 0;
  const errorTone = hasTraffic ? healthTone(health === 'unknown' ? 'healthy' : health) : 'neutral';
  return (
    <article
      className="tile model-card"
      data-health={health}
      style={{ cursor: 'pointer' }}
      role="button"
      tabIndex={0}
      aria-label={publicModelName(model)}
      onClick={onOpen}
      onKeyDown={(keyEvent) => {
        if (keyEvent.key === 'Enter' || keyEvent.key === ' ') {
          keyEvent.preventDefault();
          onOpen();
        }
      }}
    >
      <div className="model-card-head">
        <div className="stack" style={{ gap: 2, minWidth: 0 }}>
          <span className="overline">{model.upstream_name}</span>
          <h2
            className="model-card-title"
            title={isRenamedModel(model) ? t('adminModels.upstreamNameTitle', { name: model.name }) : undefined}
          >
            {publicModelName(model)}
          </h2>
          {isRenamedModel(model) ? <div className="small muted mono">{model.name}</div> : null}
        </div>
        <ModelHealthBadge model={model} />
      </div>

      {health === 'down' ? (
        <div className="model-card-alert" role="status">
          <span aria-hidden="true">⚠</span>
          <span>
            {t('models.healthDownBanner')}
            {model.upstream_last_error ? (
              <>
                {' '}
                <span className="mono">{model.upstream_last_error}</span>
              </>
            ) : null}
          </span>
        </div>
      ) : null}

      <div className="row wrap" style={{ gap: 6 }}>
        <Badge tone="neutral">{model.adapter_type}</Badge>
        {model.modalities.map((value) => (
          <Badge key={value} tone="primary">
            {titleCase(value)}
          </Badge>
        ))}
      </div>

      <dl className="model-card-stats">
        <div className="model-card-stat" data-tone={errorTone}>
          <dt>{t('models.errorRate10m')}</dt>
          <dd title={errorRateDetail(model)}>{hasTraffic ? formatErrorRate(model.error_rate_percent) : '—'}</dd>
        </div>
        <div className="model-card-stat">
          <dt>{t('models.requests10m')}</dt>
          <dd>{hasTraffic ? requests.toLocaleString() : '0'}</dd>
        </div>
        {localOnly ? null : (
          <>
            <div className="model-card-stat">
              <dt>{t('models.input')}</dt>
              <dd>{formatModelRate(model, 'rate_in_nanousd')}</dd>
            </div>
            <div className="model-card-stat">
              <dt>{t('models.output')}</dt>
              <dd>{formatModelRate(model, 'rate_out_nanousd')}</dd>
            </div>
          </>
        )}
        <div className="model-card-stat">
          <dt>{t('models.contextWindow')}</dt>
          <dd>{formatTokenCount(model.context_window)}</dd>
        </div>
        <div className="model-card-stat" data-tone={model.upstream_reachable ? 'success' : undefined}>
          <dt>{t('models.upstreamLatency')}</dt>
          <dd>{upstreamProbeLabel(model)}</dd>
        </div>
      </dl>

      <div className="model-card-foot">
        <span>{t('models.accessVia', { name: grantSourceLabel(model.grant_source) })}</span>
        <span>{probeAgeLabel(model)}</span>
      </div>
    </article>
  );
}

/**
 * A managed model (alias) card. Same silhouette as ModelCard — head, badges,
 * 2-column stat grid, footer — so the two kinds scan identically in the grid.
 * We help, we do not hide: the current target is the first stat, and the
 * "may change / usage is billed to the target" explanation lives in a tooltip
 * on the footer instead of a paragraph in the card body.
 */
function ManagedModelCard({ model: mm }: { model: ManagedModel }): ReactNode {
  return (
    <article className="tile model-card" data-managed="true" data-health={mm.broken ? 'down' : undefined} aria-label={mm.name}>
      <div className="model-card-head">
        <div className="stack" style={{ gap: 2, minWidth: 0 }}>
          <span className="overline">{t('models.managedOverline')}</span>
          <h2 className="model-card-title">{mm.name}</h2>
        </div>
        <Badge tone={mm.servable ? 'success' : 'danger'} dot>
          {mm.servable ? t('models.managedServing') : t('models.managedUnavailable')}
        </Badge>
      </div>

      {mm.description ? (
        <p className="small muted model-card-description" title={mm.description}>
          {mm.description}
        </p>
      ) : null}

      {mm.broken ? (
        <div className="model-card-alert" role="status">
          <span aria-hidden="true">⚠</span>
          <span>
            {t('models.managedBrokenBanner')}
            {mm.broken_reason ? (
              <>
                {' '}
                <span className="mono">{mm.broken_reason}</span>
              </>
            ) : null}
          </span>
        </div>
      ) : null}

      <div className="row wrap" style={{ gap: 6 }}>
        {mm.modalities.map((value) => (
          <Badge key={value} tone="primary">
            {titleCase(value)}
          </Badge>
        ))}
      </div>

      <dl className="model-card-stats">
        <div className="model-card-stat model-card-stat-wide" data-tone="primary">
          <dt>{t('models.currentlyUses')}</dt>
          <dd title={mm.target_public_name}>{mm.target_public_name}</dd>
        </div>
        <div className="model-card-stat">
          <dt>{t('adminModels.colUpstream')}</dt>
          <dd>{mm.target_upstream_name}</dd>
        </div>
        <div className="model-card-stat">
          <dt>{t('models.contextWindow')}</dt>
          <dd>{formatTokenCount(mm.context_window)}</dd>
        </div>
      </dl>

      <div className="model-card-foot">
        <span>{t('models.accessVia', { name: grantSourceLabel(mm.grant_source) })}</span>
        <span title={t('models.managedExplainer')}>{t('models.managedFootnote')}</span>
      </div>
    </article>
  );
}

/**
 * The health badge shared by cards, table rows and the detail drawer. It
 * mirrors the admin Upstreams page: a green dot with the probe latency when
 * the upstream answered, red when it did not, amber for a degraded error
 * rate, neutral when there is nothing to judge yet.
 */
function ModelHealthBadge({ model }: { model: Model }): ReactNode {
  const health = modelHealth(model);
  const latency =
    model.upstream_reachable && model.upstream_last_latency_ms !== undefined ? model.upstream_last_latency_ms : null;
  const label = healthLabel(health);
  return (
    <Badge tone={healthTone(health)} dot={health !== 'unknown'}>
      {label}
      {health === 'healthy' && latency !== null ? ` · ${t('models.healthLatency', { ms: latency })}` : null}
      {health === 'degraded' && (model.request_count_10m ?? 0) > 0 ? ` · ${formatErrorRate(model.error_rate_percent)}` : null}
    </Badge>
  );
}

function healthLabel(health: ModelHealth): string {
  switch (health) {
    case 'healthy':
      return t('models.healthy');
    case 'degraded':
      return t('models.degraded');
    case 'down':
      return t('models.down');
    default:
      return t('models.healthUnknown');
  }
}

/** "3 of 40 requests failed in the last 10 minutes" — or the no-traffic line. */
function errorRateDetail(model: Model): string {
  const requests = model.request_count_10m ?? 0;
  if (requests <= 0) return t('models.noRecentRequests');
  return t('models.errorRateDetail', { errors: model.error_count_10m ?? 0, requests });
}

/** Probe latency when reachable, otherwise why not. */
function upstreamProbeLabel(model: Model): string {
  if (model.upstream_reachable) return t('models.healthLatency', { ms: model.upstream_last_latency_ms ?? 0 });
  if (model.upstream_last_error) return t('models.healthUnreachable');
  return t('models.healthNotChecked');
}

function probeAgeLabel(model: Model): string {
  const at = model.upstream_last_check_at;
  if (!at || at.startsWith('0001-01-01')) return t('models.healthNotChecked');
  return t('models.healthLastChecked', { when: formatRelative(at) });
}

function grantSourceLabel(source: string | undefined): string {
  switch (source) {
    case 'direct':
      return t('models.grantDirect');
    case 'group':
      return t('models.grantGroup');
    case 'all users':
      return t('models.grantAllUsers');
    default:
      return t('models.grantDefault');
  }
}
