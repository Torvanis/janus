/**
 * License surfaces: the Admin → System card (state, seats, install/remove)
 * and the shell banner (only in states that need action). The ruling these
 * tests protect: expiry never disrupts work — the copy talks about what is
 * paused, and an expired/invalid banner cannot be dismissed while a soft
 * (expiring/grace) one can.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { LicenseDocument, LicenseSummary } from '../../lib/types';
import { ToastProvider } from '../../components/ui';
import { LicenseCard } from './LicenseCard';
import { LicenseBanner } from '../../app/LicenseBanner';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

let sessionMe: { role: string; license?: LicenseSummary } | null = null;
vi.mock('../../app/session', () => ({ useSession: () => ({ me: sessionMe }) }));

const mocked = vi.mocked(api);

function doc(overrides: Partial<LicenseDocument['license']> = {}, seatsUsed = 3): LicenseDocument {
  return {
    license_sync: { enabled: false, has_token: false, mode: 'manual', health: 'disabled' },
    license: {
      installed: false,
      edition: 'community',
      status: 'valid',
      seats: 25,
      nodes: 1,
      features: [],
      checked_at: '2026-09-18T00:00:00Z',
      ...overrides,
    },
    seats_used: seatsUsed,
    nodes_live: 1,
    seat_window: '30d',
    instance_id: 'inst-abc',
    version: 'dev',
    portal_url: 'https://janusedge.com/portal',
    update: { enabled: false, offline: false, current: 'dev', update_available: false, unsupported: false },
  };
}

const businessClaims = {
  key_id: '2026-09',
  license_id: 'JNS-BUS-1234-ABCD',
  org: 'Acme',
  issued_to: 'ops@acme.test',
  edition: 'business',
  seats: 100,
  nodes: 3,
  site: 'HQ',
  features: ['scim', 'ha'],
  issued: '2026-09-01T00:00:00Z',
  exp: '2027-09-01T00:00:00Z',
  grace_days: 30,
  term: 'subscription' as const,
};

function renderCard() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter>
          <LicenseCard />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

describe('LicenseCard', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });
  afterEach(cleanup);

  it('shows Community with seat usage and the install form when no key is installed', async () => {
    mocked.get.mockResolvedValue(doc());
    renderCard();
    await screen.findByText('Community');
    expect(screen.getByText('3 of 25 active in the last 30 days')).toBeTruthy();
    expect(screen.getByText('inst-abc')).toBeTruthy();
    expect(screen.getByText('No key installed')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Remove key' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Install key' })).toHaveProperty('disabled', true);
  });

  it('shows live nodes and warns, without blocking, when replicas exceed the key', async () => {
    mocked.get.mockResolvedValue({ ...doc(), nodes_live: 3 });
    renderCard();
    await screen.findByText('3 live of 1');
    expect(screen.getByText(/3 gateway replicas are running but this key covers 1/)).toBeTruthy();
    expect(screen.getByText(/Nothing is blocked/)).toBeTruthy();
  });

  it('renders Business claims, features and the expiry, and installs a pasted key', async () => {
    mocked.get.mockResolvedValue(
      doc(
        {
          installed: true,
          source: 'database',
          edition: 'business',
          status: 'valid',
          seats: 100,
          nodes: 3,
          features: ['scim', 'ha'],
          claims: businessClaims,
          expires_at: businessClaims.exp,
        },
        42,
      ),
    );
    mocked.put.mockResolvedValue({ license: doc().license });
    renderCard();
    await screen.findByText('Business');
    expect(screen.getByText('42 of 100 active in the last 30 days')).toBeTruthy();
    expect(screen.getByText('JNS-BUS-1234-ABCD')).toBeTruthy();
    expect(screen.getByText('SCIM provisioning')).toBeTruthy();
    expect(screen.getByText('High availability')).toBeTruthy();
    expect(screen.getByText('HQ')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Remove key' })).toBeTruthy();

    const user = userEvent.setup();
    await user.type(screen.getByPlaceholderText('JANUS-LICENSE-1....'), 'JANUS-LICENSE-1.abc.def');
    await user.click(screen.getByRole('button', { name: 'Install key' }));
    await waitFor(() =>
      expect(mocked.put).toHaveBeenCalledWith('/api/v1/admin/system/license', { key: 'JANUS-LICENSE-1.abc.def' }),
    );
  });

  it('explains what is paused when the key is expired, and the file-wins rule', async () => {
    mocked.get.mockResolvedValue(
      doc({
        installed: true,
        source: 'file',
        edition: 'business',
        status: 'expired',
        seats: 100,
        nodes: 3,
        features: ['scim'],
        claims: businessClaims,
      }),
    );
    renderCard();
    await screen.findByText('Expired');
    expect(screen.getByText(/Creating new tokens, models, quotas, rules, teams, schedules and captures is paused/)).toBeTruthy();
    expect(screen.getByText(/Everything already configured keeps running/)).toBeTruthy();
    // File-sourced keys cannot be removed from the UI.
    expect(screen.queryByRole('button', { name: 'Remove key' })).toBeNull();
  });

  it('surfaces the verification error for an invalid key', async () => {
    mocked.get.mockResolvedValue(
      doc({ installed: true, source: 'database', status: 'invalid', error: 'license: bad signature' }),
    );
    renderCard();
    await screen.findByText('Invalid key');
    expect(screen.getByText('license: bad signature')).toBeTruthy();
  });

  it('removes the key after confirmation', async () => {
    mocked.get.mockResolvedValue(
      doc({
        installed: true,
        source: 'database',
        edition: 'business',
        status: 'valid',
        seats: 100,
        nodes: 3,
        features: [],
        claims: businessClaims,
      }),
    );
    mocked.del.mockResolvedValue({ license: doc().license });
    renderCard();
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Remove key' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/returns to Community limits/)).toBeTruthy();
    await user.click(within(dialog).getByRole('button', { name: 'Remove key' }));
    await waitFor(() => expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/system/license'));
  });
});

function renderBanner() {
  return render(
    <MemoryRouter>
      <LicenseBanner />
    </MemoryRouter>,
  );
}

describe('LicenseBanner', () => {
  it.each(['revoked', 'subscription_attention', 'stale', 'network'])('shows %s attention even for a valid signed key', (reason) => {
    sessionMe = { role: 'admin', license: { edition: 'business', status: 'valid', restricted: false, features: [], renewal_notice: { suppress_expiring: false, reason } } };
    renderBanner();
    expect(screen.getByRole('status').textContent).toMatch(/administrator/);
    expect(screen.getByRole('link', { name: 'Manage license' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Dismiss' })).toBeNull();
  });

  afterEach(() => {
    cleanup();
    sessionMe = null;
  });

  it('is silent for Community and for a valid key', () => {
    sessionMe = { role: 'admin', license: { edition: 'community', status: 'valid', restricted: false, features: [] } };
    const { container } = renderBanner();
    expect(container.querySelector('.license-banner')).toBeNull();
    sessionMe = { role: 'admin', license: { edition: 'business', status: 'valid', restricted: false, features: [] } };
    cleanup();
    expect(renderBanner().container.querySelector('.license-banner')).toBeNull();
  });

  it('warns on expiring, links admins to System, and can be dismissed', async () => {
    sessionMe = {
      role: 'admin',
      license: { edition: 'business', status: 'expiring', restricted: false, features: [], expires_at: '2026-10-01T00:00:00Z' },
    };
    renderBanner();
    expect(screen.getByRole('status').className).toContain('banner-warning');
    expect(screen.getByRole('link', { name: 'Manage license' }).getAttribute('href')).toBe('/admin/settings/license');
    await userEvent.setup().click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByRole('status')).toBeNull();
  });

  it('cannot be dismissed when expired, and hides the admin link from users', () => {
    sessionMe = { role: 'user', license: { edition: 'business', status: 'expired', restricted: true, features: [] } };
    renderBanner();
    expect(screen.getByRole('status').className).toContain('banner-danger');
    expect(screen.getByText(/creating new tokens, models, quotas, rules, teams and schedules is paused/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Dismiss' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Manage license' })).toBeNull();
  });
});

describe('LicenseCard update check', () => {
  beforeEach(() => vi.clearAllMocks());
  afterEach(cleanup);

  it('explains how to enable the check when it is off, and the air-gap case', async () => {
    mocked.get.mockResolvedValue(doc());
    renderCard();
    await screen.findByText(/Version check is off/);
    cleanup();
    const d = doc();
    d.update = { ...d.update, offline: true };
    mocked.get.mockResolvedValue(d);
    renderCard();
    await screen.findByText(/Air-gapped/);
  });

  it('shows an available update with release notes, and an unsupported version', async () => {
    const d = doc();
    d.update = {
      enabled: true,
      offline: false,
      checked_at: '2026-09-18T06:00:00Z',
      current: '1.3.0',
      latest: '1.4.0',
      notes_url: 'https://janusedge.com/releases/1.4.0',
      update_available: true,
      unsupported: false,
    };
    mocked.get.mockResolvedValue(d);
    renderCard();
    await screen.findByText('1.4.0 is available (you are on 1.3.0).');
    expect(screen.getByRole('link', { name: 'Release notes' }).getAttribute('href')).toBe('https://janusedge.com/releases/1.4.0');
    cleanup();
    d.update = { ...d.update, unsupported: true, min_supported: '1.3.5' };
    mocked.get.mockResolvedValue(d);
    renderCard();
    await screen.findByText(/1.3.0 is below the minimum supported release 1.3.5/);
  });

  it('reports a failed check without alarm', async () => {
    const d = doc();
    d.update = {
      enabled: true,
      offline: false,
      checked_at: '2026-09-18T06:00:00Z',
      current: '1.3.0',
      error: 'update check: HTTP 503',
      update_available: false,
      unsupported: false,
    };
    mocked.get.mockResolvedValue(d);
    renderCard();
    await screen.findByText('Last check failed: update check: HTTP 503');
  });
});
