/**
 * Model catalog.
 *
 * A card carries what a user chooses on (name, provider, what it does, price,
 * context, speed) plus a health dot; a model whose upstream is down is styled
 * as such (data-health="down" drives the tinted background) with an explicit
 * alert line. Live figures (error rate, probe latency, access) are in the
 * detail pop-up and the table view.
 */
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../lib/api';
import { modelHealth, isModelDown, type ManagedModel, type Me, type Model } from '../lib/types';
import { formatErrorRate, healthTone } from '../lib/format';
import { ToastProvider } from '../components/ui';
import { ModelsPage } from './Models';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const me: Me = {
  id: 'user-1',
  email: 'ada@example.com',
  name: 'Ada',
  role: 'user',
  is_active: true,
  timezone: 'UTC',
  locale: 'en',
  groups: [],
  teams: [],
  leads_teams: [],
  active_sessions: 1,
  feature_flags: {},
  endpoint: 'http://localhost:8080/v1',
};

vi.mock('../app/session', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../app/session')>();
  return {
    ...actual,
    useLocalOnly: () => false,
    useMe: () => me,
  };
});

const mocked = vi.mocked(api);

function modelFixture(overrides: Partial<Model> = {}): Model {
  return {
    id: 'm-1',
    upstream_id: 'up-1',
    upstream_name: 'Anthropic',
    adapter_type: 'anthropic',
    name: 'claude-fable-5',
    display_name: '',
    status: 'enabled',
    modalities: ['chat'],
    context_window: 200_000,
    rate_in_nanousd: 10_000_000_000,
    rate_out_nanousd: 30_000_000_000,
    rate_cached_nanousd: 0,
    rate_cache_write_5m_nanousd: 0,
    rate_cache_write_1h_nanousd: 0,
    rate_effective_from: '0001-01-01T00:00:00Z',
    discovered_at: '2026-08-01T00:00:00Z',
    grant_count: 1,
    grant_source: 'direct',
    request_count_10m: 0,
    error_count_10m: 0,
    error_rate_percent: 0,
    upstream_reachable: true,
    upstream_last_check_at: new Date().toISOString(),
    upstream_last_error: '',
    upstream_last_latency_ms: 42,
    health: 'healthy',
    ...overrides,
  };
}

const healthy = modelFixture();
const degraded = modelFixture({
  id: 'm-2',
  name: 'gpt-fable',
  upstream_id: 'up-2',
  upstream_name: 'OpenAI',
  adapter_type: 'openai_compatible',
  request_count_10m: 40,
  error_count_10m: 10,
  error_rate_percent: 25,
  health: 'degraded',
});
const down = modelFixture({
  id: 'm-3',
  name: 'llama-local',
  upstream_id: 'up-3',
  upstream_name: 'Ollama',
  adapter_type: 'ollama',
  request_count_10m: 3,
  error_count_10m: 3,
  error_rate_percent: 100,
  upstream_reachable: false,
  upstream_last_error: 'dial tcp 10.0.0.5:11434: connection refused',
  upstream_last_latency_ms: 0,
  health: 'down',
});

function renderAt(initialEntry = '/models') {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[initialEntry]}>
          <ModelsPage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocked.get.mockResolvedValue({ models: [healthy, degraded, down], managed_models: [] });
});

describe('model health helpers', () => {
  it('prefers the gateway verdict and derives the same one without it', () => {
    expect(modelHealth(healthy)).toBe('healthy');
    expect(modelHealth({ ...healthy, health: undefined })).toBe('healthy');
    expect(modelHealth({ ...degraded, health: undefined })).toBe('degraded');
    expect(modelHealth({ ...down, health: undefined })).toBe('down');
    // Two failures out of two is not yet a verdict of "down".
    expect(
      modelHealth({ ...healthy, health: undefined, request_count_10m: 2, error_count_10m: 2, error_rate_percent: 100 }),
    ).toBe('degraded');
    // Never probed, no traffic: nothing to judge.
    expect(
      modelHealth({
        ...healthy,
        health: undefined,
        upstream_reachable: false,
        upstream_last_check_at: '0001-01-01T00:00:00Z',
        upstream_last_latency_ms: 0,
      }),
    ).toBe('unknown');
    // A gateway that sends no health block at all.
    const bare = { ...healthy } as Partial<Model>;
    for (const key of [
      'health',
      'upstream_reachable',
      'request_count_10m',
      'error_count_10m',
      'error_rate_percent',
      'upstream_last_check_at',
      'upstream_last_error',
      'upstream_last_latency_ms',
    ] as const) {
      delete bare[key];
    }
    expect(modelHealth(bare as Model)).toBe('unknown');
    expect(isModelDown(down)).toBe(true);
    expect(isModelDown(degraded)).toBe(false);
  });

  it('formats error rates with at most one decimal and clamps', () => {
    expect(formatErrorRate(0)).toBe('0%');
    expect(formatErrorRate(2.5)).toBe('2.5%');
    expect(formatErrorRate(33.333)).toBe('33.3%');
    expect(formatErrorRate(100)).toBe('100%');
    expect(formatErrorRate(140)).toBe('100%');
    expect(formatErrorRate(undefined)).toBe('—');
  });

  it('maps verdicts to the status tones the admin Upstreams page uses', () => {
    expect(healthTone('healthy')).toBe('success');
    expect(healthTone('degraded')).toBe('warning');
    expect(healthTone('down')).toBe('danger');
    expect(healthTone('unknown')).toBe('neutral');
  });
});

describe('Models page — cards', () => {
  // Unset pricing uses the literal '-not set-' label.
  it('distinguishes unknown pricing from explicit free prices in cards and detail', async () => {
    const unknown = {
      value: null,
      source: 'unknown' as const,
      override: null,
      automatic_value: null,
      automatic_source: 'unknown' as const,
    };
    const free = {
      value: 0,
      source: 'upstream' as const,
      override: null,
      automatic_value: 0,
      automatic_source: 'upstream' as const,
    };
    mocked.get.mockResolvedValue({
      models: [
        modelFixture({
          rate_in_nanousd: 0,
          rate_out_nanousd: 0,
          metadata: { rate_in_nanousd: unknown, rate_out_nanousd: free, rate_cached_nanousd: unknown },
        }),
      ],
      managed_models: [],
    });
    renderAt();
    const card = await screen.findByRole('button', { name: /claude-fable-5/ });
    // One "in / out" value on the card: the unknown side keeps the literal.
    expect(within(card).getByTitle(/^Input: -not set- · Output: \$0\.00 \/ Mtok$/).textContent).toBe('-not set- / $0');
    expect(within(card).getByText('per 1M tokens')).toBeTruthy();
    fireEvent.click(card);
    const drawer = screen.getByRole('dialog');
    expect(within(drawer).getAllByText('-not set-')).toHaveLength(2);
    expect(within(drawer).getByText(/\$0.*Mtok/)).toBeTruthy();
  });

  it('marks each card with a health dot and names trouble in words', async () => {
    renderAt();
    const healthyCard = (await screen.findByRole('button', { name: /claude-fable-5/ })) as HTMLElement;
    expect(healthyCard.dataset.health).toBe('healthy');
    // A healthy model is a green dot whose name carries the probe latency;
    // no badge, no stat boxes.
    expect(within(healthyCard).getByRole('img', { name: 'Healthy · 42 ms' })).toBeTruthy();
    expect(within(healthyCard).queryByText(/Healthy/)).toBeNull();
    expect(within(healthyCard).queryByText('Errors (10 min)')).toBeNull();

    const degradedCard = screen.getByRole('button', { name: /gpt-fable/ }) as HTMLElement;
    expect(degradedCard.dataset.health).toBe('degraded');
    expect(within(degradedCard).getByText('Degraded · 25%')).toBeTruthy();
  });

  it('keeps the live figures one click away in the detail pop-up', async () => {
    renderAt();
    fireEvent.click(await screen.findByRole('button', { name: /claude-fable-5/ }));
    const dialog = await screen.findByRole('dialog');
    // No traffic: the rate is a sentence, never a misleading "0%".
    expect(within(dialog).getByText('No requests in the last 10 minutes')).toBeTruthy();
    expect(within(dialog).getByText('42 ms')).toBeTruthy();
    expect(within(dialog).getByText(/Upstream checked/)).toBeTruthy();
    expect(within(dialog).getByText('anthropic')).toBeTruthy();
    expect(within(dialog).getByText('a grant to you directly')).toBeTruthy();
  });

  it('styles a down model as down and says why', async () => {
    renderAt();
    const downCard = (await screen.findByRole('button', { name: /llama-local/ })) as HTMLElement;
    expect(downCard.dataset.health).toBe('down');
    expect(downCard.className).toContain('model-card');
    expect(within(downCard).getByText('Down')).toBeTruthy();
    const alert = within(downCard).getByRole('status');
    expect(alert.textContent).toContain('This model is not responding right now');
    expect(alert.textContent).toContain('connection refused');
    // The healthy card carries no alert.
    const healthyCard = screen.getByRole('button', { name: /claude-fable-5/ }) as HTMLElement;
    expect(within(healthyCard).queryByRole('status')).toBeNull();
    fireEvent.click(downCard);
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Upstream unreachable')).toBeTruthy();
    expect(within(dialog).getByText('3 of 3 requests failed in the last 10 minutes')).toBeTruthy();
  });

  it('leads with what a user chooses on: provider, what it does, price, context', async () => {
    renderAt();
    const card = (await screen.findByRole('button', { name: /claude-fable-5/ })) as HTMLElement;
    expect(within(card).getByRole('heading', { name: 'claude-fable-5' })).toBeTruthy();
    expect(within(card).getByText('Anthropic')).toBeTruthy();
    expect(within(card).getByText('Chat')).toBeTruthy();
    expect(within(card).getByText('Price in / out')).toBeTruthy();
    expect(within(card).getByText('$10 / $30')).toBeTruthy();
    expect(within(card).getByTitle('200K tokens').textContent).toBe('200K');
    // The adapter type is not a user choice: it lives in the pop-up.
    expect(within(card).queryByText('anthropic')).toBeNull();
  });

  it('caps capability chips and names the rest in a tooltip', async () => {
    const yes = { value: true, source: 'provider' as const, at: '2026-11-01T00:00:00Z' };
    mocked.get.mockResolvedValue({
      models: [
        modelFixture({
          capabilities: {
            'input.image': yes,
            'input.file': yes,
            'request.tools': yes,
            'request.structured_output': yes,
            'thinking.supported': yes,
          } as unknown as Model['capabilities'],
        }),
      ],
      managed_models: [],
    });
    renderAt();
    const card = (await screen.findByRole('button', { name: /claude-fable-5/ })) as HTMLElement;
    expect(within(card).getByText('Sees images')).toBeTruthy();
    expect(within(card).queryByText('JSON schema')).toBeNull();
    expect(within(card).getByTitle('JSON schema, Thinking').textContent).toBe('+2');
  });

  it('sorts by price with unpriced models last, and by speed', async () => {
    const cheap = modelFixture({
      id: 'm-c',
      name: 'cheap-one',
      rate_in_nanousd: 1_000_000_000,
      rate_effective_from: '2026-01-01T00:00:00Z',
    });
    const unpriced = modelFixture({ id: 'm-u', name: 'a-unpriced', rate_in_nanousd: 0, rate_out_nanousd: 0 });
    const fast = modelFixture({
      id: 'm-f',
      name: 'zz-fast',
      activity: {
        jobs_per_day_7d: 1,
        jobs_per_day_prev_7d: 1,
        jobs_trend: 'flat',
        jobs_change_percent: 0,
        tokens_per_second_7d: 90,
        speed_samples_7d: 5,
      },
    });
    mocked.get.mockResolvedValue({ models: [healthy, cheap, unpriced, fast], managed_models: [] });
    const names = () => screen.getAllByRole('heading', { level: 3 }).map((h) => h.textContent);
    renderAt('/models?sort=cheap');
    await screen.findByRole('button', { name: 'cheap-one' });
    expect(names()).toEqual(['cheap-one', 'claude-fable-5', 'zz-fast', 'a-unpriced']);
    cleanup();
    renderAt('/models?sort=speed');
    await screen.findByRole('button', { name: 'zz-fast' });
    expect(names()[0]).toBe('zz-fast');
    expect(screen.queryByRole('heading', { level: 2, name: /Managed models/ })).toBeNull();
  });
});

describe('Models page — managed model cards', () => {
  const managed: ManagedModel = {
    id: 'mm-1',
    name: 'qwen-fast',
    description: 'Fast default for chat',
    status: 'enabled',
    target_model_id: 'm-9',
    target_model_name: 'qwen-3.8-27b-dragon',
    target_public_name: 'qwen-3.8-27b-dragon',
    target_status: 'enabled',
    target_upstream_id: 'up-9',
    target_upstream_name: 'Dragon',
    modalities: ['chat'],
    context_window: 262_144,
    servable: true,
    broken: false,
    fallback_model_id: '',
    fallback_triggers: [],
    fallback_broken: false,
    grant_count: 1,
    created_by_user_id: 'admin-1',
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    grant_source: 'all users',
  };

  it('keeps alias-only results and ordering when switching cards and table, including alias-only upstreams', async () => {
    mocked.get.mockResolvedValue({ models: [healthy], managed_models: [managed] });
    renderAt('/models?q=qwen-fast&upstream=Dragon');
    expect(await screen.findByRole('button', { name: 'qwen-fast' })).toBeTruthy();
    expect(screen.getByRole('option', { name: 'Dragon' })).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: 'Table' }));
    expect(await screen.findByRole('cell', { name: /qwen-fast/ })).toBeTruthy();
    expect(screen.getByRole('cell', { name: /Managed alias/ })).toBeTruthy();
    expect(screen.getByText('qwen-3.8-27b-dragon')).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: 'Cards' }));
    expect(await screen.findByRole('button', { name: 'qwen-fast' })).toBeTruthy();
  });

  it('renders the alias with the same card silhouette, in its own section', async () => {
    mocked.get.mockResolvedValue({ models: [healthy], managed_models: [managed] });
    renderAt();
    const card = (await screen.findByRole('button', { name: 'qwen-fast' })) as HTMLElement;
    expect(card.className).toContain('model-card');
    expect(card.dataset.managed).toBe('true');
    expect(card.dataset.health).toBeUndefined();
    expect(card.querySelector('.model-card-facts')).toBeTruthy();
    expect(within(card).getByText('Uses qwen-3.8-27b-dragon')).toBeTruthy();
    expect(within(card).getByRole('img', { name: 'Serving' })).toBeTruthy();
    expect(within(card).getByTitle('262,144 tokens').textContent).toBe('262K');
    expect(within(card).getByText('Chat')).toBeTruthy();
    // The target is not in this user's catalog, so its price is not guessed.
    expect(within(card).getByText('Target model’s rates')).toBeTruthy();
    expect(within(card).queryByRole('status')).toBeNull();
    // Both kinds present: two labeled sections, managed first.
    const sections = screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent);
    expect(sections).toEqual(['Managed models 1', 'Models 1']);
  });

  it("prices an alias at its target's rates when the target is in the catalog", async () => {
    const target = modelFixture({ id: 'm-9', name: 'qwen-3.8-27b-dragon', upstream_name: 'Dragon' });
    mocked.get.mockResolvedValue({ models: [target], managed_models: [managed] });
    renderAt();
    const card = (await screen.findByRole('button', { name: 'qwen-fast' })) as HTMLElement;
    expect(within(card).getByText('$10 / $30')).toBeTruthy();
  });

  it('says Free for an explicit $0 both ways, and Not priced when nothing is set', async () => {
    const free = modelFixture({
      id: 'm-free',
      name: 'local-free',
      rate_in_nanousd: 0,
      rate_out_nanousd: 0,
      rate_effective_from: '2026-01-01T00:00:00Z',
    });
    const unset = modelFixture({ id: 'm-unset', name: 'local-unset', rate_in_nanousd: 0, rate_out_nanousd: 0 });
    mocked.get.mockResolvedValue({ models: [free, unset], managed_models: [] });
    renderAt();
    expect(within(await screen.findByRole('button', { name: 'local-free' })).getByText('Free')).toBeTruthy();
    expect(within(screen.getByRole('button', { name: 'local-unset' })).getByText('Not priced')).toBeTruthy();
  });

  it('opens a detail pop-up for an alias with everything the old card carried', async () => {
    mocked.get.mockResolvedValue({ models: [healthy], managed_models: [managed] });
    renderAt();
    fireEvent.click(await screen.findByRole('button', { name: 'qwen-fast' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Fast default for chat')).toBeTruthy();
    expect(within(dialog).getByText(/Your administrator keeps this name/)).toBeTruthy();
    expect(within(dialog).getByText('Currently uses')).toBeTruthy();
    expect(within(dialog).getByText('Dragon')).toBeTruthy();
    expect(within(dialog).getByText('262,144 tokens')).toBeTruthy();
    expect(within(dialog).getByText('Serving')).toBeTruthy();
    expect(within(dialog).getByText('an all-users grant')).toBeTruthy();
    expect(dialog.textContent).toContain('"model": "qwen-fast"');
  });

  it('flags a broken alias the way a down model is flagged', async () => {
    mocked.get.mockResolvedValue({
      models: [healthy],
      managed_models: [{ ...managed, servable: false, broken: true, broken_reason: 'target disabled' }],
    });
    renderAt();
    const card = (await screen.findByRole('button', { name: 'qwen-fast' })) as HTMLElement;
    expect(card.dataset.health).toBe('down');
    const alert = within(card).getByRole('status');
    expect(alert.textContent).toContain('cannot serve requests right now');
    expect(alert.textContent).toContain('target disabled');
    expect(within(card).getByText('Unavailable')).toBeTruthy();
  });
});

describe('Models page — modality filter', () => {
  const imageModel = modelFixture({
    id: 'm-img',
    name: 'grok-2-image',
    upstream_id: 'up-4',
    upstream_name: 'xAI',
    adapter_type: 'openai_compatible',
    modalities: ['image'],
  });
  const videoModel = modelFixture({
    id: 'm-vid',
    name: 'sora-2',
    upstream_id: 'up-2',
    upstream_name: 'OpenAI',
    adapter_type: 'openai_compatible',
    modalities: ['video'],
  });

  it('offers only the modalities detected on this gateway', async () => {
    mocked.get.mockResolvedValue({ models: [healthy, imageModel, videoModel], managed_models: [] });
    renderAt();
    await screen.findByRole('button', { name: /claude-fable-5/ });
    const select = screen.getByRole('combobox', { name: 'Modality' }) as HTMLSelectElement;
    const labels = [...select.options].map((option) => option.textContent);
    expect(labels).toEqual(['All modalities', 'Chat', 'Image', 'Video']);
    // No TTS/embedding model exists here, so those kinds are not offered.
    expect(labels).not.toContain('Tts');
    expect(labels).not.toContain('Embedding');
  });

  it('limits the catalog to the chosen modality (cards and table)', async () => {
    mocked.get.mockResolvedValue({ models: [healthy, imageModel, videoModel], managed_models: [] });
    renderAt('/models?modality=image');
    expect(await screen.findByRole('button', { name: /grok-2-image/ })).toBeTruthy();
    expect(screen.queryByRole('button', { name: /claude-fable-5/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /sora-2/ })).toBeNull();
    expect((screen.getByRole('combobox', { name: 'Modality' }) as HTMLSelectElement).value).toBe('image');

    // Switching the dropdown re-filters live.
    fireEvent.change(screen.getByRole('combobox', { name: 'Modality' }), { target: { value: 'video' } });
    expect(await screen.findByRole('button', { name: /sora-2/ })).toBeTruthy();
    expect(screen.queryByRole('button', { name: /grok-2-image/ })).toBeNull();

    cleanup();
    renderAt('/models?modality=video&view=table');
    expect(await screen.findByText('sora-2')).toBeTruthy();
    expect(screen.queryByText('claude-fable-5')).toBeNull();
    expect(screen.queryByText('grok-2-image')).toBeNull();
  });

  it('includes managed aliases in the detected set and filters them too', async () => {
    const alias: ManagedModel = {
      id: 'mm-img',
      name: 'pictures',
      description: 'Image alias',
      status: 'enabled',
      target_model_id: 'm-img',
      target_model_name: 'grok-2-image',
      target_public_name: 'grok-2-image',
      target_status: 'enabled',
      target_upstream_id: 'up-4',
      target_upstream_name: 'xAI',
      modalities: ['image'],
      context_window: 0,
      servable: true,
      broken: false,
      fallback_model_id: '',
      fallback_triggers: [],
      fallback_broken: false,
      grant_count: 1,
      created_by_user_id: 'admin-1',
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
      grant_source: 'all users',
    };
    mocked.get.mockResolvedValue({ models: [healthy], managed_models: [alias] });
    renderAt('/models?modality=image');
    expect(await screen.findByRole('button', { name: 'pictures' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: /claude-fable-5/ })).toBeNull();
    const select = screen.getByRole('combobox', { name: 'Modality' }) as HTMLSelectElement;
    expect([...select.options].map((option) => option.textContent)).toEqual(['All modalities', 'Chat', 'Image']);
  });
});

describe('Models page — table and drawer', () => {
  it('adds health and error-rate columns to the table view', async () => {
    renderAt('/models?view=table');
    expect(await screen.findByRole('columnheader', { name: 'Health' })).toBeTruthy();
    expect(screen.getByRole('columnheader', { name: 'Errors (10 min)' })).toBeTruthy();
    const downRow = screen.getByText('llama-local').closest('tr') as HTMLElement;
    expect(downRow.dataset.health).toBe('down');
    expect(within(downRow).getByText('Down')).toBeTruthy();
    expect(within(downRow).getByText('100%')).toBeTruthy();
  });

  it('shows the health block in the detail drawer', async () => {
    renderAt();
    fireEvent.click(await screen.findByRole('button', { name: /gpt-fable/ }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Health')).toBeTruthy();
    expect(within(dialog).getByText(/Degraded/)).toBeTruthy();
    expect(within(dialog).getByText('10 of 40 requests failed in the last 10 minutes')).toBeTruthy();
    expect(within(dialog).getByText('Upstream latency')).toBeTruthy();
    expect(within(dialog).getByText('42 ms')).toBeTruthy();
  });
});
