import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/react';
import { seriesColor } from '../components/charts';
import type { TimePoint } from '../lib/types';
import { SortHeader, TOKEN_SIZE_THRESHOLDS, ascSortKey, tokenDirectionSeries, tokenSizeOptions, tokenSizeParams } from './shared';

describe('tokenSizeParams', () => {
  it('maps gt:<n> onto the strict greater-than param and leaves lt empty', () => {
    expect(tokenSizeParams('tokens_in', 'gt:10000')).toEqual({ tokens_in_gt: '10000', tokens_in_lt: '' });
  });

  it('maps lt:<n> onto the strict smaller-than param for the other direction', () => {
    expect(tokenSizeParams('tokens_out', 'lt:500000')).toEqual({ tokens_out_gt: '', tokens_out_lt: '500000' });
  });

  it('yields empty params for no filter or malformed input', () => {
    expect(tokenSizeParams('tokens_in', '')).toEqual({ tokens_in_gt: '', tokens_in_lt: '' });
    expect(tokenSizeParams('tokens_in', 'gt:abc')).toEqual({ tokens_in_gt: '', tokens_in_lt: '' });
    expect(tokenSizeParams('tokens_in', 'between:1:2')).toEqual({ tokens_in_gt: '', tokens_in_lt: '' });
  });

  it('offers a smaller-than and greater-than option for each of the four thresholds', () => {
    const options = tokenSizeOptions('Any');
    expect(options[0]).toEqual({ value: '', label: 'Any' });
    expect(options).toHaveLength(1 + TOKEN_SIZE_THRESHOLDS.length * 2);
    expect(options.map((o) => o.value)).toEqual([
      '',
      'lt:1000',
      'gt:1000',
      'lt:10000',
      'gt:10000',
      'lt:100000',
      'gt:100000',
      'lt:500000',
      'gt:500000',
    ]);
    expect(options.find((o) => o.value === 'gt:100000')?.label).toBe('Greater than 100,000');
    expect(options.find((o) => o.value === 'lt:1000')?.label).toBe('Smaller than 1,000');
  });
});

function renderHeader(active: string, onSort: (key: string) => void) {
  return render(
    <table>
      <thead>
        <tr>
          <SortHeader label="Cost" sortKey="cost" active={active} onSort={onSort} />
        </tr>
      </thead>
    </table>,
  );
}

function tp(bucket: string, tokensIn: number, tokensOut: number): TimePoint {
  return {
    bucket,
    totals: {
      tokens_in: tokensIn,
      tokens_out: tokensOut,
      tokens_cached: 0,
      tokens_cache_write_5m: 0,
      tokens_cache_write_1h: 0,
      cost_nanousd: 0,
      request_count: 0,
      error_count: 0,
    },
  };
}

describe('tokenDirectionSeries', () => {
  const series = [tp('a', 100, 50), tp('b', 200, 150)];

  it('builds the two-layer stacked split for total: tokens-in at the baseline, tokens-out on top', () => {
    const layers = tokenDirectionSeries(series, 'total');
    expect(layers.map((layer) => layer.key)).toEqual(['tokens_in', 'tokens_out']);
    expect(layers[0]!.values).toEqual([100, 200]);
    expect(layers[1]!.values).toEqual([50, 150]);
  });

  it('defaults to total when no direction is given', () => {
    expect(tokenDirectionSeries(series).map((layer) => layer.key)).toEqual(['tokens_in', 'tokens_out']);
  });

  it('builds a single tokens-in layer for in', () => {
    const layers = tokenDirectionSeries(series, 'in');
    expect(layers.map((layer) => layer.key)).toEqual(['tokens_in']);
    expect(layers[0]!.values).toEqual([100, 200]);
  });

  it('builds a single tokens-out layer for out', () => {
    const layers = tokenDirectionSeries(series, 'out');
    expect(layers.map((layer) => layer.key)).toEqual(['tokens_out']);
    expect(layers[0]!.values).toEqual([50, 150]);
  });

  it('keeps a stable colour per direction so In/Out match their layer in the Total view', () => {
    expect(tokenDirectionSeries(series, 'in')[0]!.color).toBe(seriesColor(0));
    expect(tokenDirectionSeries(series, 'out')[0]!.color).toBe(seriesColor(1));
    expect(tokenDirectionSeries(series, 'total').map((layer) => layer.color)).toEqual([seriesColor(0), seriesColor(1)]);
  });
});

describe('SortHeader', () => {
  it('maps time to the backend "oldest" ascending key', () => {
    expect(ascSortKey('time')).toBe('oldest');
    expect(ascSortKey('cost')).toBe('cost_asc');
  });

  it('sorts descending when the column is inactive', () => {
    const onSort = vi.fn();
    const { getByRole } = renderHeader('time', onSort);
    fireEvent.click(getByRole('button'));
    expect(onSort).toHaveBeenCalledWith('cost');
  });

  it('toggles to ascending on the active column and reports aria-sort (regression: clicking the active column was a no-op)', () => {
    const onSort = vi.fn();
    const { getByRole, rerender } = renderHeader('cost', onSort);
    expect(getByRole('columnheader').getAttribute('aria-sort')).toBe('descending');
    fireEvent.click(getByRole('button'));
    expect(onSort).toHaveBeenCalledWith('cost_asc');

    rerender(
      <table>
        <thead>
          <tr>
            <SortHeader label="Cost" sortKey="cost" active="cost_asc" onSort={onSort} />
          </tr>
        </thead>
      </table>,
    );
    expect(getByRole('columnheader').getAttribute('aria-sort')).toBe('ascending');
    fireEvent.click(getByRole('button'));
    expect(onSort).toHaveBeenLastCalledWith('cost');
  });
});
