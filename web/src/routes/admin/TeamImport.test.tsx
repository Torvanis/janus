import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import TeamImport from './TeamImport';
vi.mock('../../lib/api', () => ({ api: { post: vi.fn() } }));
afterEach(cleanup);
beforeEach(() => vi.resetAllMocks());
const csv = 'Team_name,Team_admin,Additional_members\nEngineering,lead@example.com,member@example.com';
it('renders every row error and warning and blocks apply even if valid is inconsistent', async () => {
  vi.mocked(api.post).mockResolvedValue({
    valid: true,
    rows: [
      {
        row: 2,
        team_name: 'Broken',
        team_admin: 'unknown@example.com',
        additional_members: [],
        errors: ['email not found: unknown@example.com', 'team name already exists'],
        warnings: ['duplicate email omitted: duplicate@example.com'],
      },
    ],
  });
  mount();
  await upload();
  fireEvent.click(screen.getByRole('button', { name: 'Preview import' }));
  await screen.findByText('email not found: unknown@example.com');
  expect(screen.getByText('team name already exists')).toBeTruthy();
  expect(screen.getByText('Warning: duplicate email omitted: duplicate@example.com')).toBeTruthy();
  expect((screen.getByRole('button', { name: 'Import teams' }) as HTMLButtonElement).disabled).toBe(true);
});
it('rejects oversized files before making an API call', async () => {
  mount();
  fireEvent.change(screen.getByLabelText('CSV file'), {
    target: { files: [new File([new Uint8Array(2 * 1024 * 1024 + 1)], 'large.csv')] },
  });
  expect(await screen.findByRole('alert')).toHaveProperty('textContent', 'CSV exceeds the 2 MiB limit. Choose a smaller file.');
  expect(api.post).not.toHaveBeenCalled();
});
it('invalidates the previous preview when a different file is selected', async () => {
  vi.mocked(api.post).mockResolvedValue({
    valid: true,
    rows: [{ row: 2, team_name: 'Engineering', team_admin: 'lead@example.com', additional_members: [], errors: [] }],
  });
  mount();
  await upload();
  fireEvent.click(screen.getByRole('button', { name: 'Preview import' }));
  await screen.findByText('Engineering');
  await upload();
  expect(screen.queryByText('Engineering')).toBeNull();
  expect((screen.getByRole('button', { name: 'Import teams' }) as HTMLButtonElement).disabled).toBe(true);
});
it('requires a new preview after the server rejects apply', async () => {
  vi.mocked(api.post)
    .mockResolvedValueOnce({
      valid: true,
      rows: [{ row: 2, team_name: 'Engineering', team_admin: 'lead@example.com', additional_members: [], errors: [] }],
    })
    .mockRejectedValueOnce(new Error('Team already exists'));
  mount();
  await upload();
  fireEvent.click(screen.getByRole('button', { name: 'Preview import' }));
  await screen.findByText('Engineering');
  fireEvent.click(screen.getByRole('button', { name: 'Import teams' }));
  await screen.findByText('Team already exists');
  expect((screen.getByRole('button', { name: 'Import teams' }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.queryByText('Created 1 team.')).toBeNull();
});
function mount() {
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <TeamImport />
    </QueryClientProvider>,
  );
}
async function upload() {
  fireEvent.change(screen.getByLabelText('CSV file'), {
    target: { files: [new File([csv], 'teams.csv', { type: 'text/csv' })] },
  });
  await waitFor(() => expect((screen.getByRole('button', { name: 'Preview import' }) as HTMLButtonElement).disabled).toBe(false));
}
it('requires a preview and imports the exact validated CSV, reporting the created count', async () => {
  vi.mocked(api.post)
    .mockResolvedValueOnce({
      valid: true,
      rows: [
        {
          row: 2,
          team_name: 'Engineering',
          team_admin: 'lead@example.com',
          additional_members: ['member@example.com'],
          errors: [],
        },
      ],
    })
    .mockResolvedValueOnce({ created: 1, teams: [{ id: 't1' }] });
  mount();
  expect((screen.getByRole('button', { name: 'Import teams' }) as HTMLButtonElement).disabled).toBe(true);
  await upload();
  fireEvent.click(screen.getByRole('button', { name: 'Preview import' }));
  await screen.findByText('Engineering');
  fireEvent.click(screen.getByRole('button', { name: 'Import teams' }));
  expect(await screen.findByText('Created 1 team.')).toBeTruthy();
  expect(api.post).toHaveBeenLastCalledWith('/api/v1/admin/teams/import', { csv });
});
