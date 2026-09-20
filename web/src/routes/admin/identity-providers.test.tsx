/**
 * Identity providers card: upsell when unlicensed, CRUD when licensed, and
 * the Test button that probes discovery before save.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import { IdentityProvidersCard, type IdentityProviderRow } from './IdentityProvidersCard';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

const okta: IdentityProviderRow = {
  id: 'idp-1',
  slug: 'okta',
  name: 'Okta',
  issuer_url: 'https://okta.example.com',
  client_id: 'c',
  has_secret: true,
  scopes: ['openid', 'email'],
  email_claim: 'email',
  name_claim: 'name',
  groups_claim: 'groups',
  admin_groups: ['janus-admins'],
  enabled: true,
  sort_order: 0,
  callback_url: 'https://janus.example.com/auth/callback/okta',
};

function wrap(node: React.ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>{node}</ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => vi.clearAllMocks());
afterEach(cleanup);

describe('IdentityProvidersCard', () => {
  it('shows the Business upsell and disables Add when unlicensed', async () => {
    mocked.get.mockResolvedValue({ providers: [], licensed: false });
    wrap(<IdentityProvidersCard />);
    await screen.findByText('Multiple identity providers is a Business feature');
    expect(screen.getByRole('button', { name: 'Add provider' })).toHaveProperty('disabled', true);
  });

  it('lists providers with their callback URL and deletes after confirmation', async () => {
    mocked.get.mockResolvedValue({ providers: [okta], licensed: true });
    mocked.del.mockResolvedValue(undefined);
    wrap(<IdentityProvidersCard />);
    await screen.findByText('Okta');
    expect(screen.getByText('https://janus.example.com/auth/callback/okta')).toBeTruthy();
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Delete' }));
    await screen.findByText('Delete Okta?');
    await user.click(screen.getAllByRole('button', { name: 'Delete' }).at(-1)!);
    await waitFor(() => expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/identity-providers/idp-1'));
  });

  it('tests discovery, then creates the provider with parsed lists', async () => {
    mocked.get.mockResolvedValue({ providers: [], licensed: true });
    mocked.post
      .mockResolvedValueOnce({ ok: true, issuer: 'https://login.example.com' })
      .mockResolvedValueOnce({ ...okta, id: 'idp-2', slug: 'entra', name: 'Entra' });
    wrap(<IdentityProvidersCard />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Add provider' }));
    await user.type(screen.getByLabelText('Display name', { selector: 'input' }), 'Entra');
    await user.type(screen.getByLabelText('Slug', { selector: 'input' }), 'entra');
    expect(screen.getByText(/\/auth\/callback\/entra/)).toBeTruthy();
    await user.type(screen.getByLabelText('Issuer URL', { selector: 'input' }), 'https://login.example.com');
    await user.click(screen.getByRole('button', { name: 'Test' }));
    await screen.findByText('Reachable. Issuer: https://login.example.com');
    expect(mocked.post).toHaveBeenCalledWith('/api/v1/admin/identity-providers/test', {
      issuer_url: 'https://login.example.com',
    });
    await user.type(screen.getByLabelText('Client ID', { selector: 'input' }), 'cid');
    await user.type(screen.getByLabelText('Client secret', { selector: 'input' }), 'shh');
    await user.clear(screen.getByLabelText('Admin groups', { selector: 'input' }));
    await user.type(screen.getByLabelText('Admin groups', { selector: 'input' }), 'janus-admins, platform');
    await user.click(screen.getByRole('button', { name: 'Create provider' }));
    await waitFor(() =>
      expect(mocked.post).toHaveBeenLastCalledWith('/api/v1/admin/identity-providers', {
        slug: 'entra',
        name: 'Entra',
        issuer_url: 'https://login.example.com',
        client_id: 'cid',
        client_secret: 'shh',
        scopes: ['openid', 'email', 'profile'],
        groups_claim: 'groups',
        admin_groups: ['janus-admins', 'platform'],
        enabled: true,
      }),
    );
  });
});
