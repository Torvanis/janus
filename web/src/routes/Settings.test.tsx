import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ToastProvider } from '../components/ui';
import { SettingsPage } from './Settings';
import { api } from '../lib/api';
const session = vi.hoisted(() => ({ role: 'user' }));
vi.mock('../app/session', () => ({
  useSession: () => ({
    me: { role: session.role, email: 'viewer@example.com', groups: [], timezone: '', locale: '' },
    config: {},
    refresh: vi.fn(),
  }),
}));
vi.mock('../lib/api', () => ({ api: { get: vi.fn(), patch: vi.fn(), post: vi.fn() } }));
vi.mock('./SecurityCard', () => ({ SecurityCard: () => <input aria-label="Security draft" defaultValue="" /> }));
afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  session.role = 'user';
  window.matchMedia = vi.fn().mockReturnValue({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() });
});
function State() {
  const location = useLocation();
  return <output data-testid="url">{location.pathname + location.search}</output>;
}
function mount(path = '/settings') {
  render(
    <QueryClientProvider client={new QueryClient()}>
      <ToastProvider>
        <MemoryRouter initialEntries={[path]}>
          <State />
          <SettingsPage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}
it('keeps personal scope separate and does not load admin data', () => {
  mount();
  expect(screen.getByRole('heading', { name: 'Personal settings' })).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Account' }).getAttribute('aria-current')).toBe('page');
  expect(screen.queryByRole('link', { name: 'Gateway settings' })).toBeNull();
  expect(api.get).not.toHaveBeenCalled();
});
it('keeps regional and security drafts through navigation without writing until Save', async () => {
  mount('/settings?tab=regional&from=profile');
  const field = screen.getByRole('combobox', { name: /Time zone/i });
  fireEvent.change(field, { target: { value: 'UTC' } });
  fireEvent.click(screen.getByRole('link', { name: 'Security' }));
  fireEvent.change(screen.getByLabelText('Security draft'), { target: { value: 'unsaved' } });
  fireEvent.click(screen.getByRole('link', { name: 'Appearance' }));
  expect(api.patch).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole('link', { name: 'Regional' }));
  expect((screen.getByRole('combobox', { name: /Time zone/i }) as HTMLSelectElement).value).toBe('UTC');
  expect(screen.getByTestId('url').textContent).toContain('from=profile');
  fireEvent.click(screen.getByRole('button', { name: /Save preferences/ }));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('/api/v1/me/preferences', { timezone: 'UTC', locale: '' }));
  fireEvent.click(screen.getByRole('link', { name: 'Security' }));
  expect((screen.getByLabelText('Security draft') as HTMLInputElement).value).toBe('unsaved');
});
it('offers an explicit gateway-settings bridge only to administrators', () => {
  session.role = 'admin';
  mount();
  expect(screen.getByRole('link', { name: 'Gateway settings' }).getAttribute('href')).toBe('/admin/settings/general');
  expect(api.get).not.toHaveBeenCalled();
});
