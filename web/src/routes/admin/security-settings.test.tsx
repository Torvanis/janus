import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router-dom';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import { SecuritySettingsPage, type SecurityDoc, type SecurityPolicy } from './SecuritySettings';
import { ConsentBanner } from '../Login';
import { describePolicy, passwordProblem } from '../../lib/passwordPolicy';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

const defaults: SecurityPolicy = {
  sessions: { idle_minutes_user: 240, idle_minutes_admin: 240, absolute_minutes: 1440, max_per_user: 0 },
  banner: { enabled: false, title: 'Notice', text: '', button: 'I acknowledge' },
  lockout: { max_failures: 10, window_minutes: 15, lock_minutes: 15 },
  passwords: {
    min_length: 12,
    require_upper: false,
    require_lower: false,
    require_digit: false,
    require_special: false,
    min_changed_chars: 0,
    history: 0,
    min_age_hours: 0,
    max_age_days: 0,
  },
  syslog: {
    enabled: false,
    protocol: 'tls',
    address: '',
    format: 'rfc5424',
    facility: 10,
    app_name: 'janus',
    server_name: '',
    client_cert_file: '',
    client_key_file: '',
  },
  smart_card: {
    enabled: false,
    source: 'direct',
    header: 'X-Forwarded-Client-Cert',
    header_format: 'xfcc',
    match: ['upn', 'email', 'edipi'],
    provision: false,
    require_for_admins: false,
  },
};
const stig: SecurityPolicy = {
  ...defaults,
  sessions: { idle_minutes_user: 15, idle_minutes_admin: 10, absolute_minutes: 1440, max_per_user: 3 },
  banner: {
    enabled: true,
    title: 'DoD Notice',
    text: 'You are accessing a U.S. Government (USG) Information System',
    button: 'OK',
  },
  lockout: { max_failures: 3, window_minutes: 15, lock_minutes: 0 },
  passwords: {
    min_length: 15,
    require_upper: true,
    require_lower: true,
    require_digit: true,
    require_special: true,
    min_changed_chars: 8,
    history: 5,
    min_age_hours: 24,
    max_age_days: 180,
  },
};
const doc = (over: Partial<SecurityDoc> = {}): SecurityDoc => ({
  policy: defaults,
  effective: defaults,
  defaults,
  stig_values: stig,
  stig_profile: false,
  stig_gaps: ['APSC-DV-000070: idle sign-out is 240 minutes (STIG: 15 or less)'],
  dod_banner: stig.banner.text,
  limits: { max_absolute_minutes: 1440, max_idle_minutes: 240 },
  smart_card_direct_available: false,
  syslog_status: { enabled: false, sent: 0, dropped: 0, failed: 0, queued: 0 },
  ...over,
});

function wrap(node: React.ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ToastProvider>{node}</ToastProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  mocked.get.mockImplementation((path: string) => Promise.resolve(path.includes('lockouts') ? { lockouts: [] } : doc()));
});
afterEach(cleanup);

describe('Security settings', () => {
  it('shows the STIG gap and saves the STIG values only after confirmation and Save', async () => {
    mocked.put.mockResolvedValue(doc({ policy: stig, effective: stig, stig_gaps: [] }));
    wrap(<SecuritySettingsPage />);
    expect(await screen.findByText('1 below DoD STIG')).toBeTruthy();
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Apply baseline' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: /Apply/ }));
    expect(mocked.put).not.toHaveBeenCalled();
    expect((screen.getByLabelText(/Idle sign-out, people/) as HTMLInputElement).value).toBe('15');
    await user.click(screen.getByRole('button', { name: 'Save security settings' }));
    await waitFor(() =>
      expect(mocked.put).toHaveBeenCalledWith(
        '/api/v1/admin/security/policy',
        expect.objectContaining({ lockout: stig.lockout }),
      ),
    );
  });

  it('offers industry baselines, each with its own gaps and values', async () => {
    const pci = { ...stig, sessions: { ...stig.sessions, idle_minutes_user: 15, idle_minutes_admin: 15, max_per_user: 0 } };
    mocked.get.mockImplementation((path: string) =>
      Promise.resolve(
        path.includes('lockouts')
          ? { lockouts: [] }
          : doc({
              baselines: [
                {
                  id: 'nist-800-63b',
                  name: 'NIST SP 800-63B (modern commercial)',
                  summary: 'NIST summary.',
                  values: stig,
                  gaps: [],
                },
                {
                  id: 'pci-dss-4',
                  name: 'PCI DSS 4.0 (payment card data)',
                  summary: 'PCI summary.',
                  values: pci,
                  gaps: ['a', 'b'],
                },
              ],
            }),
      ),
    );
    wrap(<SecuritySettingsPage />);
    expect(await screen.findByText('Meets NIST SP 800-63B')).toBeTruthy();
    const user = userEvent.setup();
    await user.selectOptions(screen.getByLabelText('Start from'), 'pci-dss-4');
    expect(screen.getByText('2 below PCI DSS 4.0')).toBeTruthy();
    expect(screen.getByText('PCI summary.')).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Apply baseline' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/Apply PCI DSS 4.0/)).toBeTruthy();
    await user.click(within(dialog).getByRole('button', { name: /Apply/ }));
    expect((screen.getByLabelText(/Idle sign-out, administrators/) as HTMLInputElement).value).toBe('15');
    expect(mocked.put).not.toHaveBeenCalled();
  });

  it('under the STIG profile says nothing can be set weaker and hides the reset', async () => {
    mocked.get.mockImplementation((path: string) =>
      Promise.resolve(path.includes('lockouts') ? { lockouts: [] } : doc({ stig_profile: true, policy: stig, effective: stig })),
    );
    wrap(<SecuritySettingsPage />);
    expect(await screen.findByText('STIG profile on')).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Reset to defaults' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('lists locked names and unlocks one', async () => {
    mocked.get.mockImplementation((path: string) =>
      Promise.resolve(
        path.includes('lockouts')
          ? { lockouts: [{ login: 'bob@example.com', failed: 3, locked_at: '2026-11-05T00:00:00Z', until_unlocked: true }] }
          : doc(),
      ),
    );
    mocked.post.mockResolvedValue({ unlocked: true });
    wrap(<SecuritySettingsPage />);
    expect(await screen.findByText('bob@example.com')).toBeTruthy();
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: /Unlock/ }));
    await waitFor(() =>
      expect(mocked.post).toHaveBeenCalledWith('/api/v1/admin/security/lockouts/unlock', { login: 'bob@example.com' }),
    );
  });
});

describe('Sign-in notice', () => {
  it('must be acknowledged before anything else', async () => {
    const done = vi.fn();
    mocked.post.mockResolvedValue({});
    wrap(
      <ConsentBanner
        banner={{ enabled: true, title: 'DoD Notice', text: 'First.\n\nSecond.', button: 'I agree' }}
        onAcknowledged={done}
      />,
    );
    expect(screen.getByRole('heading', { name: 'DoD Notice' })).toBeTruthy();
    expect(screen.getByText('Second.')).toBeTruthy();
    await userEvent.setup().click(screen.getByRole('button', { name: 'I agree' }));
    await waitFor(() => expect(done).toHaveBeenCalled());
    expect(mocked.post).toHaveBeenCalledWith('/auth/banner/ack', {});
  });
});

describe('password rules', () => {
  it('follow the policy', () => {
    expect(passwordProblem('short')).toMatch(/12 characters/);
    expect(passwordProblem('a long enough sentence')).toBeNull();
    expect(passwordProblem('all-lowercase-long-1', stig.passwords)).toMatch(/upper-case/);
    expect(passwordProblem('Correct-Horse-Battery-9', stig.passwords)).toBeNull();
    expect(describePolicy(stig.passwords)).toBe(
      'At least 15 characters, with an upper-case letter, a lower-case letter, a digit and a special character. Not one of your last 5.',
    );
  });
});
