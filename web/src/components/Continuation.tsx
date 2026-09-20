import type { ReactNode } from 'react';
/** Unknown totals are deliberate: these are server pages, not capped inventories. */
export function Continuation({
  offset,
  limit,
  count,
  hasMore,
  busy,
  onPage,
}: {
  offset: number;
  limit: number;
  count: number;
  hasMore: boolean;
  busy?: boolean;
  onPage: (offset: number) => void;
}): ReactNode {
  return (
    <nav className="row-between" aria-label="Collection pages">
      <span className="small muted">
        {count ? `${offset + 1}–${offset + count}` : 'No rows on this page'} · newest first · total unknown
      </span>
      <div className="row">
        <button type="button" className="btn btn-sm" disabled={busy || offset === 0} onClick={() => onPage(Math.max(0, offset - limit))}>
          Previous
        </button>
        <button type="button" className="btn btn-sm" disabled={busy || !hasMore} onClick={() => onPage(offset + limit)}>
          Next
        </button>
      </div>
    </nav>
  );
}
