/**
 * Upstream deletion preflight: the delete dialog fetches what depends on the
 * upstream, lists the hosted models and the managed models targeting them,
 * blocks the delete while enabled aliases depend on it until the admin
 * explicitly forces, and offers to purge the direct grants that would
 * otherwise dangle. The DELETE carries force / purge_grants accordingly.
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
  const dialog = await screen.findByRole('dialog', { name: 'Delete “Anthropic”?' });
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
    await user.click(within(dialog).getByText('60 hosted model(s) will be disabled'));
    const physical = within(dialog).getByRole('region', { name: 'Hosted dependents' });
    await user.click(within(physical).getByRole('button', { name: /Next/ }));
    expect(within(physical).getByText('physical-25')).toBeTruthy();
    await user.type(within(physical).getByRole('searchbox'), 'physical-59');
    expect(within(physical).getByText('physical-59')).toBeTruthy();
    expect(within(physical).getByText('1 of 60 rows')).toBeTruthy();
    const aliases = within(dialog).getByRole('region', { name: 'Managed alias dependents' });
    await user.click(within(aliases).getByRole('button', { name: /Next/ }));
    expect(within(aliases).getByText('alias-25')).toBeTruthy();
    await user.type(within(aliases).getByRole('searchbox'), 'no-match');
    expect(within(aliases).getByText('0 of 60 rows')).toBeTruthy();
    expect(within(dialog).getByText('60 managed model(s) target models on this upstream.')).toBeTruthy();
    await user.type(within(dialog).getByRole('textbox'), 'Anthropic');
    const confirm = within(dialog).getByRole('button', { name: 'Delete upstream' }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    await user.click(within(dialog).getByRole('checkbox', { name: /Delete anyway and break 60 enabled managed model/ }));
    await user.click(within(dialog).getByRole('checkbox', { name: /Also remove 75 direct grant/ }));
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
    await user.type(within(dialog).getByRole('textbox'), 'Anthropic');
    expect((within(dialog).getByRole('button', { name: 'Delete upstream' }) as HTMLButtonElement).disabled).toBe(true);
    expect(mocked.del).not.toHaveBeenCalled();
  });

  it('keeps discovery problem scope independent of the upstream table search', async () => {
    mockApi({});
    const get = mocked.get.getMockImplementation()!;
    mocked.get.mockImplementation((path: string) =>
      path === '/api/v1/admin/upstreams'
        ? Promise.resolve({
            upstreams: Array.from({ length: 60 }, (_, i) => ({
              ...upstream,
              id: `up-${i}`,
              name: `Provider-${i}`,
              last_error: `failure-${i}`,
            })),
            adapter_types: ['anthropic'],
          })
        : get(path),
    );
    renderList();
    const user = userEvent.setup();
    const problems = await screen.findByRole('region', { name: 'Discovery problems' });
    expect(within(problems).getByText('60 of 60 rows')).toBeTruthy();
    await user.click(within(problems).getByRole('button', { name: /Next/ }));
    expect(within(problems).getByText('failure-25')).toBeTruthy();
    await user.type(within(problems).getByRole('searchbox'), 'failure-59');
    expect(within(problems).getByText('failure-59')).toBeTruthy();
    expect(within(problems).getByText('1 of 60 rows')).toBeTruthy();
    const mainSearch = screen.getAllByRole('searchbox').find((input) => input !== within(problems).getByRole('searchbox'))!;
    await user.type(mainSearch, 'no-upstream');
    await waitFor(() => expect(within(screen.getByRole('region', { name: 'Upstreams' })).getByText('0 of 0 rows')).toBeTruthy());
    expect(within(problems).getByText('1 of 60 rows')).toBeTruthy();
  });

  it('with no dependent aliases, typing the name is enough and the DELETE carries no flags', async () => {
    mockApi({ models: twoModels, managed_models: [], grant_count: 0, blocking_managed_models: 0 });
    renderList();
    const user = userEvent.setup();
    const dialog = await openDeleteDialog();

    expect(within(dialog).getByText('2 hosted model(s) will be disabled')).toBeTruthy();
    expect(within(dialog).queryByRole('alert')).toBeNull();
    const confirm = within(dialog).getByRole('button', { name: 'Delete upstream' });
    expect((confirm as HTMLButtonElement).disabled).toBe(true);
    await user.type(within(dialog).getByRole('textbox'), 'Anthropic');
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

    const warning = within(dialog).getByRole('alert');
    expect(within(warning).getByText('2 managed model(s) target models on this upstream.')).toBeTruthy();
    expect(within(warning).getByText('current-best')).toBeTruthy();
    expect(within(warning).getByText(/old-alias/)).toBeTruthy();
    expect(within(warning).getByText(/already disabled/)).toBeTruthy();
    expect(
      within(warning)
        .getByRole('link', { name: /Managed models page/ })
        .getAttribute('href'),
    ).toBe('/admin/managed-models');

    const confirm = within(dialog).getByRole('button', { name: 'Delete upstream' });
    await user.type(within(dialog).getByRole('textbox'), 'Anthropic');
    // Typed name alone is not enough while an enabled alias blocks.
    expect((confirm as HTMLButtonElement).disabled).toBe(true);

    await user.click(within(dialog).getByRole('checkbox', { name: /Delete anyway and break 1 enabled managed model/ }));
    expect((confirm as HTMLButtonElement).disabled).toBe(false);
    await user.click(within(dialog).getByRole('checkbox', { name: /Also remove 3 direct grant/ }));
    await user.click(confirm);

    await waitFor(() => expect(mocked.del).toHaveBeenCalledWith('/api/v1/admin/upstreams/up-1?force=true&purge_grants=true'));
  });
});
