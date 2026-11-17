import { afterEach, expect, it, vi } from 'vitest';
import { api, ApiError } from './api';

afterEach(() => {
  vi.unstubAllGlobals();
});

it('gives up on a GET that does not answer within its timeout', async () => {
  // A fetch that only ever settles by being aborted.
  vi.stubGlobal(
    'fetch',
    vi.fn(
      (_url: string, init: RequestInit) =>
        new Promise((_resolve, reject) => {
          init.signal?.addEventListener('abort', () => reject(init.signal?.reason));
        }),
    ),
  );
  const error = await api.get('/api/v1/slow', { timeoutMs: 20 }).catch((e: unknown) => e);
  expect(error).toBeInstanceOf(ApiError);
  expect((error as ApiError).code).toBe('timeout');
  expect((error as ApiError).message).toMatch(/did not answer within/);
});

it('sends no timeout signal unless asked', async () => {
  const fetchMock = vi.fn(async () => new Response('{}', { status: 200 }));
  vi.stubGlobal('fetch', fetchMock);
  await api.get('/api/v1/fast');
  expect((fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1].signal).toBeUndefined();
});
