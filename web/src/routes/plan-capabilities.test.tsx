/**
 * Capabilities of a person's own plan models (my/<provider>/<model>): the
 * value in force with where it came from (assumed for the vendor's plans,
 * reported by the vendor, measured, set by you), Yes / No / Use default,
 * and Measure behind a confirmation that says it spends the plan. Owner
 * ruling: Set and Measure only on the Subscriptions page (mode="manage");
 * the Models page pop-up is read-only (mode="view") with a link there.
 */
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { api } from '../lib/api';
import type { Me } from '../lib/types';
import type { PlanCapabilitiesResponse, PlanCapabilityView } from '../lib/capabilities';
import { ToastProvider } from '../components/ui';
import { PlanCapabilities, planCapabilityChips } from './PlanCapabilities';
import { ModelsPage } from './Models';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});

const me = { id: 'u1', email: 'ada@example.com', name: 'Ada', role: 'user', endpoint: 'http://localhost:8080/v1' } as Me;
vi.mock('../app/session', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../app/session')>();
  return { ...actual, useLocalOnly: () => false, useMe: () => me };
});

const mocked = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function wrap(node: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter>{node}</MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

const at = '2026-11-14T10:00:00Z';

function viewFixture(overrides: Partial<PlanCapabilityView> = {}): PlanCapabilityView {
  return {
    model: 'gpt-6.1-sol',
    name: 'my/openai/gpt-6.1-sol',
    selected: true,
    resolved: {
      'input.image': { value: true, source: 'default', at: '0001-01-01T00:00:00Z', note: 'Codex lists image input.' },
      'input.audio': { value: false, source: 'provider', at },
      'input.video': { value: false, source: 'admin', at },
      'request.tools': { value: true, source: 'probe', at, note: 'it called a test function' },
      'thinking.levels': { value: ['low', 'high'], source: 'provider', at },
    },
    defaults: {},
    reported: {},
    measured: {},
    set: { 'input.video': { value: false, at } },
    probed: { at, status: 'ok', requests: 12, input_tokens: 400, output_tokens: 20, by: 'manual' },
    ...overrides,
  };
}

function body(view: PlanCapabilityView): PlanCapabilitiesResponse {
  return {
    subscription_id: 'c1',
    provider: 'openai',
    provider_name: 'OpenAI ChatGPT (Plus / Pro)',
    keys: [],
    max_requests: 30,
    measure_every_seconds: 600,
    models: [view],
  };
}

describe('PlanCapabilities', () => {
  it('shows each value with where it came from', async () => {
    mocked.get.mockResolvedValue(body(viewFixture()));
    wrap(<PlanCapabilities mode="manage" subscriptionId="c1" model="gpt-6.1-sol" />);
    const list = await screen.findByRole('list', { name: 'What it takes' });
    const row = (label: string) => within(list).getByText(label).closest('.cap-row') as HTMLElement;
    expect(row('Images').querySelector('.cap-value-text')?.textContent).toBe('Yes');
    expect(row('Sound').querySelector('.cap-value-text')?.textContent).toBe('No');
    expect(within(row('Images')).getByText('Assumed for OpenAI ChatGPT plans')).toBeTruthy();
    expect(within(row('Images')).getByText('Codex lists image input.')).toBeTruthy();
    expect(within(row('Sound')).getByText('Reported by OpenAI ChatGPT')).toBeTruthy();
    expect(within(row('Video')).getByText('Set by you')).toBeTruthy();
    expect(within(row('Function tools')).getByText(/^Measured /)).toBeTruthy();
    // The rest is folded under More details.
    expect(screen.getByText('More details')).toBeTruthy();
    expect(mocked.get).toHaveBeenCalledWith('/api/v1/me/subscriptions/c1/capabilities');
  });

  it('sets Yes and returns a value to its default', async () => {
    mocked.get.mockResolvedValue(body(viewFixture()));
    mocked.patch.mockResolvedValue(viewFixture());
    wrap(<PlanCapabilities mode="manage" subscriptionId="c1" model="gpt-6.1-sol" />);
    const user = userEvent.setup();
    const sound = await screen.findByRole('group', { name: 'Set Sound' });
    await user.click(within(sound).getByRole('button', { name: 'Yes' }));
    expect(mocked.patch).toHaveBeenCalledWith('/api/v1/me/subscriptions/c1/capabilities', {
      model: 'gpt-6.1-sol',
      set: { 'input.audio': true },
    });
    // Use default is a no-op where nothing is set, and clears a setting.
    await user.click(within(sound).getByRole('button', { name: 'Use default' }));
    expect(mocked.patch).toHaveBeenCalledTimes(1);
    const video = screen.getByRole('group', { name: 'Set Video' });
    expect(within(video).getByRole('button', { name: 'No' }).getAttribute('aria-pressed')).toBe('true');
    await user.click(within(video).getByRole('button', { name: 'Use default' }));
    expect(mocked.patch).toHaveBeenLastCalledWith('/api/v1/me/subscriptions/c1/capabilities', {
      model: 'gpt-6.1-sol',
      set: { 'input.video': null },
    });
  });

  it('measures only after a confirmation that says it spends the plan', async () => {
    mocked.get.mockResolvedValue(body(viewFixture()));
    mocked.post.mockResolvedValue(
      viewFixture({ probed: { at, status: 'running', requests: 0, input_tokens: 0, output_tokens: 0 } }),
    );
    wrap(<PlanCapabilities mode="manage" subscriptionId="c1" model="gpt-6.1-sol" />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Measure again' }));
    const dialog = screen.getByRole('dialog', { name: 'Measure gpt-6.1-sol?' });
    expect(dialog.textContent).toContain('up to 30 small requests');
    expect(dialog.textContent).toContain('through your OpenAI ChatGPT plan');
    expect(dialog.textContent).toContain('count against your plan');
    expect(dialog.textContent).toContain('once every 10 minutes');
    expect(mocked.post).not.toHaveBeenCalled();
    await user.click(within(dialog).getByRole('button', { name: 'Measure' }));
    expect(mocked.post).toHaveBeenCalledWith('/api/v1/me/subscriptions/c1/capabilities/measure', { model: 'gpt-6.1-sol' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Measuring…' })).toBeTruthy());
  });

  it('waits out the measuring window and never offers Measure for a media model', async () => {
    const soon = new Date(Date.now() + 5 * 60_000).toISOString();
    mocked.get.mockResolvedValue(body(viewFixture({ measure_after: soon })));
    wrap(<PlanCapabilities mode="manage" subscriptionId="c1" model="gpt-6.1-sol" />);
    const button = await screen.findByRole('button', { name: 'Measure again' });
    expect((button as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByRole('status').textContent).toContain('You can measure it again');
    cleanup();
    mocked.get.mockResolvedValue(body(viewFixture({ not_measurable: 'only chat models are measured', probed: null })));
    wrap(<PlanCapabilities mode="manage" subscriptionId="c1" model="gpt-6.1-sol" />);
    expect((await screen.findByRole('status')).textContent).toContain('Not measured: only chat models are measured.');
    expect(screen.queryByRole('button', { name: /Measure/ })).toBeNull();
  });

  it('view mode shows values and sources but no Set or Measure, and links to Subscriptions', async () => {
    mocked.get.mockResolvedValue(body(viewFixture()));
    wrap(<PlanCapabilities subscriptionId="c1" model="gpt-6.1-sol" />);
    const list = await screen.findByRole('list', { name: 'What it takes' });
    expect(within(list).getByText('Set by you')).toBeTruthy();
    expect(screen.queryByRole('group', { name: /^Set / })).toBeNull();
    expect(screen.queryByRole('button', { name: /Measure/ })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Yes' })).toBeNull();
    const link = screen.getByRole('link', { name: 'Change on Subscriptions' });
    expect(link.getAttribute('href')).toBe('/subscriptions/openai?capabilities=gpt-6.1-sol');
    expect(mocked.patch).not.toHaveBeenCalled();
    expect(mocked.post).not.toHaveBeenCalled();
  });

  it('chips name only what is known to be yes', () => {
    expect(planCapabilityChips(viewFixture())).toEqual(['Sees images', 'Tools']);
    expect(planCapabilityChips(undefined)).toEqual([]);
  });
});

describe('Models page: your own plans', () => {
  it('lists plan models in their own section and opens their capabilities', async () => {
    mocked.get.mockImplementation(async (path: string) => {
      if (path === '/api/v1/models') {
        return {
          models: [],
          managed_models: [],
          personal_models: [
            {
              id: 'my/openai/gpt-6.1-sol',
              owned_by: 'OpenAI ChatGPT (Plus / Pro)',
              janus: {
                native_name: 'gpt-6.1-sol',
                modalities: ['chat'],
                capabilities: { 'input.image': { value: true, source: 'default', at: '0001-01-01T00:00:00Z' } },
                personal_subscription: { provider: 'openai', subscription_id: 'c1' },
              },
            },
          ],
        };
      }
      if (path === '/api/v1/me/subscriptions/c1/capabilities') return body(viewFixture());
      throw new Error(`unexpected GET ${path}`);
    });
    wrap(<ModelsPage />);
    const section = await screen.findByRole('region', { name: /On your own plans/ });
    const card = within(section).getByRole('button', { name: 'my/openai/gpt-6.1-sol' });
    expect(within(card).getByText('Sees images')).toBeTruthy();
    const user = userEvent.setup();
    await user.click(card);
    const drawer = await screen.findByRole('dialog', { name: 'my/openai/gpt-6.1-sol' });
    expect(await within(drawer).findByRole('heading', { name: 'Capabilities' })).toBeTruthy();
    expect(within(drawer).getByText('Assumed for OpenAI ChatGPT plans')).toBeTruthy();
    // Measuring and setting happen only on the Subscriptions page.
    expect(within(drawer).queryByRole('button', { name: /Measure/ })).toBeNull();
    expect(within(drawer).queryByRole('group', { name: /^Set / })).toBeNull();
    expect(within(drawer).getByRole('link', { name: 'Change on Subscriptions' }).getAttribute('href')).toBe(
      '/subscriptions/openai?capabilities=gpt-6.1-sol',
    );
  });
});
