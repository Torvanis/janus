/**
 * Whether React Query should ask again after a failed query (the app-wide
 * default in main.tsx).
 *
 * Client errors are terminal: the same request gets the same answer, so a
 * retry only produces more 4xx responses and keeps a skeleton on screen for
 * seconds before the error shows (a refused filter value looked like an
 * empty page). Authentication and permission failures are among them. A
 * timeout (408) or rate limit (429) can pass, and so can network and server
 * errors: those get two more tries.
 */
export function shouldRetryQuery(failureCount: number, error: unknown): boolean {
  const status = (error as { status?: number } | null)?.status ?? 0;
  if (status >= 400 && status < 500 && status !== 408 && status !== 429) return false;
  return failureCount < 2;
}
