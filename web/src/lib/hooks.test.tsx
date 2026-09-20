/**
 * Tests for the state hooks that define the app's "URL owns filter state"
 * contract: deep links work, state survives refresh, and personal
 * preferences stay out of shareable links.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, renderHook } from '@testing-library/react';
import { MemoryRouter, useSearchParams } from 'react-router-dom';
import type { ReactNode } from 'react';
import { useAppearance, useDebounced, useLocalPreference, useUrlState } from './hooks';

vi.mock('../app/session', () => ({ useSession: () => ({ me: sessionMe }) }));
let sessionMe: { feature_flags: Record<string, boolean> } | null = null;

beforeEach(() => {
  cleanup();
  window.localStorage.clear();
});

function routerWrapper(initialPath: string) {
  return ({ children }: { children: ReactNode }) => <MemoryRouter initialEntries={[initialPath]}>{children}</MemoryRouter>;
}

/** Exposes the hook value together with the live query string. */
function useUrlStateProbe(key: string, fallback: string) {
  const [value, setValue] = useUrlState(key, fallback);
  const [params] = useSearchParams();
  return { value, setValue, search: params.toString() };
}

describe('useUrlState', () => {
  it('reads its initial value from the URL, falling back when absent', () => {
    const fromUrl = renderHook(() => useUrlStateProbe('status', 'all'), {
      wrapper: routerWrapper('/tokens?status=active'),
    });
    expect(fromUrl.result.current.value).toBe('active');

    const fallback = renderHook(() => useUrlStateProbe('status', 'all'), {
      wrapper: routerWrapper('/tokens'),
    });
    expect(fallback.result.current.value).toBe('all');
  });

  it('persists updates into the query string so the view is shareable', () => {
    const { result } = renderHook(() => useUrlStateProbe('status', 'all'), {
      wrapper: routerWrapper('/tokens'),
    });
    act(() => result.current.setValue('revoked'));
    expect(result.current.value).toBe('revoked');
    expect(result.current.search).toBe('status=revoked');
  });

  it('removes the parameter again when reset to the fallback, keeping links minimal', () => {
    const { result } = renderHook(() => useUrlStateProbe('status', 'all'), {
      wrapper: routerWrapper('/tokens?status=revoked'),
    });
    act(() => result.current.setValue('all'));
    expect(result.current.value).toBe('all');
    expect(result.current.search).toBe('');
  });

  it('leaves unrelated parameters untouched', () => {
    const { result } = renderHook(() => useUrlStateProbe('status', 'all'), {
      wrapper: routerWrapper('/tokens?q=cursor&sort=used'),
    });
    act(() => result.current.setValue('active'));
    const params = new URLSearchParams(result.current.search);
    expect(params.get('q')).toBe('cursor');
    expect(params.get('sort')).toBe('used');
    expect(params.get('status')).toBe('active');
  });
});

describe('useLocalPreference', () => {
  it('persists to localStorage and survives a fresh mount', () => {
    const first = renderHook(() => useLocalPreference('janus.test-pref', 'default'));
    expect(first.result.current[0]).toBe('default');
    act(() => first.result.current[1]('custom'));
    expect(first.result.current[0]).toBe('custom');
    first.unmount();

    const second = renderHook(() => useLocalPreference('janus.test-pref', 'default'));
    expect(second.result.current[0]).toBe('custom');
  });
});

/**
 * The admin flag reduce_motion_preferred must actually do what its toggle
 * says: a viewer who has not chosen starts reduced; one who chose keeps it.
 */
describe('useAppearance — reduce_motion_preferred', () => {
  beforeEach(() => {
    window.localStorage.removeItem('janus.motion');
    window.matchMedia = vi.fn().mockReturnValue({ matches: false, addEventListener() {}, removeEventListener() {} }) as never;
  });

  it('follows the OS when the flag is off and nothing is stored', () => {
    sessionMe = { feature_flags: { reduce_motion_preferred: false } };
    const { result } = renderHook(() => useAppearance());
    expect(result.current.motion).toBe('system');
    expect(document.documentElement.getAttribute('data-motion')).toBe('full');
  });

  it('seeds a fresh session with reduced motion when the flag is on', () => {
    sessionMe = { feature_flags: { reduce_motion_preferred: true } };
    const { result } = renderHook(() => useAppearance());
    expect(result.current.motion).toBe('reduced');
    expect(document.documentElement.getAttribute('data-motion')).toBe('reduced');
  });

  it("a viewer's explicit choice beats the flag, in both directions", () => {
    sessionMe = { feature_flags: { reduce_motion_preferred: true } };
    const on = renderHook(() => useAppearance());
    act(() => on.result.current.setMotion('full'));
    expect(on.result.current.motion).toBe('full');
    on.unmount();
    // Still their choice on the next visit, flag notwithstanding.
    expect(renderHook(() => useAppearance()).result.current.motion).toBe('full');
  });

  it('applies when the session loads after the hook mounted (no stale default)', () => {
    sessionMe = null;
    const { result, rerender } = renderHook(() => useAppearance());
    expect(result.current.motion).toBe('system');
    sessionMe = { feature_flags: { reduce_motion_preferred: true } };
    rerender();
    expect(result.current.motion).toBe('reduced');
  });
});

describe('useDebounced', () => {
  afterEach(() => vi.useRealTimers());

  it('only settles on the new value after the delay elapses', () => {
    vi.useFakeTimers();
    const { result, rerender } = renderHook(({ value }) => useDebounced(value, 250), {
      initialProps: { value: 'cur' },
    });
    expect(result.current).toBe('cur');

    rerender({ value: 'curso' });
    act(() => void vi.advanceTimersByTime(200));
    expect(result.current).toBe('cur');

    // A newer keystroke restarts the timer.
    rerender({ value: 'cursor' });
    act(() => void vi.advanceTimersByTime(200));
    expect(result.current).toBe('cur');
    act(() => void vi.advanceTimersByTime(50));
    expect(result.current).toBe('cursor');
  });
});
