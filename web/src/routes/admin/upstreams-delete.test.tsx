/**
 * Upstream deletion preflight: the delete dialog fetches what depends on the
 * upstream, summarizes the models that turn off and the managed models that
 * stop working (each with a Fix link), blocks the delete while enabled
 * managed models depend on it until the admin explicitly chooses "Delete
 * anyway", and offers to remove the grants that would otherwise dangle. The
 * name must be typed only when something in use breaks. The DELETE carries
 * force / purge_grants accordingly.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { Upstream } from '../../lib/types';
import { ToastProvider } from '../../components/ui';
import { UpstreamsPage } from './Upstreams';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

const upstream: Upstream = {
  id: 'up-1',
  name: 'Anthropic',
  adapter_type: 'anthropic',
  base_url: 'https://api.anthropic.example',
  api_key_mask: 'sk-…cdef',
  has_api_key: true,
  enabled: true,
  last_check_at: '2026-08-20T00:00:00Z',
  last_error: '',
  last_latency_ms: 42,
  model_count: 2,
  created_at: '2026-08-01T00:00:00Z',
};

function mockApi(dependents: unknown) {
  mocked.get.mockImplementation((path: string) => {
    if (path === '/api/v1/admin/upstreams') {
      return Promise.resolve({ upstreams: [upstream], adapter_types: ['anthropic'] });
    }
    if (path === '/api/v1/admin/upstreams/up-1/dependents') return Promise.resolve(dependents);
    if (path.startsWith('/api/v1/admin/models')) return Promise.resolve({ models: [], counts: {}, modalities: [] });
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
  mocked.del.mockImplementation(() =>
    Promise.resolve({ id: 'up-1', deleted: true, models_disabled: 2, managed_models_broken: [], grants_removed: 0 }),
  );
}

function renderList() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={['/admin/upstreams']}>
          <Routes>
            <Route path="/admin/upstreams" element={<UpstreamsPage />} />
            <Route path="/admin/upstreams/:id" element={<UpstreamsPage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

async function openDeleteDialog(): Promise<HTMLElement> {
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: 'Delete' }));
  const dialog = await screen.findByRole('dialog', { name: 'Delete upstream “Anthropic”?' });
  await within(dialog).findByTestId('upstream-dependents');
  return dialog;
}

const twoModels = [
  { id: 'm-1', name: 'claude-fable-5', display_name: '', status: 'enabled' },
  { id: 'm-2', name: 'claude-haiku-9', display_name: 'Haiku', status: 'enabled' },
];

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('upstream delete dialog — dependents preflight', () => {
  it('searches and pages complete dependencies without narrowing confirmation impact', async () => {
    mockApi({
      models: Array.from({ length: 60 }, (_, i) => ({
        id: `m-${i}`,
        name: `physical-${i}`,
        display_name: '',
        status: 'enabled',
      })),
      managed_models: Array.from({ length: 60 }, (_, i) => ({
        id: `a-${i}`,
        name: `alias-${i}`,
        target_model_name: `physical-${i}`,
        status: 'enabled',
      })),
      grant_count: 75,
      blocking_managed_models: 60,
    });
    renderList();
    const user = userEvent.setup();
    const dialog = await openDeleteDialog();
    await user.click(within(dialog).getByText('60 models turn off'));
    const physical = within(dialog).getByRole('region', { name: 'Affected models' });
    await user.click(within(physical).getByRole('button', { name: /Next/ }));
    expect(within(physical).getByText('physical-25')).toBeTruthy();
    await user.type(within(physical).getByRole('searchbox'), 'physical-59');
    expect(within(physical).getByText('physical-59')).toBeTruthy();
    expect(within(physical).getByText('1 of 60 rows')).toBeTruthy();
    const aliases = within(dialog).getByRole('region', { name: 'Affected managed models' });
    await user.click(within(aliases).getByRole('button', { name: /Next/ }));
    expect(within(aliases).getByText('alias-25')).toBeTruthy();
    await user.type(within(aliases).getByRole('searchbox'), 'no-match');
    expect(within(aliases).getByText('0 of 60 rows')).toBeTruthy();
    expect(within(dialog).getByText('60 managed models stop working')).toBeTruthy();
    await user.type(within(dialog).getByRole('textbox'), 'Anthropic');
    const confirm = within(dialog).getByRole('button', { name: 'Delete upstream' }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    await user.click(within(dialog).getByRole('checkbox', { name: /Delete anyway and break 60 managed models/ }));
    await user.click(within(dialog).getByRole('checkbox', { name: /Also remove the 75 grants/ }));
    expect(mocked.del).not.toHaveBeenCalled();
    await user.click(confirm);
    await waitFor(() => expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/upstreams/up-1?force=true&purge_grants=true'));
  });

  it('does not permit deletion after a failed preflight', async () => {
    mockApi({});
    const get = mocked.get.getMockImplementation()!;
    mocked.get.mockImplementation((path: string) =>
      path.endsWith('/dependents') ? Promise.reject(new Error('unavailable')) : get(path),
    );
    renderList();
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Delete' }));
    const dialog = await screen.findByRole('dialog');
    // The blocker is named and comes with a way out; no name field, no delete.
    expect((await within(dialog).findByRole('alert')).textContent).toContain('Couldn’t check what uses Anthropic');
    expect(within(dialog).queryByRole('textbox')).toBeNull();
    expect((within(dialog).getByRole('button', { name: 'Delete upstream' }) as HTMLButtonElement).disabled).toBe(true);
    // Try again re-runs the check; once it succeeds the delete can proceed.
    mocked.get.mockImplementation((path: string) =>
      path.endsWith('/dependents')
        ? Promise.resolve({ models: twoModels, managed_models: [], grant_count: 0, blocking_managed_models: 0 })
        : get(path),
    );
    await user.click(within(dialog).getByRole('button', { name: 'Try again' }));
    await within(dialog).findByTestId('upstream-dependents');
    await user.type(within(dialog).getByRole('textbox'), 'Anthropic');
    expect((within(dialog).getByRole('button', { name: 'Delete upstream' }) as HTMLButtonElement).disabled).toBe(false);
    expect(mocked.del).not.toHaveBeenCalled();
  });

  it("shows a failing upstream's reason in its own row, with no separate problems panel", async () => {
    mockApi({});
    const get = mocked.get.getMockImplementation()!;
    mocked.get.mockImplementation((path: string) =>
      path === '/api/v1/admin/upstreams'
        ? Promise.resolve({
            upstreams: [
              {
                ...upstream,
                id: 'up-ok',
                name: 'Healthy',
                last_error: '',
                last_check_at: '2026-11-06T00:00:00Z',
                last_latency_ms: 12,
              },
              {
                ...upstream,
                id: 'up-bad',
                name: 'Broken',
                last_error: 'reach upstream: context deadline exceeded',
                last_check_at: '2026-11-06T00:00:00Z',
              },
              { ...upstream, id: 'up-off', name: 'Parked', enabled: false, last_error: 'DEV COPY: credentials omitted' },
            ],
            adapter_types: ['anthropic'],
          })
        : get(path),
    );
    renderList();
    const table = await screen.findByRole('region', { name: 'Upstreams' });
    const broken = (await within(table).findByRole('button', { name: 'Broken' })).closest('tr')!;
    expect(within(broken).getByText('reach upstream: context deadline exceeded')).toBeTruthy();
    expect(within(broken).getByText(/checked/)).toBeTruthy();
    const parked = within(table).getByRole('button', { name: 'Parked' }).closest('tr')!;
    expect(within(parked).getByText('Last error: DEV COPY: credentials omitted')).toBeTruthy();
    expect(screen.queryByRole('region', { name: 'Discovery problems' })).toBeNull();
  });

  it('with no dependent aliases, typing the name is enough and the DELETE carries no flags', async () => {
    mockApi({ models: twoModels, managed_models: [], grant_count: 0, blocking_managed_models: 0 });
    renderList();
    const user = userEvent.setup();
    const dialog = await openDeleteDialog();

    expect(within(dialog).getByText('2 models turn off')).toBeTruthy();
    expect(within(dialog).getByText('· 2 in use')).toBeTruthy();
    expect(within(dialog).queryByRole('alert')).toBeNull();
    expect(within(dialog).queryByRole('checkbox')).toBeNull();
    const confirm = within(dialog).getByRole('button', { name: 'Delete upstream' });
    expect((confirm as HTMLButtonElement).disabled).toBe(true);
    // Models in use break, so the name field appears and takes focus.
    const field = within(dialog).getByRole('textbox');
    await waitFor(() => expect(document.activeElement).toBe(field));
    await user.type(field, 'Anthropic');
    expect((confirm as HTMLButtonElement).disabled).toBe(false);
    await user.click(confirm);
    await waitFor(() => expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/upstreams/up-1'));
  });

  it('blocks the delete while enabled managed models depend on it until the admin forces, then sends force=true', async () => {
    mockApi({
      models: twoModels,
      managed_models: [
        { id: 'mm-1', name: 'current-best', status: 'enabled', target_model_name: 'claude-fable-5' },
        { id: 'mm-2', name: 'old-alias', status: 'disabled', target_model_name: 'claude-haiku-9' },
      ],
      grant_count: 3,
      blocking_managed_models: 1,
    });
    renderList();
    const user = userEvent.setup();
    const dialog = await openDeleteDialog();

    // Only the enabled one stops working; the blocker says what to do.
    expect(within(dialog).getByText('1 managed model stops working')).toBeTruthy();
    expect(within(dialog).getByRole('alert').textContent).toBe('Fix it first: point it at another model or turn it off.');
    const aliases = within(dialog).getByRole('region', { name: 'Affected managed models' });
    expect(within(aliases).getByText('current-best')).toBeTruthy();
    expect(within(aliases).getByText(/already off/)).toBeTruthy();
    // A short list is just the list: no search box or page-size picker.
    expect(within(aliases).queryByRole('searchbox')).toBeNull();
    expect(within(aliases).queryByRole('combobox')).toBeNull();
    // Each managed model links straight to its row on the Managed models page.
    expect(within(aliases).getByRole('link', { name: 'Fix current-best on the Managed models page' }).getAttribute('href')).toBe(
      '/admin/managed-models?q=current-best',
    );
    expect(within(aliases).getByRole('link', { name: 'Fix old-alias on the Managed models page' }).getAttribute('href')).toBe(
      '/admin/managed-models?q=old-alias',
    );

    const confirm = within(dialog).getByRole('button', { name: 'Delete upstream' });
    await user.type(within(dialog).getByRole('textbox'), 'Anthropic');
    // Typed name alone is not enough while an enabled alias blocks.
    expect((confirm as HTMLButtonElement).disabled).toBe(true);

    await user.click(within(dialog).getByRole('checkbox', { name: /Delete anyway and break 1 managed model/ }));
    expect((confirm as HTMLButtonElement).disabled).toBe(false);
    await user.click(within(dialog).getByRole('checkbox', { name: /Also remove the 3 grants/ }));
    await user.click(confirm);

    await waitFor(() => expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/upstreams/up-1?force=true&purge_grants=true'));
  });

  it('deletes an upstream with nothing in use in one click, without typing the name', async () => {
    mockApi({
      models: [{ id: 'm-1', name: 'claude-fable-5', display_name: '', status: 'pending_approval' }],
      managed_models: [{ id: 'mm-1', name: 'parked', status: 'disabled', target_model_name: 'claude-fable-5' }],
      grant_count: 1,
      blocking_managed_models: 0,
    });
    renderList();
    const user = userEvent.setup();
    const dialog = await openDeleteDialog();

    expect(within(dialog).getByText('1 model turns off')).toBeTruthy();
    expect(within(dialog).getByText('1 managed model(s) point here, all already off')).toBeTruthy();
    expect(within(dialog).queryByRole('textbox')).toBeNull();
    expect(within(dialog).queryByRole('checkbox', { name: /Delete anyway/ })).toBeNull();
    // Focus stays on the safe choice.
    expect(document.activeElement).toBe(within(dialog).getByRole('button', { name: 'Cancel' }));
    const confirm = within(dialog).getByRole('button', { name: 'Delete upstream' }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(false);
    await user.click(confirm);
    await waitFor(() => expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/upstreams/up-1'));
    expect(await screen.findByText('Anthropic deleted. Its models are off, and usage history is kept.')).toBeTruthy();
  });

  it('never sends force when the admin unticks "Delete anyway" again', async () => {
    mockApi({
      models: twoModels,
      managed_models: [{ id: 'mm-1', name: 'current-best', status: 'enabled', target_model_name: 'claude-fable-5' }],
      grant_count: 0,
      blocking_managed_models: 1,
    });
    renderList();
    const user = userEvent.setup();
    const dialog = await openDeleteDialog();
    const force = within(dialog).getByRole('checkbox', { name: /Delete anyway/ });
    const confirm = within(dialog).getByRole('button', { name: 'Delete upstream' }) as HTMLButtonElement;
    await user.type(within(dialog).getByRole('textbox'), 'Anthropic');
    await user.click(force);
    expect(confirm.disabled).toBe(false);
    await user.click(force);
    expect(confirm.disabled).toBe(true);
    expect(mocked.del).not.toHaveBeenCalled();
  });
});
