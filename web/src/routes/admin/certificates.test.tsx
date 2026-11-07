/**
 * Certificates card: mounted sources are read-only, uploads post the PEM,
 * removal confirms, expiry is flagged.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import { CertificatesCard, type TrustStoreResponse } from './CertificatesCard';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

const store: TrustStoreResponse = {
  trust_store: {
    dod_roots: true,
    directory: '/etc/janus/ca.d',
    certificates: [
      {
        id: 'up1',
        name: 'Lab CA',
        source: 'uploaded',
        subject: 'CN=Lab CA',
        issuer: 'CN=Lab CA',
        sha256: 'aa',
        is_ca: true,
        not_before: '2026-01-01T00:00:00Z',
        not_after: '2026-11-20T00:00:00Z',
        read_only: false,
        expires_soon: true,
        expired: false,
      },
      {
        name: 'corp-root',
        source: 'directory',
        path: '/etc/janus/ca.d/corp.crt',
        subject: 'CN=Corp Root',
        issuer: 'CN=Corp Root',
        sha256: 'bb',
        is_ca: true,
        not_before: '2026-01-01T00:00:00Z',
        not_after: '2036-01-01T00:00:00Z',
        read_only: true,
        expires_soon: false,
        expired: false,
      },
      {
        name: 'DoD Root CA 6',
        source: 'dod',
        subject: 'CN=DoD Root CA 6',
        issuer: 'CN=DoD Root CA 6',
        sha256: 'cc',
        is_ca: true,
        not_before: '2023-01-01T00:00:00Z',
        not_after: '2053-01-24T00:00:00Z',
        read_only: true,
        expires_soon: false,
        expired: false,
      },
    ],
    untrusted_allowed: false,
    expiring_soon: 1,
    expired: 0,
  },
  untrusted_tls_available: false,
  expiry_warning_days: 30,
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

describe('CertificatesCard', () => {
  it('lists every source; only uploads can be removed; expiry is flagged', async () => {
    mocked.get.mockResolvedValue(store);
    wrap(<CertificatesCard />);
    const rows = (await screen.findAllByRole('row')) as [HTMLElement, HTMLElement, HTMLElement, HTMLElement];
    expect(rows).toHaveLength(4); // header + 3
    expect(within(rows[1]).getByText('Expires soon')).toBeTruthy();
    expect(within(rows[1]).getByRole('button', { name: 'Remove' })).toBeTruthy();
    expect(within(rows[2]).getByText('Managed by the deployment')).toBeTruthy();
    expect(within(rows[2]).queryByRole('button', { name: 'Remove' })).toBeNull();
    expect(within(rows[3]).getByText('DoD PKI (built in)')).toBeTruthy();
    expect(screen.getByText(/DoD PKI root CAs: included/)).toBeTruthy();
    expect(screen.getByText(/Allow untrusted certificate is turned off/)).toBeTruthy();
  });

  it('uploads a pasted PEM', async () => {
    mocked.get.mockResolvedValue({ ...store, trust_store: { ...store.trust_store, certificates: [] } });
    mocked.post.mockResolvedValue({ certificate: { id: 'n' } });
    wrap(<CertificatesCard />);
    const user = userEvent.setup();
    await screen.findByText(/No certificates beyond/);
    await user.click(screen.getByRole('button', { name: 'Add certificate' }));
    expect(await screen.findByText('Paste a PEM certificate or choose a file.')).toBeTruthy();
    expect(mocked.post).not.toHaveBeenCalled();
    await user.type(screen.getByLabelText(/^Name/, { selector: 'input' }), 'Corp');
    await user.type(screen.getByLabelText(/^PEM certificate/, { selector: 'textarea' }), '-----BEGIN CERTIFICATE-----x');
    await user.click(screen.getByRole('button', { name: 'Add certificate' }));
    await waitFor(() =>
      expect(mocked.post).toHaveBeenCalledWith('/api/v1/admin/certificates', {
        name: 'Corp',
        pem: '-----BEGIN CERTIFICATE-----x',
      }),
    );
  });

  it('removes an upload after confirming', async () => {
    mocked.get.mockResolvedValue(store);
    mocked.del.mockResolvedValue({ deleted: 'up1' });
    wrap(<CertificatesCard />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Remove' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Stop trusting Lab CA?')).toBeTruthy();
    await user.click(within(dialog).getByRole('button', { name: 'Remove' }));
    await waitFor(() => expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/certificates/up1'));
  });
});
