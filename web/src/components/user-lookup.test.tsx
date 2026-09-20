import { afterEach, it, expect, vi } from 'vitest';
import { render, screen, cleanup } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { LookupInput } from './LookupInput';
import { userLookup, principalLookup } from '../lib/lookups';
const mock = vi.hoisted(() => ({ get: vi.fn() }));
vi.mock('../lib/api', () => ({ api: mock }));
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});
it('searches the server and resolves a selected ID outside the suggested page independently', async () => {
  mock.get.mockImplementation(async (path: string) =>
    path.includes('/users/later')
      ? { user: { id: 'later', name: 'Selected person', email: 'selected@test' } }
      : { users: [{ id: 'beyond500', name: 'Beyond first page', email: 'later@test' }] },
  );
  const change = vi.fn();
  render(<LookupInput label="Person" value={['later']} onChange={change} {...userLookup} debounceMs={0} />);
  await screen.findByText('Selected person');
  await userEvent.type(screen.getByRole('combobox'), 'Beyond');
  await userEvent.click(await screen.findByRole('option', { name: /Beyond first page/ }));
  expect(change).toHaveBeenCalledWith(['later', 'beyond500']);
  expect(mock.get.mock.calls.some(([p]) => String(p).includes('search=Beyond'))).toBe(true);
  expect(mock.get.mock.calls.some(([p]) => String(p).includes('limit=500'))).toBe(false);
});
it('does not turn search failures into no-match claims', async () => {
  mock.get.mockRejectedValue(new Error('offline'));
  render(<LookupInput label="Person" value={[]} onChange={() => {}} {...userLookup} debounceMs={0} />);
  await userEvent.type(screen.getByRole('combobox'), 'Nobody');
  expect(await screen.findByRole('alert')).toHaveProperty('textContent', 'Search failed. Try again.');
  expect(screen.queryByText(/No matches/)).toBeNull();
});
it('does not hide service token suggestions behind a full user result page', async () => {
  mock.get.mockImplementation(async (path: string) =>
    path.includes('service-tokens')
      ? { service_tokens: [{ id: 'svc', name: 'Service' }] }
      : { users: Array.from({ length: 12 }, (_, i) => ({ id: String(i), name: `Person ${i}`, email: `${i}@test` })) },
  );
  const suggestions = await principalLookup.search('');
  expect(suggestions.some((s) => s.id === 'svc')).toBe(true);
  expect(suggestions).toHaveLength(12);
});
