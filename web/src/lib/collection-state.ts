import { useCallback, useEffect } from 'react';
import { pageOffset } from './collections';
import { useUrlState, useUrlStateBatch } from './hooks';

/** Change one collection criterion and reset its offset in a single navigation. */
export function useCollectionState<T extends string = string>(
  key: string,
  fallback: NoInfer<T>,
  pageKey = 'page',
): [T, (value: T) => void] {
  const [value] = useUrlState<T>(key, fallback);
  const batch = useUrlStateBatch();
  const update = useCallback(
    (next: T) => batch({ [key]: next === fallback ? null : next, [pageKey]: null }),
    [batch, key, fallback, pageKey],
  );
  return [value, update];
}

/** Repair stale/out-of-range offsets after the authoritative count arrives. */
export function useRepairCollectionPage(total: number | undefined, limit: number, pageKey = 'page'): void {
  const [page, setPage] = useUrlState(pageKey, '0');
  const offset = pageOffset(page, limit, total);
  useEffect(() => {
    if (total !== undefined && page !== String(offset)) setPage(String(offset));
  }, [total, page, offset, setPage]);
}
