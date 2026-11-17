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
import { FirstRunSetup, LocalSignIn, qualifiedName, windowsDomainFor } from './LocalSignIn';
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

describe('LocalSignIn with a directory', () => {
  it('accepts a short name, says which domain it uses, and sends it as typed', async () => {
    mocked.post.mockResolvedValue({ signed_in: true, redirect_to: '/dashboard' });
    wrap(<LocalSignIn redirectTo="/dashboard" directory defaultDomain="corp.example.com" />);
    const user = userEvent.setup();
    const name = screen.getByLabelText('Email or user name', { selector: 'input' }) as HTMLInputElement;
    // A user name must pass the browser's own validation (type=email refuses it).
    expect(name.type).toBe('text');
    expect(screen.getByText('Use jdoe, jdoe@corp.example.com or CORP\\jdoe.')).toBeTruthy();
    await user.type(name, 'jdoe');
    expect(name.checkValidity()).toBe(true);
    expect(screen.getByText('Signs in as jdoe@corp.example.com')).toBeTruthy();
    expect(name.getAttribute('aria-describedby')).toBe('signin-name-hint');
    await user.type(screen.getByLabelText('Password', { selector: 'input', exact: true }), 'pw');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    // The server applies the domain; the form sends what was typed.
    await waitFor(() =>
      expect(mocked.post).toHaveBeenCalledWith('/auth/local/login', { email: 'jdoe', password: 'pw', redirect_to: '/dashboard' }),
    );
  });

  it('leaves a name that already has a domain alone', async () => {
    wrap(<LocalSignIn redirectTo="/dashboard" directory defaultDomain="corp.example.com" />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText('Email or user name', { selector: 'input' }), 'jdoe@other.example.org');
    expect(screen.queryByText(/Signs in as/)).toBeNull();
    expect(screen.getByText('Use jdoe, jdoe@corp.example.com or CORP\\jdoe.')).toBeTruthy();
  });

  it('takes user names without a hint when no default domain is set', () => {
    wrap(<LocalSignIn redirectTo="/dashboard" directory />);
    const name = screen.getByLabelText('Email or user name', { selector: 'input' }) as HTMLInputElement;
    expect(name.type).toBe('text');
    expect(screen.queryByText(/^Use jdoe/)).toBeNull();
  });

  it('keeps the email field when no directory is on', () => {
    wrap(<LocalSignIn redirectTo="/dashboard" defaultDomain="corp.example.com" />);
    const name = screen.getByLabelText('Work email', { selector: 'input' }) as HTMLInputElement;
    expect(name.type).toBe('email');
    expect(screen.queryByText(/^Use jdoe/)).toBeNull();
  });

  it('qualifies the three name forms like the server', () => {
    expect(qualifiedName('ada', 'corp.example.com')).toBe('ada@corp.example.com');
    expect(qualifiedName(' ada ', '@corp.example.com')).toBe('ada@corp.example.com');
    expect(qualifiedName('ada@x.org', 'corp.example.com')).toBeNull();
    // DOMAIN\\name: derived (CORP) or set, any case.
    expect(qualifiedName('CORP\\ada', 'corp.example.com')).toBe('ada@corp.example.com');
    expect(qualifiedName('corp\\ada', 'Corp.Example.com')).toBe('ada@corp.example.com');
    expect(qualifiedName('acme\\ada', 'corp.example.com', 'ACME')).toBe('ada@corp.example.com');
    expect(qualifiedName('CORP\\ada', 'corp.example.com', 'ACME')).toBeNull();
    expect(qualifiedName('OTHER\\ada', 'corp.example.com')).toBeNull();
    expect(qualifiedName('CORP\\', 'corp.example.com')).toBeNull();
    expect(qualifiedName('CORP\\ada@x.org', 'corp.example.com')).toBeNull();
    expect(qualifiedName('CORP\\ada', '')).toBeNull();
    expect(qualifiedName('ada', '')).toBeNull();
    expect(qualifiedName('', 'corp.example.com')).toBeNull();
  });

  it('derives the Windows domain like the server', () => {
    expect(windowsDomainFor('corp.example.com')).toBe('CORP');
    expect(windowsDomainFor('averyveryverylongname.example.com')).toBe('AVERYVERYVERYLO');
    expect(windowsDomainFor('corp.example.com', 'acme')).toBe('ACME');
    expect(windowsDomainFor('')).toBe('');
  });

  it('shows the full name a Windows-style name signs in as', async () => {
    wrap(<LocalSignIn redirectTo="/dashboard" directory defaultDomain="corp.example.com" windowsDomain="ACME" />);
    const user = userEvent.setup();
    expect(screen.getByText('Use jdoe, jdoe@corp.example.com or ACME\\jdoe.')).toBeTruthy();
    const name = screen.getByLabelText('Email or user name', { selector: 'input' });
    await user.type(name, 'acme\\jdoe');
    expect(screen.getByText('Signs in as jdoe@corp.example.com')).toBeTruthy();
    // Another domain: no promise, just the accepted forms again.
    await user.clear(name);
    await user.type(name, 'OTHER\\jdoe');
    expect(screen.queryByText(/Signs in as/)).toBeNull();
    expect(screen.getByText('Use jdoe, jdoe@corp.example.com or ACME\\jdoe.')).toBeTruthy();
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
  it('sends an SSO user without a password to an administrator instead of offering to set one', async () => {
    mocked.get.mockResolvedValue({ has_password: false, totp_enabled: false, must_change: false, recovery_codes_left: 0 });
    wrap(<SecurityCard />);
    await screen.findByText('No password');
    expect(screen.queryByRole('button', { name: /password/i })).toBeNull();
    expect(screen.getByText(/An administrator can add a password from People/)).toBeTruthy();
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

  it('FIPS edition: a SHA-256 enrollment says which apps work', async () => {
    mocked.get.mockResolvedValue({ has_password: true, totp_enabled: false, must_change: false, recovery_codes_left: 0 });
    mocked.post.mockResolvedValueOnce({
      secret: 'JBSWY3DPEHPK3PXP',
      otpauth_url: 'otpauth://totp/x?algorithm=SHA256',
      algorithm: 'SHA256',
    });
    wrap(<SecurityCard />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Set up' }));
    expect(await screen.findByText(/codes use HMAC-SHA-256/)).toBeTruthy();
  });

  it('FIPS edition: a legacy SHA-1 enrollment asks the user to set two-factor up again', async () => {
    mocked.get.mockResolvedValue({
      has_password: true,
      totp_enabled: true,
      must_change: false,
      recovery_codes_left: 9,
      totp_algorithm: 'SHA1',
      totp_reenroll: true,
    });
    wrap(<SecurityCard />);
    expect(await screen.findByText(/does not accept/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Set up' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Disable' })).toBeNull();
  });
});
