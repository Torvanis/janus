import { useEffect, useRef, type ReactNode } from 'react';
import { useUrlState, useUrlStateBatch } from '../lib/hooks';
import { pageOffset, sortCollection } from '../lib/collections';
import { Pagination } from './ui';

export interface CollectionColumn<T> {
  id: string;
  label: string;
  value?: (row: T) => string | number | null | undefined;
  render: (row: T) => ReactNode;
}
/** Only for complete returned inventories or frozen results, never server pages.
 * Filter/sort happen before slicing. URL keys are namespaced for nested collections.
 */
export function Collection<T>({
  rows,
  columns,
  rowKey,
  name,
  scope = 'complete-client',
  children,
  resetKey,
  hideSearch = false,
}: {
  rows: readonly T[];
  columns: CollectionColumn<T>[];
  rowKey: (row: T) => string;
  name: string;
  scope?: 'complete-client' | 'frozen-report';
  children?: (rows: T[]) => ReactNode;
  resetKey?: string;
  hideSearch?: boolean;
}): ReactNode {
  const [search] = useUrlState(`${name}.q`, '');
  const [sort] = useUrlState(`${name}.sort`, '');
  const [page] = useUrlState(`${name}.page`, '0');
  const [size] = useUrlState(`${name}.size`, '25');
  const batch = useUrlStateBatch();
  const limit = [10, 25, 50, 100].includes(Number(size)) ? Number(size) : 25;
  const update = (key: string, value: string) => batch({ [`${name}.${key}`]: value, [`${name}.page`]: null });
  const term = hideSearch ? '' : search.trim().toLowerCase();
  const filtered = term
    ? rows.filter((row) =>
        columns.some((c) =>
          String(c.value?.(row) ?? '')
            .toLowerCase()
            .includes(term),
        ),
      )
    : [...rows];
  const column = children ? undefined : columns.find((c) => c.id === sort.replace(/:(asc|desc)$/, ''));
  const ordered = column?.value ? sortCollection(filtered, column.value, !sort.endsWith(':desc')) : filtered;
  const previousResetKey = useRef(resetKey);
  const offset = resetKey !== previousResetKey.current ? 0 : pageOffset(page, limit, ordered.length);
  useEffect(() => {
    previousResetKey.current = resetKey;
  }, [resetKey]);
  useEffect(() => {
    if (page !== String(offset)) batch({ [`${name}.page`]: offset ? String(offset) : null });
  }, [page, offset, name, batch]);
  return (
    <section className="stack" aria-label={name} data-collection-mode={scope}>
      <div className="row wrap">
        {!hideSearch && (
          <input
            className="input"
            type="search"
            aria-label={`Search ${name}`}
            placeholder={`Search ${name}`}
            value={search}
            onChange={(e) => update('q', e.target.value)}
          />
        )}
        <label className="row">
          Rows per page{' '}
          <select className="select" value={limit} onChange={(e) => update('size', e.target.value)}>
            {[10, 25, 50, 100].map((n) => (
              <option key={n}>{n}</option>
            ))}
          </select>
        </label>
        <span className="small muted">
          {filtered.length} of {rows.length} {scope === 'frozen-report' ? 'snapshot rows' : 'rows'}
        </span>
        {((!hideSearch && search) || (!children && sort)) && (
          <button type="button"
            className="btn btn-sm"
            onClick={() => batch({ [`${name}.q`]: null, [`${name}.sort`]: null, [`${name}.page`]: null })}
          >
            Clear filters and sort
          </button>
        )}
      </div>
      {children ? (
        <div className="table-wrap" role="region" aria-label={`${name} table`} tabIndex={0}>
          {children(ordered.slice(offset, offset + limit))}
        </div>
      ) : (
        <div className="table-wrap" role="region" aria-label={`${name} table`} tabIndex={0}>
          <table className="data">
            <caption className="sr-only">{name}</caption>
            <thead>
              <tr>
                {columns.map((c) => (
                  <th
                    key={c.id}
                    scope="col"
                    aria-sort={sort === `${c.id}:asc` ? 'ascending' : sort === `${c.id}:desc` ? 'descending' : 'none'}
                  >
                    {c.value ? (
                      <button type="button" onClick={() => update('sort', `${c.id}:${sort === `${c.id}:asc` ? 'desc' : 'asc'}`)}>
                        {c.label} {sort.startsWith(c.id + ':') ? (sort.endsWith(':desc') ? '↓' : '↑') : '↕'}
                      </button>
                    ) : (
                      c.label
                    )}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {ordered.slice(offset, offset + limit).map((row) => (
                <tr key={rowKey(row)}>
                  {columns.map((c) => (
                    <td key={c.id}>{c.render(row)}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {!ordered.length && (
        <p role="status">{rows.length ? 'No matching rows. Clear the search to see all rows.' : 'No rows yet.'}</p>
      )}
      <Pagination offset={offset} limit={limit} total={ordered.length} onChange={(n) => batch({ [`${name}.page`]: String(n) })} />
    </section>
  );
}
