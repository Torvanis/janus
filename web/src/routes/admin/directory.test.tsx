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

  it('saves a default domain and shows the full name a short test login searched for', async () => {
    mocked.get.mockResolvedValue({ directory: dir, licensed: true });
    mocked.post.mockResolvedValue({
      ok: true,
      login: 'ada@example.com',
      user: { dn: 'cn=ada,dc=example,dc=com', email: 'ada@example.com', name: 'Ada', groups: [], would_be_admin: false },
    });
    mocked.put.mockResolvedValue({ ...dir, default_domain: 'example.com' });
    wrap(<DirectoryCard />);
    const user = userEvent.setup();
    await screen.findByDisplayValue('ldaps://ad.example.com:636');
    const domain = screen.getByLabelText('Default domain', { selector: 'input' });
    expect((domain as HTMLInputElement).value).toBe('');
    await user.type(domain, ' example.com ');
    await user.type(screen.getByLabelText('Login to resolve (optional)', { selector: 'input' }), 'ada');
    await user.click(screen.getByRole('button', { name: 'Test' }));
    await screen.findByText('Searched for ada@example.com');
    expect(mocked.post).toHaveBeenCalledWith(
      '/api/v1/admin/directory/test',
      expect.objectContaining({ login: 'ada', default_domain: 'example.com' }),
    );
    await user.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() =>
      expect(mocked.put).toHaveBeenCalledWith(
        '/api/v1/admin/directory',
        expect.objectContaining({ default_domain: 'example.com' }),
      ),
    );
  });

  it('loads a saved default domain', async () => {
    mocked.get.mockResolvedValue({ directory: { ...dir, default_domain: 'corp.example.com' }, licensed: true });
    wrap(<DirectoryCard />);
    await screen.findByDisplayValue('corp.example.com');
  });

  it('derives the Windows domain placeholder, saves a set one, and shows the name a DOMAIN\\name test searched for', async () => {
    mocked.get.mockResolvedValue({ directory: { ...dir, default_domain: 'corp.example.com' }, licensed: true });
    mocked.post.mockResolvedValue({
      ok: true,
      login: 'ada@lab.example.org',
      user: { dn: 'cn=ada,dc=corp', email: 'ada@lab.example.org', name: 'Ada', groups: [], would_be_admin: false },
    });
    mocked.put.mockResolvedValue({ ...dir, default_domain: 'corp.example.com', windows_domain: 'ACME' });
    wrap(<DirectoryCard />);
    const user = userEvent.setup();
    await screen.findByDisplayValue('corp.example.com');
    const windows = screen.getByLabelText('Windows domain name', { selector: 'input' }) as HTMLInputElement;
    expect(windows.value).toBe('');
    expect(windows.placeholder).toBe('CORP');
    expect(screen.getByText(/The short name people type before a backslash/)).toBeTruthy();
    // The placeholder follows the default domain as it is edited.
    const domain = screen.getByLabelText('Default domain', { selector: 'input' });
    await user.clear(domain);
    expect(windows.placeholder).toBe('CORP');
    expect(screen.getByText('Set a default domain first.')).toBeTruthy();
    await user.type(domain, 'lab.example.org');
    expect(windows.placeholder).toBe('LAB');
    await user.type(screen.getByLabelText('Login to resolve (optional)', { selector: 'input' }), 'LAB\\ada');
    await user.click(screen.getByRole('button', { name: 'Test' }));
    await screen.findByText('Searched for ada@lab.example.org');
    await user.type(windows, 'acme');
    await user.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() =>
      expect(mocked.put).toHaveBeenCalledWith(
        '/api/v1/admin/directory',
        expect.objectContaining({ default_domain: 'lab.example.org', windows_domain: 'acme' }),
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
