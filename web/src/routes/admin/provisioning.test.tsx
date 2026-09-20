import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ProvisioningPage } from './Provisioning';

vi.mock('../../lib/api', async (original) => ({
  ...(await original<typeof import('../../lib/api')>()),
  api: { get: vi.fn(), post: vi.fn(), del: vi.fn() },
}));
const mocked = vi.mocked(api);
const config = { base_url: 'https://janus.example/scim/v2', enabled: true, documentation: '/docs/scim' };
const token = {
  id: 'token-1',
  name: 'Directory',
  prefix: 'scim_abc',
  created_at: '2026-01-01T00:00:00Z',
  expires_at: '2099-01-01T00:00:00Z',
  revoked_at: null as string | null,
  last_used_at: null as string | null,
};
let rows: (typeof token)[];
function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ProvisioningPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return client;
}
beforeEach(() => {
  vi.resetAllMocks();
  rows = [];
  mocked.get.mockImplementation((path) =>
    Promise.resolve(
      path.endsWith('/config')
        ? config
        : path.endsWith('/identity-providers')
          ? { providers: [], licensed: false }
          : { tokens: rows },
    ),
  );
});
afterEach(cleanup);

describe('SCIM provisioning setup', () => {
  it('shows loading and retryable load errors and disables issuance when config is unavailable', async () => {
    mocked.get.mockImplementation(() => new Promise(() => {}));
    mount();
    expect(screen.getByText('Loading provisioning configuration')).toBeTruthy();
    expect(screen.getByText('Loading provisioning tokens')).toBeTruthy();
    cleanup();
    mocked.get.mockRejectedValue(new Error('Load unavailable'));
    mount();
    expect(await screen.findAllByText('Load unavailable')).toHaveLength(2);
    expect((screen.getByRole('button', { name: 'Issue token' }) as HTMLButtonElement).disabled).toBe(true);
    mocked.get.mockImplementation((path) =>
      Promise.resolve(
        path.endsWith('/config')
          ? config
          : path.endsWith('/identity-providers')
            ? { providers: [], licensed: false }
            : { tokens: [] },
      ),
    );
    fireEvent.click(screen.getAllByRole('button', { name: 'Try again' })[0]!);
    expect(await screen.findByText(config.base_url)).toBeTruthy();
  });
  it('handles mutation and clipboard failures without losing the one-time secret', async () => {
    rows = [token];
    mount();
    await screen.findByText('Directory');
    fireEvent.click(screen.getByRole('button', { name: 'Rotate Directory' }));
    mocked.post.mockRejectedValueOnce(new Error('Server unavailable'));
    fireEvent.click(screen.getByRole('button', { name: 'Rotate token' }));
    expect(await screen.findByText(/Could not update token/)).toBeTruthy();
    mocked.post.mockResolvedValueOnce({ token, secret: 'COPY-MANUALLY' });
    fireEvent.click(screen.getByRole('button', { name: 'Rotate token' }));
    const dialog = await screen.findByRole('dialog', { name: 'Copy your provisioning token' });
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText: vi.fn().mockRejectedValue(new Error('denied')) },
    });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Copy token' }));
    expect(await screen.findByText(/Clipboard unavailable/)).toBeTruthy();
    expect(screen.getByDisplayValue('COPY-MANUALLY')).toBeTruthy();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Close dialog' }));
    expect(screen.queryByDisplayValue('COPY-MANUALLY')).toBeNull();
  });
  it('renders expiry warnings and actual last use without enabling revoked actions', async () => {
    const lastUse = '2026-01-02T12:34:56Z';
    rows = [{ ...token, expires_at: new Date(Date.now() + 86400000).toISOString(), last_used_at: lastUse }];
    mount();
    expect(await screen.findByText('Expires soon')).toBeTruthy();
    expect(screen.getByText(new Date(lastUse).toLocaleString())).toBeTruthy();
  });
  it('validates the backend UTF-8 byte limit for token names', async () => {
    mount();
    await screen.findByText(config.base_url);
    fireEvent.click(screen.getByRole('button', { name: 'Issue token' }));
    fireEvent.change(screen.getByLabelText('Token name'), { target: { value: '界'.repeat(70) } });
    fireEvent.change(screen.getByLabelText('Expires at (local time)'), { target: { value: '2099-01-01T12:00' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create token' }));
    expect(await screen.findByText(/200 UTF-8 bytes/)).toBeTruthy();
    expect(mocked.post).not.toHaveBeenCalled();
  });
  it('treats Go zero timestamps as unset rather than revoked, expired or used', async () => {
    rows = [
      { ...token, expires_at: '0001-01-01T00:00:00Z', revoked_at: '0001-01-01T00:00:00Z', last_used_at: '0001-01-01T00:00:00Z' },
    ];
    mount();
    expect(await screen.findByText('Active')).toBeTruthy();
    expect(screen.getByText('No expiry')).toBeTruthy();
    expect(screen.getByText('Never used')).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Rotate Directory' }) as HTMLButtonElement).disabled).toBe(false);
  });
  it('shows refresh errors instead of silently presenting stale token data', async () => {
    rows = [token];
    mount();
    await screen.findByText('Directory');
    mocked.get.mockRejectedValue(new Error('offline'));
    fireEvent.click(screen.getByRole('button', { name: 'Refresh tokens' }));
    expect(await screen.findByText(/Could not refresh tokens/)).toBeTruthy();
  });
  it('confirms revocation and rotation, refreshes metadata, and renders actual lifecycle status', async () => {
    rows = [
      token,
      { ...token, id: 'old', name: 'Old', expires_at: '2020-01-01T00:00:00Z' },
      { ...token, id: 'revoked', name: 'Revoked token', revoked_at: '2026-01-02T00:00:00Z' } as typeof token,
    ];
    mount();
    await screen.findByText('Directory');
    expect(screen.getByText('Expired')).toBeTruthy();
    expect(screen.getByRole('cell', { name: 'Revoked' })).toBeTruthy();
    expect(screen.getAllByText('Never used')).toHaveLength(3);
    fireEvent.click(screen.getByRole('button', { name: 'Rotate Directory' }));
    expect(mocked.post).not.toHaveBeenCalled();
    const dialog = screen.getByRole('dialog', { name: 'Rotate provisioning token?' });
    expect(within(dialog).getByText(/immediately invalidates/)).toBeTruthy();
    mocked.post.mockResolvedValue({ token: { ...token, prefix: 'new_prefix' }, secret: 'ROTATED-SECRET' });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Rotate token' }));
    await screen.findByRole('dialog', { name: 'Copy your provisioning token' });
    expect(mocked.post).toHaveBeenCalledWith('/api/v1/admin/scim/tokens/token-1/rotate');
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(screen.queryByDisplayValue('ROTATED-SECRET')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Revoke Directory' }));
    expect(mocked.del).not.toHaveBeenCalled();
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }));
    expect(mocked.del).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Revoke Directory' }));
    mocked.del.mockImplementation(async () => {
      rows = [];
    });
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Revoke token' }));
    expect(await screen.findByText('No provisioning tokens')).toBeTruthy();
    expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/scim/tokens/token-1');
  });
  it('issues a validated token and keeps the one-time secret out of caches and storage', async () => {
    const client = mount();
    await screen.findByText(config.base_url);
    fireEvent.click(screen.getByRole('button', { name: 'Issue token' }));
    fireEvent.click(screen.getByRole('button', { name: 'Create token' }));
    expect(await screen.findByText(/Enter a token name/)).toBeTruthy();
    expect(mocked.post).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText('Token name'), { target: { value: ' Directory ' } });
    fireEvent.change(screen.getByLabelText('Expires at (local time)'), { target: { value: '2020-01-01T12:00' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create token' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Choose a future expiry');
    fireEvent.change(screen.getByLabelText('Expires at (local time)'), { target: { value: '2099-01-01T12:00' } });
    mocked.post.mockImplementation(async () => {
      rows = [token];
      return { token, secret: 'ONE-TIME-SECRET' };
    });
    fireEvent.click(screen.getByRole('button', { name: 'Create token' }));
    const dialog = await screen.findByRole('dialog', { name: 'Copy your provisioning token' });
    expect(within(dialog).getByText(/Copy now/)).toBeTruthy();
    expect(mocked.post).toHaveBeenCalledWith('/api/v1/admin/scim/tokens', {
      name: 'Directory',
      expires_at: new Date('2099-01-01T12:00').toISOString(),
    });
    const copy = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: copy } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Copy token' }));
    await waitFor(() => expect(copy).toHaveBeenCalledWith('ONE-TIME-SECRET'));
    expect(
      JSON.stringify(
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
      ),
    ).not.toContain('ONE-TIME-SECRET');
    expect(client.getMutationCache().getAll()).toHaveLength(0);
    expect(JSON.stringify(localStorage)).not.toContain('ONE-TIME-SECRET');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Done' }));
    expect(screen.queryByDisplayValue('ONE-TIME-SECRET')).toBeNull();
    expect(await screen.findByText('Directory')).toBeTruthy();
  });
  it('explains separate login, safe account matching and team reconciliation without claiming connectivity', async () => {
    mount();
    expect(await screen.findByText(config.base_url)).toBeTruthy();
    expect(screen.getByText(/OIDC and direct LDAP sign-in are separate/)).toBeTruthy();
    expect(screen.getByText(/externalId/)).toBeTruthy();
    expect(screen.getByText(/manual team memberships are preserved/i)).toBeTruthy();
    expect(screen.getByText(/No team grants are automatically created/)).toBeTruthy();
    expect(screen.getByRole('link', { name: /Map directory groups/ }).getAttribute('href')).toBe('/teams');
    expect(await screen.findByText('No provisioning tokens')).toBeTruthy();
    expect(screen.queryByText(/^connected$/i)).toBeNull();
  });
});
