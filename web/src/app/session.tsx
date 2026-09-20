import { createContext, useContext, type ReactNode } from 'react';
import { useQuery, type UseQueryResult } from '@tanstack/react-query';
import { api, ApiError } from '../lib/api';
import type { Me } from '../lib/types';

export interface PublicConfig {
  public_url: string;
  version: string;
  build: string;
  dev_auth: boolean;
  feature_flags: Record<string, boolean>;
  provider_label: string;
}

interface SessionValue {
  me: Me | null;
  config: PublicConfig | null;
  loading: boolean;
  error: unknown;
  refresh: () => void;
}

/** Exported for tests that need a fixed session without the network. */
export const SessionContext = createContext<SessionValue>({
  me: null,
  config: null,
  loading: true,
  error: null,
  refresh: () => {},
});

export function usePublicConfig(): UseQueryResult<PublicConfig> {
  return useQuery({
    queryKey: ['config'],
    queryFn: () => api.get<PublicConfig>('/api/v1/config'),
    staleTime: 60_000,
  });
}

export function SessionProvider({ children }: { children: ReactNode }): ReactNode {
  const config = usePublicConfig();
  const me = useQuery({
    queryKey: ['me'],
    queryFn: () => api.get<Me>('/api/v1/me'),
    retry: false,
    staleTime: 30_000,
  });

  // A 401 is the normal signed-out state, not an error to surface.
  const signedOut = me.error instanceof ApiError && me.error.isUnauthenticated;

  const value: SessionValue = {
    me: me.data ?? null,
    config: config.data ?? null,
    loading: me.isLoading || config.isLoading,
    error: signedOut ? null : (me.error ?? config.error),
    refresh: () => {
      void me.refetch();
      void config.refetch();
    },
  };

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionValue {
  return useContext(SessionContext);
}

/** The signed-in user, or null when the session has expired. */
export function useMe(): Me | null {
  return useSession().me;
}

/**
 * True when the instance runs in local-only mode (JANUS_LOCAL_ONLY): cost
 * tracking is disabled, so every cost/spend/pricing surface must stay hidden.
 * Token, request, and latency metrics render exactly as usual.
 */
export function useLocalOnly(): boolean {
  return useSession().me?.local_only === true;
}

/** The metric union shared by every dashboard graph selector. */
export type DashboardMetric = 'requests' | 'tokens' | 'cost';

/**
 * Instance-wide default metric for dashboard graphs, driven by the admin
 * spend_emphasis feature flag surfaced in /api/v1/me feature_flags: 'cost'
 * when the instance emphasises spend, 'tokens' (usage emphasis, the shipped
 * default) otherwise. Callers pass this as the useUrlState('metric', …)
 * fallback, so an explicit ?metric= URL value or a manual selector click
 * still wins for that view.
 */
/**
 * Whether the installed license carries a Business feature. Used to show
 * the upsell (and disable the control) before the API would answer 402;
 * the API remains the authority.
 */
export function useLicensed(feature: string): boolean {
  const lic = useSession().me?.license;
  // Unknown (session still loading, or an older gateway without the field):
  // leave the control enabled — the API answers 402 with the reason. Only a
  // known-Community or restricted license greys it out with the upsell.
  if (!lic || !Array.isArray(lic.features)) return true;
  return !lic.restricted && lic.features.includes(feature);
}

export function useDefaultMetric(): 'tokens' | 'cost' {
  const flags = useSession().me?.feature_flags;
  return flags?.spend_emphasis ? 'cost' : 'tokens';
}
