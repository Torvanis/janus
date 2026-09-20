import { describe, it, expect, vi } from 'vitest';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, cleanup } from '@testing-library/react';
import { Breadcrumb } from './Breadcrumb';
vi.mock('./session', () => ({ useSession: () => ({ me: { id: 'viewer', role: 'user' } }) }));

describe('named team breadcrumb', () => {
  it('uses the authorized team name instead of exposing a route identifier', () => {
    const client = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } });
    client.setQueryData(['teams', 'breadcrumb', false, 'viewer'], { teams: [{ id: 'opaque-team-id', name: 'Research', my_role: 'member' }] });
    render(
      <QueryClientProvider client={client}>
        <Breadcrumb path="/teams/opaque-team-id" />
      </QueryClientProvider>,
    );
    expect(screen.getByText('Research')).toBeTruthy();
    expect(screen.queryByText('opaque team id')).toBeNull();
    cleanup();
  });
});
