import { useMemo, useState, type ReactNode } from 'react';
import { sortCollection } from '../lib/collections';
import { Link } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import {
  isRenamedModel,
  modelHealth,
  publicModelName,
  type ManagedModel,
  type Model,
  type ModelActivity,
  type ModelHealth,
  type ModelPerformanceSummary,
  type PersonalCatalogModel,
} from '../lib/types';
import {
  formatDuration,
  formatErrorRate,
  formatModelRate,
  formatNumber,
  formatRelative,
  formatTokenCount,
  healthTone,
  modalityLabel,
} from '../lib/format';
import { Sparkline } from '../components/charts';
import { AsyncSection, Badge, CodeBlock, Drawer, EmptyState } from '../components/ui';
import { useDebounced, useUrlState } from '../lib/hooks';
import { useLocalOnly, useMe } from '../app/session';
import { t } from '../lib/i18n';
import { capabilityBadges, knownCapabilities } from '../lib/capabilities';
import { CapabilityMeasure } from './admin/ModelCapabilities';
import { PlanCapabilities } from './PlanCapabilities';
import { DetailRow, FilterSelect, SearchInput } from './shared';

type SortKey = 'name' | 'cheap' | 'cost' | 'speed' | 'used';
type CatalogEntry = Model | ManagedModel;

function isManaged(entry: CatalogEntry): entry is ManagedModel {
  return 'target_model_id' in entry;
}

function entryName(entry: CatalogEntry): string {
  return isManaged(entry) ? entry.name : publicModelName(entry);
}

/** Capability chips on a card; the rest are named in the "+N" chip's tooltip. */
const MAX_CARD_CHIPS = 3;

export function ModelsPage(): ReactNode {
  const me = useMe();
  // Local-only mode: per-model rates are a pricing surface, so the catalog
  // hides them (and the price sorts) while access/modality info stays.
  const localOnly = useLocalOnly();
  const [search, setSearch] = useUrlState('q', '');
  const [upstream, setUpstream] = useUrlState('upstream', '');
  const [modality, setModality] = useUrlState('modality', '');
  const [sortParam, setSort] = useUrlState<SortKey>('sort', 'name');
  const [view, setView] = useUrlState<'cards' | 'table'>('view', 'cards');
  const [detail, setDetail] = useState<CatalogEntry | null>(null);
  const [planDetail, setPlanDetail] = useState<PersonalCatalogModel | null>(null);
  const debounced = useDebounced(search);
  const sort: SortKey = localOnly && (sortParam === 'cost' || sortParam === 'cheap') ? 'name' : sortParam;

  const models = useQuery({
    queryKey: ['models'],
    queryFn: () =>
      api.get<{ models: Model[]; managed_models: ManagedModel[]; personal_models?: PersonalCatalogModel[] }>('/api/v1/models'),
  });

  // A managed model's price is its target's price. The alias contract carries
  // no rates, so the target is looked up in the user's own catalog; a target
  // the user cannot see directly reads as "target's rates", never as free.
  const byId = useMemo(() => new Map((models.data?.models ?? []).map((model) => [model.id, model])), [models.data]);
  const targetOf = (mm: ManagedModel): Model | undefined => byId.get(mm.target_model_id);

  // Managed models are aliases an admin repoints over time; their card states
  // plainly what the alias currently resolves to.
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
    return (models.data?.models ?? []).filter((model) => {
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
  }, [models.data, debounced, upstream, modality]);

  // Each section is ordered by the same key; unknown values (an unpriced
  // model, an unmeasured speed) always sort last, never as zero.
  const order = <T extends CatalogEntry>(rows: T[]): T[] =>
    sortCollection<T>(
      rows,
      (row) => {
        const entry: CatalogEntry = row;
        const priced: Model | undefined = isManaged(entry) ? targetOf(entry) : entry;
        const activity = row.activity;
        switch (sort) {
          case 'cost':
          case 'cheap':
            return priced ? knownRate(priced) : null;
          case 'speed':
            return activity && activity.tokens_per_second_7d > 0 ? activity.tokens_per_second_7d : null;
          case 'used':
            return activity ? activity.jobs_per_day_7d : null;
          default:
            return entryName(row);
        }
      },
      sort === 'name' || sort === 'cheap',
    );
  // The caller's own plans: paid by them, never filtered by upstream (they
  // have none); the search and kind filters still apply.
  const personal = useMemo(() => {
    const term = debounced.trim().toLowerCase();
    if (upstream) return [];
    return (models.data?.personal_models ?? [])
      .filter((m) => {
        const mods = m.janus?.modalities ?? [];
        if (modality && !mods.includes(modality)) return false;
        return !term || m.id.toLowerCase().includes(term) || m.owned_by.toLowerCase().includes(term);
      })
      .sort((a, b) => a.id.localeCompare(b.id));
  }, [models.data, debounced, upstream, modality]);
  const managedSorted = order(managedModels);
  const modelsSorted = order(filtered);
  const catalog: CatalogEntry[] = [...managedSorted, ...modelsSorted];
  const sectioned = managedSorted.length > 0 && modelsSorted.length > 0;

  const renderCard = (entry: CatalogEntry) =>
    isManaged(entry) ? (
      <ManagedModelCard
        key={`alias-${entry.id}`}
        model={entry}
        target={targetOf(entry)}
        localOnly={localOnly}
        onOpen={() => setDetail(entry)}
      />
    ) : (
      <ModelCard key={entry.id} model={entry} localOnly={localOnly} onOpen={() => setDetail(entry)} />
    );

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('models.title')}</h1>
          <p className="page-subtitle">{t('models.subtitle')}</p>
        </div>
      </header>

      <div className="models-toolbar">
        <div className="models-toolbar-search">
          <SearchInput
            value={search}
            onChange={setSearch}
            placeholder={t('models.searchLabel')}
            label={t('models.searchLabel')}
          />
        </div>
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
            ...modalities.map((value) => ({ value, label: modalityLabel(value) })),
          ]}
        />
        <FilterSelect
          label={t('models.sort')}
          value={sort}
          onChange={(next) => setSort(next as SortKey)}
          options={[
            { value: 'name', label: t('models.sortName') },
            ...(localOnly
              ? []
              : [
                  { value: 'cheap', label: t('models.sortCheap') },
                  { value: 'cost', label: t('models.sortCost') },
                ]),
            { value: 'speed', label: t('models.sortSpeed') },
            { value: 'used', label: t('models.sortUsed') },
          ]}
        />
        <div className="segmented models-view" role="group" aria-label={t('models.view')}>
          <button type="button" aria-pressed={view === 'cards'} onClick={() => setView('cards')}>
            {t('models.cards')}
          </button>
          <button type="button" aria-pressed={view === 'table'} onClick={() => setView('table')}>
            {t('models.table')}
          </button>
        </div>
      </div>

      <AsyncSection
        query={models}
        empty={{
          when: (data) =>
            data.models.length === 0 && (data.managed_models?.length ?? 0) === 0 && (data.personal_models?.length ?? 0) === 0,
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
          catalog.length === 0 && personal.length === 0 ? (
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
            <>
              {sectioned ? (
                <>
                  <CatalogSection
                    id="managed"
                    title={t('models.managedSection')}
                    hint={t('models.managedSectionHint')}
                    count={managedSorted.length}
                  >
                    {managedSorted.map(renderCard)}
                  </CatalogSection>
                  <CatalogSection id="models" title={t('models.modelsSection')} count={modelsSorted.length}>
                    {modelsSorted.map(renderCard)}
                  </CatalogSection>
                </>
              ) : catalog.length > 0 && personal.length > 0 ? (
                <CatalogSection id="models" title={t('models.modelsSection')} count={catalog.length}>
                  {catalog.map(renderCard)}
                </CatalogSection>
              ) : catalog.length > 0 ? (
                <div className="model-grid">{catalog.map(renderCard)}</div>
              ) : null}
              {personal.length > 0 ? (
                <CatalogSection
                  id="plans"
                  title="On your own plans"
                  hint="Models from plans you connected on the Subscriptions page. Your plan pays for them."
                  count={personal.length}
                >
                  {personal.map((m) => (
                    <PersonalModelCard key={m.id} model={m} onOpen={() => setPlanDetail(m)} />
                  ))}
                </CatalogSection>
              ) : null}
            </>
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
                      <th scope="col" className="num">
                        {t('models.jobsPerDay')}
                      </th>
                      <th scope="col" className="num">
                        {t('models.speed')}
                      </th>
                      <th scope="col">{t('models.colAccess')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {catalog.map((model) =>
                      isManaged(model) ? (
                        <tr
                          key={`alias-${model.id}`}
                          data-clickable="true"
                          onClick={() => setDetail(model)}
                          tabIndex={0}
                          onKeyDown={(keyEvent) => {
                            if (keyEvent.key === 'Enter') setDetail(model);
                          }}
                        >
                          <td>
                            {model.name}
                            <div className="small muted">{model.description}</div>
                          </td>
                          <td>
                            <Badge tone="primary">Managed alias</Badge>
                            <div className="small">{model.target_public_name || '—'}</div>
                          </td>
                          <td>{model.target_upstream_name}</td>
                          <td>{model.modalities.map(modalityLabel).join(', ')}</td>
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
                          <ActivityCells activity={model.activity} />
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
                          <td className="small">{model.modalities.map(modalityLabel).join(', ')}</td>
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
                          <ActivityCells activity={model.activity} />
                          <td className="small muted">{grantSourceLabel(model.grant_source)}</td>
                        </tr>
                      ),
                    )}
                    {personal.map((m) => (
                      <tr
                        key={`plan-${m.id}`}
                        data-clickable="true"
                        onClick={() => setPlanDetail(m)}
                        tabIndex={0}
                        onKeyDown={(keyEvent) => {
                          if (keyEvent.key === 'Enter') setPlanDetail(m);
                        }}
                      >
                        <td className="mono">{m.id}</td>
                        <td>
                          <Badge tone="neutral">Your plan</Badge>
                        </td>
                        <td className="small muted">{m.owned_by}</td>
                        <td className="small">{(m.janus?.modalities ?? []).map(modalityLabel).join(', ')}</td>
                        {localOnly ? null : (
                          <>
                            <td className="small muted">Your plan</td>
                            <td className="small muted">Your plan</td>
                          </>
                        )}
                        <td className="num small">—</td>
                        <td>—</td>
                        <td>—</td>
                        <td className="num">—</td>
                        <td className="num">—</td>
                        <td className="small muted">Your own plan</td>
                      </tr>
                    ))}
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
        title={detail ? entryName(detail) : t('models.drawerFallbackTitle')}
      >
        {detail === null ? null : isManaged(detail) ? (
          <ManagedModelDetail model={detail} target={targetOf(detail)} localOnly={localOnly} endpoint={me?.endpoint} />
        ) : (
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
              <DetailRow label={t('models.jobsPerDay')}>
                <JobsPerDay activity={detail.activity} />
              </DetailRow>
              <DetailRow label={t('models.speed')}>
                {detail.activity && detail.activity.tokens_per_second_7d > 0
                  ? `${t('models.speedValue', { value: formatSpeed(detail.activity.tokens_per_second_7d) })} · ${t('models.speedDetail', { count: detail.activity.speed_samples_7d.toLocaleString() })}`
                  : t('models.speedNone')}
              </DetailRow>
              <ResponseFigures id={detail.id} />
              <DetailRow label={t('models.upstreamLatency')}>
                {upstreamProbeLabel(detail)}
                <div className="small muted">{probeAgeLabel(detail)}</div>
              </DetailRow>
              <DetailRow label={t('adminModels.colUpstream')}>{detail.upstream_name}</DetailRow>
              <DetailRow label={t('models.providerType')}>{detail.adapter_type}</DetailRow>
              <DetailRow label={t('models.colModalities')}>{detail.modalities.map(modalityLabel).join(', ')}</DetailRow>
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
            <section className="stack" data-testid="model-capabilities">
              <h3 style={{ margin: 0 }}>What it can do</h3>
              {knownCapabilities(detail.capabilities).length === 0 ? (
                <p className="small muted">Nothing is known yet about what this model takes, gives or can be asked.</p>
              ) : (
                knownCapabilities(detail.capabilities).map((group) => (
                  <div key={group.group} className="stack" style={{ gap: 'var(--janus-space-1)' }}>
                    <h4 className="overline" style={{ margin: 0 }}>
                      {group.group}
                    </h4>
                    {group.items.map((item) => (
                      <DetailRow key={item.key} label={item.label}>
                        {item.value}
                      </DetailRow>
                    ))}
                  </div>
                ))
              )}
              {me?.role === 'admin' ? <CapabilityMeasure model={detail} /> : null}
            </section>
            <UseThisModel name={publicModelName(detail)} endpoint={me?.endpoint} />
          </>
        )}
      </Drawer>

      <Drawer
        open={planDetail !== null}
        onClose={() => setPlanDetail(null)}
        title={planDetail?.id ?? ''}
        description={planDetail ? `Billed to your ${planDetail.owned_by} plan.` : undefined}
      >
        {planDetail ? (
          <>
            {planDetail.janus?.personal_subscription ? (
              <PlanCapabilities
                mode="view"
                subscriptionId={planDetail.janus.personal_subscription.subscription_id}
                model={planDetail.janus.native_name || planDetail.id.split('/').slice(2).join('/')}
              />
            ) : null}
            <UseThisModel name={planDetail.id} endpoint={me?.endpoint} />
          </>
        ) : null}
      </Drawer>
    </div>
  );
}

/** One of the caller's own plan models: name, whose plan, what it takes. */
function PersonalModelCard({ model, onOpen }: { model: PersonalCatalogModel; onOpen: () => void }): ReactNode {
  return (
    <CardShell name={model.id} health={undefined} dotLabel="Your own plan" meta={<span>{model.owned_by}</span>} onOpen={onOpen}>
      <CardChips modalities={model.janus?.modalities ?? []} capabilities={capabilityBadges(model.janus?.capabilities)} />
      <p className="small muted" style={{ margin: 0 }}>
        Billed to your plan
      </p>
    </CardShell>
  );
}

/** A titled group of cards ("Managed models", "Models") with its count. */
function CatalogSection({
  id,
  title,
  hint,
  count,
  children,
}: {
  id: string;
  title: string;
  hint?: string;
  count: number;
  children: ReactNode;
}): ReactNode {
  const headingId = `models-section-${id}`;
  return (
    <section className="model-section" aria-labelledby={headingId}>
      <div className="model-section-head">
        <h2 id={headingId}>
          {title} <span className="model-section-count">{count}</span>
        </h2>
        {hint ? <p className="small muted">{hint}</p> : null}
      </div>
      <div className="model-grid">{children}</div>
    </section>
  );
}

/**
 * The shell both card kinds share: a health dot beside the name (the dot is a
 * fixed 10 px, so it never squeezes a long name), a one-line "who · what"
 * line, capability chips, and one quiet row of the facts people choose on.
 * The health verdict also drives the accent rail; a down model keeps its
 * tinted background and alert line, so trouble is visible without opening it.
 */
function CardShell({
  name,
  nameTitle,
  subtitle,
  health,
  dotTone,
  dotLabel,
  managed,
  meta,
  onOpen,
  children,
}: {
  name: string;
  nameTitle?: string;
  /** The upstream's own name when an admin renamed the model; search matches it too. */
  subtitle?: string;
  health: ModelHealth | undefined;
  /** Defaults to the health tone; a managed model's dot follows whether it can serve. */
  dotTone?: 'success' | 'danger';
  dotLabel: string;
  managed?: boolean;
  meta: ReactNode;
  onOpen: () => void;
  children: ReactNode;
}): ReactNode {
  return (
    <article
      className="tile model-card"
      data-health={health}
      data-managed={managed ? 'true' : undefined}
      role="button"
      tabIndex={0}
      aria-label={name}
      onClick={onOpen}
      onKeyDown={(keyEvent) => {
        if (keyEvent.key === 'Enter' || keyEvent.key === ' ') {
          keyEvent.preventDefault();
          onOpen();
        }
      }}
    >
      <div className="model-card-head">
        <span
          className="model-card-dot"
          data-tone={dotTone ?? (health ? healthTone(health) : 'neutral')}
          role="img"
          aria-label={dotLabel}
          title={dotLabel}
        />
        <h3 className="model-card-title" title={nameTitle}>
          {name}
        </h3>
      </div>
      {subtitle ? <div className="small muted mono model-card-native">{subtitle}</div> : null}
      <div className="model-card-meta">{meta}</div>
      {children}
    </article>
  );
}

/** Modality and capability chips, capped so every card keeps one silhouette. */
function CardChips({ modalities, capabilities }: { modalities: string[]; capabilities: string[] }): ReactNode {
  const shown = capabilities.slice(0, MAX_CARD_CHIPS);
  const rest = capabilities.slice(MAX_CARD_CHIPS);
  if (modalities.length === 0 && capabilities.length === 0) return null;
  return (
    <div className="model-card-chips">
      {modalities.map((value) => (
        <Badge key={value} tone="primary">
          {modalityLabel(value)}
        </Badge>
      ))}
      {shown.map((label) => (
        <Badge key={label} tone="neutral">
          {label}
        </Badge>
      ))}
      {rest.length > 0 ? (
        <span className="badge badge-neutral" title={rest.join(', ')} aria-label={rest.join(', ')}>
          +{rest.length}
        </span>
      ) : null}
    </div>
  );
}

/** The facts row: price, context window, speed. */
function CardFacts({
  price,
  context,
  activity,
  localOnly,
}: {
  price: ReactNode;
  context: number | undefined;
  activity: ModelActivity | undefined;
  localOnly: boolean;
}): ReactNode {
  const speed = activity?.tokens_per_second_7d ?? 0;
  return (
    <dl className="model-card-facts">
      {localOnly ? null : (
        <div className="model-card-fact">
          <dt>{t('models.price')}</dt>
          <dd>{price}</dd>
        </div>
      )}
      <div className="model-card-fact">
        <dt>{t('models.context')}</dt>
        <dd title={formatTokenCount(context)}>{compactTokens(context)}</dd>
      </div>
      <div className="model-card-fact" data-testid="model-speed">
        <dt>{t('models.speed')}</dt>
        <dd
          title={
            speed > 0 && activity
              ? t('models.speedDetail', { count: activity.speed_samples_7d.toLocaleString() })
              : t('models.speedNone')
          }
        >
          {speed > 0 ? t('models.speedValue', { value: formatSpeed(speed) }) : '—'}
        </dd>
      </div>
    </dl>
  );
}

/** "$10 / $50" with "per 1M tokens" under it; "Free" when both are an explicit $0, "Not priced" when neither is known. */
function PriceValue({ model }: { model: Model }): ReactNode {
  const input = formatModelRate(model, 'rate_in_nanousd');
  const output = formatModelRate(model, 'rate_out_nanousd');
  const unpriced = (value: string) => value === 'not priced';
  if (unpriced(input) && unpriced(output)) return <span className="muted">{t('models.notPriced')}</span>;
  const short = (value: string) => (unpriced(value) ? '—' : shortRate(value));
  if (short(input) === '$0' && short(output) === '$0') return <span>{t('models.free')}</span>;
  return (
    <>
      <span title={`${t('models.input')}: ${input} · ${t('models.output')}: ${output}`}>
        {short(input)} / {short(output)}
      </span>
      <span className="model-card-unit">{t('models.perMtok')}</span>
    </>
  );
}

/**
 * One catalog card: what a user picks a model on. Live figures (errors,
 * requests, jobs per day, probe latency) are in the detail pop-up and the
 * table view, so the card stays scannable.
 */
function ModelCard({ model, localOnly, onOpen }: { model: Model; localOnly: boolean; onOpen: () => void }): ReactNode {
  const health = modelHealth(model);
  return (
    <CardShell
      name={publicModelName(model)}
      nameTitle={isRenamedModel(model) ? t('adminModels.upstreamNameTitle', { name: model.name }) : undefined}
      subtitle={isRenamedModel(model) ? model.name : undefined}
      health={health}
      dotLabel={healthText(model)}
      meta={
        <>
          <span>{model.upstream_name}</span>
          {health === 'degraded' || health === 'down' ? <ModelHealthBadge model={model} /> : null}
        </>
      }
      onOpen={onOpen}
    >
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
      <CardChips modalities={model.modalities} capabilities={capabilityBadges(model.capabilities)} />
      <CardFacts
        price={<PriceValue model={model} />}
        context={model.context_window}
        activity={model.activity}
        localOnly={localOnly}
      />
    </CardShell>
  );
}

/**
 * A managed model (alias) card, same silhouette as ModelCard. We help, we do
 * not hide: the meta line names the model it uses right now, and the price is
 * that model's price.
 */
function ManagedModelCard({
  model: mm,
  target,
  localOnly,
  onOpen,
}: {
  model: ManagedModel;
  target: Model | undefined;
  localOnly: boolean;
  onOpen: () => void;
}): ReactNode {
  const status = mm.servable ? t('models.managedServing') : t('models.managedUnavailable');
  return (
    <CardShell
      name={mm.name}
      health={mm.broken ? 'down' : undefined}
      dotTone={mm.servable ? 'success' : 'danger'}
      dotLabel={status}
      managed
      meta={
        <>
          <span className="model-card-uses">{t('models.usesTarget', { model: mm.target_public_name || '—' })}</span>
          {mm.servable ? null : (
            <Badge tone="danger" dot>
              {status}
            </Badge>
          )}
        </>
      }
      onOpen={onOpen}
    >
      {mm.description ? (
        <p className="small muted model-card-description" title={mm.description}>
          {mm.description}
        </p>
      ) : null}
      {mm.broken ? <ManagedBrokenAlert model={mm} /> : null}
      <CardChips modalities={mm.modalities} capabilities={[]} />
      <CardFacts
        price={target ? <PriceValue model={target} /> : <span className="muted">{t('models.targetRates')}</span>}
        context={mm.context_window}
        activity={mm.activity}
        localOnly={localOnly}
      />
    </CardShell>
  );
}

function ManagedBrokenAlert({ model: mm }: { model: ManagedModel }): ReactNode {
  return (
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
  );
}

/** Detail pop-up for a managed model: everything the old card carried, and more room to explain it. */
function ManagedModelDetail({
  model: mm,
  target,
  localOnly,
  endpoint,
}: {
  model: ManagedModel;
  target: Model | undefined;
  localOnly: boolean;
  endpoint: string | undefined;
}): ReactNode {
  return (
    <>
      <div className="stack">
        {mm.description ? <p style={{ margin: 0 }}>{mm.description}</p> : null}
        <p className="small muted" style={{ margin: 0 }}>
          {t('models.managedExplainer')}
        </p>
        {mm.broken ? <ManagedBrokenAlert model={mm} /> : null}
        <DetailRow label={t('models.health')}>
          <Badge tone={mm.servable ? 'success' : 'danger'} dot>
            {mm.servable ? t('models.managedServing') : t('models.managedUnavailable')}
          </Badge>
        </DetailRow>
        <DetailRow label={t('models.currentlyUses')}>{mm.target_public_name || '—'}</DetailRow>
        <DetailRow label={t('adminModels.colUpstream')}>{mm.target_upstream_name}</DetailRow>
        <DetailRow label={t('models.colModalities')}>{mm.modalities.map(modalityLabel).join(', ')}</DetailRow>
        <DetailRow label={t('models.contextWindow')}>{formatTokenCount(mm.context_window)}</DetailRow>
        {localOnly ? null : target ? (
          <>
            <DetailRow label={t('models.inputRate')}>{formatModelRate(target, 'rate_in_nanousd')}</DetailRow>
            <DetailRow label={t('models.outputRate')}>{formatModelRate(target, 'rate_out_nanousd')}</DetailRow>
          </>
        ) : (
          <DetailRow label={t('models.price')}>{t('models.targetRates')}</DetailRow>
        )}
        <DetailRow label={t('models.jobsPerDay')}>
          <JobsPerDay activity={mm.activity} />
        </DetailRow>
        <DetailRow label={t('models.speed')}>
          {mm.activity && mm.activity.tokens_per_second_7d > 0
            ? t('models.speedValue', { value: formatSpeed(mm.activity.tokens_per_second_7d) })
            : t('models.speedNone')}
        </DetailRow>
        <ResponseFigures id={mm.id} />
        <DetailRow label={t('models.yourAccess')}>{grantSourceLabel(mm.grant_source)}</DetailRow>
      </div>
      <UseThisModel name={mm.name} endpoint={endpoint} />
    </>
  );
}

function UseThisModel({ name, endpoint }: { name: string; endpoint: string | undefined }): ReactNode {
  return (
    <section>
      <h3 style={{ marginBottom: 'var(--janus-space-3)' }}>{t('models.useThisModel')}</h3>
      <CodeBlock
        language="bash"
        code={`curl ${endpoint ?? '/v1'}/chat/completions \\
  -H "Authorization: Bearer ***" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${name}",
    "messages": [{"role": "user", "content": "Hello"}]
  }'`}
      />
      <p className="small muted" style={{ marginTop: 'var(--janus-space-3)' }}>
        {t('models.snippetHelpPrefix')} <Link to="/help">{t('models.snippetHelpLink')}</Link> {t('models.snippetHelpSuffix')}
      </p>
    </section>
  );
}

/**
 * Time to first token, output size and cache hit rate over the last 7 days,
 * each with a one-point-per-day sparkline. Org-wide aggregates from a
 * separate request, so the catalog itself stays light; the pop-up opens at
 * once and these rows fill in.
 */
function ResponseFigures({ id }: { id: string }): ReactNode {
  const q = useQuery({
    queryKey: ['models', id, 'performance'],
    queryFn: () => api.get<ModelPerformanceSummary>(`/api/v1/models/${encodeURIComponent(id)}/performance`),
    staleTime: 60_000,
  });
  if (q.isError) return null;
  const p = q.data;
  const loading = <span className="muted">{t('models.trendLoading')}</span>;
  return (
    <>
      <DetailRow label={t('models.firstToken')}>
        {!p ? (
          loading
        ) : p.avg_ttft_ms != null ? (
          <span title={t('models.firstTokenDetail', { count: p.ttft_samples.toLocaleString() })}>
            {formatDuration(p.avg_ttft_ms)}
            <Sparkline values={p.ttft_ms_series} />
          </span>
        ) : (
          <span className="muted">{t('models.firstTokenNone')}</span>
        )}
      </DetailRow>
      <DetailRow label={t('models.outputSize')}>
        {!p ? (
          loading
        ) : p.avg_output_tokens != null ? (
          <span>
            {t('models.outputSizeValue', { value: formatNumber(Math.round(p.avg_output_tokens)) })}
            <Sparkline values={p.output_tokens_series} />
          </span>
        ) : (
          <span className="muted">{t('models.outputSizeNone')}</span>
        )}
      </DetailRow>
      <DetailRow label={t('models.cacheHit')}>
        {!p ? (
          loading
        ) : !p.cache_reported ? (
          <span className="muted" title={t('models.cacheNotReportedDetail')}>
            {t('models.cacheNotReported')}
          </span>
        ) : p.cache_hit_rate != null ? (
          <span title={t('models.cacheHitDetail')}>
            {`${Number((p.cache_hit_rate * 100).toFixed(1))}%`}
            <Sparkline values={p.cache_hit_rate_series} />
          </span>
        ) : (
          <span className="muted">{t('models.outputSizeNone')}</span>
        )}
      </DetailRow>
    </>
  );
}

/** Jobs per day over 7 days with its trend arrow and the comparison in words. */
function JobsPerDay({ activity }: { activity: ModelActivity | undefined }): ReactNode {
  if (!activity || (activity.jobs_per_day_7d <= 0 && activity.jobs_per_day_prev_7d <= 0)) {
    return <span data-testid="model-jobs-per-day">—</span>;
  }
  return (
    <span data-testid="model-jobs-per-day">
      {formatJobsPerDay(activity.jobs_per_day_7d)} <TrendArrow activity={activity} />
      <span className="small muted"> · {jobsTrendDetail(activity)}</span>
    </span>
  );
}

/** Table-view cells for jobs per day and speed. */
function ActivityCells({ activity }: { activity: ModelActivity | undefined }): ReactNode {
  const speed = activity?.tokens_per_second_7d ?? 0;
  return (
    <>
      <td className="num small" title={activity ? jobsTrendDetail(activity) : undefined}>
        {activity && (activity.jobs_per_day_7d > 0 || activity.jobs_per_day_prev_7d > 0) ? (
          <>
            {formatJobsPerDay(activity.jobs_per_day_7d)} <TrendArrow activity={activity} />
          </>
        ) : (
          '—'
        )}
      </td>
      <td className="num small">{speed > 0 ? t('models.speedValue', { value: formatSpeed(speed) }) : '—'}</td>
    </>
  );
}

/** The input rate when it is known (an explicit $0 included); null when nobody set one. */
function knownRate(model: Model): number | null {
  if (model.rate_in_nanousd > 0) return model.rate_in_nanousd;
  const metadata = model.metadata?.rate_in_nanousd;
  const known = metadata
    ? metadata.value !== null
    : Boolean(model.rate_effective_from) && !model.rate_effective_from.startsWith('0001-01-01');
  return known ? 0 : null;
}

/** "$10.00 / Mtok" → "$10"; "$0.25 / Mtok" → "$0.25". Other labels pass through. */
function shortRate(label: string): string {
  return label.replace(/ \/ Mtok$/, '').replace(/^(\$[\d,]+)\.00$/, '$1');
}

/** 1,048,576 → "1.05M", 262,144 → "262K"; the exact count is in the tooltip. */
function compactTokens(tokens: number | undefined): string {
  if (!tokens || tokens <= 0) return '—';
  return new Intl.NumberFormat('en-US', { notation: 'compact', maximumSignificantDigits: 3 }).format(tokens);
}

/** The words behind a card's health dot: "Healthy · 42 ms", "Down", … */
function healthText(model: Model): string {
  const health = modelHealth(model);
  const latency =
    model.upstream_reachable && model.upstream_last_latency_ms !== undefined ? model.upstream_last_latency_ms : null;
  if (health === 'healthy' && latency !== null) return `${healthLabel(health)} · ${t('models.healthLatency', { ms: latency })}`;
  if (health === 'degraded' && (model.request_count_10m ?? 0) > 0)
    return `${healthLabel(health)} · ${formatErrorRate(model.error_rate_percent)}`;
  return healthLabel(health);
}

/** ▲ / ▼ for a ±10% change, a dash when flat, "new" when there is no baseline. */
function TrendArrow({ activity }: { activity: ModelActivity }): ReactNode {
  const label = jobsTrendDetail(activity);
  switch (activity.jobs_trend) {
    case 'up':
      return (
        <span className="trend" data-trend="up" role="img" aria-label={label}>
          ▲
        </span>
      );
    case 'down':
      return (
        <span className="trend" data-trend="down" role="img" aria-label={label}>
          ▼
        </span>
      );
    case 'new':
      return (
        <span className="trend" data-trend="new" aria-label={label}>
          new
        </span>
      );
    default:
      return (
        <span className="trend" data-trend="flat" role="img" aria-label={label}>
          –
        </span>
      );
  }
}

function jobsTrendDetail(activity: ModelActivity): string {
  const prev = formatJobsPerDay(activity.jobs_per_day_prev_7d);
  const pct = Math.abs(Math.round(activity.jobs_change_percent));
  switch (activity.jobs_trend) {
    case 'up':
      return t('models.jobsTrendUp', { percent: pct, rate: prev });
    case 'down':
      return t('models.jobsTrendDown', { percent: pct, rate: prev });
    case 'new':
      return t('models.jobsTrendNew');
    default:
      return t('models.jobsTrendFlat', { rate: prev });
  }
}

/** 0.1/day resolution below 10, whole numbers above. */
function formatJobsPerDay(value: number): string {
  if (value <= 0) return '0';
  if (value < 10) return value.toFixed(1).replace(/\.0$/, '');
  return Math.round(value).toLocaleString();
}

function formatSpeed(value: number): string {
  return value < 10 ? value.toFixed(1) : Math.round(value).toLocaleString();
}

/**
 * The health badge shared by cards, table rows and the detail drawer. It
 * mirrors the admin Upstreams page: a green dot with the probe latency when
 * the upstream answered, red when it did not, amber for a degraded error
 * rate, neutral when there is nothing to judge yet.
 */
function ModelHealthBadge({ model }: { model: Model }): ReactNode {
  const health = modelHealth(model);
  return (
    <Badge tone={healthTone(health)} dot={health !== 'unknown'}>
      {healthText(model)}
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
