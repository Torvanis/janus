import { useEffect, useId, useState, type ReactNode } from 'react';
import { pageOffset, sortCollection } from '../lib/collections';
import { Pagination } from './ui';

/** Only the returned server summary, not an inventory or a database-wide ranking.
 * Local controls are independent of the containing page's filters and other charts.
 */
export function BreakdownDetails({
  rows,
  metricLabel,
  format,
}: {
  rows: { key: string; label: string; value: number }[];
  metricLabel: string;
  format: (value: number) => string;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState('');
  const [limit, setLimit] = useState(25);
  const [page, setPage] = useState(0);
  const [sort, setSort] = useState('value:desc');
  const region = useId();
  const filtered = rows.filter((row) => `${row.label} ${row.key}`.toLowerCase().includes(search.toLowerCase()));
  const ordered = sortCollection(filtered, (row) => (sort.startsWith('label') ? row.label : row.value), sort.endsWith('asc'));
  const offset = pageOffset(String(page), limit, ordered.length);
  useEffect(() => {
    if (offset !== page) setPage(offset);
  }, [offset, page]);
  const setOrder = (column: string) => {
    setSort(`${column}:${sort === `${column}:asc` ? 'desc' : 'asc'}`);
    setPage(0);
  };
  return (
    <div className="stack" data-collection-mode="returned-summary">
      <button type="button" className="btn btn-sm" aria-expanded={open} aria-controls={region} onClick={() => setOpen(!open)}>
        {open ? 'Hide returned rows' : `View all returned rows (${rows.length})`}
      </button>
      {open && (
        <div id={region} className="stack">
          <p className="small muted">
            Search and sorting apply only to this returned summary. The server may omit other categories.
          </p>
          <div className="row wrap">
            <input
              className="input"
              type="search"
              aria-label="Search returned summary"
              value={search}
              onChange={(e) => {
                setSearch(e.target.value);
                setPage(0);
              }}
            />
            <label>
              Rows per page{' '}
              <select
                aria-label="Returned summary rows per page"
                value={limit}
                onChange={(e) => {
                  setLimit(Number(e.target.value));
                  setPage(0);
                }}
              >
                {[10, 25, 50, 100].map((size) => (
                  <option key={size} value={size}>
                    {size}
                  </option>
                ))}
              </select>
            </label>
            <span className="small muted">
              {filtered.length} matching of {rows.length} returned rows
            </span>
          </div>
          <div className="table-wrap" role="region" aria-label="Returned summary table" tabIndex={0}>
            <table className="data" aria-label="Returned summary rows">
              <thead>
                <tr>
                  {[
                    ['label', 'Category'],
                    ['value', metricLabel],
                  ].map(([key, label]) => (
                    <th
                      key={key}
                      scope="col"
                      aria-sort={sort.startsWith(key!) ? (sort.endsWith('asc') ? 'ascending' : 'descending') : 'none'}
                    >
                      <button type="button" className="btn btn-ghost btn-sm" onClick={() => setOrder(key!)}>
                        {label}
                      </button>
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {ordered.slice(offset, offset + limit).map((row, i) => (
                  <tr key={`${row.key}:${i}`}>
                    <td>{row.label || '—'}</td>
                    <td className="num">{Number.isFinite(row.value) ? format(row.value) : '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {ordered.length === 0 && <p>No matching returned rows.</p>}
          <Pagination total={ordered.length} offset={offset} limit={limit} onChange={setPage} />
        </div>
      )}
    </div>
  );
}
