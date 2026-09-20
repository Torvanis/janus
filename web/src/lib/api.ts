/**
 * Typed client for the Janus application API.
 *
 * Every failure is normalised into ApiError so views can render the gateway's
 * human-readable message and error code instead of a raw status or stack trace.
 */

export interface ApiErrorPayload {
  message: string;
  code: string;
  type: string;
  param?: string;
  reason?: string;
  retry_after?: number;
  reset_at?: string;
  request_id?: string;
  docs_url?: string;
}

export class ApiError extends Error {
  readonly status: number;
  readonly payload: ApiErrorPayload;

  constructor(status: number, payload: ApiErrorPayload) {
    super(payload.message);
    this.name = 'ApiError';
    this.status = status;
    this.payload = payload;
  }

  get code(): string {
    return this.payload.code;
  }

  /** True when the caller simply needs to sign in again. */
  get isUnauthenticated(): boolean {
    return this.status === 401;
  }

  get isForbidden(): boolean {
    return this.status === 403;
  }
}

function readCookie(name: string): string {
  const match = document.cookie.split('; ').find((row) => row.startsWith(`${name}=`));
  return match ? decodeURIComponent(match.slice(name.length + 1)) : '';
}

type Method = 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE';

async function request<T>(method: Method, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
  }
  if (method !== 'GET') {
    // Double-submit CSRF: the cookie is readable, the header is not forgeable
    // cross-origin. Bearer-token callers do not need this.
    const csrf = readCookie('janus_csrf');
    if (csrf) {
      headers['X-Janus-CSRF'] = csrf;
    }
  }

  let response: Response;
  try {
    response = await fetch(path, {
      method,
      headers,
      credentials: 'same-origin',
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new ApiError(0, {
      message: 'The gateway could not be reached. Check your connection — Janus will retry automatically.',
      code: 'network_error',
      type: 'server_error',
    });
  }

  if (response.status === 204) {
    return undefined as T;
  }

  const text = await response.text();
  let parsed: unknown = null;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = null;
    }
  }

  if (!response.ok) {
    const envelope = (parsed as { error?: ApiErrorPayload } | null)?.error;
    throw new ApiError(
      response.status,
      envelope ?? {
        message: `The gateway returned an unexpected response (HTTP ${response.status}).`,
        code: 'server_error',
        type: 'server_error',
      },
    );
  }
  return parsed as T;
}

export const api = {
  get: <T>(path: string) => request<T>('GET', path),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body ?? {}),
  patch: <T>(path: string, body?: unknown) => request<T>('PATCH', path, body ?? {}),
  put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body ?? {}),
  del: <T>(path: string) => request<T>('DELETE', path),
};

/** Builds a query string, omitting empty values so URLs stay readable. */
export function qs(params: Record<string, string | number | undefined | null>): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== '') {
      search.set(key, String(value));
    }
  }
  const encoded = search.toString();
  return encoded ? `?${encoded}` : '';
}
