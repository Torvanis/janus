/**
 * A14 System page — running-database indicator (FR: "clear indication of the
 * running database in the admin page").
 *
 * The Database tile renders the sanitized `database.backend`/`location` fields
 * from GET /api/v1/admin/system/status. SQLite shows an inline single-node
 * caveat badge linking to /docs/admin/deployment; PostgreSQL does not. A raw
 * DSN or credential must never reach the DOM — even if a hostile or buggy
 * payload carries one in an unexpected field.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { DiscoveryInterval, SystemStatus } from '../../lib/types';
import { ToastProvider } from '../../components/ui';
import { SystemPage } from './System';
import { AdminSettingsPage } from './Settings';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

it('separates system status from editable configuration and retains a timeout draft across tabs', async () => {
  mocked.get.mockImplementation((path) => {
    if (path === '/api/v1/admin/system/status') return Promise.resolve(sqliteStatus);
    if (path === '/api/v1/admin/system/license') return Promise.resolve(communityLicense);
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
  renderPage('/admin/settings/general?scope=keep');
  const field = await screen.findByRole('spinbutton', { name: /Time to first byte/ });
  fireEvent.change(field, { target: { value: '123' } });
  expect(screen.queryByRole('link', { name: /Embedded SQLite/ })).toBeNull();
  expect(mocked.get).not.toHaveBeenCalledWith('/api/v1/admin/system/license');
  fireEvent.click(screen.getByRole('link', { name: 'System status' }));
  expect(await screen.findByRole('link', { name: /Embedded SQLite/ })).toBeTruthy();
  expect(screen.queryByRole('spinbutton', { name: /Time to first byte/ })).toBeNull();
  fireEvent.click(screen.getByRole('link', { name: 'License & updates' }));
  await waitFor(() => expect(mocked.get).toHaveBeenCalledWith('/api/v1/admin/system/license'));
  fireEvent.click(screen.getByRole('link', { name: 'General' }));
  expect((screen.getByRole('spinbutton', { name: /Time to first byte/ }) as HTMLInputElement).value).toBe('123');
  expect(mocked.patch).not.toHaveBeenCalled();
});

function statusFixture(database: SystemStatus['database']): SystemStatus {
  return {
    database,
    identity_provider: { ok: true, provider: 'Authentik', dev_auth: false },
    quota_ledger: { ok: true, mode: 'durable ledger with event-log reconciliation' },
    email: { ok: true, detail: 'SMTP is configured; email alerts will be delivered.' },
    telemetry: { prometheus: '/metrics' },
    upstreams: [],
    discovery: {
      last_run_at: '2025-01-02T10:00:00Z',
      interval_minutes: 15,
      interval: defaultDiscoveryInterval,
    },
    metering: {
      ok: true,
      window_days: 30,
      summary: { upstream_reported: 140, upstream_reported_cost: 0, byte_estimated: 0, unmetered: 0, gaps: [] },
    },
    retention: { usage_days: 365, audit_days: 730, purge_at_utc: '03:15' },
    build: { version: '1.2.0', sha: 'abc123def456abc123de', started_at: '2025-01-01T00:00:00Z', uptime_seconds: 7200 },
    // spend_emphasis ships off (usage emphasis, the repo default for flags);
    // the tests below assert its
    // toggle renders with translated copy and PATCHes through the confirm flow.
    feature_flags: { leaderboards_enabled: false, spend_emphasis: false },
    docs_feedback: [],
    adapters: ['openai_compatible', 'anthropic'],
    upstream_timeouts: defaultTimeouts,
  };
}

// Environment defaults in force, nothing overridden (a fresh instance).
const defaultTimeouts: SystemStatus['upstream_timeouts'] = {
  effective: { connect_seconds: 10, ttfb_seconds: 30, total_seconds: 600 },
  defaults: { connect_seconds: 10, ttfb_seconds: 30, total_seconds: 600 },
  overrides: { connect_seconds: 0, ttfb_seconds: 0, total_seconds: 0 },
  source: { connect: 'env', ttfb: 'env', total: 'env' },
  max_seconds: 86400,
};

// TTFB overridden by an administrator; connect/total still from the environment.
const overriddenTimeouts: SystemStatus['upstream_timeouts'] = {
  effective: { connect_seconds: 10, ttfb_seconds: 120, total_seconds: 600 },
  defaults: { connect_seconds: 10, ttfb_seconds: 30, total_seconds: 600 },
  overrides: { connect_seconds: 0, ttfb_seconds: 120, total_seconds: 0 },
  source: { connect: 'env', ttfb: 'admin', total: 'env' },
  updated_at: '2025-01-02T09:00:00Z',
  max_seconds: 86400,
};

const defaultDiscoveryInterval: DiscoveryInterval = {
  effective_minutes: 15,
  default_minutes: 15,
  override_minutes: 0,
  source: 'default',
  min_minutes: 1,
  max_minutes: 10080,
  last_run_at: '2025-01-02T10:00:00Z',
  next_run_at: '2025-01-02T10:15:00Z',
};

const overriddenDiscoveryInterval: DiscoveryInterval = {
  ...defaultDiscoveryInterval,
  effective_minutes: 2,
  override_minutes: 2,
  source: 'override',
  updated_at: '2025-01-02T09:00:00Z',
};

const sqliteStatus = statusFixture({
  ok: true,
  engine: 'sqlite',
  migrations: ['0001_init', '0002_quotas', '0003_ratelimits'],
  backend: 'sqlite',
  location: '/data/janus.db',
  defaulted: true,
});

const postgresStatus = statusFixture({
  ok: true,
  engine: 'postgres',
  migrations: ['0001_init', '0002_quotas'],
  backend: 'postgres',
  location: 'postgres.internal.example.com:5432/janus',
  defaulted: false,
});

function renderPage(workspace?: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[workspace ?? '/admin/system']}>
          {workspace ? (
            <Routes>
              <Route path="/admin/settings/:tab" element={<AdminSettingsPage />} />
            </Routes>
          ) : (
            <SystemPage />
          )}
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

/** Troubleshooting mode never enabled: the System page's card renders its off state. */
const troubleshootingOff = {
  active: false,
  session: null,
  stats: { count: 0, total_bytes: 0, oldest_at: '0001-01-01T00:00:00Z', newest_at: '0001-01-01T00:00:00Z' },
  backends: ['database'],
  encryption_available: true,
  max_body_bytes_ceiling: 33554432,
  max_session_hours: 168,
  retention_last_run_at: '0001-01-01T00:00:00Z',
  warnings: [],
};

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// Community license with no key: what a fresh install reports. Every System
// page render now also fetches this, so each mock answers it.
const communityLicense = {
  license: {
    installed: false,
    edition: 'community',
    status: 'valid',
    seats: 25,
    nodes: 1,
    features: [],
    checked_at: '2026-09-18T00:00:00Z',
  },
  seats_used: 1,
  nodes_live: 1,
  seat_window: '30d',
  instance_id: 'inst-test',
  version: 'dev',
  portal_url: 'https://janusedge.com/portal',
  update: { enabled: false, offline: false, current: 'dev', update_available: false, unsupported: false },
};

describe('A14 Database tile — sqlite backend', () => {
  beforeEach(() => {
    mocked.get.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/system/license') return Promise.resolve(communityLicense);
      if (path === '/api/v1/admin/system/status') return Promise.resolve(sqliteStatus);
      if (path === '/api/v1/admin/troubleshooting') return Promise.resolve(troubleshootingOff);
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
  });

  it('renders the backend label and sanitized file location', async () => {
    renderPage();
    expect(await screen.findByText('Embedded SQLite')).toBeTruthy();
    expect(screen.getByText('/data/janus.db')).toBeTruthy();
  });

  it('shows the single-node caveat badge linking to the deployment guide', async () => {
    renderPage();
    const badge = await screen.findByRole('link', {
      name: /single-node evaluation mode/i,
    });
    expect(badge.textContent).toContain('Embedded SQLite · single-node evaluation mode');
    expect(badge.getAttribute('href')).toBe('/docs/admin/deployment');
    expect(badge.className).toContain('badge-warning');
  });

  it('explains that the default database is in force when JANUS_DATABASE_URL is unset', async () => {
    renderPage();
    expect(await screen.findByText(/JANUS_DATABASE_URL is not set/)).toBeTruthy();
  });
});

describe('A14 Database tile — postgres backend', () => {
  beforeEach(() => {
    mocked.get.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/system/license') return Promise.resolve(communityLicense);
      if (path === '/api/v1/admin/system/status') return Promise.resolve(postgresStatus);
      if (path === '/api/v1/admin/troubleshooting') return Promise.resolve(troubleshootingOff);
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
  });

  it('renders the backend label and sanitized host:port/dbname location', async () => {
    renderPage();
    expect(await screen.findByText('PostgreSQL')).toBeTruthy();
    expect(screen.getByText('postgres.internal.example.com:5432/janus')).toBeTruthy();
  });

  it('does not show the sqlite caveat badge or the defaulted note', async () => {
    renderPage();
    await screen.findByText('PostgreSQL');
    expect(screen.queryByText(/single-node evaluation mode/)).toBeNull();
    expect(screen.queryByText(/JANUS_DATABASE_URL is not set/)).toBeNull();
  });
});

describe('A14 Database tile — no credentials in the DOM', () => {
  it('never renders a DSN, username, or password, even if the payload smuggles one', async () => {
    // Simulate a regressed/hostile API payload that carries a raw DSN in an
    // unexpected field. The tile renders only the sanitized backend/location
    // fields, so none of this may appear anywhere in the document.
    const smuggled = {
      ...postgresStatus,
      database: {
        ...postgresStatus.database,
        database_url: 'postgresql://dbadmin:sup3r-s3cret@postgres.internal.example.com:5432/janus?sslmode=require',
      },
    } as SystemStatus;
    mocked.get.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/system/license') return Promise.resolve(communityLicense);
      if (path === '/api/v1/admin/system/status') return Promise.resolve(smuggled);
      if (path === '/api/v1/admin/troubleshooting') return Promise.resolve(troubleshootingOff);
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });

    const { container } = renderPage();
    await screen.findByText('PostgreSQL');
    const html = container.innerHTML;
    expect(html).not.toContain('postgresql://');
    expect(html).not.toContain('postgres://');
    expect(html).not.toContain('sqlite://');
    expect(html).not.toContain('dbadmin');
    expect(html).not.toContain('sup3r-s3cret');
    expect(html).not.toContain('sslmode');
  });
});

describe('Spend-emphasis feature flag toggle', () => {
  beforeEach(() => {
    mocked.get.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/system/license') return Promise.resolve(communityLicense);
      if (path === '/api/v1/admin/system/status') return Promise.resolve(sqliteStatus);
      if (path === '/api/v1/admin/troubleshooting') return Promise.resolve(troubleshootingOff);
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
  });

  it('renders the toggle with its translated label and description, unchecked by default', async () => {
    renderPage();

    expect(await screen.findByText('Emphasise spend')).toBeTruthy();
    expect(screen.getByText(/Dashboard graphs open on the Spend metric instead of Tokens/)).toBeTruthy();

    const toggle = screen.getByRole('checkbox', { name: 'Emphasise spend' }) as HTMLInputElement;
    expect(toggle.checked).toBe(false);
  });

  it('confirms before patching the flag and shows the success toast', async () => {
    mocked.patch.mockResolvedValue({ features: { spend_emphasis: true } });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('checkbox', { name: 'Emphasise spend' }));

    // The existing confirm-before-apply dialog names the flag and its consequence.
    const dialog = await screen.findByRole('dialog');
    expect(dialog.textContent).toContain('Turn on Emphasise spend?');
    expect(dialog.textContent).toContain('will open showing cost instead of token usage');

    await user.click(within(dialog).getByRole('button', { name: 'Turn on' }));

    await waitFor(() => {
      expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/features', { spend_emphasis: true });
    });
    expect(await screen.findByText(/Feature flag updated/)).toBeTruthy();
  });

  it('cancelling the confirm dialog leaves the flag untouched', async () => {
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('checkbox', { name: 'Emphasise spend' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));

    expect(mocked.patch).not.toHaveBeenCalled();
  });
});

describe('Upstream timeouts panel', () => {
  function serveStatus(status: SystemStatus) {
    mocked.get.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/system/license') return Promise.resolve(communityLicense);
      if (path === '/api/v1/admin/system/status') return Promise.resolve(status);
      if (path === '/api/v1/admin/troubleshooting') return Promise.resolve(troubleshootingOff);
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
  }

  it('shows each hop with its effective value sourced from the environment, and no reset button', async () => {
    serveStatus(sqliteStatus);
    renderPage();

    const card = (await screen.findByRole('heading', { name: 'Upstream timeouts' })).closest('section') as HTMLElement;
    const connect = screen.getByRole('spinbutton', { name: 'Connect' }) as HTMLInputElement;
    const ttfb = screen.getByRole('spinbutton', { name: 'Time to first byte' }) as HTMLInputElement;
    const total = screen.getByRole('spinbutton', { name: 'Total' }) as HTMLInputElement;
    expect(connect.value).toBe('10');
    expect(ttfb.value).toBe('30');
    expect(total.value).toBe('600');
    expect(within(card).getAllByText('Environment')).toHaveLength(3);
    expect(screen.queryByText('Admin override')).toBeNull();
    expect(screen.queryByRole('button', { name: /Reset to environment defaults/ })).toBeNull();
    // Nothing changed yet, so there is nothing to save.
    expect((screen.getByRole('button', { name: 'Save timeouts' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('PATCHes only the changed hop when the admin raises the TTFB, then reports success', async () => {
    serveStatus(sqliteStatus);
    mocked.patch.mockResolvedValue(overriddenTimeouts);
    const user = userEvent.setup();
    renderPage();

    const ttfb = await screen.findByRole('spinbutton', { name: 'Time to first byte' });
    await user.clear(ttfb);
    await user.type(ttfb, '120');
    await user.click(screen.getByRole('button', { name: 'Save timeouts' }));

    await waitFor(() => {
      expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/system/upstream-timeouts', { ttfb_seconds: 120 });
    });
    expect(await screen.findByText(/Upstream timeouts updated/)).toBeTruthy();
  });

  it('sends 0 to drop an override when a hop is typed back to its environment default', async () => {
    serveStatus({ ...sqliteStatus, upstream_timeouts: overriddenTimeouts });
    mocked.patch.mockResolvedValue(defaultTimeouts);
    const user = userEvent.setup();
    renderPage();

    const ttfb = (await screen.findByRole('spinbutton', { name: 'Time to first byte' })) as HTMLInputElement;
    expect(ttfb.value).toBe('120');
    await user.clear(ttfb);
    await user.type(ttfb, '30');
    await user.click(screen.getByRole('button', { name: 'Save timeouts' }));

    await waitFor(() => {
      expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/system/upstream-timeouts', { ttfb_seconds: 0 });
    });
  });

  it('flags an admin override with its environment default and offers a reset that DELETEs the overrides', async () => {
    serveStatus({ ...sqliteStatus, upstream_timeouts: overriddenTimeouts });
    mocked.del.mockResolvedValue(defaultTimeouts);
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByText('Admin override')).toBeTruthy();
    expect(screen.getByText('Environment default: 30s')).toBeTruthy();
    expect(screen.getByText(/Last changed/)).toBeTruthy();

    await user.click(screen.getByRole('button', { name: 'Reset to environment defaults' }));
    await waitFor(() => {
      expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/system/upstream-timeouts');
    });
    expect(await screen.findByText(/reset to the environment defaults/)).toBeTruthy();
  });

  it('refuses a first-byte timeout longer than the total before any request is sent', async () => {
    serveStatus(sqliteStatus);
    const user = userEvent.setup();
    renderPage();

    const ttfb = await screen.findByRole('spinbutton', { name: 'Time to first byte' });
    await user.clear(ttfb);
    await user.type(ttfb, '900'); // total is 600

    expect(await screen.findByText('Must not exceed the total timeout.')).toBeTruthy();
    const save = screen.getByRole('button', { name: 'Save timeouts' }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    expect(mocked.patch).not.toHaveBeenCalled();

    // Raising the total alongside clears the error and enables saving.
    const total = screen.getByRole('spinbutton', { name: 'Total' });
    await user.clear(total);
    await user.type(total, '1800');
    await waitFor(() => expect(screen.queryByText('Must not exceed the total timeout.')).toBeNull());
    expect(save.disabled).toBe(false);
  });

  it('surfaces a server rejection as a danger toast', async () => {
    serveStatus(sqliteStatus);
    mocked.patch.mockRejectedValue(new Error('total_seconds must not exceed 86400 seconds (24 hours)'));
    const user = userEvent.setup();
    renderPage();

    const total = await screen.findByRole('spinbutton', { name: 'Total' });
    await user.clear(total);
    await user.type(total, '1200');
    await user.click(screen.getByRole('button', { name: 'Save timeouts' }));

    expect(await screen.findByText(/must not exceed 86400 seconds/)).toBeTruthy();
  });
});

describe('A14 System page — loading and error states', () => {
  it('renders the skeleton while the status query is in flight', () => {
    mocked.get.mockImplementation(() => new Promise(() => {})); // never settles
    const { container } = renderPage();
    expect(container.querySelector('.skeleton')).toBeTruthy();
    expect(screen.queryByText('Embedded SQLite')).toBeNull();
  });

  it('renders a human-readable error with a retry action when the status query fails', async () => {
    mocked.get.mockRejectedValue(new Error('The gateway could not be reached.'));
    renderPage();

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toContain('The gateway could not be reached.');

    // Retry refetches: flip the mock to succeed and the tile appears.
    mocked.get.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/system/license') return Promise.resolve(communityLicense);
      if (path === '/api/v1/admin/system/status') return Promise.resolve(sqliteStatus);
      if (path === '/api/v1/admin/troubleshooting') return Promise.resolve(troubleshootingOff);
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
    await userEvent.setup().click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByText('Embedded SQLite')).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
  });
});

describe('Model discovery interval panel', () => {
  function serveStatus(status: SystemStatus) {
    mocked.get.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/system/license') return Promise.resolve(communityLicense);
      if (path === '/api/v1/admin/system/status') return Promise.resolve(status);
      if (path === '/api/v1/admin/troubleshooting') return Promise.resolve(troubleshootingOff);
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
  }

  it('shows the effective interval sourced from the environment with no reset button', async () => {
    serveStatus(sqliteStatus);
    renderPage();

    expect(await screen.findByRole('heading', { name: 'Model discovery' })).toBeTruthy();
    const input = screen.getByRole('spinbutton', { name: 'Poll every' }) as HTMLInputElement;
    expect(input.value).toBe('15');
    expect(screen.getByText('In force: every 15 min')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Reset to environment default' })).toBeNull();
    expect((screen.getByRole('button', { name: 'Save interval' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('PATCHes the new interval in minutes and reports success', async () => {
    serveStatus(sqliteStatus);
    mocked.patch.mockResolvedValue(overriddenDiscoveryInterval);
    const user = userEvent.setup();
    renderPage();

    const input = await screen.findByRole('spinbutton', { name: 'Poll every' });
    await user.clear(input);
    await user.type(input, '2');
    await user.click(screen.getByRole('button', { name: 'Save interval' }));

    await waitFor(() => {
      expect(mocked.patch).toHaveBeenCalledWith('/api/v1/admin/system/discovery-interval', { minutes: 2 });
    });
    expect(await screen.findByText(/Discovery interval updated/)).toBeTruthy();
  });

  it('refuses an interval outside the allowed range before any request is sent', async () => {
    serveStatus(sqliteStatus);
    const user = userEvent.setup();
    renderPage();

    const input = await screen.findByRole('spinbutton', { name: 'Poll every' });
    await user.clear(input);
    await user.type(input, '0');
    expect(await screen.findByText(/whole number of minutes from 1 to 10080/)).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Save interval' }) as HTMLButtonElement).disabled).toBe(true);
    expect(mocked.patch).not.toHaveBeenCalled();
  });

  it('flags an admin override with its environment default and offers a reset that DELETEs it', async () => {
    serveStatus({
      ...sqliteStatus,
      discovery: { ...sqliteStatus.discovery, interval_minutes: 2, interval: overriddenDiscoveryInterval },
    });
    mocked.del.mockResolvedValue(defaultDiscoveryInterval);
    const user = userEvent.setup();
    renderPage();

    const card = (await screen.findByRole('heading', { name: 'Model discovery' })).closest('section') as HTMLElement;
    expect(within(card).getByText('Admin override')).toBeTruthy();
    expect(within(card).getByText('Environment default: 15 min')).toBeTruthy();

    await user.click(within(card).getByRole('button', { name: 'Reset to environment default' }));
    await waitFor(() => {
      expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/system/discovery-interval');
    });
    expect(await screen.findByText(/reset to the environment default/)).toBeTruthy();
  });
});

describe('Metering health tile', () => {
  function serveStatus(status: SystemStatus) {
    mocked.get.mockImplementation((path: string) => {
      if (path === '/api/v1/admin/system/license') return Promise.resolve(communityLicense);
      if (path === '/api/v1/admin/system/status') return Promise.resolve(status);
      if (path === '/api/v1/admin/troubleshooting') return Promise.resolve(troubleshootingOff);
      return Promise.reject(new Error(`unexpected GET ${path}`));
    });
  }

  it('reads OK when every request carried upstream usage', async () => {
    serveStatus(sqliteStatus);
    renderPage();
    const tile = await screen.findByTestId('metering-tile');
    expect(within(tile).getByText('OK')).toBeTruthy();
    expect(within(tile).getByText(/carried upstream usage or a text estimate/)).toBeTruthy();
  });

  it('flags unmetered media responses as a configuration gap, names the models and links to the requests', async () => {
    serveStatus({
      ...sqliteStatus,
      metering: {
        ok: false,
        window_days: 30,
        summary: {
          upstream_reported: 120,
          upstream_reported_cost: 3,
          byte_estimated: 2,
          unmetered: 17,
          gaps: [
            {
              model_name: 'tts-1',
              upstream_id: 'up-1',
              upstream_name: 'openai',
              modality: 'tts',
              requests: 12,
              last_seen_at: '2025-01-02T10:00:00Z',
            },
            {
              model_name: 'grok-2-image',
              upstream_id: 'up-2',
              upstream_name: 'xai',
              modality: 'image',
              requests: 5,
              last_seen_at: '2025-01-02T10:00:00Z',
            },
          ],
        },
      },
    });
    renderPage();
    const tile = await screen.findByTestId('metering-tile');
    expect(within(tile).getByText('Attention')).toBeTruthy();
    expect(within(tile).getByText(/17 media response\(s\) in the last 30 days/)).toBeTruthy();
    expect(within(tile).getByText('tts-1')).toBeTruthy();
    expect(within(tile).getByText('grok-2-image')).toBeTruthy();
    expect(
      within(tile)
        .getByRole('link', { name: /View unmetered requests/ })
        .getAttribute('href'),
    ).toBe('/admin/requests?accounting=unmetered_modality');
  });
});
