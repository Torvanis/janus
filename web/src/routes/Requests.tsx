import { useCollectionState, useRepairCollectionPage } from '../lib/collection-state';
import { useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api, qs } from '../lib/api';
import { publicModelName, type Model, type UsageEvent } from '../lib/types';
import { formatDateTime, formatDuration, formatNumber, formatUSD, statusTone, titleCase } from '../lib/format';
import { AsyncSection, Badge, Drawer, EmptyState, Pagination } from '../components/ui';
import { ChargedTeam } from '../components/ChargedTeam';
import { useUrlState, useUrlStateBatch } from '../lib/hooks';
import { t } from '../lib/i18n';
import { FilterSelect, RangePicker, RequestDetail, SortHeader, TokenSizeFilter, tokenSizeParams, type RangeKey } from './shared';
import { useLocalOnly } from '../app/session';

const PAGE_SIZE = 50;

const MODALITIES = [
  'chat',
  'embedding',
  'image',
  'audio',
  'video',
  'tts',
  'stt',
  'function',
  'response',
  'assistant',
  'thread',
  'file',
  'moderation',
  'fine_tune',
];

export function RequestsPage(): ReactNode {
  const localOnly = useLocalOnly();
  const [range] = useUrlState<RangeKey>('range', 'week');
  const [modality] = useUrlState('modality', '');
  const [status] = useUrlState('status', '');
  const [model] = useUrlState('model', '');
  const [tokenID] = useUrlState('token_id', '');
  // Token-size filters live in the URL as `gt:<n>` / `lt:<n>` per direction
  // and are translated to the API's tokens_in_gt / tokens_out_lt parameters.
  const [tokensIn] = useUrlState('tokens_in', '');
  const [tokensOut] = useUrlState('tokens_out', '');
  const [sort, setSort] = useCollectionState('sort', 'time');
  const [page, setPage] = useUrlState('page', '0');
  // Filter/range changes must atomically reset the page offset, otherwise a
  // user standing on page ≥2 who narrows the list lands on an out-of-range
  // offset and gets an empty page.
  const batchParams = useUrlStateBatch();
  const setRange = (next: RangeKey) => batchParams({ range: next === 'week' ? null : next, page: null });
  const setModality = (next: string) => batchParams({ modality: next, page: null });
  const setStatus = (next: string) => batchParams({ status: next, page: null });
  const setModel = (next: string) => batchParams({ model: next, page: null });
  const setTokensIn = (next: string) => batchParams({ tokens_in: next, page: null });
  const setTokensOut = (next: string) => batchParams({ tokens_out: next, page: null });
  const clearFilters = () =>
    batchParams({ modality: null, status: null, model: null, token_id: null, tokens_in: null, tokens_out: null, page: null });
  const [selected, setSelected] = useState<UsageEvent | null>(null);

  const offset = Number.parseInt(page, 10) || 0;
  const filterParams = {
    range,
    modality,
    status,
    model,
    token_id: tokenID,
    sort,
    ...tokenSizeParams('tokens_in', tokensIn),
    ...tokenSizeParams('tokens_out', tokensOut),
  };

  const requests = useQuery({
    queryKey: ['requests', range, modality, status, model, tokenID, tokensIn, tokensOut, sort, offset],
    queryFn: () =>
      api.get<{ requests: UsageEvent[]; total_count: number }>(
        `/api/v1/requests${qs({ ...filterParams, limit: PAGE_SIZE, offset })}`,
      ),
    refetchInterval: 15_000,
  });
  useRepairCollectionPage(requests.isFetching ? undefined : requests.data?.total_count, PAGE_SIZE);

  // Filter options come from the granted-model list, not from the visible
  // page: a model absent from the current 50 rows must still be selectable.
  // Names seen on the page (e.g. models whose grant was since revoked) are
  // unioned in, and the active filter value always stays in the list.
  const grantedModels = useQuery({
    queryKey: ['models'],
    queryFn: () => api.get<{ models: Model[] }>('/api/v1/models'),
    staleTime: 60_000,
  });
  // Usage events record the model's public name at request time (display name
  // when a rename was in force), so the filter options must offer the same.
  const modelOptions = [
    ...new Set(
      [
        ...(grantedModels.data?.models ?? []).map((m) => publicModelName(m)),
        ...(requests.data?.requests ?? []).map((event) => event.model),
        model,
      ].filter(Boolean),
    ),
  ].sort();

  const exportUrl = `/api/v1/requests.csv${qs(filterParams)}`;
  const hasFilters = Boolean(modality || status || model || tokenID || tokensIn || tokensOut);

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('requests.title')}</h1>
          <p className="page-subtitle">{t('requests.subtitle')}</p>
        </div>
        <div className="row">
          <RangePicker value={range} onChange={setRange} />
          <a className="btn btn-sm" href={exportUrl} download>
            {t('requests.exportCsv')}
          </a>
        </div>
      </header>

      <div className="row wrap">
        <FilterSelect
          label={t('tables.modality')}
          value={modality}
          onChange={setModality}
          options={[
            { value: '', label: t('requests.allModalities') },
            ...MODALITIES.map((value) => ({ value, label: titleCase(value) })),
          ]}
        />
        <FilterSelect
          label={t('tables.status')}
          value={status}
          onChange={setStatus}
          options={[
            { value: '', label: t('requests.allOutcomes') },
            { value: 'success', label: t('requests.succeeded') },
            { value: 'error', label: t('requests.failed') },
          ]}
        />
        <FilterSelect
          label={t('tables.model')}
          value={model}
          onChange={setModel}
          options={[{ value: '', label: t('requests.allModels') }, ...modelOptions.map((name) => ({ value: name, label: name }))]}
        />
        <TokenSizeFilter direction="in" value={tokensIn} onChange={setTokensIn} />
        <TokenSizeFilter direction="out" value={tokensOut} onChange={setTokensOut} />
        {tokenID ? <span className="small">Filtered to the selected API token</span> : null}
        {hasFilters ? (
          <button type="button" className="btn btn-ghost btn-sm" onClick={clearFilters}>
            {t('tables.clearFilters')}
          </button>
        ) : null}
      </div>

      <section className="card card-flush">
        <AsyncSection
          query={requests}
          empty={{
            when: (data) => data.total_count === 0 && !hasFilters,
            title: t('requests.emptyTitle'),
            body: t('requests.emptyBody'),
            action: (
              <Link className="btn btn-primary" to="/help">
                {t('requests.connectTool')}
              </Link>
            ),
          }}
        >
          {(data) =>
            data.requests.length === 0 ? (
              <EmptyState
                title={t('requests.noMatchTitle')}
                body={t('requests.noMatchBody')}
                action={
                  <button type="button" className="btn" onClick={clearFilters}>
                    {t('tables.clearFilters')}
                  </button>
                }
              />
            ) : (
              <>
                <div className="table-wrap" style={{ maxHeight: '64dvh', overflowY: 'auto' }}>
                  <table className="data">
                    <thead>
                      <tr>
                        <SortHeader label={t('tables.time')} sortKey="time" active={sort} onSort={setSort} />
                        <th scope="col">{t('tables.model')}</th>
                        <th scope="col">Charged to</th>
                        <th scope="col">{t('tables.modality')}</th>
                        <th scope="col">{t('tables.status')}</th>
                        <SortHeader label={t('tables.latency')} sortKey="latency" active={sort} onSort={setSort} />
                        <SortHeader label={t('tables.tokensIn')} sortKey="tokens_in" active={sort} onSort={setSort} />
                        <SortHeader label={t('tables.tokensOut')} sortKey="tokens_out" active={sort} onSort={setSort} />
                        {localOnly ? null : <SortHeader label={t('tables.cost')} sortKey="cost" active={sort} onSort={setSort} />}
                      </tr>
                    </thead>
                    <tbody>
                      {data.requests.map((event) => (
                        <tr
                          key={event.id}
                          data-clickable="true"
                          onClick={() => setSelected(event)}
                          tabIndex={0}
                          onKeyDown={(keyEvent) => {
                            if (keyEvent.key === 'Enter') setSelected(event);
                          }}
                        >
                          <td className="small muted">{formatDateTime(event.created_at)}</td>
                          <td className="truncate">{event.model || '—'}</td>
                          <td className="small">
                            <ChargedTeam event={event} />
                          </td>
                          <td className="small">{titleCase(event.modality)}</td>
                          <td>
                            <Badge tone={statusTone(event.http_status)}>
                              {event.http_status}
                              {event.streaming ? ` · ${t('requests.stream')}` : ''}
                            </Badge>
                          </td>
                          <td className="num small">{formatDuration(event.latency_ms)}</td>
                          <td className="num small">{formatNumber(event.tokens_in)}</td>
                          <td className="num small">{formatNumber(event.tokens_out)}</td>
                          {localOnly ? null : <td className="num small">{formatUSD(event.cost_nanousd)}</td>}
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                <Pagination
                  offset={offset}
                  limit={PAGE_SIZE}
                  total={data.total_count}
                  onChange={(next) => setPage(String(next))}
                />
              </>
            )
          }
        </AsyncSection>
      </section>

      <Drawer open={Boolean(selected)} onClose={() => setSelected(null)} title={t('requests.detailTitle')}>
        {selected ? <RequestDetail event={selected} /> : null}
      </Drawer>
    </div>
  );
}
