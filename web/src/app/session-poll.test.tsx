import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ApiError, api } from '../lib/api';
import { SessionProvider, useSession } from './session';
import { LICENSE_POLL_MS } from '../lib/licenseNotice';

vi.mock('../lib/api', async (original) => {
  const real = await original<typeof import('../lib/api')>();
  return { ...real, api: { get: vi.fn() } };
});
const mocked = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

function Who() {
  const { me } = useSession();
  return <p>{me ? `signed in as ${me.email}` : 'signed out'}</p>;
}

it('stops polling "who am I?" once the session has ended, and shows the person signed out', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  let alive = true;
  const meCalls = () => mocked.get.mock.calls.filter(([p]) => p === '/api/v1/me').length;
  mocked.get.mockImplementation((path: string) => {
    if (path === '/api/v1/config') return Promise.resolve({});
    if (alive) return Promise.resolve({ id: 'u', email: 'a@example.com', role: 'user' });
    return Promise.reject(
      new ApiError(401, { message: 'Sign in', code: 'unauthenticated', type: 'authentication_error' } as never),
    );
  });
  render(
    <QueryClientProvider client={new QueryClient()}>
      <SessionProvider>
        <Who />
      </SessionProvider>
    </QueryClientProvider>,
  );
  expect(await screen.findByText('signed in as a@example.com')).toBeTruthy();
  alive = false;
  await act(async () => {
    await vi.advanceTimersByTimeAsync(LICENSE_POLL_MS + 10);
  });
  expect(await screen.findByText('signed out')).toBeTruthy();
  const after401 = meCalls();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(LICENSE_POLL_MS * 5);
  });
  expect(meCalls()).toBe(after401);
});
