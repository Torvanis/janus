import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { describe, it, expect, vi } from 'vitest';
import { ActiveTeamSelector } from './ActiveTeamSelector';
import { api } from '../lib/api';
vi.mock('../lib/api', () => ({ api: { put: vi.fn() } }));
describe('active team selection', () => {
  it('changes browser context explicitly and offers personal context', async () => {
    vi.mocked(api.put).mockResolvedValue({});
    const refresh = vi.fn();
    render(
      <QueryClientProvider client={new QueryClient()}>
        <ActiveTeamSelector teams={[{ id: 'a', name: 'Alpha' }]} activeTeam="" refresh={refresh} />
      </QueryClientProvider>,
    );
    fireEvent.change(screen.getByLabelText('Working under'), { target: { value: 'a' } });
    await waitFor(() => expect(api.put).toHaveBeenCalledWith('/api/v1/me/active-team', { team_id: 'a' }));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    expect(screen.getByText('Personal')).toBeTruthy();
    expect(screen.getByTitle(/does not change API keys. Manage each/)).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Manage token teams' }).getAttribute('href')).toBe('/tokens');
    expect(screen.getByLabelText('Working under').getAttribute('title')).toMatch(/browser.*model/i);
  });
  it('keeps removed context visible with an escape instead of silently selecting another team', () => {
    render(
      <QueryClientProvider client={new QueryClient()}>
        <ActiveTeamSelector teams={[]} activeTeam="removed" refresh={() => {}} />
      </QueryClientProvider>,
    );
    expect(screen.getByText('Team unavailable — select another context')).toBeTruthy();
    expect(screen.getByText('Personal')).toBeTruthy();
  });
});
