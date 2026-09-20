import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { BarList, DonutChart } from './charts';
import type { Breakdown } from '../lib/types';

afterEach(cleanup);
const row = (key: string, requests: number, cost = 0): Breakdown => ({
  key,
  label: key,
  totals: {
    request_count: requests,
    cost_nanousd: cost,
    tokens_in: 0,
    tokens_out: 0,
    tokens_cached: 0,
    tokens_cache_write_5m: 0,
    tokens_cache_write_1h: 0,
    error_count: 0,
  },
});

describe('bounded breakdown collections', () => {
  it('ranks by the current metric before selecting the preview and never claims database-wide ranking', () => {
    const items = [row('cheap', 100, 1), row('costly', 1, 100)];
    const view = render(<BarList items={items} metric="requests" max={1} />);
    expect(screen.getByRole('list').textContent).toContain('cheap');
    expect(screen.getByText(/within the returned server summary/i)).toBeTruthy();
    view.rerender(<BarList items={items} metric="cost" max={1} />);
    expect(screen.getByRole('list').textContent).toContain('costly');
    expect(items[0]?.key).toBe('cheap');
  });

  it('makes every returned row reachable with search and paging, keeping independent charts isolated', async () => {
    const user = userEvent.setup();
    const items = Array.from({ length: 31 }, (_, i) => row(`model-${i}`, i));
    render(
      <>
        <BarList items={items} metric="requests" />
        <BarList items={[row('other', 1)]} metric="requests" />
      </>,
    );
    await user.click(screen.getAllByRole('button', { name: /view all returned rows/i })[0]!);
    const table = screen.getByRole('table', { name: 'Returned summary rows' });
    expect(within(table).getAllByRole('row')).toHaveLength(26);
    await user.click(screen.getByRole('button', { name: /next/i }));
    expect(within(table).getAllByRole('row')).toHaveLength(7);
    await user.type(screen.getByRole('searchbox', { name: 'Search returned summary' }), 'model-30');
    expect(within(table).getAllByRole('row')).toHaveLength(2);
    expect(within(table).getByText('model-30')).toBeTruthy();
    expect(screen.getAllByRole('list')[1]?.textContent).toContain('other');
  });

  it('preserves the donut denominator by grouping omitted returned categories', () => {
    render(<DonutChart items={Array.from({ length: 10 }, (_, i) => row(`category-${i}`, 10))} metric="requests" />);
    expect(screen.getByText('Other returned categories')).toBeTruthy();
    expect(screen.getByText('30%')).toBeTruthy();
    expect(screen.getByText(/percentages cover the returned summary/i)).toBeTruthy();
  });
});
