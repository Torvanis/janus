import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router-dom';
import { api, ApiError } from '../../lib/api';
import { LicenseSyncControls } from './LicenseSyncControls';
import { LicenseBanner } from '../../app/LicenseBanner';
import { LicenseCard } from './LicenseCard';
import { ToastProvider } from '../../components/ui';
import type { LicenseDocument, LicenseSummary, LicenseSync } from '../../lib/types';

vi.mock('../../lib/api', async (original) => ({
  ...(await original<typeof import('../../lib/api')>()),
  api: { get: vi.fn(), put: vi.fn(), post: vi.fn() },
}));
let license: LicenseSummary;
vi.mock('../../app/session', () => ({ useSession: () => ({ me: { role: 'user', license } }) }));
const disabled: LicenseSync = { enabled: false, has_token: false, mode: 'manual', health: 'disabled' };
function mount(node = <LicenseSyncControls />) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter>{node}</MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.resetAllMocks();
});
describe('license sync controls', () => {
  it('defaults off; storing a token never opts in; explicit enable and disable persist', async () => {
    vi.mocked(api.get).mockResolvedValue({ license_sync: disabled });
    vi.mocked(api.put).mockResolvedValue({});
    mount();
    await screen.findByText('License sync health: disabled');
    expect(screen.getByLabelText('Enable automatic license sync')).toHaveProperty('checked', false);
    const input = screen.getByLabelText('License sync token');
    expect(input).toHaveProperty('type', 'password');
    fireEvent.change(input, { target: { value: 'fixture-token-only' } });
    fireEvent.click(screen.getByText('Save license sync'));
    await waitFor(() =>
      expect(api.put).toHaveBeenLastCalledWith('/api/v1/admin/system/license/sync', { token: 'fixture-token-only' }),
    );
    await waitFor(() => expect(input).toHaveProperty('value', ''));
    fireEvent.click(screen.getByLabelText('Enable automatic license sync'));
    fireEvent.click(screen.getByText('Save license sync'));
    await waitFor(() => expect(api.put).toHaveBeenLastCalledWith('/api/v1/admin/system/license/sync', { enabled: true }));
  });
  it.each(['enabled_managed', 'token_managed'] as const)('pins only %s', async (flag) => {
    vi.mocked(api.get).mockResolvedValue({ license_sync: { ...disabled, [flag]: true } });
    mount();
    await screen.findByText('License sync health: disabled');
    expect(screen.getByLabelText('Enable automatic license sync')).toHaveProperty('disabled', flag === 'enabled_managed');
    expect(screen.getByLabelText('License sync token')).toHaveProperty('disabled', flag === 'token_managed');
  });
  it('clears explicitly, preserves blank and runs Sync now without token echo', async () => {
    vi.mocked(api.get).mockResolvedValue({ license_sync: { ...disabled, enabled: true, has_token: true, mode: 'online' } });
    vi.mocked(api.post).mockResolvedValue({});
    vi.mocked(api.put).mockResolvedValue({});
    mount();
    await screen.findByText('License sync health: disabled');
    fireEvent.click(screen.getByText('Sync now'));
    await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/admin/system/license/sync', {}));
    await screen.findByText('License sync completed.');
    fireEvent.click(screen.getByLabelText('Enable automatic license sync'));
    fireEvent.click(screen.getByText('Save license sync'));
    await waitFor(() => expect(api.put).toHaveBeenLastCalledWith('/api/v1/admin/system/license/sync', { enabled: false }));
    await waitFor(() => expect(screen.getByText('Save license sync')).toHaveProperty('disabled', true));
    fireEvent.click(screen.getByLabelText('Clear stored sync token'));
    fireEvent.click(screen.getByText('Save license sync'));
    await waitFor(() => expect(api.put).toHaveBeenLastCalledWith('/api/v1/admin/system/license/sync', { clear_token: true }));
  });
  it.each(['offline', 'file'] as const)('blocks network action in %s mode', async (mode) => {
    vi.mocked(api.get).mockResolvedValue({ license_sync: { ...disabled, enabled: true, has_token: true, mode } });
    mount();
    await screen.findByText('License sync health: disabled');
    expect(screen.getByText('Sync now')).toHaveProperty('disabled', true);
    expect(
      screen.getByText(mode === 'file' ? /File-backed license sync is unsupported/ : /unavailable in offline mode/),
    ).toBeTruthy();
  });
  it('handles old-server 404 safely', async () => {
    vi.mocked(api.get).mockRejectedValue(
      new ApiError(404, { code: 'not_found', message: 'fixture-secret-not-for-display', type: 'error' }),
    );
    mount();
    await screen.findByText(/does not support license sync/);
    expect(screen.getByLabelText('Enable automatic license sync')).toHaveProperty('checked', false);
    expect(screen.queryByText(/fixture-secret/)).toBeNull();
  });
  it('shows cancellation, payment and stale health even without expiry', async () => {
    vi.mocked(api.get).mockResolvedValue({
      license_sync: {
        ...disabled,
        health: 'stale',
        subscription: { status: 'past_due', auto_renew: false, cancel_at_period_end: true },
      },
    });
    mount();
    await screen.findByText('License sync health: stale');
    expect(screen.getByText('Subscription: past_due')).toBeTruthy();
    expect(screen.getByText(/Cancellation is scheduled/)).toBeTruthy();
  });
});
describe('license sync polling', () => {
  it('polls health without overwriting an unsaved token or opting in', async () => {
    vi.useFakeTimers();
    vi.mocked(api.get).mockResolvedValue({ license_sync: disabled });
    mount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    fireEvent.change(screen.getByLabelText('License sync token'), { target: { value: 'fixture-draft-only' } });
    const count = vi.mocked(api.get).mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(vi.mocked(api.get).mock.calls.length).toBeGreaterThan(count);
    expect(screen.getByLabelText('License sync token')).toHaveProperty('value', 'fixture-draft-only');
    expect(screen.getByLabelText('Enable automatic license sync')).toHaveProperty('checked', false);
    expect(api.put).not.toHaveBeenCalled();
  });
});
describe('safe notices', () => {
  const future = '2026-09-20T00:00:05Z';
  const notice = { suppress_expiring: true, reason: 'auto_renew', fresh_until: future };
  function setup() {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-09-20T00:00:00Z'));
    license = {
      edition: 'business',
      status: 'expiring',
      restricted: false,
      features: [],
      expires_at: '2026-09-21T00:00:00Z',
      renewal_notice: notice,
    };
  }
  it('returns the shell warning at the deadline while offline, without refetch', () => {
    setup();
    mount(<LicenseBanner />);
    expect(screen.queryByRole('status')).toBeNull();
    act(() => vi.advanceTimersByTime(5000));
    expect(screen.getByRole('status')).toBeTruthy();
  });
  it('resets dismissal on changed status or notice', () => {
    setup();
    license.renewal_notice = undefined;
    const view = mount(<LicenseBanner />);
    fireEvent.click(screen.getByText('Dismiss'));
    expect(screen.queryByRole('status')).toBeNull();
    license = { ...license, status: 'grace' };
    view.rerender(
      <MemoryRouter>
        <LicenseBanner />
      </MemoryRouter>,
    );
    expect(screen.getByRole('status')).toBeTruthy();
  });
  it.each(['grace', 'expired', 'invalid'] as const)('never hides %s', (status) => {
    setup();
    license.status = status;
    mount(<LicenseBanner />);
    expect(screen.getByRole('status')).toBeTruthy();
  });
  it('keeps card expiry details and restores its warning at the deadline', async () => {
    setup();
    const doc: LicenseDocument = {
      license: {
        installed: true,
        edition: 'business',
        status: 'expiring',
        seats: 25,
        nodes: 1,
        features: [],
        checked_at: '',
        expires_at: license.expires_at,
      },
      renewal_notice: notice,
      seats_used: 1,
      nodes_live: 1,
      seat_window: '30d',
      instance_id: 'fixture',
      version: 'dev',
      portal_url: 'https://janusedge.com/portal',
      update: { enabled: false, offline: false, current: 'dev', update_available: false, unsupported: false },
    };
    vi.mocked(api.get).mockImplementation(async (path) => (path.endsWith('/sync') ? { license_sync: disabled } : doc));
    mount(<LicenseCard />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(screen.queryByText(/Renew at the portal before/)).toBeNull();
    expect(screen.getByText('Expires')).toBeTruthy();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(screen.getByText(/Renew at the portal before/)).toBeTruthy();
  });
});
