import { expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { CapabilityResolved } from '../../lib/capabilities';
import { ChatSettingsDrawer, effortOptions } from './ChatSettings';

vi.mock('../../lib/api', async (original) => ({
  ...(await original<typeof import('../../lib/api')>()),
  api: { get: vi.fn().mockResolvedValue({ instructions: '' }), put: vi.fn(), post: vi.fn(), patch: vi.fn() },
}));

const at = '2026-11-13T10:00:00Z';
const fact = (value: CapabilityResolved['value'], source: CapabilityResolved['source'] = 'probe'): CapabilityResolved => ({
  value,
  source,
  at,
});
const values = (caps: Record<string, CapabilityResolved> | undefined, current = '', thinkingOff?: string) =>
  effortOptions(caps, current, thinkingOff).options.map((o) => o.value);

it('offers the thinking settings the model takes', () => {
  // Nothing known: the three every client knows.
  expect(values(undefined)).toEqual(['', 'low', 'medium', 'high']);
  expect(values({})).toEqual(['', 'low', 'medium', 'high']);
  // Its own levels, least to most, whatever order the provider listed them in.
  expect(values({ 'thinking.levels': fact(['xhigh', 'low', 'high', 'medium']) })).toEqual(['', 'low', 'medium', 'high', 'xhigh']);
  // A model without the setting.
  expect(effortOptions({ 'thinking.supported': fact(false) })).toEqual({
    options: [{ value: '', label: 'Model default', hint: 'Let the model decide how long to think.' }],
    note: 'This model does not have a thinking setting.',
  });
  expect(values({ 'thinking.supported': fact(true), 'thinking.levels': fact([]) })).toEqual(['']);
  // What the chat already holds stays visible on a model that does not take it.
  const moved = effortOptions({ 'thinking.levels': fact(['low', 'high']) }, 'max');
  expect(moved.options.map((o) => o.value)).toEqual(['', 'low', 'high', 'max']);
  expect(moved.options[3]?.hint).toContain('nearest');
});

it('offers Off only where Janus can turn thinking off', () => {
  const levels = { 'thinking.levels': fact(['low', 'medium', 'high', 'xhigh', 'max'], 'provider') };
  // Claude Haiku: the provider's own switch, which Janus sends.
  expect(values(levels, '', 'thinking_disabled')).toEqual(['', 'none', 'low', 'medium', 'high', 'xhigh', 'max']);
  expect(values({ 'thinking.levels': fact(['none', 'low', 'max']) }, '', 'reasoning_effort_none')).toEqual([
    '',
    'none',
    'low',
    'max',
  ]);
  expect(values(undefined, '', 'think_false')).toEqual(['', 'none', 'low', 'medium', 'high']);
  // Not known how to turn it off: no Off, even if a level list holds none.
  expect(values(levels)).toEqual(['', 'low', 'medium', 'high', 'xhigh', 'max']);
  expect(values({ 'thinking.levels': fact(['none', 'low', 'high']) })).toEqual(['', 'low', 'high']);
  // A model that always thinks has no Off, says so, and names its lowest setting.
  const always = effortOptions({ 'thinking.levels': fact(['high', 'low', 'none']) }, '', 'never');
  expect(always.options.map((o) => o.value)).toEqual(['', 'low', 'high']);
  expect(always.note).toBe('This model always thinks and cannot turn it off. Its lowest setting is Low.');
  expect(effortOptions(undefined, '', 'never').note).toBe('This model always thinks and cannot turn it off.');
  // A chat set to Off on such a model says what is sent instead.
  const held = effortOptions({ 'thinking.levels': fact(['medium', 'high']) }, 'none', 'never');
  expect(held.options.map((o) => o.value)).toEqual(['', 'medium', 'high', 'none']);
  expect(held.options[3]?.hint).toBe('This model always thinks; Medium is used instead.');
});

it('shows those settings in the chat settings panel', async () => {
  const drawer = (capabilities: Record<string, CapabilityResolved> | undefined, effort: string, thinkingOff?: string) =>
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <ChatSettingsDrawer
          open
          onClose={() => {}}
          instructions=""
          effort={effort}
          capabilities={capabilities}
          thinkingOff={thinkingOff}
          onSave={async () => {}}
        />
      </QueryClientProvider>,
    );
  drawer({ 'thinking.levels': fact(['none', 'low', 'xhigh']) }, 'xhigh', 'reasoning_effort_none');
  const names = () => screen.getAllByRole('radio').map((r) => (r as HTMLInputElement).value);
  expect(names()).toEqual(['', 'none', 'low', 'xhigh']);
  expect((screen.getByRole('radio', { name: /Extra high/ }) as HTMLInputElement).checked).toBe(true);
  expect(screen.getByRole('radio', { name: /^Off/ })).toBeTruthy();
  cleanup();
  drawer({ 'thinking.supported': fact(false) }, '');
  expect(names()).toEqual(['']);
  expect(screen.getByText(/does not have a thinking setting/)).toBeTruthy();
  cleanup();
  drawer(undefined, 'medium');
  expect(names()).toEqual(['', 'low', 'medium', 'high']);
  cleanup();
  drawer({ 'thinking.levels': fact(['low', 'high'], 'provider') }, '', 'never');
  expect(names()).toEqual(['', 'low', 'high']);
  expect(screen.getByText(/always thinks and cannot turn it off\. Its lowest setting is Low\./)).toBeTruthy();
});
