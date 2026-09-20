import { useCallback, useEffect, useMemo, useState } from 'react';
import { useSearchParams } from 'react-router-dom';

import { useSession } from '../app/session';

/**
 * URL-owned state. Filters, sort, tab, and range live in the query string so a
 * view survives a refresh and can be shared as a link (definition of done:
 * deep links work, state survives refresh).
 */
export function useUrlState<T extends string = string>(
  key: string,
  // NoInfer keeps the generic at `string` unless the caller names a union,
  // instead of narrowing it to the literal type of the fallback value.
  fallback: NoInfer<T>,
): [T, (next: T) => void] {
  const [params, setParams] = useSearchParams();
  const value = (params.get(key) as T | null) ?? fallback;

  const setValue = useCallback(
    (next: T) => {
      setParams(
        (current) => {
          const updated = new URLSearchParams(current);
          if (!next || next === fallback) {
            updated.delete(key);
          } else {
            updated.set(key, next);
          }
          return updated;
        },
        { replace: true },
      );
    },
    [fallback, key, setParams],
  );

  return [value, setValue];
}

/**
 * Atomically update several URL-owned keys in one history replacement. Two
 * useUrlState setters called in the same event handler do NOT compose:
 * react-router's functional setSearchParams receives the params captured at
 * render time, so the second navigate silently overwrites the first. Any
 * handler that must touch multiple keys at once (e.g. "apply a filter AND
 * reset the page offset") goes through this instead. Pass null or '' to
 * remove a key, i.e. reset it to its useUrlState fallback.
 */
export function useUrlStateBatch(): (updates: Record<string, string | null>) => void {
  const [, setParams] = useSearchParams();
  return useCallback(
    (updates: Record<string, string | null>) => {
      setParams(
        (current) => {
          const updated = new URLSearchParams(current);
          for (const [key, value] of Object.entries(updates)) {
            if (!value) {
              updated.delete(key);
            } else {
              updated.set(key, value);
            }
          }
          return updated;
        },
        { replace: true },
      );
    },
    [setParams],
  );
}

/** Preferences that belong to the person, not the link. */
export function useLocalPreference<T extends string>(key: string, fallback: NoInfer<T>): [T, (next: T) => void] {
  const [value, setValue] = useState<T>(() => {
    try {
      return (window.localStorage.getItem(key) as T | null) ?? fallback;
    } catch {
      return fallback;
    }
  });

  const update = useCallback(
    (next: T) => {
      setValue(next);
      try {
        window.localStorage.setItem(key, next);
      } catch {
        /* storage may be unavailable in hardened browsers; preference is then per-session */
      }
    },
    [key],
  );

  return [value, update];
}

/** Debounces fast-changing input such as a search box. */
export function useDebounced<T>(value: T, delayMs = 250): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = window.setTimeout(() => setDebounced(value), delayMs);
    return () => window.clearTimeout(timer);
  }, [value, delayMs]);
  return debounced;
}

export type Theme = 'dark' | 'light' | 'system';
export type MotionMode = 'full' | 'reduced' | 'system';
export type Density = 'comfortable' | 'compact';

export function resolveTheme(theme: Theme): 'dark' | 'light' {
  if (theme !== 'system') return theme;
  return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
}

function resolveMotion(mode: MotionMode): 'full' | 'reduced' {
  if (mode !== 'system') return mode;
  return window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'reduced' : 'full';
}

/**
 * Appearance preferences. Changing motion mode takes effect immediately without
 * a reload, and the ambient visual layer observes the resolved value.
 */
export function useAppearance() {
  const [theme, setTheme] = useLocalPreference<Theme>('janus.theme', 'system');
  // The admin flag reduce_motion_preferred is the default for a viewer who
  // has not chosen. Only an explicit choice is stored (janus.motion); the
  // effective mode is resolved on every render as "stored choice, else the
  // instance default", so it is right whether the session loads before or
  // after this hook mounts, and a viewer who opted back in stays opted in —
  // which is what the flag's admin copy promises.
  const reduceByDefault = useSession().me?.feature_flags?.reduce_motion_preferred === true;
  const [storedMotion, setMotion] = useLocalPreference<MotionMode | ''>('janus.motion', '');
  const motion: MotionMode = storedMotion || (reduceByDefault ? 'reduced' : 'system');
  const [density, setDensity] = useLocalPreference<Density>('janus.density', 'comfortable');

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', resolveTheme(theme));
  }, [theme]);

  useEffect(() => {
    document.documentElement.setAttribute('data-motion', resolveMotion(motion));
  }, [motion]);

  useEffect(() => {
    document.documentElement.setAttribute('data-density', density);
  }, [density]);

  const reducedMotion = useMemo(() => resolveMotion(motion) === 'reduced', [motion]);

  return { theme, setTheme, motion, setMotion, density, setDensity, reducedMotion };
}

/** True while the viewport is narrower than the given breakpoint. */
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() => window.matchMedia(query).matches);
  useEffect(() => {
    const media = window.matchMedia(query);
    const listener = (event: MediaQueryListEvent) => setMatches(event.matches);
    media.addEventListener('change', listener);
    setMatches(media.matches);
    return () => media.removeEventListener('change', listener);
  }, [query]);
  return matches;
}

/** Warns before navigating away from a form with unsaved edits. */
export function useUnsavedGuard(dirty: boolean): void {
  useEffect(() => {
    if (!dirty) return undefined;
    const handler = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = '';
    };
    window.addEventListener('beforeunload', handler);
    return () => window.removeEventListener('beforeunload', handler);
  }, [dirty]);
}

/** Tracks whether the document is visible, so polling can pause in the background. */
export function useDocumentVisible(): boolean {
  const [visible, setVisible] = useState(() => document.visibilityState === 'visible');
  useEffect(() => {
    const handler = () => setVisible(document.visibilityState === 'visible');
    document.addEventListener('visibilitychange', handler);
    return () => document.removeEventListener('visibilitychange', handler);
  }, []);
  return visible;
}
