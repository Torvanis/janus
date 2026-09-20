/** Sort a complete result set, with missing values last in either direction.
 * Array.sort is stable: equal keys retain the source order, including blanks.
 */
export function sortCollection<T>(
  rows: readonly T[],
  value: (row: T) => string | number | null | undefined,
  ascending: boolean,
): T[] {
  return [...rows].sort((a, b) => {
    const left = value(a),
      right = value(b);
    const blank = (v: unknown) => v == null || v === '' || (typeof v === 'number' && Number.isNaN(v));
    if (blank(left)) return blank(right) ? 0 : 1;
    if (blank(right)) return -1;
    const order =
      typeof left === 'string' || typeof right === 'string'
        ? String(left).localeCompare(String(right))
        : Number(left) - Number(right);
    return ascending ? order : -order;
  });
}

export function pageOffset(value: string, limit: number, total?: number): number {
  const n = Number(value);
  const offset = Number.isFinite(n) ? Math.max(0, Math.floor(n / limit) * limit) : 0;
  return total === undefined ? offset : Math.min(offset, Math.max(0, Math.ceil(total / limit) - 1) * limit);
}
