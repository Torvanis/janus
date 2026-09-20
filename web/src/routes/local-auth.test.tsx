/**
 * Local sign-in surfaces: first-run setup, password + TOTP steps, and the
 * Settings security card. The server contract these protect: the SPA never
 * sees a redirect — it gets JSON and navigates itself; wrong credentials are
 * a single message; the TOTP step needs the pending token from step one.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ApiError, api } from '../lib/api';
import { ToastProvider } from '../components/ui';
import { FirstRunSetup, LocalSignIn } from './LocalSignIn';
import { SecurityCard } from './SecurityCard';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});
const mocked = vi.mocked(api);

function wrap(node: React.ReactNode, initial = '/') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[initial]}>{node}</MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

const assign = vi.fn();
beforeEach(() => {
  vi.clearAllMocks();
  Object.defineProperty(window, 'location', { value: { assign }, writable: true });
});
afterEach(cleanup);

describe('LocalSignIn', () => {
  it('signs in with email + password and navigates to redirect_to', async () => {
    mocked.post.mockResolvedValue({ signed_in: true, redirect_to: '/models' });
    wrap(<LocalSignIn redirectTo="/models" />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText('Work email', { selector: 'input' }), 'alice@example.com');
    await user.type(screen.getByLabelText('Password', { selector: 'input', exact: true }), 'alice-password-12');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await waitFor(() =>
      expect(mocked.post).toHaveBeenCalledWith('/auth/local/login', {
        email: 'alice@example.com',
        password: 'alice-password-12',
        redirect_to: '/models',
      }),
    );
    await waitFor(() => expect(assign).toHaveBeenCalledWith('/models'));
  });

  it('shows the server message on bad credentials and offers SSO when given', async () => {
    mocked.post.mockRejectedValue(
      new ApiError(401, { message: 'email or password is incorrect', code: 'bad_credentials', type: 'auth_error' }),
    );
    wrap(
      <LocalSignIn
        redirectTo="/dashboard"
        providers={[
          { slug: '', name: 'Corporate SSO', url: '/auth/start?redirect_uri=%2Fdashboard' },
          { slug: 'okta', name: 'Okta', url: '/auth/start/okta?redirect_uri=%2Fdashboard' },
        ]}
      />,
    );
    const user = userEvent.setup();
    await user.type(screen.getByLabelText('Work email', { selector: 'input' }), 'alice@example.com');
    await user.type(screen.getByLabelText('Password', { selector: 'input', exact: true }), 'wrong-password-1');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await screen.findByText('email or password is incorrect');
    expect(screen.getByRole('link', { name: 'Continue with corporate SSO' }).getAttribute('href')).toBe(
      '/auth/start?redirect_uri=%2Fdashboard',
    );
    expect(assign).not.toHaveBeenCalled();
  });

  it('moves to the code step on totp_required and completes with the pending token', async () => {
    mocked.post
      .mockResolvedValueOnce({ signed_in: false, totp_required: true, pending: 'tok-1' })
      .mockResolvedValueOnce({ signed_in: true, redirect_to: '/dashboard' });
    wrap(<LocalSignIn redirectTo="/dashboard" />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText('Work email', { selector: 'input' }), 'alice@example.com');
    await user.type(screen.getByLabelText('Password', { selector: 'input', exact: true }), 'alice-password-12');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await user.type(await screen.findByLabelText('Verification code', { selector: 'input' }), '123 456');
    await user.click(screen.getByRole('button', { name: 'Verify' }));
    await waitFor(() =>
      expect(mocked.post).toHaveBeenLastCalledWith('/auth/local/totp', {
        pending: 'tok-1',
        code: '123 456',
        redirect_to: '/dashboard',
      }),
    );
    await waitFor(() => expect(assign).toHaveBeenCalledWith('/dashboard'));
  });
});

describe('FirstRunSetup', () => {
  it('requires a 12+ char password that matches, then creates the admin', async () => {
    mocked.post.mockResolvedValue({ signed_in: true, redirect_to: '/dashboard' });
    wrap(<FirstRunSetup />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText('Work email', { selector: 'input' }), 'owner@example.com');
    await user.type(screen.getByLabelText('Name', { selector: 'input' }), 'Owner');
    await user.type(screen.getByLabelText('Password', { selector: 'input', exact: true }), 'short');
    await user.type(screen.getByLabelText('Confirm password', { selector: 'input' }), 'shortx');
    expect(screen.getByText('Passwords do not match.')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Create administrator' })).toHaveProperty('disabled', true);
    await user.clear(screen.getByLabelText('Password', { selector: 'input', exact: true }));
    await user.clear(screen.getByLabelText('Confirm password', { selector: 'input' }));
    await user.type(screen.getByLabelText('Password', { selector: 'input', exact: true }), 'correct horse battery');
    await user.type(screen.getByLabelText('Confirm password', { selector: 'input' }), 'correct horse battery');
    await user.click(screen.getByRole('button', { name: 'Create administrator' }));
    await waitFor(() =>
      expect(mocked.post).toHaveBeenCalledWith('/auth/local/setup', {
        email: 'owner@example.com',
        name: 'Owner',
        password: 'correct horse battery',
      }),
    );
    await waitFor(() => expect(assign).toHaveBeenCalledWith('/dashboard'));
  });
});

describe('SecurityCard', () => {
  it('offers to add a password (break-glass) for an SSO user without one', async () => {
    mocked.get.mockResolvedValue({ has_password: false, totp_enabled: false, must_change: false, recovery_codes_left: 0 });
    wrap(<SecurityCard />);
    await screen.findByText('No password');
    expect(screen.getByRole('button', { name: 'Set a password' })).toBeTruthy();
    expect(screen.getByText(/identity provider is unavailable/)).toBeTruthy();
    expect(screen.queryByText('Two-factor authentication')).toBeNull();
  });

  it('forces a change without current password when ?password=change', async () => {
    mocked.get.mockResolvedValue({ has_password: true, totp_enabled: false, must_change: true, recovery_codes_left: 0 });
    mocked.put.mockResolvedValue({ changed: true });
    wrap(<SecurityCard />, '/settings?password=change');
    await screen.findByText(/temporary password/);
    expect(screen.queryByLabelText('Current password')).toBeNull();
    const user = userEvent.setup();
    await user.type(screen.getByLabelText('New password', { selector: 'input' }), 'my-own-password-1');
    await user.type(screen.getByLabelText('Confirm password', { selector: 'input' }), 'my-own-password-1');
    await user.click(screen.getByRole('button', { name: 'Change password' }));
    await waitFor(() =>
      expect(mocked.put).toHaveBeenCalledWith('/api/v1/me/local/password', {
        current_password: '',
        new_password: 'my-own-password-1',
      }),
    );
  });

  it('enrolls TOTP: start → code → recovery codes shown once', async () => {
    mocked.get.mockResolvedValue({ has_password: true, totp_enabled: false, must_change: false, recovery_codes_left: 0 });
    mocked.post
      .mockResolvedValueOnce({ secret: 'JBSWY3DPEHPK3PXP', otpauth_url: 'otpauth://totp/x' })
      .mockResolvedValueOnce({ enabled: true, recovery_codes: ['aaaa1111', 'bbbb2222'] });
    wrap(<SecurityCard />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Set up' }));
    await screen.findByText((_, el) => el?.tagName === 'CODE' && el.textContent === 'JBSWY3DPEHPK3PXP');
    expect(screen.getByRole('img', { name: /QR code/ }).getAttribute('src')).toContain('/api/v1/me/local/totp/qr.png');
    await user.type(screen.getByLabelText('Verification code', { selector: 'input' }), '654321');
    await user.click(screen.getByRole('button', { name: 'Enable' }));
    await waitFor(() => expect(mocked.post).toHaveBeenLastCalledWith('/api/v1/me/local/totp/confirm', { code: '654321' }));
    await screen.findByText('Save your recovery codes');
    expect(screen.getByText(/aaaa1111/)).toBeTruthy();
  });
});
