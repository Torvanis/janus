import { beforeEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { PriceCatalogCard, type PriceCatalogStatus } from './PriceCatalogCard';

vi.mock('../../lib/api', async (original) => ({
  ...(await original<typeof import('../../lib/api')>()),
  api: { get: vi.fn(), patch: vi.fn(), post: vi.fn() },
}));

function status(over: Partial<PriceCatalogStatus['settings']> = {}, extra: Partial<PriceCatalogStatus> = {}): PriceCatalogStatus {
  return {
    catalog: {
      version: '2026.11.16.1',
      label: 'Janus price catalog 2026.11.16.1',
      publisher: 'Janus',
      generated_at: '2026-11-16T00:00:00Z',
      origin: 'bundled',
      key_id: 'f85a9048ee1fa231',
      entries: 174,
      defaults: 6,
    },
    settings: {
      refresh_enabled: false,
      refresh_locked: false,
      refresh_source: 'settings',
      url: 'https://janusedge.com/pricing/catalog.json',
      url_locked: false,
      url_source: 'default',
      require_signature: true,
      offline: false,
      offline_blocked: false,
      ...over,
    },
    saved: { refresh_enabled: false },
    status: {},
    default_url: 'https://janusedge.com/pricing/catalog.json',
    trusted_keys: [{ id: 'f85a9048ee1fa231', source: 'built-in' }],
    ...extra,
  };
}

function renderCard() {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}>
      <PriceCatalogCard />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.resetAllMocks();
});

it('shows the bundled catalog, refresh off by default, and turns the refresh on', async () => {
  vi.mocked(api.get).mockResolvedValue(status());
  vi.mocked(api.patch).mockResolvedValue({});
  renderCard();
  const inUse = await screen.findByTestId('price-catalog-in-use');
  expect(within(inUse).getByText('Janus price catalog 2026.11.16.1')).toBeTruthy();
  expect(within(inUse).getByText('Built in')).toBeTruthy();
  const toggle = screen.getByLabelText('Refresh the price catalog daily') as HTMLInputElement;
  expect(toggle.checked).toBe(false);
  expect(toggle.disabled).toBe(false);
  expect((screen.getByRole('button', { name: 'Check now' }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByText('Never checked')).toBeTruthy();
  expect(screen.queryByText(/Signature checking is off/)).toBeNull();
  await userEvent.click(toggle);
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('/api/v1/admin/price-catalog/settings', { refresh_enabled: true }));
  await userEvent.type(screen.getByLabelText('Catalog URL'), 'https://prices.internal.example/catalog.json');
  await userEvent.click(screen.getByRole('button', { name: 'Save URL' }));
  await waitFor(() =>
    expect(api.patch).toHaveBeenCalledWith('/api/v1/admin/price-catalog/settings', {
      url: 'https://prices.internal.example/catalog.json',
    }),
  );
});

it('locks what the environment sets, warns when signatures are off, and checks now', async () => {
  vi.mocked(api.get).mockResolvedValue(
    status(
      {
        refresh_enabled: true,
        refresh_locked: true,
        refresh_source: 'env',
        url: 'https://mirror.example/c.json',
        url_locked: true,
        url_source: 'env',
        require_signature: false,
      },
      {
        status: {
          checked_at: '2026-11-16T03:00:00Z',
          result: 'updated',
          applied_version: '2026.11.17.1',
          lines_changed: 12,
          models_changed: 4,
        },
      },
    ),
  );
  vi.mocked(api.post).mockResolvedValue({});
  renderCard();
  const toggle = (await screen.findByLabelText('Refresh the price catalog daily')) as HTMLInputElement;
  expect(toggle.checked).toBe(true);
  expect(toggle.disabled).toBe(true);
  expect(screen.getByText('Set by JANUS_PRICE_CATALOG_REFRESH.')).toBeTruthy();
  const url = screen.getByLabelText('Catalog URL') as HTMLInputElement;
  expect(url.disabled).toBe(true);
  expect(url.value).toBe('https://mirror.example/c.json');
  expect(screen.getByText(/Signature checking is off/)).toBeTruthy();
  expect(screen.getByTestId('price-catalog-last-check').textContent).toMatch(
    /2026\.11\.17\.1 applied, 12 prices changed on 4 models/,
  );
  await userEvent.click(screen.getByRole('button', { name: 'Check now' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/admin/price-catalog/check', {}));
});

it('explains offline mode', async () => {
  vi.mocked(api.get).mockResolvedValue(
    status({ refresh_locked: true, refresh_source: 'offline', offline: true, offline_blocked: true }),
  );
  renderCard();
  expect(await screen.findByText(/Offline mode \(JANUS_OFFLINE\) keeps the refresh off/)).toBeTruthy();
  expect((screen.getByLabelText('Refresh the price catalog daily') as HTMLInputElement).disabled).toBe(true);
});
