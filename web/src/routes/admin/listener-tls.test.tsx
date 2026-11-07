import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import { ListenerTLSCard, type ListenerTLSInfo } from './ListenerTLSCard';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

const selfSigned: ListenerTLSInfo = {
  mode: 'self_signed',
  subject: 'CN=janus.test',
  names: ['janus.test', '127.0.0.1'],
  sha256: 'abc',
  not_after: '2027-11-04T00:00:00Z',
  expires_soon: false,
  expired: false,
  required: true,
  generated_by: 'startup',
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

describe('ListenerTLSCard', () => {
  it('explains plain HTTP behind a proxy', async () => {
    mocked.get.mockResolvedValue({ listener: { mode: 'none', expires_soon: false, expired: false, required: false } });
    wrap(<ListenerTLSCard />);
    expect(await screen.findByText(/Janus serves plain HTTP/)).toBeTruthy();
    expect(screen.queryByRole('link', { name: 'Download certificate' })).toBeNull();
  });

  it('self-signed: temporary note, download link, confirmed regenerate', async () => {
    mocked.get.mockResolvedValue({ listener: selfSigned });
    mocked.post.mockResolvedValue({ listener: { ...selfSigned, sha256: 'def' } });
    wrap(<ListenerTLSCard />);
    expect(await screen.findByText(/Self-signed \(temporary\)/)).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Download certificate' }).getAttribute('href')).toBe(
      '/api/v1/admin/listener-tls/certificate.pem',
    );
    expect(screen.getByText(/JANUS_TLS_REQUIRE is on/)).toBeTruthy();
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Regenerate' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Regenerate' }));
    await waitFor(() => expect(mocked.post).toHaveBeenCalledWith('/api/v1/admin/listener-tls/regenerate', {}));
  });

  it('mounted: no regenerate button', async () => {
    mocked.get.mockResolvedValue({ listener: { ...selfSigned, mode: 'mounted', cert_path: '/tls/tls.crt', expires_soon: true } });
    wrap(<ListenerTLSCard />);
    expect(await screen.findByText(/Issued, mounted from files/)).toBeTruthy();
    expect(screen.getByText('Expires soon')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Regenerate' })).toBeNull();
  });
});
