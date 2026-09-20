import { describe, it, expect, afterEach } from 'vitest';
import { render, screen, cleanup, within, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { Collection } from './Collection';
import { sortCollection, pageOffset } from '../lib/collections';
import { useCollectionState } from '../lib/collection-state';
afterEach(cleanup);
function Location() {
  return <output data-testid="url">{useLocation().search}</output>;
}
describe('complete collections', () => {
  it.each([true, false])('keeps blanks last and ties stable (ascending=%s)', (asc) => {
    const rows = [
      { v: null, id: 0 },
      { v: '', id: 1 },
      { v: undefined, id: 2 },
      { v: 2, id: 3 },
      { v: 2, id: 4 },
      { v: 1, id: 5 },
    ];
    const sorted = sortCollection(rows, (r) => r.v, asc);
    expect(sorted.slice(-3).map((r) => r.id)).toEqual([0, 1, 2]);
    expect(sorted.filter((r) => r.v === 2).map((r) => r.id)).toEqual([3, 4]);
    expect(rows[0]?.id).toBe(0);
  });
  it('normalizes invalid offsets and clamps a shrunken collection', () => {
    expect(pageOffset('bogus', 25)).toBe(0);
    expect(pageOffset('-5', 25)).toBe(0);
    expect(pageOffset('200', 25, 12)).toBe(0);
    expect(pageOffset('76', 25, 100)).toBe(75);
  });
  it('sorts and searches all 250 rows before paging, resets page atomically, retains other URL state', async () => {
    const user = userEvent.setup();
    const rows = Array.from({ length: 250 }, (_, id) => ({ id, name: `Item ${String(id).padStart(3, '0')}` }));
    render(
      <MemoryRouter initialEntries={['/?keep=yes&Inventory.page=25']}>
        <Collection
          name="Inventory"
          rows={rows}
          rowKey={(r) => String(r.id)}
          columns={[{ id: 'name', label: 'Name', value: (r) => r.name, render: (r) => r.name }]}
        />
        <Location />
      </MemoryRouter>,
    );
    expect(screen.getByText('Item 025')).toBeTruthy();
    await user.click(screen.getByRole('button', { name: /Name/ }));
    await user.click(screen.getByRole('button', { name: /Name/ }));
    expect(within(screen.getByRole('table')).getAllByRole('row')[1]?.textContent).toBe('Item 249');
    expect(screen.getByTestId('url').textContent).toContain('keep=yes');
    expect(screen.getByTestId('url').textContent).not.toContain('Inventory.page');
    await user.type(screen.getByRole('searchbox'), 'Item 001');
    expect(screen.getByText('Item 001')).toBeTruthy();
    expect(screen.queryByText('Item 249')).toBeNull();
  });
  it('repairs the URL after deletion shrinks the last page', async () => {
    const rows = Array.from({ length: 26 }, (_, id) => ({ id }));
    const View = ({ items }: { items: typeof rows }) => (
      <Collection
        name="Items"
        rows={items}
        rowKey={(r) => String(r.id)}
        columns={[{ id: 'id', label: 'ID', value: (r) => r.id, render: (r) => r.id }]}
      />
    );
    const view = render(
      <MemoryRouter initialEntries={['/?Items.page=25']}>
        <View items={rows} />
        <Location />
      </MemoryRouter>,
    );
    view.rerender(
      <MemoryRouter initialEntries={['/?Items.page=25']}>
        <View items={rows.slice(0, 2)} />
        <Location />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByTestId('url').textContent).not.toContain('Items.page'));
    expect(screen.getByRole('cell', { name: '0' })).toBeTruthy();
  });
  it('resets a server sort offset without discarding filters', async () => {
    function ServerSort() {
      const [sort, setSort] = useCollectionState('sort', 'time');
      return <button onClick={() => setSort('latency')}>{sort}</button>;
    }
    render(
      <MemoryRouter initialEntries={['/?page=100&user=later&token=abc']}>
        <ServerSort />
        <Location />
      </MemoryRouter>,
    );
    await userEvent.click(screen.getByRole('button'));
    expect(screen.getByTestId('url').textContent).toBe('?user=later&token=abc&sort=latency');
  });
});
