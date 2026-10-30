/**
 * Progressive page data: one query per independently computed piece, so a
 * page paints its header and fast numbers at once and each chart fills in
 * when its own piece arrives (with a placeholder until then) instead of the
 * whole page waiting on its slowest query.
 */
import type { ReactNode } from 'react';
import { AsyncSection } from './ui';

/** A shimmering block the size of the chart it stands in for. */
export function ChartPlaceholder({ height = 200, label = 'Loading chart…' }: { height?: number; label?: string }): ReactNode {
  return (
    <div className="chart-placeholder skeleton" style={{ height }} role="status" aria-live="polite" aria-label={label}>
      <span className="chart-placeholder-label">{label}</span>
    </div>
  );
}

/** Bars standing in for a ranked list. */
export function ListPlaceholder({ rows = 5, label = 'Loading…' }: { rows?: number; label?: string }): ReactNode {
  return (
    <div className="list-placeholder" role="status" aria-live="polite" aria-label={label}>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="list-placeholder-row skeleton" style={{ width: `${92 - i * 12}%` }} />
      ))}
    </div>
  );
}

/** A tile-shaped placeholder (overline, big number, meta line). */
export function TilePlaceholder({ label }: { label: string }): ReactNode {
  return (
    <article className="tile" aria-busy="true">
      <div className="overline">{label}</div>
      <div className="tile-value">
        <span className="skeleton tile-placeholder-value" role="status" aria-label={`Loading ${label}`} />
      </div>
      <div className="tile-meta">
        <span className="skeleton tile-placeholder-meta" />
      </div>
    </article>
  );
}

/**
 * One piece of a page: its own placeholder while loading, its own inline
 * error with retry if it failed (other pieces keep working), content when
 * ready. Background refetches keep the last content on screen.
 */
export function Piece<T>({
  query,
  placeholder,
  children,
}: {
  query: { data: T | undefined; isLoading: boolean; error: unknown; refetch: () => void };
  placeholder: ReactNode;
  children: (data: T) => ReactNode;
}): ReactNode {
  return (
    <AsyncSection query={query} skeleton={placeholder}>
      {children}
    </AsyncSection>
  );
}
