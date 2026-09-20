import { beforeEach, expect, it, vi } from 'vitest';
import { render, screen, within, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router-dom';
import { GrantsPage } from './Grants';
import { ToastProvider } from '../../components/ui';
import { api } from '../../lib/api';

beforeEach(() => {
  vi.restoreAllMocks();
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url.endsWith('/managed-models')) return { managed_models: [{ id: 'alias', name: 'Alias', status: 'enabled', broken: false }] };
    if (url.endsWith('/models')) return { models: [{ id: 'real', name: 'Real', status: 'enabled' }] };
    if (url.endsWith('/teams')) return { teams: [{ id: 'team-a', name: 'Research' }] };
    return { grants: [], groups: [], users: [], service_tokens: [] };
  });
  vi.spyOn(api, 'post').mockResolvedValue({});
});
function mount(url = '/admin/grants') {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ToastProvider><MemoryRouter initialEntries={[url]}><GrantsPage /></MemoryRouter></ToastProvider></QueryClientProvider>);
}
it('confirms all-teams grants with an empty grantee id for both model kinds', async () => {
  mount();
  const user = userEvent.setup();
  await user.click(screen.getByRole('button', { name: 'Grant access' }));
  const dialog = within(screen.getByRole('dialog'));
  await user.selectOptions(dialog.getByRole('listbox'), ['model:real', 'managed:alias']);
  await user.selectOptions(dialog.getAllByRole('combobox')[0]!, 'all_teams');
  expect(dialog.getByRole('option', { name: 'All teams (including future teams)' })).toBeTruthy();
  await user.click(dialog.getByRole('button', { name: 'Grant access' }));
  expect(api.post).not.toHaveBeenCalled();
  const confirm = within(screen.getAllByRole('dialog').at(-1)!);
  await user.click(confirm.getByRole('button', { name: /Grant/ }));
  await waitFor(() => expect(api.post).toHaveBeenCalledTimes(2));
  for (const model_kind of ['model', 'managed']) expect(api.post).toHaveBeenCalledWith('/api/v1/admin/grants', expect.objectContaining({ model_kind, grantee_type: 'all_teams', grantee_id: '' }));
});
it('filters and groups team grants through deep links without unioning personal grants', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({ grants: [
    { id: 't', model_id: 'real', model_name: 'Team model', grantee_type: 'team', grantee_id: 'team-a', grantee_name: 'Research' },
    { id: 'u', model_id: 'other', model_name: 'Personal model', grantee_type: 'user', grantee_id: 'u', grantee_name: 'Ada' },
  ], models: [] });
  mount('/admin/grants?view=grantee&type=team&grantee=team%3Ateam-a');
  expect((await screen.findByTestId('grants-group-count')).textContent).toBe('1 of 1 grantees');
  expect(screen.getByText('Team model')).toBeTruthy();
  expect(screen.queryByText('Personal model')).toBeNull();
  expect(screen.getByRole('option', { name: 'Research' })).toBeTruthy();
  expect(screen.getByText('Team', { selector: '.badge' })).toBeTruthy();
  expect(screen.getByText(/Only this team/)).toBeTruthy();
});
it('creates team grants for real and managed models without merging channels', async () => {
  mount();
  const user = userEvent.setup();
  await user.click(screen.getByRole('button', { name: 'Grant access' }));
  const dialog = within(screen.getByRole('dialog'));
  await user.selectOptions(dialog.getByRole('listbox'), ['model:real', 'managed:alias']);
  await user.selectOptions(dialog.getAllByRole('combobox')[0]!, 'team');
  await user.selectOptions(dialog.getByRole('combobox', { name: /Team/ }), 'team-a');
  await user.click(dialog.getByRole('button', { name: 'Grant access' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/admin/grants', { model_ids: ['real'], model_kind: 'model', grantee_type: 'team', grantee_id: 'team-a' }));
  expect(api.post).toHaveBeenCalledWith('/api/v1/admin/grants', { model_ids: ['alias'], model_kind: 'managed', grantee_type: 'team', grantee_id: 'team-a' });
});
