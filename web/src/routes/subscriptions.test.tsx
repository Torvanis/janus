/**
 * Personal subscriptions UI: the per-connection model picker (only ticked
 * models are offered) and the admin card on Settings → General (hidden on an
 * offline install, vendor toggles only while the feature is on).
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { api } from '../lib/api';
import type { SubscriptionConnection } from '../lib/types';
import { ToastProvider } from '../components/ui';
import { ModelPicker, SelectedModels, SubscriptionsPage } from './Subscriptions';
import { PersonalSubscriptionsCard } from './admin/PersonalSubscriptionsCard';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
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

function Where() {
  return <span data-testid="where">{useLocation().pathname}</span>;
}

/** The page under its real routes, starting at path. */
function wrapPage(path = '/subscriptions') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route path="/subscriptions" element={<SubscriptionsPage />} />
            <Route path="/subscriptions/:provider" element={<SubscriptionsPage />} />
          </Routes>
          <Where />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

const conn = (selected: string[]): SubscriptionConnection =>
  ({
    id: 'c1',
    provider: 'xai',
    models: ['grok-a', 'grok-b', 'grok-c'],
    selected_models: selected,
    status: 'active',
  }) as SubscriptionConnection;

describe('ModelPicker (Add models panel)', () => {
  it('prompts for a choice when nothing is selected and saves only ticked models in plan order', async () => {
    const onSave = vi.fn();
    wrap(<ModelPicker connection={conn([])} saving={false} onSave={onSave} onClose={vi.fn()} />);
    expect(screen.getByRole('dialog', { name: 'Models to use' })).toBeTruthy();
    const save = screen.getByRole('button', { name: 'Save selection' });
    expect((save as HTMLButtonElement).disabled).toBe(true);

    const user = userEvent.setup();
    await user.click(screen.getByRole('checkbox', { name: 'grok-c' }));
    await user.click(screen.getByRole('checkbox', { name: 'grok-a' }));
    expect(screen.getByText(/Unsaved changes/)).toBeTruthy();
    await user.click(save);
    expect(onSave).toHaveBeenCalledWith(['grok-a', 'grok-c']);
  });

  it('select all / clear and reflects the saved state', async () => {
    const onSave = vi.fn();
    wrap(<ModelPicker connection={conn(['grok-b'])} saving={false} onSave={onSave} onClose={vi.fn()} />);
    expect((screen.getByRole('checkbox', { name: 'grok-b' }) as HTMLInputElement).checked).toBe(true);
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Select all' }));
    await user.click(screen.getByRole('button', { name: 'Save selection' }));
    expect(onSave).toHaveBeenLastCalledWith(['grok-a', 'grok-b', 'grok-c']);
    await user.click(screen.getByRole('button', { name: 'Clear' }));
    await user.click(screen.getByRole('button', { name: 'Save selection' }));
    expect(onSave).toHaveBeenLastCalledWith([]);
  });

  it('filters the list and closes on Cancel', async () => {
    const onClose = vi.fn();
    wrap(<ModelPicker connection={conn(['grok-b'])} saving={false} onSave={vi.fn()} onClose={onClose} />);
    const user = userEvent.setup();
    await user.type(screen.getByRole('searchbox', { name: 'Filter models' }), 'grok-c');
    expect(screen.queryByRole('checkbox', { name: 'grok-a' })).toBeNull();
    expect(screen.getByRole('checkbox', { name: 'grok-c' })).toBeTruthy();
    await user.clear(screen.getByRole('searchbox', { name: 'Filter models' }));
    await user.type(screen.getByRole('searchbox', { name: 'Filter models' }), 'nothing');
    expect(screen.getByText('No models match the filter.')).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onClose).toHaveBeenCalled();
  });
});

describe('SelectedModels', () => {
  it('lists only selected models with the name to call, a copy button and a remove button', async () => {
    const onRemove = vi.fn();
    wrap(
      <SelectedModels
        connection={conn(['grok-b'])}
        modelName={(m) => `my/xai/${m}`}
        saving={false}
        onSave={vi.fn()}
        onRemove={onRemove}
      />,
    );
    expect(screen.getByText((_, el) => el?.textContent === 'my/xai/grok-b' && el.classList.contains('mono'))).toBeTruthy();
    expect(screen.queryByText(/grok-a/)).toBeNull();
    expect(screen.getByText(/1 of 3 from this plan/)).toBeTruthy();
    // The full list is hidden until Add models.
    expect(screen.queryByRole('checkbox')).toBeNull();
    const user = userEvent.setup();
    expect(screen.queryByRole('button', { name: 'Copy' })).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Copy my/xai/grok-b' }));
    expect(await navigator.clipboard.readText()).toBe('my/xai/grok-b');
    await user.click(screen.getByRole('button', { name: 'Remove grok-b' }));
    expect(onRemove).toHaveBeenCalledWith('grok-b');
  });

  it('opens the Add models panel and closes it after a successful save', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    wrap(<SelectedModels connection={conn([])} modelName={(m) => m} saving={false} onSave={onSave} onRemove={vi.fn()} />);
    expect(screen.getByText(/No models selected yet/)).toBeTruthy();
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Add models' }));
    await user.click(screen.getByRole('checkbox', { name: 'grok-a' }));
    await user.click(screen.getByRole('button', { name: 'Save selection' }));
    expect(onSave).toHaveBeenCalledWith(['grok-a']);
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('shows the reasoning range for selected models and the full list in the panel', async () => {
    const c = {
      ...conn(['o-think', 'gpt-4o']),
      models: ['gpt-4o', 'o-think', 'mystery'],
      reasoning_efforts: { 'gpt-4o': [], 'o-think': ['low', 'medium', 'high', 'xhigh'] },
    } as SubscriptionConnection;
    wrap(<SelectedModels connection={c} modelName={(m) => m} saving={false} onSave={vi.fn()} onRemove={vi.fn()} />);
    const range = screen.getByText('reasoning: low → xhigh');
    expect(range.getAttribute('title')).toContain('low · medium · high · xhigh');
    // Models without a reasoning setting show nothing in the compact list.
    expect(screen.queryByText('no reasoning setting')).toBeNull();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Add models' }));
    const panel = screen.getByRole('dialog');
    expect(within(panel).getByText('no reasoning setting')).toBeTruthy();
    expect(within(panel).getByText('low · medium · high · xhigh')).toBeTruthy();
    // Unknown models show nothing rather than a guess.
    expect(within(panel).getAllByText(/no reasoning setting|·/)).toHaveLength(2);
  });
});

describe('SubscriptionsPage', () => {
  it('shows sign-in health, plan usage meters and Janus activity, and connects key providers with a pasted key', async () => {
    const copilot = {
      id: 'copilot',
      name: 'GitHub Copilot',
      description: '',
      enabled: true,
      auth: 'device',
      connection: {
        id: 'c2',
        provider: 'copilot',
        account_email: 'octo',
        account_name: 'octo',
        models: ['gpt-4o'],
        selected_models: ['gpt-4o'],
        status: 'active',
        last_error: '',
        last_used_at: '',
        created_at: '',
        updated_at: '',
        auto_renews: false,
        checked_at: new Date(Date.now() - 5 * 60_000).toISOString(),
        plan_usage: {
          plan: 'Copilot Free',
          windows: [
            { label: 'Chat requests', used_percent: 25, used: 50, limit: 200 },
            { label: 'Premium requests', used_percent: 0, unlimited: true },
          ],
        },
        activity: {
          day: { requests: 3, tokens_in: 1000, tokens_out: 200 },
          week: { requests: 9, tokens_in: 5000, tokens_out: 900 },
        },
      },
    };
    const mistral = {
      id: 'mistral',
      name: 'Mistral Vibe',
      description: '',
      enabled: true,
      auth: 'key',
      key_help: 'Create a Vibe key.',
      key_url: 'https://chat.mistral.ai/code/extensions',
      connection: null,
    };
    mocked.get.mockResolvedValue({ available: true, providers: [copilot, mistral], model_prefix: 'my/' });
    mocked.post.mockResolvedValue({ status: 'failed', message: 'Mistral does not accept this key.' });
    wrapPage();

    // No provider in the URL: the first connected provider's tab opens.
    expect(await screen.findByText(/Sign-in does not expire · checked 5 minutes ago/)).toBeTruthy();
    expect(screen.getByTestId('where').textContent).toBe('/subscriptions/copilot');
    expect(screen.getByText('50 of 200 used')).toBeTruthy();
    expect(screen.getByRole('meter', { name: 'Chat requests' }).getAttribute('aria-valuenow')).toBe('25');
    expect(screen.getByText('Unlimited')).toBeTruthy();
    expect(screen.getByText(/Last 24 h: 3 requests, 1,200 tokens/)).toBeTruthy();
    expect(screen.getByText(/Copilot Free/)).toBeTruthy();

    const user = userEvent.setup();
    // Every enabled provider has a tab, connected or not.
    const nav = screen.getByRole('navigation', { name: 'Subscriptions' });
    expect(
      within(nav)
        .getByRole('link', { name: /GitHub Copilot/ })
        .getAttribute('aria-current'),
    ).toBe('page');
    expect(within(nav).getByRole('img', { name: 'Connected' })).toBeTruthy();
    await user.click(within(nav).getByRole('link', { name: 'Mistral Vibe' }));
    expect(screen.getByTestId('where').textContent).toBe('/subscriptions/mistral');
    // The other provider's panel is gone, not stacked below.
    expect(screen.queryByText(/Copilot Free/)).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Connect' }));
    expect(await screen.findByText('Create a Vibe key.')).toBeTruthy();
    const input = document.querySelector('input[type="password"]') as HTMLInputElement;
    await user.type(input, '  bad-key  ');
    const dialogConnect = screen.getAllByRole('button', { name: 'Connect' });
    await user.click(dialogConnect[dialogConnect.length - 1]!);
    expect(mocked.post).toHaveBeenCalledWith('/api/v1/me/subscriptions/mistral/key', { key: 'bad-key' });
    expect(await screen.findByRole('alert')).toBeTruthy();
    expect(screen.getByText('Mistral does not accept this key.')).toBeTruthy();
  });
});

describe('SubscriptionsPage tabs', () => {
  const provider = (id: string, name: string, selected: string[] | null) => ({
    id,
    name,
    description: `${name} description`,
    enabled: true,
    auth: 'device',
    connection:
      selected === null
        ? null
        : {
            id: `conn-${id}`,
            provider: id,
            account_email: 'me@x',
            account_name: '',
            models: ['m-a', 'm-b', 'm-c'],
            selected_models: selected,
            status: 'active',
            last_error: '',
            last_used_at: '',
            created_at: '',
            updated_at: '',
            auto_renews: true,
          },
  });

  it('opens the provider named in the URL and redirects an unknown one', async () => {
    mocked.get.mockResolvedValue({
      available: true,
      model_prefix: 'my/',
      providers: [provider('xai', 'xAI Grok (SuperGrok / X Premium+)', ['m-a']), provider('openai', 'OpenAI', ['m-b'])],
    });
    wrapPage('/subscriptions/openai');
    // Tabs use the short name; the panel heading keeps the full one.
    expect((await screen.findByRole('link', { name: /xAI Grok$/ })).textContent).not.toContain('SuperGrok');
    expect(await screen.findByRole('button', { name: 'Copy my/openai/m-b' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Copy my/xai/m-a' })).toBeNull();
    cleanup();
    wrapPage('/subscriptions/nope');
    expect(await screen.findByRole('button', { name: 'Copy my/xai/m-a' })).toBeTruthy();
    expect(screen.getByTestId('where').textContent).toBe('/subscriptions/xai');
  });

  it('shows a Connect panel with no dot for a provider that is not connected', async () => {
    mocked.get.mockResolvedValue({
      available: true,
      model_prefix: 'my/',
      providers: [provider('xai', 'xAI Grok', null)],
    });
    wrapPage();
    expect(await screen.findByText('xAI Grok description')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Connect' })).toBeTruthy();
    expect(screen.queryByRole('img', { name: 'Connected' })).toBeNull();
  });

  it('removes a model at once and Undo restores the previous selection in order', async () => {
    mocked.get.mockResolvedValue({
      available: true,
      model_prefix: 'my/',
      providers: [provider('xai', 'xAI Grok', ['m-a', 'm-b'])],
    });
    mocked.put.mockResolvedValue({});
    wrapPage();
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Remove m-a' }));
    expect(mocked.put).toHaveBeenLastCalledWith('/api/v1/me/subscriptions/conn-xai/models', { selected: ['m-b'] });
    expect(await screen.findByText('Removed m-a')).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Undo' }));
    expect(mocked.put).toHaveBeenLastCalledWith('/api/v1/me/subscriptions/conn-xai/models', { selected: ['m-a', 'm-b'] });
    await waitFor(() => expect(screen.queryByText('Removed m-a')).toBeNull());
  });

  it('reports a failed remove instead of offering Undo', async () => {
    mocked.get.mockResolvedValue({
      available: true,
      model_prefix: 'my/',
      providers: [provider('xai', 'xAI Grok', ['m-a'])],
    });
    mocked.put.mockRejectedValue(new Error('Saving failed.'));
    wrapPage();
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Remove m-a' }));
    expect(await screen.findByText('Saving failed.')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Undo' })).toBeNull();
  });
});

describe('PersonalSubscriptionsCard', () => {
  it('renders nothing on an offline install', async () => {
    mocked.get.mockResolvedValue({ offline: true, enabled: false, providers: [], connections: [] });
    const { container } = wrap(<PersonalSubscriptionsCard />);
    await waitFor(() => expect(mocked.get).toHaveBeenCalledWith('/api/v1/admin/subscriptions'));
    await waitFor(() => expect(container.querySelector('#personal-subscriptions')).toBeNull());
  });

  it('shows vendor switches with their terms note only while the feature is on', async () => {
    const providers = [
      { id: 'xai', name: 'xAI Grok', description: '', admin_note: 'xAI allowlists clients.', enabled: true, connections: 1 },
      { id: 'openai', name: 'OpenAI ChatGPT', description: '', admin_note: 'Codex terms.', enabled: false, connections: 0 },
    ];
    mocked.get.mockResolvedValue({ offline: false, enabled: false, providers, connections: [] });
    wrap(<PersonalSubscriptionsCard />);
    expect(await screen.findByRole('heading', { name: 'Personal subscriptions' })).toBeTruthy();
    expect(screen.queryByRole('checkbox', { name: 'xAI Grok' })).toBeNull();
    cleanup();

    mocked.get.mockResolvedValue({ offline: false, enabled: true, providers, connections: [] });
    mocked.put.mockResolvedValue({});
    wrap(<PersonalSubscriptionsCard />);
    const openai = (await screen.findByRole('checkbox', { name: 'OpenAI ChatGPT' })) as HTMLInputElement;
    expect(openai.checked).toBe(false);
    expect(screen.getByText(/Codex terms\./)).toBeTruthy();
    await userEvent.setup().click(openai);
    expect(mocked.put).toHaveBeenCalledWith('/api/v1/admin/subscriptions/providers', { enabled: { openai: true } });
  });
});
