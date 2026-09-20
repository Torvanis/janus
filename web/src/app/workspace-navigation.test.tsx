import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useLocation, useNavigate } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { LegacyWorkspaceRedirect, legacyWorkspaceTarget } from './LegacyWorkspaceRedirect';
import { AdminSettingsPage } from '../routes/admin/Settings';
import { Breadcrumb } from './Breadcrumb';
import { api } from '../lib/api';
import { TeamUsageRedirect } from '../routes/TeamUsageRedirect';

vi.mock('../lib/api', () => ({ api: { get: vi.fn() } }));
vi.mock('../routes/admin/System', () => ({
  SystemPage: ({ section }: { section: string }) => (
    <div>
      <h2>{section}</h2>
      <input aria-label="System draft" defaultValue="" />
    </div>
  ),
}));
vi.mock('../routes/admin/Provisioning', () => ({
  ProvisioningPage: () => <input aria-label="Directory draft" defaultValue="" />,
}));
vi.mock('../routes/admin/LicenseCard', () => ({ LicenseCard: () => <input aria-label="License draft" defaultValue="" /> }));
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function State() {
  const location = useLocation();
  const navigate = useNavigate();
  return (
    <>
      <output data-testid="url">{location.pathname + location.search + location.hash}</output>
      <button onClick={() => navigate(-1)}>Back</button>
      <button onClick={() => navigate(1)}>Forward</button>
    </>
  );
}
function mount(url: string) {
  render(
    <MemoryRouter initialEntries={[url]}>
      <State />
      <Routes>
        <Route path="/admin/settings/:tab" element={<AdminSettingsPage />} />
        <Route path="/admin/settings" element={<LegacyWorkspaceRedirect />} />
        <Route path="/admin/system" element={<LegacyWorkspaceRedirect />} />
        <Route path="/teams/:teamId/usage" element={<TeamUsageRedirect />} />
        <Route path="/teams/:id" element={<h1>Team workspace</h1>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe('workspace navigation', () => {
  it('keeps legacy team usage range, metric and fragment in the shared team workspace', () => {
    mount('/teams/t1/usage?range=month&metric=tokens#daily');
    expect(screen.getByTestId('url').textContent).toBe('/teams/t1?range=month&metric=tokens&view=manage&section=usage#daily');
  });
  it.each([
    ['/admin/system', '', 'status'],
    ['/admin/system', '#license', 'license'],
    ['/admin/system', '#license-heading', 'license'],
    ['/admin/system', '#upstream-timeouts-heading', 'general'],
    ['/admin/system', '#discovery-interval-heading', 'general'],
    ['/admin/system', '#troubleshooting-title', 'troubleshooting'],
    ['/admin/system', '#updates', 'license'],
    ['/admin/system', '#upstream-timeouts', 'general'],
    ['/admin/system', '#discovery-interval', 'general'],
    ['/admin/system', '#feature-flags', 'general'],
    ['/admin/system', '#capture', 'troubleshooting'],
    ['/admin/system', '#docs-feedback', 'troubleshooting'],
    ['/admin/system', '#unknown-anchor', 'status'],
    ['/admin/provisioning', '#setup-title', 'sign-in'],
    ['/admin/settings', '', 'general'],
  ])('migrates %s %s without losing opaque state', (path, hash, tab) => {
    expect(legacyWorkspaceTarget(path!, '?q=a%2Bb&filter=one&filter=two', hash!)).toEqual({
      pathname: `/admin/settings/${tab}`,
      search: '?q=a%2Bb&filter=one&filter=two',
      hash,
    });
  });
  it('moves import and legacy People teams to Administration People, not personal Team', () => {
    expect(legacyWorkspaceTarget('/admin/team-import', '?q=eng', '#preview')).toEqual({
      pathname: '/admin/teams/import',
      search: '?q=eng',
      hash: '#preview',
    });
    expect(legacyWorkspaceTarget('/admin/users', '?tab=teams&q=eng&team=t1', '#members')).toEqual({
      pathname: '/admin/teams',
      search: '?q=eng&team=t1&view=browse',
      hash: '#members',
    });
  });
  it('defaults Settings to General, rejects unknown sections, and uses native links', () => {
    mount('/admin/settings?return=overview');
    expect(screen.getByTestId('url').textContent).toBe('/admin/settings/general?return=overview');
    expect(screen.getByRole('link', { name: 'General' }).getAttribute('aria-current')).toBe('page');
    expect(screen.queryByRole('tab')).toBeNull();
    cleanup();
    mount('/admin/settings/not-a-section');
    expect(screen.queryByRole('navigation', { name: 'Gateway settings' })).toBeNull();
  });
  it('retains visited drafts across path navigation and Back, without mounting unvisited forms', () => {
    mount('/admin/settings/general?filter=keep');
    expect(screen.queryByLabelText('License draft')).toBeNull();
    fireEvent.change(screen.getByLabelText('System draft'), { target: { value: 'unsaved timeout' } });
    fireEvent.click(screen.getByRole('link', { name: 'Sign-in & provisioning' }));
    fireEvent.change(screen.getByLabelText('Directory draft'), { target: { value: 'unsaved directory' } });
    expect(screen.getByLabelText('System draft').closest('[hidden]')).not.toBeNull();
    fireEvent.click(screen.getByRole('link', { name: 'License & updates' }));
    fireEvent.change(screen.getByLabelText('License draft'), { target: { value: 'unsaved license' } });
    fireEvent.click(screen.getByRole('button', { name: 'Back' }));
    expect((screen.getByLabelText('Directory draft') as HTMLInputElement).value).toBe('unsaved directory');
    expect(screen.getByTestId('url').textContent).toBe('/admin/settings/sign-in?filter=keep');
    fireEvent.click(screen.getByRole('link', { name: 'General' }));
    expect((screen.getByLabelText('System draft') as HTMLInputElement).value).toBe('unsaved timeout');
  });
  it('does not resolve the static import route as a team ID', () => {
    render(
      <QueryClientProvider client={new QueryClient()}>
        <Breadcrumb path="/teams/import" />
      </QueryClientProvider>,
    );
    expect(screen.getByText('Import teams')).toBeTruthy();
    expect(api.get).not.toHaveBeenCalled();
  });
});
