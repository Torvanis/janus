/**
 * Business-feature upsells in the SPA: with a known-Community session the
 * enabling control is disabled and says why; with Business it works; with
 * no license info yet (loading / older gateway) nothing is greyed out —
 * the API's 402 is the authority.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import type { LicenseSummary } from '../../lib/types';
import { SecurityPage } from './Security';
import { AuditPage } from './Audit';
import { SessionContext } from '../../app/session';

let license: LicenseSummary | undefined;
vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

const community: LicenseSummary = { edition: 'community', status: 'valid', restricted: false, features: [] };
const business: LicenseSummary = {
  edition: 'business',
  status: 'valid',
  restricted: false,
  features: ['guardrails_enforce', 'audit_export'],
};

function renderAt(path: string, el: React.ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <SessionContext.Provider
        value={{ me: { role: 'admin', license } as never, config: null, loading: false, error: null, refresh: () => {} }}
      >
        <ToastProvider>
          <MemoryRouter initialEntries={[path]}>
            <Routes>
              <Route path="/admin/security/:tab" element={el} />
              <Route path="/admin/audit" element={el} />
            </Routes>
          </MemoryRouter>
        </ToastProvider>
      </SessionContext.Provider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  mocked.get.mockImplementation((path: string) =>
    Promise.resolve(
      path.includes('/policies')
        ? { policies: [] }
        : path.includes('/term-lists')
          ? { term_lists: [] }
          : path.includes('/classifiers')
            ? { classifiers: [] }
            : path.includes('/rules')
              ? { rules: [] }
              : path.includes('/audit')
                ? { entries: [], total_count: 0, note: '' }
                : {},
    ),
  );
});
afterEach(cleanup);

describe('Business upsells', () => {
  it('greys out Redact/Block on Community with the reason, and leaves Observe', async () => {
    license = community;
    renderAt('/admin/security/policies', <SecurityPage />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'New policy' }));
    const drawer = screen.getByRole('dialog');
    await user.click(within(drawer).getByLabelText('Secrets: Enabled'));
    const modes = within(drawer).getByRole('group', { name: 'Secrets: Mode' });
    const block = within(modes).getByRole('button', { name: 'Block' }) as HTMLButtonElement;
    expect(block.disabled).toBe(true);
    expect(block.title).toMatch(/Business features/);
    expect((within(modes).getByRole('button', { name: 'Observe' }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('enables Block with a Business key', async () => {
    license = business;
    renderAt('/admin/security/policies', <SecurityPage />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'New policy' }));
    const drawer = screen.getByRole('dialog');
    await user.click(within(drawer).getByLabelText('Secrets: Enabled'));
    const modes = within(drawer).getByRole('group', { name: 'Secrets: Mode' });
    expect((within(modes).getByRole('button', { name: 'Block' }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('does not grey anything out while the license is unknown', async () => {
    license = undefined;
    renderAt('/admin/security/policies', <SecurityPage />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'New policy' }));
    const drawer = screen.getByRole('dialog');
    await user.click(within(drawer).getByLabelText('Secrets: Enabled'));
    const modes = within(drawer).getByRole('group', { name: 'Secrets: Mode' });
    expect((within(modes).getByRole('button', { name: 'Block' }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('audit CSV download: disabled with the upsell on Community, a real link on Business', async () => {
    license = community;
    const first = renderAt('/admin/audit', <AuditPage />);
    await screen.findByText('CSV export is a Business feature.');
    expect((screen.getByRole('button', { name: 'Download CSV' }) as HTMLButtonElement).disabled).toBe(true);
    first.unmount();

    license = business;
    renderAt('/admin/audit?action=license.install', <AuditPage />);
    const link = (await screen.findByRole('link', { name: 'Download CSV' })) as HTMLAnchorElement;
    expect(link.getAttribute('href')).toBe('/api/v1/admin/audit/export?action=license.install');
    expect(link.hasAttribute('download')).toBe(true);
  });
});
