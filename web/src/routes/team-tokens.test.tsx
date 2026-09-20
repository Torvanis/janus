import { beforeEach, expect, it, vi } from 'vitest';
import { render, screen, within, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter, Routes, Route } from 'react-router-dom';
import { TokensPage } from './Tokens';
import { ToastProvider } from '../components/ui';
import { api } from '../lib/api';
const session = vi.hoisted(() => ({ me: { active_team_id: 'team-a', teams: [{ id: 'team-a', name: 'Research' }] } }));
vi.mock('../app/session', () => ({ useMe: () => session.me, useLocalOnly: () => true }));
beforeEach(() => {
  vi.restoreAllMocks();
  session.me = { active_team_id: 'team-a', teams: [{ id: 'team-a', name: 'Research' }] };
  vi.spyOn(api, 'get').mockResolvedValue({ tokens: [] });
  vi.spyOn(api, 'post').mockResolvedValue({ token: { description: 'Key' }, value: 'secret' });
});
function mount(url = '/tokens', client = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[url]}>
          <Routes>
            <Route path="/tokens/:id?" element={<TokensPage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}
it('changes an existing key to Personal for future requests by default and refreshes its row', async () => {
  const token = { id: 'key', description: 'Existing key', prefix: 'j_key', team_id: 'team-a', created_at: '', revoked_at: '' };
  vi.mocked(api.get).mockResolvedValue({ tokens: [token] });
  const put = vi.spyOn(api, 'put').mockImplementation(async () => {
    token.team_id = '';
    return { token, reattributed_requests: 0 };
  });
  mount();
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: 'Change team' }));
  const dialog = within(screen.getByRole('dialog'));
  expect((dialog.getByRole('combobox', { name: 'Token context' }) as HTMLSelectElement).value).toBe('team-a');
  expect((dialog.getByRole('radio', { name: 'Future requests only' }) as HTMLInputElement).checked).toBe(true);
  expect(dialog.getByText(/plaintext key stays unchanged/i)).toBeTruthy();
  expect(dialog.getByText(/in-flight requests retain/i)).toBeTruthy();
  await user.selectOptions(dialog.getByRole('combobox'), '');
  await user.click(dialog.getByRole('button', { name: 'Save team' }));
  await waitFor(() => expect(put).toHaveBeenCalledWith('/api/v1/tokens/key/team', { team_id: '', move_history: false }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(within(screen.getByRole('table')).getByText('Personal')).toBeTruthy();
});

it('reattributes only the selected key with explicit consent and invalidates detail and usage caches', async () => {
  const token = { id: 'key', description: 'Existing key', prefix: 'j_key', team_id: '', created_at: '', revoked_at: '' };
  vi.mocked(api.get).mockResolvedValue({ tokens: [token] });
  const put = vi.spyOn(api, 'put').mockResolvedValue({ token: { ...token, team_id: 'team-a' }, reattributed_requests: 12 });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  for (const key of [['token-usage', 'key'], ['dashboard'], ['quota'], ['requests']]) client.setQueryData(key, {});
  mount('/tokens', client);
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: 'Change team' }));
  const dialog = within(screen.getByRole('dialog'));
  await user.selectOptions(dialog.getByRole('combobox'), 'team-a');
  await user.click(dialog.getByRole('radio', { name: 'Reattribute existing usage for this token' }));
  expect(dialog.getByRole('note').textContent).toMatch(/Historical totals change.*quotas/);
  expect(put).not.toHaveBeenCalled();
  await user.click(dialog.getByRole('button', { name: 'Save team' }));
  await waitFor(() => expect(put).toHaveBeenCalledWith('/api/v1/tokens/key/team', { team_id: 'team-a', move_history: true }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(screen.getByText(/12 existing requests reattributed/)).toBeTruthy();
  for (const key of [['token-usage', 'key'], ['dashboard'], ['quota'], ['requests']])
    expect(client.getQueryState(key)?.isInvalidated).toBe(true);
});

it('allows a removed binding to switch to a current team and surfaces save errors for retry', async () => {
  const token = { id: 'key', description: 'Existing key', prefix: 'j_key', team_id: 'removed', created_at: '', revoked_at: '' };
  vi.mocked(api.get).mockResolvedValue({ tokens: [token] });
  const put = vi
    .spyOn(api, 'put')
    .mockRejectedValueOnce(new Error('Membership changed; choose another team.'))
    .mockResolvedValue({ token: { ...token, team_id: 'team-a' }, reattributed_requests: 0 });
  mount();
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: 'Change team' }));
  const dialog = within(screen.getByRole('dialog'));
  expect((dialog.getByRole('combobox') as HTMLSelectElement).value).toBe('removed');
  expect((dialog.getByRole('button', { name: 'Save team' }) as HTMLButtonElement).disabled).toBe(true);
  await user.selectOptions(dialog.getByRole('combobox'), 'team-a');
  await user.click(dialog.getByRole('button', { name: 'Save team' }));
  expect(await dialog.findByRole('alert')).toHaveProperty('textContent', 'Membership changed; choose another team.');
  expect(screen.getByRole('dialog')).toBeTruthy();
  await user.click(dialog.getByRole('button', { name: 'Save team' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(put).toHaveBeenLastCalledWith('/api/v1/tokens/key/team', { team_id: 'team-a', move_history: false });
});

it('does not offer team changes for revoked keys and cancellation does not mutate', async () => {
  const token = { id: 'key', description: 'Existing key', prefix: 'j_key', team_id: '', created_at: '', revoked_at: '' };
  vi.mocked(api.get).mockResolvedValue({
    tokens: [token, { ...token, id: 'revoked', description: 'Revoked key', revoked_at: '2026-01-01T00:00:00Z' }],
  });
  const put = vi.spyOn(api, 'put');
  mount();
  const user = userEvent.setup();
  await screen.findByText('Revoked key');
  expect(screen.getAllByRole('button', { name: 'Change team' })).toHaveLength(1);
  await user.click(screen.getByRole('button', { name: 'Change team' }));
  await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }));
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(put).not.toHaveBeenCalled();
});

it('shows the recorded charged team in token recent requests rather than current key affinity', async () => {
  const token = { id: 'key', description: 'Existing key', prefix: 'j_key', team_id: 'team-a', created_at: '', revoked_at: '' };
  vi.mocked(api.get).mockImplementation(async (url) =>
    url.includes('/dashboard/')
      ? {
          token,
          totals: { request_count: 1 },
          per_model: [],
          recent_requests: [{ id: 1, model: 'model', created_at: '', team_ids: 'old-team', team_names: ['Historical team'] }],
        }
      : { tokens: [token] },
  );
  mount('/tokens/key');
  const dialog = within(await screen.findByRole('dialog'));
  expect(await dialog.findByText('Historical team')).toBeTruthy();
  expect(dialog.getByText('Research')).toBeTruthy();
});

it('blocks a removed active team instead of silently falling back to personal', async () => {
  session.me.teams = [];
  mount();
  const user = userEvent.setup();
  await user.click(screen.getByRole('button', { name: 'Create token' }));
  const dialog = within(screen.getByRole('dialog'));
  expect((dialog.getByRole('combobox', { name: 'Token context' }) as HTMLSelectElement).value).toBe('team-a');
  await user.type(dialog.getByRole('textbox'), 'Key');
  await user.click(dialog.getByRole('button', { name: 'Create token' }));
  expect(api.post).not.toHaveBeenCalled();
  expect(dialog.getByText(/Choose Personal or a current team/)).toBeTruthy();
  await user.selectOptions(dialog.getByRole('combobox'), '');
  await user.click(dialog.getByRole('button', { name: 'Create token' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/tokens', { description: 'Key' }));
});
it('creates explicit personal keys with zero teams', async () => {
  session.me = { active_team_id: '', teams: [] };
  mount();
  const user = userEvent.setup();
  await user.click(screen.getByRole('button', { name: 'Create token' }));
  const dialog = within(screen.getByRole('dialog'));
  expect(dialog.getAllByRole('option')).toHaveLength(1);
  await user.type(dialog.getByRole('textbox'), 'Key');
  await user.click(dialog.getByRole('button', { name: 'Create token' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/tokens', { description: 'Key' }));
});
it('labels personal and team keys and makes removed memberships actionable in list and detail', async () => {
  const token = {
    id: 'removed',
    description: 'Old key',
    prefix: 'j_old',
    team_id: 'deleted-team',
    created_at: '',
    revoked_at: '',
  };
  vi.spyOn(api, 'get').mockImplementation(async (url) =>
    url.includes('/dashboard/')
      ? { token, totals: {}, per_model: [], recent_requests: [] }
      : {
          tokens: [
            token,
            { ...token, id: 'personal', description: 'Personal key', team_id: '' },
            { ...token, id: 'team', description: 'Team key', team_id: 'team-a' },
          ],
        },
  );
  mount('/tokens/removed');
  const dialog = within(await screen.findByRole('dialog'));
  await waitFor(() => expect(dialog.getByText('deleted-team')).toBeTruthy());
  expect(dialog.getByText('Membership removed')).toBeTruthy();
  expect(dialog.getByText(/Change this key/)).toBeTruthy();
  const table = within(screen.getByRole('table'));
  expect(table.getByText('Research')).toBeTruthy();
  expect(table.getByText('Personal')).toBeTruthy();
  expect(table.getByText('deleted-team')).toBeTruthy();
  expect(table.getByText('Membership removed')).toBeTruthy();
});
it('shows the active team binding explicitly and explains editable key affinity', async () => {
  mount();
  const user = userEvent.setup();
  await user.click(screen.getByRole('button', { name: 'Create token' }));
  const dialog = within(screen.getByRole('dialog'));
  expect((dialog.getByRole('combobox', { name: 'Token context' }) as HTMLSelectElement).value).toBe('team-a');
  expect(dialog.getByText(/Change team/)).toBeTruthy();
  await user.type(dialog.getByRole('textbox'), 'Key');
  await user.click(dialog.getByRole('button', { name: 'Create token' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/tokens', { description: 'Key', team_id: 'team-a' }));
});
