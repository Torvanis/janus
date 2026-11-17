import { beforeEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import {
  capabilityBadges,
  capabilityValueText,
  knownCapabilities,
  probeSummary,
  sortEfforts,
  type ModelCapabilityView,
} from '../../lib/capabilities';
import { CapabilityProbeControls } from './CapabilityProbeControls';
import type { Model } from '../../lib/types';
import { ModelCapabilities } from './ModelCapabilities';

vi.mock('../../lib/api', async (original) => ({
  ...(await original<typeof import('../../lib/api')>()),
  api: { get: vi.fn(), put: vi.fn(), post: vi.fn() },
}));

const at = '2026-11-12T10:00:00Z';
const view: ModelCapabilityView = {
  model_id: 'm',
  keys: [
    { key: 'input.image', kind: 'bool' },
    { key: 'request.tools', kind: 'bool' },
    { key: 'thinking.supported', kind: 'bool' },
    { key: 'thinking.off', kind: 'string', enum: ['reasoning_effort_none', 'never'] },
    { key: 'thinking.levels', kind: 'list' },
    { key: 'limits.max_output_tokens', kind: 'int' },
  ],
  resolved: {
    'input.image': { value: false, source: 'provider', at },
    'request.tools': { value: true, source: 'probe', at, note: 'the model called the tool' },
    'thinking.levels': { value: ['low', 'high'], source: 'admin', at },
  },
  provider: { 'input.image': { value: false, at }, 'thinking.levels': { value: ['low', 'medium', 'high'], at } },
  probe: { 'request.tools': { value: true, at, note: 'the model called the tool' } },
  admin: { 'thinking.levels': { value: ['low', 'high'], at } },
  probed: { at, status: 'ok', requests: 6, input_tokens: 120, output_tokens: 40, by: 'enabled' },
};

function renderIt() {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}>
      <ToastProvider>
        <ModelCapabilities model={{ id: 'm', name: 'native', modalities: ['chat'] } as Model} />
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.mocked(api.get).mockReset().mockResolvedValue(view);
  vi.mocked(api.put).mockReset().mockResolvedValue(view);
  vi.mocked(api.post).mockReset();
});

/** One capability's row: its value, where the value came from and how it was measured. */
function shown(name: string) {
  const row = screen.getByRole('rowheader', { name }).closest('[data-cap]')!;
  return {
    value: row.querySelector('.cap-value-text')!.textContent,
    source: Array.from(row.querySelectorAll('.cap-source')).map((e) => e.textContent),
    note: row.querySelector('.cap-note')?.textContent ?? null,
    row,
  };
}
const change = (name: string) => fireEvent.click(screen.getByRole('button', { name: `Change ${name}` }));

it('shows each value with where it came from, and unknown as unknown', async () => {
  renderIt();
  await screen.findByRole('rowheader', { name: 'Images' });
  expect(shown('Images')).toMatchObject({ value: 'No', source: ['From the provider'], note: null });
  expect(shown('Function tools')).toMatchObject({
    value: 'Yes',
    source: ['Measured by Janus'],
    note: 'How: the model called the tool',
  });
  expect(shown('Thinking levels accepted')).toMatchObject({ value: 'low, high', source: ['Set by an administrator'] });
  // Nothing said it: not shown as No, and kept out of the way with the other unknowns of its group.
  const unknown = shown('Can think before answering');
  expect(unknown).toMatchObject({ value: 'Not known', source: [], note: null });
  const fold = unknown.row.closest('details')!;
  expect(fold.open).toBe(false);
  expect(fold.querySelector('summary')!.textContent).toBe('Not known (2)Can think before answering, How thinking is turned off');
  // A known value is never folded away.
  expect(shown('Images').row.closest('details')).toBeNull();
  expect(screen.getByRole('status').textContent).toContain('when it was enabled: 6 requests, 160 tokens.');
  expect(screen.getByRole('button', { name: 'Measure again' })).toBeTruthy();
  expect(api.get).toHaveBeenCalledWith('/api/v1/admin/models/m/capabilities');
  // No editing control is on screen until a row is opened: one action per row.
  expect(screen.queryByLabelText('Images')).toBeNull();
  expect(screen.getAllByRole('button', { name: /^Change / })).toHaveLength(6);
});

it('saves only what was changed: a value to override, null to go back to automatic', async () => {
  renderIt();
  await screen.findByRole('rowheader', { name: 'Images' });
  // Nothing to save yet, so nothing offers to.
  expect(screen.queryByRole('button', { name: 'Save capabilities' })).toBeNull();
  change('Images');
  fireEvent.change(screen.getByLabelText('Images'), { target: { value: 'true' } });
  expect(shown('Images')).toMatchObject({ value: 'Yes', source: ['Set by an administrator', 'Not saved yet'] });
  change('How thinking is turned off');
  fireEvent.change(screen.getByLabelText('How thinking is turned off'), { target: { value: 'never' } });
  // Once it is being changed, a row that was not known moves up with the others.
  expect(shown('How thinking is turned off').row.closest('details')).toBeNull();
  change('Most output tokens');
  fireEvent.change(screen.getByLabelText('Most output tokens'), { target: { value: '8192' } });
  // Returning an existing override to automatic sends null; the row previews the automatic value.
  change('Thinking levels accepted');
  fireEvent.click(screen.getByRole('button', { name: 'Use the automatic value for Thinking levels accepted' }));
  expect(shown('Thinking levels accepted')).toMatchObject({
    value: 'low, medium, high',
    source: ['From the provider', 'Not saved yet'],
  });
  expect(screen.getByText('4 changes not saved')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Save capabilities' }));
  await waitFor(() => expect(api.put).toHaveBeenCalledTimes(1));
  expect(api.put).toHaveBeenCalledWith('/api/v1/admin/models/m/capabilities', {
    overrides: { 'input.image': true, 'thinking.off': 'never', 'limits.max_output_tokens': 8192, 'thinking.levels': null },
  });
  // Saved: the editors close and the bar goes.
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Save capabilities' })).toBeNull());
  expect(screen.queryByLabelText('Images')).toBeNull();
});

it('closes an opened row without a change, and discards changes', async () => {
  renderIt();
  await screen.findByRole('rowheader', { name: 'Images' });
  change('Images');
  fireEvent.click(screen.getByRole('button', { name: 'Close' }));
  expect(screen.queryByLabelText('Images')).toBeNull();
  expect(screen.queryByRole('button', { name: 'Save capabilities' })).toBeNull();
  change('Images');
  fireEvent.change(screen.getByLabelText('Images'), { target: { value: 'true' } });
  expect(screen.getByText('1 change not saved')).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
  expect(shown('Images')).toMatchObject({ value: 'No', source: ['From the provider'] });
  expect(screen.queryByRole('button', { name: 'Save capabilities' })).toBeNull();
  expect(api.put).not.toHaveBeenCalled();
});

it('takes a typed list, with none for an empty one, and shows a refused save', async () => {
  vi.mocked(api.put).mockRejectedValue(new Error('thinking.levels has too many values'));
  renderIt();
  await screen.findByRole('rowheader', { name: 'Images' });
  change('Thinking levels accepted');
  fireEvent.change(screen.getByLabelText('Thinking levels accepted'), { target: { value: ' Low, HIGH ,' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save capabilities' }));
  await waitFor(() =>
    expect(api.put).toHaveBeenCalledWith(expect.anything(), { overrides: { 'thinking.levels': ['low', 'high'] } }),
  );
  expect((await screen.findByRole('alert')).textContent).toContain('too many values');
  fireEvent.change(screen.getByLabelText('Thinking levels accepted'), { target: { value: 'none' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save capabilities' }));
  await waitFor(() => expect(api.put).toHaveBeenLastCalledWith(expect.anything(), { overrides: { 'thinking.levels': [] } }));
});

it('says so when the server has no capabilities to give', async () => {
  vi.mocked(api.get).mockResolvedValue({ models: [] });
  renderIt();
  expect(await screen.findByText('Capabilities could not be loaded.')).toBeTruthy();
});

it('badges only what is known to be true', () => {
  expect(capabilityBadges(undefined)).toEqual([]);
  expect(
    capabilityBadges({
      'input.image': { value: true, source: 'provider', at },
      'input.audio': { value: false, source: 'provider', at },
      'request.tools': { value: true, source: 'probe', at },
      'thinking.supported': { value: true, source: 'provider', at },
      'thinking.levels': { value: ['high', 'none', 'low', 'medium'], source: 'provider', at },
    }),
  ).toEqual(['Sees images', 'Tools', 'Thinking: low–high']);
  expect(capabilityBadges({ 'thinking.supported': { value: true, source: 'probe', at } })).toEqual(['Thinking']);
  expect(sortEfforts(['max', 'low', 'custom', 'none'])).toEqual(['none', 'low', 'max', 'custom']);
  expect([
    capabilityValueText(undefined),
    capabilityValueText([]),
    capabilityValueText(64000),
    capabilityValueText('never'),
  ]).toEqual(['Not known', 'None', '64,000', 'Cannot be turned off']);
});

it('starts a measurement and follows it until it is done', async () => {
  const never: ModelCapabilityView = { ...view, probe: {}, probed: null };
  const running: ModelCapabilityView = {
    ...never,
    probed: { at, status: 'running', requests: 0, input_tokens: 0, output_tokens: 0, by: 'manual' },
  };
  // Read once by the section and once more when the button mounts; after that it is polled.
  vi.mocked(api.get).mockResolvedValueOnce(never).mockResolvedValueOnce(never).mockResolvedValue(view);
  vi.mocked(api.post).mockResolvedValue(running);
  renderIt();
  expect((await screen.findByRole('status')).textContent).toBe('Not measured yet.');
  fireEvent.click(screen.getByRole('button', { name: 'Measure now' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('/api/v1/admin/models/m/capabilities/probe'));
  await waitFor(() => expect(screen.getByRole('status').textContent).toContain('Measuring now'));
  expect((screen.getByRole('button', { name: 'Measuring…' }) as HTMLButtonElement).disabled).toBe(true);
  // The view is read again on its own until the run is over.
  await waitFor(() => expect(screen.getByRole('status').textContent).toContain('6 requests, 160 tokens'), { timeout: 8000 });
  expect(screen.getByRole('button', { name: 'Measure again' })).toBeTruthy();
}, 12000);

it('offers no measurement for a model that is not a chat model', async () => {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ToastProvider>
        <ModelCapabilities model={{ id: 'm', name: 'painter', modalities: ['image'] } as Model} />
      </ToastProvider>
    </QueryClientProvider>,
  );
  expect(await screen.findByText(/Only chat models are measured/)).toBeTruthy();
  expect(screen.queryByRole('button', { name: /Measure/ })).toBeNull();
});

it('says what happened in the last measurement', () => {
  const run = { at, requests: 9, input_tokens: 200, output_tokens: 1050, by: 'manual' };
  expect(probeSummary(null, '')).toBe('Not measured yet.');
  expect(probeSummary({ ...run, status: 'ok' }, '2 minutes ago')).toBe(
    'Measured 2 minutes ago on request: 9 requests, 1,250 tokens.',
  );
  expect(probeSummary({ ...run, status: 'failed', detail: 'a plain request was refused (HTTP 401: bad key)' }, 'just now')).toBe(
    'Could not be measured just now: a plain request was refused (HTTP 401: bad key).',
  );
  expect(probeSummary({ ...run, status: 'queued' }, '')).toBe('Waiting its turn to be measured.');
  expect(knownCapabilities(view.resolved)).toEqual([
    { group: 'Takes in', items: [{ key: 'input.image', label: 'Images', value: 'No' }] },
    { group: 'Thinking', items: [{ key: 'thinking.levels', label: 'Thinking levels accepted', value: 'low, high' }] },
    { group: 'Requests', items: [{ key: 'request.tools', label: 'Function tools', value: 'Yes' }] },
  ]);
  expect(knownCapabilities(undefined)).toEqual([]);
});

function renderControls(count = 3) {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}>
      <ToastProvider>
        <CapabilityProbeControls enabledChatModels={count} />
      </ToastProvider>
    </QueryClientProvider>,
  );
}

it('turns measuring on enable off, and measures every enabled model after a confirmation', async () => {
  vi.mocked(api.get).mockResolvedValue({ on_enable: true });
  vi.mocked(api.put).mockResolvedValue({ on_enable: false });
  vi.mocked(api.post).mockResolvedValue({ started: 2 });
  renderControls();
  const box = (await screen.findByLabelText('Measure what a model can do when it is enabled')) as HTMLInputElement;
  expect(box.checked).toBe(true);
  fireEvent.click(box);
  await waitFor(() => expect(api.put).toHaveBeenCalledWith('/api/v1/admin/system/capability-probe', { on_enable: false }));
  await waitFor(() => expect(box.checked).toBe(false));

  fireEvent.click(screen.getByRole('button', { name: 'Measure enabled models' }));
  expect(api.post).not.toHaveBeenCalled();
  expect(screen.getByText(/each enabled chat model \(3 now\)/)).toBeTruthy();
  fireEvent.click(screen.getByLabelText('Skip models that already have a measurement'));
  fireEvent.click(screen.getByRole('button', { name: 'Measure' }));
  await waitFor(() =>
    expect(api.post).toHaveBeenCalledWith('/api/v1/admin/models/capabilities/probe', { only_unmeasured: false }),
  );
  expect(await screen.findByText('Measuring 2 models, one after another')).toBeTruthy();
});

it('shows no controls on a server without the setting', async () => {
  vi.mocked(api.get).mockResolvedValue({ models: [] });
  const { container } = renderControls();
  await waitFor(() => expect(api.get).toHaveBeenCalled());
  expect(container.querySelector('[data-testid="capability-probe-controls"]')).toBeNull();
});
