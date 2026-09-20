/**
 * LookupInput must search as you type, keep the chosen ids on the wire and
 * names on the chips, and be fully keyboard-driven.
 */
import { describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { LookupInput, type LookupOption } from './LookupInput';

const PEOPLE: LookupOption[] = [
  { id: 'u-casey', label: 'Casey Sample', detail: 'casey@sample.test' },
  { id: 'u-alice', label: 'Alice Example', detail: 'alice@example.com' },
  { id: 'u-bob', label: 'Bob Builder', detail: 'bob@example.com' },
];

function Harness({
  allowFreeText = false,
  search,
}: {
  allowFreeText?: boolean;
  search?: (q: string) => Promise<LookupOption[]>;
}) {
  const [value, setValue] = useState<string[]>([]);
  const defaultSearch = async (q: string) =>
    PEOPLE.filter((p) => `${p.label} ${p.detail}`.toLowerCase().includes(q.toLowerCase()));
  return (
    <>
      <LookupInput
        label="People"
        value={value}
        onChange={setValue}
        search={search ?? defaultSearch}
        resolve={async (ids) => PEOPLE.filter((p) => ids.includes(p.id))}
        allowFreeText={allowFreeText}
        debounceMs={0}
      />
      <output data-testid="wire">{value.join(',')}</output>
    </>
  );
}

describe('LookupInput', () => {
  it('matches a fragment of the name as you type and puts the id on the wire', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.type(screen.getByRole('combobox', { name: 'People' }), 'samp');
    const opt = await screen.findByRole('option', { name: /Casey Sample/ });
    expect(screen.queryByRole('option', { name: /Alice/ })).toBeNull();
    await user.click(opt);
    expect(screen.getByTestId('wire').textContent).toBe('u-casey');
    expect(screen.getByText('Casey Sample')).toBeTruthy(); // chip shows the name
  });

  it('is keyboard driven: arrows move, Enter picks, Backspace removes the last chip', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    const box = screen.getByRole('combobox', { name: 'People' });
    await user.type(box, 'example');
    await screen.findByRole('option', { name: /Alice/ });
    await user.keyboard('{ArrowDown}{Enter}'); // second match: Bob
    expect(screen.getByTestId('wire').textContent).toBe('u-bob');
    await user.keyboard('{Backspace}');
    expect(screen.getByTestId('wire').textContent).toBe('');
  });

  it('never offers something already chosen, and only offers free text when allowed', async () => {
    const user = userEvent.setup();
    render(<Harness allowFreeText />);
    const box = screen.getByRole('combobox', { name: 'People' });
    await user.type(box, 'casey');
    await user.click(await screen.findByRole('option', { name: /Casey/ }));
    await user.type(box, 'casey');
    await waitFor(() => expect(screen.queryByRole('option', { name: /^Casey Sample/ })).toBeNull());
    expect(screen.getByRole('option', { name: /Use “casey” as typed/ })).toBeTruthy();
    await user.keyboard('{Enter}');
    expect(screen.getByTestId('wire').textContent).toBe('u-casey,casey');
  });

  it('drops a stale search response that lands after a newer query', async () => {
    const user = userEvent.setup();
    let release: (v: LookupOption[]) => void = () => {};
    const search = vi.fn((q: string) => {
      if (q === 'a') return new Promise<LookupOption[]>((r) => (release = r)); // slow
      return Promise.resolve(PEOPLE.filter((p) => p.label.toLowerCase().includes(q)));
    });
    render(<Harness search={search} />);
    const box = screen.getByRole('combobox', { name: 'People' });
    await user.type(box, 'a'); // slow request in flight
    await user.type(box, 'lice'); // now "alice": fast
    await screen.findByRole('option', { name: /Alice/ });
    release([PEOPLE[2]!]); // the stale "a" answer arrives late with Bob
    await new Promise((r) => setTimeout(r, 10));
    expect(screen.queryByRole('option', { name: /Bob/ })).toBeNull();
  });
});
