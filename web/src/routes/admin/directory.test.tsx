/**
 * Directory card: upsell when unlicensed; Test resolves a user and shows the
 * admin prediction; Connect sends the parsed payload; Disconnect confirms.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import { DirectoryCard, type DirectoryView } from './DirectoryCard';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

const dir: DirectoryView = {
  url: 'ldaps://ad.example.com:636',
  start_tls: false,
  skip_verify: false,
  bind_dn: 'cn=janus,dc=example,dc=com',
  has_bind_password: true,
  base_dn: 'dc=example,dc=com',
  user_filter: '(mail={login})',
  email_attr: 'mail',
  name_attr: 'displayName',
  groups_attr: 'memberOf',
  group_filter: '',
  group_name_attr: 'cn',
  admin_groups: ['Janus Admins'],
  enabled: true,
  updated_at: '2026-09-18T00:00:00Z',
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

describe('DirectoryCard', () => {
  it('upsells and disables the form when unlicensed', async () => {
    mocked.get.mockResolvedValue({ directory: null, licensed: false });
    wrap(<DirectoryCard />);
    await screen.findByText('Directory sign-in is a Business feature');
    expect(screen.getByLabelText('Server URL', { selector: 'input' }).closest('fieldset')).toHaveProperty('disabled', true);
  });

  it('tests with a login and shows the resolved user and admin prediction', async () => {
    mocked.get.mockResolvedValue({ directory: dir, licensed: true });
    mocked.post.mockResolvedValue({
      ok: true,
      user: {
        dn: 'cn=ada,dc=example,dc=com',
        email: 'ada@example.com',
        name: 'Ada',
        groups: ['Janus Admins', 'staff'],
        would_be_admin: true,
      },
    });
    wrap(<DirectoryCard />);
    const user = userEvent.setup();
    await screen.findByDisplayValue('ldaps://ad.example.com:636');
    await user.type(screen.getByLabelText('Login to resolve (optional)', { selector: 'input' }), 'ada@example.com');
    await user.click(screen.getByRole('button', { name: 'Test' }));
    await screen.findByText('cn=ada,dc=example,dc=com');
    expect(screen.getByText(/would be an administrator/)).toBeTruthy();
    expect(screen.getByText('Groups: Janus Admins, staff')).toBeTruthy();
    expect(mocked.post).toHaveBeenCalledWith(
      '/api/v1/admin/directory/test',
      expect.objectContaining({ login: 'ada@example.com', bind_password: undefined, admin_groups: ['Janus Admins'] }),
    );
  });

  it('connects a new directory with the parsed payload', async () => {
    mocked.get.mockResolvedValue({ directory: null, licensed: true });
    mocked.put.mockResolvedValue(dir);
    wrap(<DirectoryCard />);
    const user = userEvent.setup();
    await screen.findByRole('button', { name: 'Connect directory' });
    await user.type(screen.getByLabelText('Server URL', { selector: 'input' }), 'ldaps://ad.example.com:636');
    await user.type(screen.getByLabelText('Base DN', { selector: 'input' }), 'dc=example,dc=com');
    await user.type(screen.getByLabelText('Service account password', { selector: 'input' }), 'secret');
    await user.type(screen.getByLabelText('Admin groups', { selector: 'input' }), 'Janus Admins, ops');
    await user.click(screen.getByRole('button', { name: 'Connect directory' }));
    await waitFor(() =>
      expect(mocked.put).toHaveBeenCalledWith(
        '/api/v1/admin/directory',
        expect.objectContaining({
          url: 'ldaps://ad.example.com:636',
          base_dn: 'dc=example,dc=com',
          bind_password: 'secret',
          admin_groups: ['Janus Admins', 'ops'],
          enabled: true,
        }),
      ),
    );
  });

  it('disconnects after confirmation', async () => {
    mocked.get.mockResolvedValue({ directory: dir, licensed: true });
    mocked.del.mockResolvedValue(undefined);
    wrap(<DirectoryCard />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Disconnect' }));
    await screen.findByText('Disconnect the directory?');
    await user.click(screen.getAllByRole('button', { name: 'Disconnect' }).at(-1)!);
    await waitFor(() => expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/directory'));
  });
});
